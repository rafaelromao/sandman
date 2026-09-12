package batch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/shellenv"
)

func readRetryLogLines(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{}
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	lines := make([]string, 0, len(parts))
	for _, line := range parts {
		if !isRetryMarker(line) {
			lines = append(lines, line)
		}
	}
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

func isRetryMarker(line string) bool {
	const (
		prefix = "--- retry "
		suffix = " ---"
	)
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
		return false
	}
	attempt, maxAttempts, ok := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(line, prefix), suffix), "/")
	if !ok || attempt == "" || maxAttempts == "" {
		return false
	}
	for _, value := range []string{attempt, maxAttempts} {
		for _, r := range value {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// mapRetryReason picks the closed-vocabulary reason for a run.retry emit
// from the previous attempt's status, the heartbeat-trips signal, and the
// parent context. The vocabulary (agent-stalled, agent-failed,
// sandbox-timeout, kill-timeout, manual, context-exhausted) is locked in
// ADR-0030 and the context-rollover contract
// and must not be silently extended. If a future code path
// surfaces a status that does not map to a known arm, the function
// panics so the new condition is added to the ADR and the mapping
// explicitly, rather than collapsing to an empty string that violates
// the slice-3 contract "never null, never empty" (#1501 acceptance #3).
func mapRetryReason(previousStatus string, abortedByHeartbeat bool, parentCtx context.Context) string {
	switch previousStatus {
	case "failure":
		return "agent-failed"
	case "aborted":
		if abortedByHeartbeat {
			return "agent-stalled"
		}
		if parentCtx != nil && parentCtx.Err() != nil {
			return "kill-timeout"
		}
	}
	panic(fmt.Sprintf("mapRetryReason: unmapped previous_status=%q abortedByHeartbeat=%v; add a vocabulary arm via ADR-0035", previousStatus, abortedByHeartbeat))
}

// logRetry writes a run.retry event at the top of a retry iteration. It is
// called from runOnce for both the issue-driven and prompt-only loops, with
// attempt (1-indexed, the about-to-start attempt), maxAttempts, and the
// status of the previous iteration passed through verbatim. branch is the
// run's branch; logPath is the per-run log file the heartbeat and the retry
// event both tail. issueNumber == 0 denotes a prompt-only run, matching the
// existing prompt-only convention (issue: 0 in the JSON payload). No-op when
// the orchestrator has no event log.
func logRetry(eventLog events.EventLog, runID, branch string, attempt, maxAttempts int, previousStatus, reason, logPath string, issueNumber int) {
	if eventLog == nil {
		return
	}
	event := events.Event{
		Type:      "run.retry",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     issueNumber,
		Payload: map[string]any{
			"attempt":         attempt,
			"max_attempts":    maxAttempts,
			"previous_status": previousStatus,
			"reason":          reason,
			"branch":          branch,
			"last_log_lines":  readRetryLogLines(logPath, 3),
		},
	}
	if issueNumber > 0 {
		event.IssueRef = issueRef(issueNumber)
	}
	_ = eventLog.Log(event)
}

// runOnce runs the retry loop for a session. mergeRequired gates the
// issue-driven flavour's checkPRMerged check (the sole success signal);
// prepareAttempt returns (_, &errResult) to short-circuit. A short-circuit
// result with Status="success" (e.g. the pre-retry guard on a merged PR)
// propagates as a started run so the terminal success event is emitted;
// any other short-circuit status propagates as a non-started failure.
//
// The returned `terminalExtras` carries extra payload keys the terminal
// event should merge in (e.g. "blocker" / "pr_number" when an open PR
// blocks the run from being declared success — see issue #1684). It may
// be nil. Started mirrors the second return value of the original
// signature.
func (s *runSession) runOnce(
	ctx context.Context,
	issue *github.Issue,
	branch string,
	wt sandbox.Sandbox,
	logPath string,
	runID string,
	mergeRequired bool,
	prepareAttempt func(attempt int, previous AgentRunResult) (prompt.RenderConfig, *AgentRunResult),
) (AgentRunResult, map[string]any, bool) {
	if s.renderCfg.PromptFile == "" {
		s.renderCfg.PromptFile = filepath.Join(".", ".sandman", "prompt.md")
	}
	if s.renderCfg.RenderedPromptFile == "" {
		s.renderCfg.RenderedPromptFile = filepath.Join(".", ".sandman", "task.md")
	}

	attempts := s.retries + 1
	var result AgentRunResult
	var abortedByHeartbeat bool

	factory := s.deps.runnableFactory
	if factory == nil {
		factory = defaultRunnableFactory{}
	}

	var terminalExtras map[string]any
loop:
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Session reuse is a launch choice, not retry state. Retries and
			// context-rollover recovery always start a fresh conversation.
			s.reuseSession = false
			// An operator cancellation must win before recovery can replace the
			// Task or start a fresh session.
			if result.ContextExhausted && ctx.Err() != nil {
				result.ContextExhausted = false
				break loop
			}
			if result.ContextExhausted {
				// Container sandboxes leave worktree metadata addressed inside the
				// container after Exec. Restore it before the recovery Task is
				// written; ContainerSandbox.Exec reapplies container paths for the
				// next command.
				if err := wt.RestoreHostPaths(); err != nil {
					fmt.Fprintf(s.deps.errorLog, "error: restore host paths before context recovery: %v\n", err)
					result.Status = "failure"
					break loop
				}
			}
		}
		attemptRenderCfg, errResult := prepareAttempt(attempt, result)
		if errResult != nil {
			return *errResult, nil, events.RunStatusFromPayload(errResult.Status).IsSuccess()
		}
		// prepareAttempt builds a recovery Task from the preserved Task. Check
		// again after that work so cancellation cannot launch its replacement.
		if result.ContextExhausted && ctx.Err() != nil {
			result.ContextExhausted = false
			break loop
		}

		if attempt > 0 {
			reason := mapRetryReason(result.Status, abortedByHeartbeat, s.parentCtx)
			if result.ContextExhausted {
				reason = contextExhaustedRetryReason
			}
			logRetry(s.deps.eventLog, runID, branch, attempt+1, attempts, result.Status, reason, logPath, s.issueNumber)
		}

		var runnable Runnable
		// When launching a continuation, copy the original
		// task.md that lives in the worktree into the new per-row run
		// folder as a sibling of run.json / run.log. The worktree file
		// is about to be overwritten by the continuation prompt; this
		// snapshot preserves the prior wording as a historical artifact
		// for the operator to revisit. The copy is best-effort: if the
		// worktree's task.md is missing (already warned about upstream)
		// we silently skip the snapshot — the operator still has the
		// live run.log and event log to reconstruct state. The
		// runFolder is the same path the AgentRun (or any other
		// Runnable implementation that respects the per-row folder)
		// would write to, so the snapshot lands alongside run.json /
		// run.log regardless of which Runnable factory is in use.
		if s.mode == ModeContinue {
			runFolder := s.runFolderFor(runID)
			if runFolder != "" {
				if err := snapshotOriginalTask(wt.WorkDir(), runFolder); err != nil {
					fmt.Fprintf(s.deps.errorLog, "warning: snapshot task.md for continuation run %s: %v\n", runID, err)
				}
			}
		}
		var alreadyResolved bool
	relaunch:
		// In-session resume loop (issue #2595): the agent may be relaunched
		// within the same attempt when the PR gate turns resume-worthy
		// (ready-to-merge / actionable-feedback) after a clean completion.
		// A resume relaunch reuses the attempt index — no run.retry, no
		// retry branch reset, no snapshot — and carries the request-scoped
		// review evidence in the prompt. The per-session resume cap
		// (awaitResumeMax) bounds the loop; exhaustion falls back to
		// run.await on the same gate.
		for {
			runnable = factory.NewRunnable(issue, branch, wt)
			if agentRun, ok := runnable.(*AgentRun); ok {
				agentRun.env = s.agentCfg.Env
				agentRun.preset = s.agentCfg.Preset
				agentRun.contextRolloverLiterals = append([]string(nil), s.opts.contextRolloverLiterals...)
				if s.opts.taskWriter != nil {
					agentRun.taskWriter = s.opts.taskWriter
				}
				agentRun.model = s.agentCfg.Model
				agentRun.modelProvider = s.agentCfg.ModelProvider
				agentRun.modelName = s.agentCfg.ModelName
				agentRun.variant = s.variant
				agentRun.opencodePermissionMode = s.agentCfg.OpencodePermissionMode
				agentRun.baseBranch = s.baseBranch
				agentRun.runID = runID
				agentRun.review = s.review
				agentRun.outputWriter = s.outputWriter
				agentRun.dangerouslySkipPermissions = &s.dangerouslySkipPermissions
				agentRun.sessionName = "Sandman " + runID + ": "
				agentRun.runFolder = s.runFolderFor(runID)
				agentRun.batchID = s.batchID
				agentRun.previousRunID = s.previousRunIDs[s.issueNumber]
				agentRun.previousBatchID = s.previousRunBatchIDs[s.issueNumber]
				agentRun.reuseSession = s.reuseSession
				agentRun.sessionWarning = s.deps.errorLog
			}

			s.reviewRegistrationAttempted = false
			s.reviewRegistrationObserved = false
			s.reviewAttemptStartedAt = s.reviewNow()
			result, abortedByHeartbeat = s.withHeartbeat(ctx, runID, attempt, logPath, wt, func() AgentRunResult {
				return s.withClosingReferenceGuard(ctx, branch, func() AgentRunResult {
					return runnable.Run(ctx, s.deps.renderer, s.agentCfg.Command, attemptRenderCfg)
				})
			})
			if result.Issue == nil && s.issueNumber > 0 {
				result.Issue = issueRef(s.issueNumber)
			}
			if result.IssueNumber == 0 && s.issueNumber > 0 {
				result.IssueNumber = s.issueNumber
			}
			result.RetriesTotal = attempt + 1

			taskPath := filepath.Join(wt.WorkDir(), ".sandman", "task.md")
			taskContent, _, _ := ReadTaskContent(taskPath)
			alreadyResolved = hasExactTaskStatus(taskContent, "## Status: already resolved")
			if s.issueNumber > 0 && !(alreadyResolved && s.mode != ModeContinue) && events.RunStatusFromPayload(result.Status).IsSuccess() && ctx.Err() == nil {
				hostPathsReady := s.restoreHostPathsBeforeExternalGate(wt)
				if gateStatus, extras, handled := s.handleLifecycleDecisionAfterAgent(ctx, wt.WorkDir(), branch, logPath, runID, hostPathsReady); handled {
					if gateStatus == "success" || gateStatus == "failure" || gateStatus == "aborted" {
						// A terminal lifecycle decision is authoritative. Do not
						// let the legacy post-decision PR arbitration replace it.
						result.Status = gateStatus
						terminalExtras = mergeBlockerExtras(terminalExtras, extras)
						break loop
					}
					gate, _ := extras["gate"].(string)
					observe := gateStatus == "await" && (gate != gateReadyToMerge && gate != gateActionableFeedback || s.resumeCount >= s.resumeCapFor())
					observe = observe || gateStatus == "resume" && s.resumeCount >= s.resumeCapFor()
					if observe {
						if gateStatus == "resume" {
							gateStatus = "await"
						}
						if !s.opts.foregroundLifecycle {
							s.emitAwait(ctx, runID, result, extras)
							result.Status = gateStatus
							break loop
						}
						s.emitAwait(ctx, runID, result, extras)
						gateStatus, extras, _ = s.observeLifecycle(ctx, wt.WorkDir(), branch, logPath, runID, result, extras, hostPathsReady)
					}
					if resumePrompt, resume := s.resumePromptFromGate(ctx, wt, branch, runID, extras); resume {
						s.reuseSession = true
						s.previousRunIDs = map[int]string{s.issueNumber: runID}
						s.previousRunBatchIDs = map[int]string{s.issueNumber: s.batchID}
						attemptRenderCfg.TaskPrompt = resumePrompt
						continue relaunch
					}
					if gateStatus == "resume" {
						gate, _ := extras["gate"].(string)
						if isCIRemediationGate(gate) {
							gateStatus = "await"
						} else {
							gateStatus = "failure"
							extras = map[string]any{
								"gate":        gate,
								"reason":      "REMEDIATION_BUDGET_EXHAUSTED",
								"next_action": "advance the pull-request head before requesting another remediation run",
							}
						}
					}
					result.Status = gateStatus
					terminalExtras = mergeBlockerExtras(terminalExtras, extras)
					break loop
				}
			}
			break relaunch
		}
		if mergeRequired {
			prMerged := checkPRMergedForIssue(ctx, s.deps.githubClient, branch, s.issueNumber)
			if events.RunStatusFromPayload(result.Status).IsAborted() {
				continue
			}
			if events.RunStatusFromPayload(result.Status).IsSuccess() && mergedPRMissingClosingReference(ctx, s.deps.githubClient, branch, s.issueNumber) {
				terminalExtras = mergeCompletionFailureExtras(terminalExtras, s.issueNumber)
				result.Status = "failure"
				break
			}
			if prMerged || alreadyResolved {
				if ctx.Err() != nil {
					break
				}
				if alreadyResolved {
					pr := lookupPRForVerify(ctx, s.deps.githubClient, s.deps.errorLog, branch)
					outcome, checks := runVerifyPath(s.deps.verifyPath, VerifyInput{Context: ctx, Issue: issue, Branch: branch, WorkDir: wt.WorkDir(), PR: pr})
					if outcome != VerifyNoSignal {
						terminalExtras = mergeVerificationExtras(terminalExtras, outcome, checks)
						if outcome == VerifyFailed {
							result.Status = "failure"
							break
						}
						// VerifyVerified: drop the conservative backstop;
						// the oracle proved the issue is already resolved.
						result.Status = "success"
						if issue != nil && !github.IsIssueClosed(issue) && s.deps.githubClient != nil {
							if err := s.deps.githubClient.CloseIssue(ctx, issue.Number, "Closed by sandman — issue already completed."); err != nil {
								fmt.Fprintf(s.deps.errorLog, "error: close issue %d: %v\n", issue.Number, err)
							}
						}
						break
					}
					// VerifyNoSignal: record the chain's checks (if any)
					// so the operator can see why we abstained, then
					// fall through to the conservative backstop. The
					// blocker payload and pr_number fields are
					// preserved verbatim.
					if len(checks) > 0 {
						terminalExtras = mergeVerificationExtras(terminalExtras, outcome, checks)
					}
					if extras, blocked := hasBlockingOpenPR(s.deps.errorLog, branch); blocked {
						terminalExtras = mergeBlockerExtras(terminalExtras, extras)
						result.Status = "failure"
						break
					}
				}
				result.Status = "success"
				if alreadyResolved && issue != nil && !github.IsIssueClosed(issue) && s.deps.githubClient != nil {
					if err := s.deps.githubClient.CloseIssue(ctx, issue.Number, "Closed by sandman — issue already completed."); err != nil {
						fmt.Fprintf(s.deps.errorLog, "error: close issue %d: %v\n", issue.Number, err)
					}
				}
				break
			}
			if github.IsIssueClosed(issue) {
				if events.RunStatusFromPayload(result.Status).IsSuccess() {
					break
				}
			}
			result.Status = "failure"
		} else {
			if alreadyResolved && issue != nil && !github.IsIssueClosed(issue) && s.deps.githubClient != nil {
				if err := s.deps.githubClient.CloseIssue(ctx, issue.Number, "Closed by sandman — issue already completed."); err != nil {
					fmt.Fprintf(s.deps.errorLog, "error: close issue %d: %v\n", issue.Number, err)
				}
			}
			if events.RunStatusFromPayload(result.Status).IsSuccess() || alreadyResolved {
				if issue != nil && s.deps.githubClient != nil {
					prMerged := checkPRMergedForIssue(ctx, s.deps.githubClient, branch, s.issueNumber)
					if events.RunStatusFromPayload(result.Status).IsSuccess() && mergedPRMissingClosingReference(ctx, s.deps.githubClient, branch, s.issueNumber) {
						terminalExtras = mergeCompletionFailureExtras(terminalExtras, s.issueNumber)
						result.Status = "failure"
						break
					}
					if prMerged || alreadyResolved {
						if alreadyResolved {
							pr := lookupPRForVerify(ctx, s.deps.githubClient, s.deps.errorLog, branch)
							outcome, checks := runVerifyPath(s.deps.verifyPath, VerifyInput{Context: ctx, Issue: issue, Branch: branch, WorkDir: wt.WorkDir(), PR: pr})
							if outcome != VerifyNoSignal {
								terminalExtras = mergeVerificationExtras(terminalExtras, outcome, checks)
								if outcome == VerifyFailed {
									result.Status = "failure"
									break
								}
								result.Status = "success"
								break
							}
							if len(checks) > 0 {
								terminalExtras = mergeVerificationExtras(terminalExtras, outcome, checks)
							}
							if extras, blocked := hasBlockingOpenPR(s.deps.errorLog, branch); blocked {
								terminalExtras = mergeBlockerExtras(terminalExtras, extras)
								result.Status = "failure"
								break
							}
							result.Status = "success"
						}
						break
					}
					if events.RunStatusFromPayload(result.Status).IsSuccess() && ctx.Err() == nil {
						hostPathsReady := s.restoreHostPathsBeforeExternalGate(wt)
						if gateStatus, extras, handled := s.handleLifecycleDecisionAfterAgent(ctx, wt.WorkDir(), branch, logPath, runID, hostPathsReady); handled {
							if gateStatus == "resume" {
								gateStatus = "await"
							}
							result.Status = gateStatus
							terminalExtras = mergeBlockerExtras(terminalExtras, extras)
							break loop
						}
					}
					result.Status = "failure"
				} else {
					break
				}
			}
		}
		if s.shouldAwaitUsageLimit(result) {
			break loop
		}
	}

	if result.ContextExhausted {
		if terminalExtras == nil {
			terminalExtras = make(map[string]any)
		}
		terminalExtras["context_exhausted"] = true
	}
	return result, terminalExtras, true
}

func resetRetryBranch(opts runSessionOptions, ctx context.Context, sb sandbox.Sandbox, branch, baseBranch string) error {
	if opts.retryReset != nil {
		return opts.retryReset(ctx, sb, branch, baseBranch)
	}

	var output bytes.Buffer
	command := fmt.Sprintf("git reset --hard && git checkout -f -B %s %s && git clean -fd", shellenv.Quote(branch), shellenv.Quote(baseBranch))
	if err := sb.Exec(ctx, command, &output, &output); err != nil {
		return fmt.Errorf("reset retry branch: %w\n%s", err, output.String())
	}
	return nil
}
