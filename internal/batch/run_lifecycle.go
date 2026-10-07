package batch

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/runid"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// execute owns the resources and persistence for every row. Mode-specific
// inputs, retry preparation, and metadata remain private session policies.
func (s *runSession) execute(ctx context.Context) (AgentRunResult, bool) {
	issueDriven := s.isIssueDriven()
	// Re-entry can inherit an awaited endpoint even if preparation fails before
	// this invocation starts one. Only a legitimate await transfers ownership
	// back to the batch; all other returns (and panics) close the endpoint.
	retainEndpoint := false
	defer func() {
		if !retainEndpoint {
			if err := s.coord.stopCommandServer(s.issueNumber); err != nil && issueDriven {
				s.logLifecycleError("error", "stop command server", err)
			}
		}
	}()

	issue, branch, errResult, ok := s.prepareInputs(ctx)
	if !ok {
		return errResult, false
	}
	if s.mode != ModeContinue {
		if err := syncBaseBranch(s.deps.runSessionOpts, s.deps.sandboxFactory, ".", s.baseBranch); err != nil {
			return s.lifecycleEarlyFailure("sync base branch", branch, s.runID, err), false
		}
	}
	sandboxStarted := time.Now()
	var container sandbox.Container
	if s.containerAlloc != nil {
		lease, err := s.containerAlloc.Acquire()
		if err != nil {
			return s.lifecycleEarlyFailure("acquire container", branch, s.runID, err), false
		}
		defer lease.Release()
		container = lease.container
	}

	wt := s.sbFactory.NewSandbox(".", s.worktreeDir(), branch, s.baseBranch, container)
	opts, errResult, ok := s.startOptsFor(branch)
	if !ok {
		if issueDriven {
			s.emitEarlyFailure("resolve git identity", branch, nil)
		}
		return errResult, false
	}
	if err := wt.Start(opts); err != nil {
		return s.lifecycleEarlyFailure("start sandbox", branch, s.runID, err), false
	}
	// Register restoration immediately: even start extras or later persistence
	// failures must leave preserved container worktrees addressed on the host.
	defer func() { _ = wt.RestoreHostPaths() }()
	if errResult, ok := s.startExtras(ctx, branch, sandboxStarted); !ok {
		return errResult, false
	}

	activeKey := 0
	if issueDriven {
		activeKey = s.issueNumber
	}
	s.coord.registerActiveRun(activeKey, wt)
	defer s.coord.unregisterActiveRun(activeKey)

	// Register before spawning so batch shutdown cannot miss this supervisor.
	supervisorDone := make(chan struct{})
	s.coord.trackShutdownSupervisor(supervisorDone)
	sessionCtx, cancelSession := context.WithCancel(ctx)
	go func() {
		defer close(supervisorDone)
		<-sessionCtx.Done()
		if s.parentCtx != nil && s.parentCtx.Err() != nil {
			if proc := wt.Process(); proc != nil {
				<-superviseShutdown(ctx, proc, s.opts.killTimeout)
			}
		}
	}()
	defer func() {
		cancelSession()
		<-supervisorDone
	}()

	batchDir, manifest := s.initialRunManifest(branch, wt)
	runID := manifest.RunID
	if err := daemon.WriteRunManifest(batchDir, runID, manifest); err != nil {
		return s.lifecycleEarlyFailure("write run manifest", branch, runID, err), false
	}
	if errResult, ok := s.persistStartExtras(batchDir, manifest); !ok {
		return errResult, false
	}
	// Publish only after the artifacts are readable by command clients.
	if err := s.coord.startCommandServer(s.issueNumber, daemon.RunFolder(batchDir, runID), s.commander); err != nil {
		s.logLifecycleError("error", "start command server", err)
	}
	s.emitStarted(issue, branch, runID)

	logPath := s.runLogPathFor(runID)
	if issueDriven && !s.usageLimitProbe {
		if entryResult, entryStarted, handled := s.tryEntryResume(ctx, branch, wt, logPath, runID); handled {
			retainEndpoint = entryStarted && entryResult.Status == "await"
			return entryResult, entryStarted
		}
	}
	result, terminalExtras, started := s.runOnce(ctx, issue, branch, wt, logPath, runID, issueDriven && s.mode != ModeContinue, func(attempt int, previous AgentRunResult) (prompt.RenderConfig, *AgentRunResult) {
		if issueDriven {
			return s.prepareIssueAttempt(ctx, branch, wt, logPath, attempt, previous)
		}
		return s.preparePromptAttempt(ctx, branch, wt, logPath, attempt, previous)
	})
	if !issueDriven {
		// Preserve the public result's legacy ID even when the persisted ID was
		// minted from timestamp components or the fallback clock.
		result.Review = s.review
		result.RunID = s.runID
	}
	if !started {
		return result, false
	}
	if issueDriven {
		if result.ContextExhausted && result.CleanupError != nil {
			fmt.Fprintf(s.deps.errorLog, "warning: context-exhausted cleanup failed for issue %d: %v\n", s.issueNumber, result.CleanupError)
			if terminalExtras == nil {
				terminalExtras = make(map[string]any)
			}
			terminalExtras["cleanup_error"] = result.CleanupError.Error()
		}
		// Provider quota recovery is non-terminal and retains the per-run
		// endpoint for the batch-owned polling continuation.
		if !s.lifecycleTerminal && s.shouldAwaitUsageLimit(result) {
			if s.usageLimitDeadline.IsZero() {
				s.usageLimitDeadline = s.runtimeNow().Add(usageLimitRetryWindow)
			}
			result.UsageLimitDeadline = s.usageLimitDeadline
			result.Status = s.emitAwait(ctx, runID, result, map[string]any{
				"await_reason":                      "usage-limit",
				"usage_limit_poll_seconds":          int(usageLimitPollInterval / time.Second),
				"usage_limit_waited_seconds":        int(s.usageLimitWaited / time.Second),
				"usage_limit_retry_window_seconds":  int(usageLimitRetryWindow / time.Second),
				"usage_limit_deadline_unix_seconds": s.usageLimitDeadline.Unix(),
			})
		}
		if result.Status == "await" {
			retainEndpoint = true
			return result, true
		}
	}
	result.Status = s.finishTerminal(ctx, runID, result, terminalExtras, wt, branch)
	if issueDriven {
		s.verifyNoRemainingProcesses(runID)
	}
	return result, true
}

func (s *runSession) isIssueDriven() bool {
	return s.issueNumber > 0 && !s.review
}

func (s *runSession) prepareInputs(ctx context.Context) (*github.Issue, string, AgentRunResult, bool) {
	if !s.isIssueDriven() {
		return nil, s.branches[0], AgentRunResult{}, true
	}
	_ = s.runLogWriter()
	issue, err := fetchIssueContent(ctx, s.deps.githubClient, s.issueNumber)
	if err != nil {
		result := s.lifecycleEarlyFailure("fetch issue", s.branches[s.issueNumber], s.runID, err)
		// Fetch failure historically omits the branch in the returned result.
		result.Branch = ""
		return nil, "", result, false
	}
	s.issueState = issue.State
	branch := s.branches[s.issueNumber]
	if branch == "" {
		branch = BranchName(issue.Number, issue.Title, s.baseBranch)
	}
	return issue, branch, AgentRunResult{}, true
}

func (s *runSession) logLifecycleError(level, reason string, err error) {
	if s.isIssueDriven() {
		fmt.Fprintf(s.deps.errorLog, "%s: %s for issue %d: %v\n", level, reason, s.issueNumber, err)
	} else {
		fmt.Fprintf(s.deps.errorLog, "%s: %s for prompt-only run: %v\n", level, reason, err)
	}
}

// lifecycleEarlyFailure keeps the legacy distinction: prompt-only early
// failures have result metadata but no terminal event.
func (s *runSession) lifecycleEarlyFailure(reason, branch, runID string, err error) AgentRunResult {
	if reason == "fetch issue" {
		fmt.Fprintf(s.deps.errorLog, "error: fetch issue %d: %v\n", s.issueNumber, err)
	} else {
		s.logLifecycleError("error", reason, err)
	}
	if s.isIssueDriven() {
		s.emitEarlyFailure(reason, branch, err)
		return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "failure", Branch: branch}
	}
	return AgentRunResult{Status: "failure", Branch: branch, Review: s.review, RunID: runID}
}

func (s *runSession) startExtras(ctx context.Context, branch string, sandboxStarted time.Time) (AgentRunResult, bool) {
	if s.review && s.qualityRulesFile != "" {
		if err := s.copyQualityRulesIntoWorktree(branch); err != nil {
			s.logLifecycleError("warn", "copy quality rules into review worktree", err)
		}
	}
	if !s.isIssueDriven() {
		return AgentRunResult{}, true
	}
	s.coord.firstSandboxStart(sandboxStarted)
	blockedBy, err := recheckBlockedBy(ctx, s.deps.githubClient, s.externalBlockers)
	if err != nil {
		return s.lifecycleEarlyFailure("recheck blockers", branch, s.runID, err), false
	}
	runID := s.issueRunID()
	if len(blockedBy) > 0 {
		logBlocked(s.deps.eventLog, s.issueNumber, blockedBy, runID, s.batchID)
		return AgentRunResult{IssueNumber: s.issueNumber, Issue: issueRef(s.issueNumber), Status: "blocked", Branch: branch}, false
	}
	return AgentRunResult{}, true
}

func (s *runSession) initialRunManifest(branch string, wt sandbox.Sandbox) (string, batchindex.RunManifest) {
	runID := s.runID
	batchDir := s.deps.layout.BatchDir(s.batchID)
	manifestBatchID := s.batchID
	kind := batchindex.KindIssue
	if s.isIssueDriven() {
		runID = s.issueRunID()
		if s.batchID == "" {
			// Legacy issue callers write directly beneath BatchesDir without
			// changing the session's batch ID or started-event payload.
			batchDir = s.deps.layout.BatchesDir
			manifestBatchID = batchIDFromRunID(runID)
		}
	} else {
		if s.batchTS != "" && s.batchShortID != "" {
			// Canonical prompt IDs include the optional user-provided subject.
			runID = runid.NewRunID(runid.KindPromptOnly, s.userProvidedRunID, s.batchTS, s.batchShortID)
		} else if runID == "" {
			runID = fmt.Sprintf("run-0-%d", time.Now().UnixNano())
		}
		if s.batchID == "" {
			s.batchID = batchIDFromRunID(runID)
		}
		batchDir = s.deps.layout.BatchDir(s.batchID)
		manifestBatchID = s.batchID
		kind = batchindex.KindPromptOnly
		if s.review {
			kind = batchindex.KindReview
		}
	}
	manifest := batchindex.RunManifest{
		RunID:        runID,
		BatchID:      manifestBatchID,
		Issue:        s.issueNumber,
		Branch:       branch,
		BaseBranch:   s.baseBranch,
		WorktreePath: wt.WorkDir(),
		Kind:         kind,
		CreatedAt:    time.Now(),
		Status:       batchindex.RunManifestStatusActive,
	}
	if !s.isIssueDriven() {
		manifest.PR = s.prNumber
		manifest.PortalHidden = s.portalHidden
	}
	return batchDir, manifest
}

func (s *runSession) persistStartExtras(batchDir string, manifest batchindex.RunManifest) (AgentRunResult, bool) {
	if s.isIssueDriven() || !s.portalHidden {
		return AgentRunResult{}, true
	}
	batchManifest, err := daemon.ReadManifest(batchDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return s.lifecycleEarlyFailure("read batch manifest", manifest.Branch, manifest.RunID, err), false
		}
		batchManifest = daemon.BatchManifest{BatchId: manifest.BatchID, CreatedAt: manifest.CreatedAt}
	}
	if batchManifest.BatchId == "" {
		batchManifest.BatchId = manifest.BatchID
	}
	batchManifest.PortalHidden = true
	if err := daemon.WriteManifest(batchDir, batchManifest); err != nil {
		return s.lifecycleEarlyFailure("write batch manifest", manifest.Branch, manifest.RunID, err), false
	}
	return AgentRunResult{}, true
}

func (s *runSession) emitStarted(issue *github.Issue, branch, runID string) {
	if s.deps.eventLog == nil {
		return
	}
	promptSourceType, promptSourceValue := "current", ""
	switch {
	case s.renderCfg.PromptFlag != "":
		promptSourceType, promptSourceValue = "prompt", s.renderCfg.PromptFlag
	case s.renderCfg.TemplateFlag != "":
		promptSourceType, promptSourceValue = "template", s.renderCfg.TemplateFlag
	}
	payload := map[string]any{
		"branch":                 branch,
		"base_branch":            s.baseBranch,
		"prompt_source_type":     promptSourceType,
		"parallel":               s.parallel,
		"start_delay":            int(s.startDelay / time.Second),
		"review_timeout":         s.renderCfg.ReviewTimeout,
		"retries":                s.retries,
		"sandbox":                s.sandboxMode,
		"container_capacity":     s.containerCapacity,
		"container_capacity_set": s.containerCapacitySet,
		"max_containers":         s.maxContainers,
		"max_containers_set":     s.maxContainersSet,
	}
	if s.mode == ModeContinue {
		previousKey := 0
		if s.isIssueDriven() {
			previousKey = s.issueNumber
		}
		payload["previous_run_id"] = s.previousRunIDs[previousKey]
	}
	if s.isIssueDriven() {
		payload["issue_title"] = issue.Title
		if promptSourceValue != "" && s.mode != ModeContinue {
			payload["prompt_source_value"] = promptSourceValue
		}
	} else {
		if s.review {
			payload["review"] = true
			payload["pr_number"] = s.prNumber
			payload["review_focus"] = s.reviewFocus
			if s.issueNumber > 0 {
				// Linked reviews stay prompt-only, but retain their issue
				// association for portal/event consumers.
				payload["issue_number"] = s.issueNumber
			}
		}
		if s.portalHidden {
			payload["portal_hidden"] = true
		}
	}
	if len(s.renderCfg.PromptArgs) > 0 {
		payload["prompt_args"] = s.renderCfg.PromptArgs
	}
	if s.renderCfg.ReviewCommandSet {
		payload["review_command"] = s.renderCfg.ReviewCommand
	}
	if s.agentName != "" {
		payload["agent"] = s.agentName
	}
	if model := strings.TrimSpace(s.agentCfg.Model); model != "" {
		payload["model"] = model
	}
	if variant := strings.TrimSpace(s.variant); variant != "" {
		payload["variant"] = variant
	}
	if s.batchID != "" {
		payload["batch_id"] = s.batchID
	}
	eventType := "run.started"
	if s.mode == ModeContinue {
		eventType = "run.continued"
	}
	event := events.Event{
		Type:      eventType,
		Timestamp: time.Now(),
		RunID:     runID,
		Payload:   payload,
	}
	if s.isIssueDriven() {
		event.Issue = s.issueNumber
		event.IssueRef = issueRef(s.issueNumber)
	}
	_ = s.deps.eventLog.Log(event)
}
