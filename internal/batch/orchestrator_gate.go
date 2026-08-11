package batch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/config"
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
	return pollPRGateWithHeadAndReviewCommand(ctx, client, branch, "", false, config.DefaultReviewCommand, opts)
}

func pollPRGateAtHead(ctx context.Context, client github.Client, branch, headSHA string, opts runSessionOptions) gateResult {
	return pollPRGateWithHeadAndReviewCommand(ctx, client, branch, headSHA, true, config.DefaultReviewCommand, opts)
}

func pollPRGateWithHead(ctx context.Context, client github.Client, branch, headSHA string, requireHead bool, opts runSessionOptions) gateResult {
	return pollPRGateWithHeadAndReviewCommand(ctx, client, branch, headSHA, requireHead, config.DefaultReviewCommand, opts)
}

func pollPRGateWithHeadAndReviewCommand(ctx context.Context, client github.Client, branch, headSHA string, requireHead bool, reviewCommand string, opts runSessionOptions) gateResult {
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

		gate, err := checkPRExternalGateWithHeadAndReviewCommand(pollCtx, client, branch, headSHA, requireHead, reviewCommand)
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
	return checkPRExternalGateWithHeadAndReviewCommand(ctx, client, branch, "", false, config.DefaultReviewCommand)
}

func checkPRExternalGateAtHead(ctx context.Context, client github.Client, branch, headSHA string) (string, error) {
	return checkPRExternalGateWithHeadAndReviewCommand(ctx, client, branch, headSHA, true, config.DefaultReviewCommand)
}

func checkPRExternalGateWithHead(ctx context.Context, client github.Client, branch, headSHA string, requireHead bool) (string, error) {
	return checkPRExternalGateWithHeadAndReviewCommand(ctx, client, branch, headSHA, requireHead, config.DefaultReviewCommand)
}

func checkPRExternalGateAtHeadWithReviewCommand(ctx context.Context, client github.Client, branch, headSHA, reviewCommand string) (string, error) {
	return checkPRExternalGateWithHeadAndReviewCommand(ctx, client, branch, headSHA, true, reviewCommand)
}

func checkPRExternalGateWithHeadAndReviewCommand(ctx context.Context, client github.Client, branch, headSHA string, requireHead bool, reviewCommand string) (string, error) {
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
		triggerStatus, err := reviewTriggerStatus(ctx, client, pr.Number, reviewCommand)
		if err != nil {
			return "unavailable", err
		}
		if triggerStatus == reviewTriggerChangesRequested {
			return "failed", nil
		}
		if triggerStatus == reviewTriggerPending {
			return "pending", nil
		}
		return gateReadyToMerge, nil
	}

	return "pending", nil
}

// handleExternalGate turns a clean agent exit into a terminal external-gate
// result when the branch has an open PR waiting on CI or review. It is shared
// by fresh and continuation runs so an explicit intervention cannot re-enter
// the old failure/retry path while the same PR gate is still unresolved.
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
	reviewCommand := strings.TrimSpace(s.renderCfg.ReviewCommand)
	if reviewCommand == "" && s.cfg != nil {
		reviewCommand = s.cfg.EffectiveReviewCommand()
	}
	if reviewCommand == "" {
		reviewCommand = config.DefaultReviewCommand
	}
	gate, err := checkPRExternalGateWithHeadAndReviewCommand(ctx, s.deps.githubClient, branch, headSHA, true, reviewCommand)
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

	polled := pollPRGateWithHeadAndReviewCommand(ctx, s.deps.githubClient, branch, headSHA, true, reviewCommand, s.opts)
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

type reviewTriggerState string

const (
	reviewTriggerApproved         reviewTriggerState = "approved"
	reviewTriggerChangesRequested reviewTriggerState = "changes-requested"
	reviewTriggerPending          reviewTriggerState = "pending"
)

func reviewTriggerStatus(ctx context.Context, client github.Client, prNumber int, reviewCommand string) (reviewTriggerState, error) {
	reviewCommand = strings.TrimSpace(reviewCommand)
	if client == nil || prNumber <= 0 || reviewCommand == "" {
		return reviewTriggerApproved, nil
	}
	comments, err := client.ListPRComments(ctx, prNumber)
	if err != nil {
		return "", err
	}
	latest, ok := latestReviewComment(comments)
	if !ok {
		return reviewTriggerApproved, nil
	}
	triggerAt := latest.CreatedAt
	if !reviewTriggerPrefixMatches(latest.Body, reviewCommand) {
		if isInformalReviewApproval(latest.Body) {
			return reviewTriggerApproved, nil
		}
		return reviewTriggerPending, nil
	}
	if lister, ok := client.(github.PRReviewLister); ok {
		state, responded, err := formalReviewTriggerStatus(ctx, lister, prNumber, triggerAt)
		if err != nil {
			return "", err
		}
		if responded {
			return state, nil
		}
	}
	if lister, ok := client.(github.PRReviewCommentLister); ok {
		responded, err := hasLaterInlineReview(ctx, lister, prNumber, triggerAt)
		if err != nil {
			return "", err
		}
		if responded {
			return reviewTriggerPending, nil
		}
	}
	return reviewTriggerPending, nil
}

func latestReviewComment(comments []github.PRComment) (github.PRComment, bool) {
	if len(comments) == 0 {
		return github.PRComment{}, false
	}
	latest := comments[0]
	for _, comment := range comments[1:] {
		if latest.CreatedAt.IsZero() || comment.CreatedAt.IsZero() || comment.CreatedAt.After(latest.CreatedAt) || comment.CreatedAt.Equal(latest.CreatedAt) {
			latest = comment
		}
	}
	return latest, true
}

func formalReviewTriggerStatus(ctx context.Context, lister github.PRReviewLister, prNumber int, triggerAt time.Time) (reviewTriggerState, bool, error) {
	reviews, err := lister.ListPRReviews(ctx, prNumber)
	if err != nil {
		return "", false, err
	}
	var latestReview github.PRReview
	hasResponse := false
	for _, review := range reviews {
		if !reviewSurfaceAfterTrigger(triggerAt, review.CreatedAt) {
			continue
		}
		state := strings.ToUpper(strings.TrimSpace(review.State))
		if state != "COMMENTED" && state != "APPROVED" && state != "CHANGES_REQUESTED" {
			continue
		}
		if !hasResponse || reviewSurfaceLater(latestReview.CreatedAt, review.CreatedAt) {
			latestReview = review
			hasResponse = true
		}
	}
	if !hasResponse {
		return "", false, nil
	}
	if strings.EqualFold(strings.TrimSpace(latestReview.State), "CHANGES_REQUESTED") {
		return reviewTriggerChangesRequested, true, nil
	}
	if strings.EqualFold(strings.TrimSpace(latestReview.State), "APPROVED") {
		return reviewTriggerApproved, true, nil
	}
	return reviewTriggerPending, true, nil
}

func hasLaterInlineReview(ctx context.Context, lister github.PRReviewCommentLister, prNumber int, triggerAt time.Time) (bool, error) {
	comments, err := lister.ListPRReviewComments(ctx, prNumber)
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		if reviewSurfaceAfterTrigger(triggerAt, comment.CreatedAt) {
			return true, nil
		}
	}
	return false, nil
}

func isInformalReviewApproval(body string) bool {
	body = strings.ToLower(strings.TrimSpace(body))
	for _, phrase := range []string{
		"lgtm",
		"looks good",
		"looks great",
		"nice work",
		"good work",
		"approved",
		"ship it",
		"+1",
		"thumbs up",
		"all good",
		"all set",
		"good to go",
		"no major issues",
		"minor issues only",
	} {
		if strings.Contains(body, phrase) {
			return true
		}
	}
	return false
}

func reviewTriggerPrefixMatches(body, reviewCommand string) bool {
	if reviewCommand != config.DefaultReviewCommand {
		return strings.HasPrefix(strings.TrimSpace(body), reviewCommand)
	}
	lower := strings.ToLower(body)
	const commandPrefix = "/sandman"
	for offset := 0; offset < len(lower); {
		match := strings.Index(lower[offset:], commandPrefix)
		if match < 0 {
			return false
		}
		match += offset
		remainder := lower[match+len(commandPrefix):]
		if remainder != "" && (remainder[0] == ' ' || remainder[0] == '\t' || remainder[0] == '\n' || remainder[0] == '\r') {
			remainder = strings.TrimLeft(remainder, " \t\n\r")
			if strings.HasPrefix(remainder, "review") && (len(remainder) == len("review") || !isReviewCommandWord(remainder[len("review")])) {
				return true
			}
		}
		offset = match + len(commandPrefix)
	}
	return false
}

func isReviewCommandWord(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_'
}

func reviewSurfaceAfterTrigger(triggerAt, responseAt time.Time) bool {
	return triggerAt.IsZero() || responseAt.IsZero() || responseAt.After(triggerAt)
}

func reviewSurfaceLater(currentAt, candidateAt time.Time) bool {
	return currentAt.IsZero() || candidateAt.IsZero() || candidateAt.After(currentAt) || candidateAt.Equal(currentAt)
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
