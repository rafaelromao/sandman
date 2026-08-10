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
)

var (
	defaultGatePollInitial  = time.Millisecond
	defaultGatePollMaxSleep = time.Millisecond
	defaultGatePollBudget   = 5 * time.Millisecond
)

func pollPRGate(ctx context.Context, client github.Client, branch string, opts runSessionOptions) gateResult {
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

		gate, _ := checkPRExternalGate(pollCtx, client, branch)
		switch gate {
		case "resolved":
			return gateResolved
		case "failed":
			return gateFailed
		case "unavailable":
			lastLookupUnavailable = true
		case "none":
			return gatePollPRMissing
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

	if hasCIPending || review == "" || review == "REVIEW_REQUIRED" || mergeStatus == "BLOCKED" {
		return "pending", nil
	}

	return "pending", nil
}

// handleExternalGate turns a clean agent exit into a terminal external-gate
// result when the branch has an open PR waiting on CI or review. It is shared
// by fresh and continuation runs so an explicit intervention cannot re-enter
// the old failure/retry path while the same PR gate is still unresolved.
func (s *runSession) handleExternalGate(ctx context.Context, workDir, branch, logPath, runID string) (string, map[string]any, bool) {
	if s.deps.githubClient == nil {
		return "", nil, false
	}

	gate, err := checkPRExternalGate(ctx, s.deps.githubClient, branch)
	initialUnavailable := err != nil
	if err != nil && s.deps.errorLog != nil {
		fmt.Fprintf(s.deps.errorLog, "warning: external gate lookup for branch %q: %v\n", branch, err)
		gate = "pending"
	}

	if gate == "none" {
		return "", nil, false
	}
	if gate == "resolved" {
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	}
	if gate == "failed" {
		return s.blockExternalGate(workDir, logPath, runID, "failed")
	}
	if gate == "unavailable" && !initialUnavailable {
		return s.blockExternalGate(workDir, logPath, runID, "unavailable")
	}

	polled := pollPRGate(ctx, s.deps.githubClient, branch, s.opts)
	if polled == gateResolved {
		return s.confirmExternalGate(ctx, workDir, branch, logPath, runID)
	}
	if polled == gateFailed {
		return s.blockExternalGate(workDir, logPath, runID, "failed")
	}
	if polled == gatePollUnavailable || polled == gatePollPRMissing {
		return s.blockExternalGate(workDir, logPath, runID, "unavailable")
	}
	if initialUnavailable {
		return s.blockExternalGate(workDir, logPath, runID, "unavailable")
	}
	return s.blockExternalGate(workDir, logPath, runID, "pending")
}

func (s *runSession) confirmExternalGate(ctx context.Context, workDir, branch, logPath, runID string) (string, map[string]any, bool) {
	if checkPRMergedForIssue(ctx, s.deps.githubClient, branch, s.issueNumber) {
		return "success", nil, true
	}
	if mergedPRMissingClosingReference(ctx, s.deps.githubClient, branch, s.issueNumber) {
		return "failure", mergeCompletionFailureExtras(nil, s.issueNumber), true
	}
	return s.blockExternalGate(workDir, logPath, runID, "unverified")
}

func (s *runSession) blockExternalGate(workDir, logPath, runID, reason string) (string, map[string]any, bool) {
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
		_, writeErr := fmt.Fprintf(prefixed, "external gate %s: %s; next action: %s\n", reason, failure, nextAction)
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
