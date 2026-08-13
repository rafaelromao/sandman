package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/github"
)

type gateResult int

const (
	gateResolved gateResult = iota
	gateFailed
	gatePollBudgetExhausted
	gatePollUnavailable
	gatePollPRMissing
	gatePollReadyToMerge
)

const gateReadyToMerge = "ready-to-merge"

var (
	defaultGatePollInitial  = time.Millisecond
	defaultGatePollMaxSleep = time.Millisecond
	defaultGatePollBudget   = 5 * time.Millisecond
)

func pollPRGate(ctx context.Context, client github.Client, branch string, opts runSessionOptions) gateResult {
	return pollPRGateWithHead(ctx, client, branch, "", false, opts)
}

func pollPRGateAtHead(ctx context.Context, client github.Client, branch, headSHA string, opts runSessionOptions) gateResult {
	return pollPRGateWithHead(ctx, client, branch, headSHA, true, opts)
}

func pollPRGateWithHead(ctx context.Context, client github.Client, branch, headSHA string, requireHead bool, opts runSessionOptions) gateResult {
	initial := opts.gatePollInitial
	if initial <= 0 {
		initial = defaultGatePollInitial
	}
	maxSleep := opts.gatePollMaxSleep
	if maxSleep <= 0 {
		maxSleep = defaultGatePollMaxSleep
	}
	budget := opts.gatePollBudget
	if budget <= 0 {
		budget = defaultGatePollBudget
	}

	pollCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	deadline := time.Now().Add(budget)
	delay := initial
	lastLookupUnavailable := false

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastLookupUnavailable {
				return gatePollUnavailable
			}
			return gatePollBudgetExhausted
		}
		if delay > remaining {
			delay = remaining
		}
		select {
		case <-pollCtx.Done():
			if lastLookupUnavailable {
				return gatePollUnavailable
			}
			return gatePollBudgetExhausted
		case <-time.After(delay):
		}

		gate, err := checkPRExternalGateWithHead(pollCtx, client, branch, headSHA, requireHead)
		switch gate {
		case "resolved":
			return gateResolved
		case "failed":
			return gateFailed
		case "unavailable":
			if err == nil {
				return gatePollUnavailable
			}
			lastLookupUnavailable = true
		case "none":
			return gatePollPRMissing
		case gateReadyToMerge:
			return gatePollReadyToMerge
		default:
			lastLookupUnavailable = false
			delay = delay * 2
			if delay > maxSleep {
				delay = maxSleep
			}
		}
	}
}

func checkPRExternalGate(ctx context.Context, client github.Client, branch string) (string, error) {
	return checkPRExternalGateWithHead(ctx, client, branch, "", false)
}

func checkPRExternalGateAtHead(ctx context.Context, client github.Client, branch, headSHA string) (string, error) {
	return checkPRExternalGateWithHead(ctx, client, branch, headSHA, true)
}

func checkPRExternalGateWithHead(ctx context.Context, client github.Client, branch, headSHA string, requireHead bool) (string, error) {
	if client == nil || strings.TrimSpace(branch) == "" {
		return "none", nil
	}
	pr, err := client.FindPRByBranch(ctx, branch)
	if err != nil {
		return "unavailable", err
	}
	if pr == nil {
		return "none", err
	}
	if pr.Merged || strings.EqualFold(pr.State, "merged") {
		return "resolved", nil
	}
	if !strings.EqualFold(pr.State, "open") {
		return "unavailable", nil
	}

	checkRollup := strings.ToLower(strings.TrimSpace(pr.StatusCheckRollup))
	hasCIPending := checkRollup == "pending"
	hasCIFailure := checkRollup == "failure"
	review := strings.ToUpper(strings.TrimSpace(pr.ReviewDecision))
	mergeStatus := strings.ToUpper(strings.TrimSpace(pr.MergeStateStatus))

	if hasCIFailure {
		return "failed", nil
	}
	if review == "CHANGES_REQUESTED" {
		return "failed", nil
	}
	if mergeStatus == "DIRTY" || mergeStatus == "CONFLICTING" {
		return "failed", nil
	}

	if hasCIPending || review == "REVIEW_REQUIRED" || mergeStatus == "BLOCKED" {
		return "pending", nil
	}
	if requireHead {
		if strings.TrimSpace(headSHA) == "" || strings.TrimSpace(pr.HeadRefOid) == "" {
			return "pending", nil
		}
		if !strings.EqualFold(pr.HeadRefOid, headSHA) {
			return "pending", nil
		}
	}
	if (checkRollup == "" || checkRollup == "success") && (review == "" || review == "APPROVED") && mergeStatus == "CLEAN" {
		return gateReadyToMerge, nil
	}

	return "pending", nil
}

// handleExternalGate turns an AgentRun exit into a terminal external-gate
// result when the branch has a PR waiting on CI or review. It is shared by
// fresh and continuation runs so an explicit intervention cannot re-enter the
// old failure/retry path while the same PR gate is still unresolved.
func (s *runSession) handleExternalGate(ctx context.Context, workDir, branch, logPath, runID string) (string, map[string]any, bool) {
	return s.handleExternalGateWithHostPaths(ctx, workDir, branch, logPath, runID, true)
}

func (s *runSession) handleExternalGateWithHostPaths(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) (string, map[string]any, bool) {
	if s.deps.githubClient == nil {
		return "", nil, false
	}

	headSHA := s.currentGateHead(workDir)
	if !hostPathsReady {
		headSHA = ""
	}
	if gateStatus, extras, handled := s.handleReviewTimeoutGate(ctx, workDir, branch, logPath, runID, headSHA); handled {
		return gateStatus, extras, true
	}
	gate, err := checkPRExternalGateWithHead(ctx, s.deps.githubClient, branch, headSHA, true)
	initialUnavailable := err != nil
	if err != nil && s.deps.errorLog != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: external gate lookup for branch %q: %v\n", branch, err)
		gate = "pending"
	}

	if gate == "none" {
		return "", nil, false
	}
	if gate == gateReadyToMerge {
		return s.blockExternalGate(ctx, workDir, logPath, runID, gateReadyToMerge)
	}
	if gate == "resolved" {
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	}
	if gate == "failed" {
		return s.blockExternalGate(ctx, workDir, logPath, runID, "failed")
	}
	if gate == "unavailable" && !initialUnavailable {
		return s.blockExternalGate(ctx, workDir, logPath, runID, "unavailable")
	}

	polled := pollPRGateWithHead(ctx, s.deps.githubClient, branch, headSHA, true, s.opts)
	if polled == gateResolved {
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	}
	if polled == gatePollReadyToMerge {
		return s.blockExternalGate(ctx, workDir, logPath, runID, gateReadyToMerge)
	}
	if polled == gateFailed {
		return s.blockExternalGate(ctx, workDir, logPath, runID, "failed")
	}
	if polled == gatePollUnavailable || polled == gatePollPRMissing {
		return s.blockExternalGate(ctx, workDir, logPath, runID, "unavailable")
	}
	return s.blockExternalGate(ctx, workDir, logPath, runID, "pending")
}

func (s *runSession) handleReviewTimeoutGate(ctx context.Context, workDir, branch, logPath, runID, currentHead string) (string, map[string]any, bool) {
	if !reviewTimeoutArtifactsPresent(workDir) {
		return "", nil, false
	}
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	repository, err := s.deps.githubClient.RepoName(ctx)
	if err != nil {
		return s.blockReviewTimeoutStateError(ctx, workDir, logPath, runID)
	}
	pr, err := s.deps.githubClient.FindPRByBranch(ctx, branch)
	if err != nil || pr == nil {
		return s.blockReviewTimeoutStateError(ctx, workDir, logPath, runID)
	}
	handoff, err := readReviewTimeoutHandoff(workDir, repository, pr, currentHead)
	if err != nil {
		return s.blockReviewTimeoutStateError(ctx, workDir, logPath, runID)
	}
	if handoff == nil {
		if pr.Merged || strings.EqualFold(pr.State, "merged") {
			return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
		}
		return "", nil, false
	}
	if pr.Merged || strings.EqualFold(pr.State, "merged") {
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	}
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	s.recordReviewTimeoutGateBlocker(workDir, logPath, runID, handoff)
	extras := handoff.payload()
	extras["blocker"] = "external-gate"
	extras["gate"] = gateReviewTimeout
	return "blocked", extras, true
}

func (s *runSession) blockReviewTimeoutStateError(ctx context.Context, workDir, logPath, runID string) (string, map[string]any, bool) {
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	s.recordExternalGateBlocker(workDir, logPath, runID, gateReviewTimeoutError)
	return "blocked", map[string]any{
		"blocker":     "external-gate",
		"gate":        gateReviewTimeoutError,
		"reason":      "REVIEW_TIMEOUT_STATE_ERROR",
		"next_action": "repair or remove the invalid retained review request, then continue only after confirming a new review trigger",
	}, true
}

func (s *runSession) currentGateHead(workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	resolver := s.opts.currentHead
	if resolver == nil {
		resolver = currentBranchHeadFn
	}
	headSHA, err := resolver(workDir)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(headSHA)
}

func (s *runSession) confirmExternalGate(ctx context.Context, workDir, branch, logPath, runID string) (string, map[string]any, bool) {
	if checkPRMergedForIssue(ctx, s.deps.githubClient, branch, s.issueNumber) {
		return "success", nil, true
	}
	if mergedPRMissingClosingReference(ctx, s.deps.githubClient, branch, s.issueNumber) {
		return "failure", mergeCompletionFailureExtras(nil, s.issueNumber), true
	}
	return s.blockExternalGate(ctx, workDir, logPath, runID, "unverified")
}

func (s *runSession) blockExternalGate(ctx context.Context, workDir, logPath, runID, reason string) (string, map[string]any, bool) {
	if ctx.Err() != nil {
		return "aborted", nil, true
	}
	s.recordExternalGateBlocker(workDir, logPath, runID, reason)
	return "blocked", map[string]any{"blocker": "external-gate", "gate": reason}, true
}

func (s *runSession) recordExternalGateBlocker(workDir, logPath, runID, reason string) {
	failure := fmt.Sprintf("pull request external gate is %s", reason)
	nextAction := "recheck CI and delegated review, then continue the run when intervention is required"
	if reason == "failed" {
		nextAction = "inspect the failed CI or requested review changes, then continue the run to address them"
	}
	if reason == gateReviewTimeoutError {
		nextAction = "repair or remove the invalid retained review request, then continue only after confirming a new review trigger"
	}
	blocker := fmt.Sprintf("\n\n## External Gate\n\n- Failure: %s.\n- Next action: %s.\n", failure, nextAction)
	logSummary := failure
	if reason == gateReadyToMerge {
		nextAction = "revalidate current-head approval, CI, and mergeability, then execute the normal pull-request merge gate"
		blocker = fmt.Sprintf("\n\n## External Gate\n\n- State: pull request external gate is ready-to-merge.\n- Next action: %s.\n", nextAction)
		logSummary = "pull request is ready to merge"
	}

	if strings.TrimSpace(workDir) != "" {
		taskPath := filepath.Join(workDir, ".sandman", "task.md")
		content, err := os.ReadFile(taskPath)
		if err != nil && !os.IsNotExist(err) {
			s.logExternalGateWriteError("read task.md", err)
		} else {
			updated := replaceExternalGateBlocker(content, blocker)
			if err := atomicfs.WriteAtomic(taskPath, updated, 0o644); err != nil {
				s.logExternalGateWriteError("write task.md", err)
			}
		}
	}

	if strings.TrimSpace(logPath) != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			s.logExternalGateWriteError("create run.log directory", err)
			return
		}
		file, err := atomicfs.OpenAppend(logPath, 0o644)
		if err != nil {
			s.logExternalGateWriteError("open run.log", err)
			return
		}
		prefixed := NewLinePrefixWriter(runID, file)
		_, writeErr := fmt.Fprintf(prefixed, "external gate %s: %s; next action: %s\n", reason, logSummary, nextAction)
		flushErr := prefixed.Flush()
		closeErr := file.Close()
		if writeErr != nil {
			s.logExternalGateWriteError("write run.log", writeErr)
		}
		if flushErr != nil {
			s.logExternalGateWriteError("flush run.log", flushErr)
		}
		if closeErr != nil {
			s.logExternalGateWriteError("close run.log", closeErr)
		}
	}
}

func (s *runSession) recordReviewTimeoutGateBlocker(workDir, logPath, runID string, handoff *reviewTimeoutHandoff) {
	request := handoff.Request
	counts := handoff.ResponseCounts
	blocker := fmt.Sprintf(
		"\n\n## External Gate\n\n- State: delegated review request exhausted its deadline.\n- Reason: %s.\n- Pull request: #%d.\n- Current head: %s.\n- Trigger: %s.\n- Deadline: %s (%d).\n- Budget: %d seconds.\n- Response counts: top-level=%d, formal=%d, inline=%d.\n- Next action: %s.\n",
		reviewTimeoutReason,
		request.PullRequest,
		request.HeadSHA,
		request.TriggerID,
		request.DeadlineAt,
		request.DeadlineUnixSeconds,
		request.EffectiveTimeout,
		counts.TopLevel,
		counts.FormalReviews,
		counts.Inline,
		reviewTimeoutNextAction,
	)

	if strings.TrimSpace(workDir) != "" {
		taskPath := filepath.Join(workDir, ".sandman", "task.md")
		content, err := os.ReadFile(taskPath)
		if err != nil && !os.IsNotExist(err) {
			s.logExternalGateWriteError("read task.md", err)
		} else {
			updated := replaceExternalGateBlocker(content, blocker)
			if err := atomicfs.WriteAtomic(taskPath, updated, 0o644); err != nil {
				s.logExternalGateWriteError("write task.md", err)
			}
		}
	}

	if strings.TrimSpace(logPath) == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		s.logExternalGateWriteError("create run.log directory", err)
		return
	}
	file, err := atomicfs.OpenAppend(logPath, 0o644)
	if err != nil {
		s.logExternalGateWriteError("open run.log", err)
		return
	}
	prefixed := NewLinePrefixWriter(runID, file)
	_, writeErr := fmt.Fprintf(prefixed, "external gate %s: %s; pull request #%d head %s trigger %s deadline %s budget %d counters top-level=%d formal=%d inline=%d; next action: %s\n", gateReviewTimeout, reviewTimeoutReason, request.PullRequest, request.HeadSHA, request.TriggerID, request.DeadlineAt, request.EffectiveTimeout, counts.TopLevel, counts.FormalReviews, counts.Inline, reviewTimeoutNextAction)
	flushErr := prefixed.Flush()
	closeErr := file.Close()
	if writeErr != nil {
		s.logExternalGateWriteError("write run.log", writeErr)
	}
	if flushErr != nil {
		s.logExternalGateWriteError("flush run.log", flushErr)
	}
	if closeErr != nil {
		s.logExternalGateWriteError("close run.log", closeErr)
	}
}

func replaceExternalGateBlocker(content []byte, blocker string) []byte {
	text := string(content)
	const heading = "## External Gate"
	start := strings.Index(text, heading)
	if start < 0 {
		return append(append([]byte(nil), content...), []byte(blocker)...)
	}

	end := len(text)
	rest := text[start+len(heading):]
	if next := strings.Index(rest, "\n## "); next >= 0 {
		end = start + len(heading) + next + 1
	}
	updated := text[:start] + strings.TrimSpace(blocker) + "\n\n" + text[end:]
	return []byte(updated)
}

func (s *runSession) logExternalGateWriteError(operation string, err error) {
	if s.deps.errorLog != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: %s for external gate blocker: %v\n", operation, err)
	}
}
