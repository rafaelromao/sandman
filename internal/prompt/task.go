package prompt

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

const continuationFreshnessGuardHeading = "## Continuation Freshness Guard"
const contextRecoveryGuardHeading = "## Context Recovery Guard"
const ciOwnershipContextHeading = "## Sandman Failed CI Remediation Context"

const ciOwnershipContext = `## Sandman Failed CI Remediation Context

A failed required check on the current pull request head remains implementation work you own. Pending sibling checks, review delivery, approval, a saved blocker, or historical runtime-wait directions do not transfer an untouched failure to Sandman. Diagnose the concrete current-head failure and either make and validate the narrow repair or initiate an authorized, bounded recovery action before yielding.


For deterministic source, test, configuration, or workflow failures, make the required repair and validate it. For a verified provider-pin deprecation, update the coordinated documented pins. For a diagnosed transient provider or network timeout, use the authorized helper in the shared Sandman skill to rerun the identified failed execution after verifying its repository, pull request, source head, workflow run, and job identity. The canary's one CLI invocation and no-retry-loop rule applies within one job invocation; it does not prohibit a bounded rerun of the existing execution, and workflow_dispatch is not a prerequisite. Do not blindly rerun deterministic failures, invent credentials or permissions, or manufacture green evidence.

The runtime owns asynchronous observation and admission, not CI repair. Verify the corrective action and current-head execution first. Only after no implementor-owned work remains and required checks are queued or running should you checkpoint and yield for runtime observation.`

const (
	reviewTimeoutContextHeading = "## Sandman Runtime Context"
	reviewTimeoutContextPrefix  = "- Delegated review response timeout:"
)

var continuationFreshnessGuard = `## Continuation Freshness Guard

This section is injected for every retry and sandman run --continue and overrides persisted blocker and next-action text from earlier attempts.

Before acting on any persisted blocker or next action:

1. Treat every persisted blocker and next action as historical evidence, never current truth.
2. Re-check its authoritative live source now: Git and worktree state, change-request state and checks, review state, authentication, required tools, or the relevant test command.
3. If the blocker no longer exists, remove or mark it resolved in .sandman/task.md, recompute ## Next Step from current live state and the first unchecked checklist item, and continue automatically.
4. If the blocker still exists, refresh its evidence and next executable action before following it.

### Runtime-managed waits

When a Sandman-created run has no agent-owned work left and current-head CI is queued/running, checkpoint the head and checks in .sandman/task.md, then end the agent session successfully. After a delegated-review request is confirmed against the current PR head, checkpoint that request and the next action, then end the agent session before polling. Sandman's runtime owns admitted non-terminal waits, execution-capacity release, request observation, evidence migration, and re-entry. This overrides persisted instructions to keep polling; after continuation, re-check the live head and request-scoped evidence. Do not yield for a missing PR or review request, generic pending/REVIEW_REQUIRED/BLOCKED state, absent checks, stale or unverifiable heads, failed lookups/state reads, or exhausted budgets. Complete work the implementor owns (including branch publication, PR creation, CI/conflict repair, review feedback, and merge); without an authorized resolver, fail with a specific next action instead of waiting. Outside a Sandman-managed run, poll within the documented budget.

Never stop or exit solely because an earlier attempt recorded a blocker.`

var contextRecoveryGuard = `## Context Recovery Guard

This is a clean OpenCode session after the previous session exhausted its context. Do not resume implementation from memory.

Before changing implementation files:

The recovery checkpoint must be durable before implementation resumes.

1. Reconstruct the handoff from the current worktree, git status and diff, recent commits, run log, and this Task.
2. Write a durable checkpoint and handoff summary into this Task, including completed work, remaining work, validation, and the next step.
3. Only after the checkpoint is durable, continue implementation from that reconstructed state.

Preserve both committed and uncommitted work unless the current Task explicitly directs otherwise.`

// ContinuationTaskPrompt returns the resume prompt for `sandman run --continue`
// while preserving the verbatim content of `.sandman/task.md`. The previous
// implementation routed the file through ParseTask → BuildTaskPrompt, which
// rewrote the file into a different scaffold (## Completed / ## Pending /
// ## Blockers / ## Key Decisions / ## Next Step) and silently destroyed the
// original default-task-prompt.md layout (# Task, ## Issue Context, ##
// Runtime Context, ## Execution Checklist). This function preserves that
// content byte-for-byte and appends one canonical freshness guard so historical
// blockers cannot be mistaken for current state, including for task files
// created by older Sandman versions.
//
// When content is empty (e.g. the file existed but was blank), this falls
// back to DefaultPrompt() so the agent still gets a usable scaffold. The
// "no task file" path lives in batch.ReadTaskContent and returns its own
// EmptyTaskTemplate.
func ContinuationTaskPrompt(content string) string {
	return continuationTaskPrompt(content, 0)
}

// ContinuationTaskPromptWithReviewTimeout returns a continuation prompt whose
// runtime context reflects the current AgentRun's effective review budget.
func ContinuationTaskPromptWithReviewTimeout(content string, reviewTimeout int) string {
	return continuationTaskPrompt(content, reviewTimeout)
}

// ContextRecoveryTaskPrompt composes the preserved Task with a canonical,
// checkpoint-first recovery instruction for a fresh OpenCode session. An
// existing recovery section is preserved so a checkpoint written inside it is
// not discarded when a retry is itself retried.
func ContextRecoveryTaskPrompt(content string, reviewTimeout int) string {
	if strings.Contains(content, contextRecoveryGuardHeading) {
		if reviewTimeout > 0 {
			content = EnsureReviewTimeoutContext(content, reviewTimeout)
		}
		return EnsureCIOwnershipContext(trimTrailingNewlines(content) + "\n")
	}
	content = continuationTaskPrompt(content, reviewTimeout)
	return EnsureCIOwnershipContext(trimTrailingNewlines(content) + "\n\n" + contextRecoveryGuard + "\n")
}

func continuationTaskPrompt(content string, reviewTimeout int) string {
	if strings.TrimSpace(content) == "" {
		content = DefaultPrompt()
	}
	content = removeCanonicalGuard(content)
	if reviewTimeout > 0 {
		content = EnsureReviewTimeoutContext(content, reviewTimeout)
	}
	guard := continuationFreshnessGuard
	if reviewTimeout > 0 {
		effective := (&config.Config{ReviewTimeout: reviewTimeout}).EffectiveReviewTimeout()
		guard += fmt.Sprintf("\n\nCurrent AgentRun delegated review response timeout is `%d` seconds; it supersedes any persisted timeout wording above.", effective)
	}
	return EnsureCIOwnershipContext(trimTrailingNewlines(content) + "\n\n" + guard + "\n")
}

// EnsureCIOwnershipContext appends the canonical implementation-owned failed
// CI guidance while preserving all non-runtime task content. The section is
// replaced on each composition so fresh custom templates and continuations
// receive the same idempotent instruction without changing generic rendering.
func EnsureCIOwnershipContext(content string) string {
	content = removeCanonicalSection(content, ciOwnershipContextHeading)
	return trimTrailingNewlines(content) + "\n\n" + ciOwnershipContext + "\n"
}

const reviewEvidenceHeading = "## Review Evidence"

// TaskWithReviewEvidence appends a canonical "## Review Evidence" section to
// preserved task content when the runtime resumes an agent session with
// request-scoped review evidence (actionable feedback or a ready-to-merge
// approval). When no evidence is present (nil or empty map) the content is
// returned byte-identical so continuation replay without evidence stays
// unchanged. An earlier evidence section from a previous resume is replaced
// so the agent always sees the latest request-scoped evidence.
func TaskWithReviewEvidence(content string, evidence map[string]any) string {
	if len(evidence) == 0 {
		return content
	}
	content = removeReviewEvidenceSection(content)
	var b strings.Builder
	b.WriteString(trimTrailingNewlines(content))
	b.WriteString("\n\n## Review Evidence\n\n")
	b.WriteString("The pull request gate resumed this session with request-scoped evidence. Revalidate the evidence against the live pull request and the worktree head before acting; stale evidence must never drive merge work on its own.\n\n")
	if evidence["reason"] == "REVIEW_APPROVED" {
		b.WriteString("The runtime accepted approval for this request and head. A request-scoped informal approval is sufficient; formal GitHub approval is not required. This supersedes a preserved instruction to wait for formal approval: do not request a replacement review solely because no formal APPROVED review or aggregate reviewDecision is present. Revalidate the current request/head approval, green CI, and mergeability, then execute the normal merge gate.\n\n")
	}
	if reason, _ := evidence["reason"].(string); reason == "CI_FAILURE" || evidence["gate"] == "ci-failure" {
		b.WriteString("A failed required check on the current pull request head is implementation work you own. Diagnose the concrete failure and either make and validate the narrow repair or initiate an authorized, bounded rerun of the identified current-head execution. Pending sibling checks, review delivery, or approval do not transfer this work to the runtime. Do not hand off an untouched failure.\n\n")
		b.WriteString("The canary's no-retry rule applies within one job invocation; it does not prohibit a bounded rerun of the existing failed execution. A transient provider or network timeout may justify rerunning that identified execution after verifying its repository, pull request, source head, workflow run, and failed job. Do not blindly rerun deterministic source failures, use workflow_dispatch as a replacement for the existing run, or invent credentials or permissions.\n\n")
		b.WriteString("The runtime owns asynchronous observation and admission, not CI repair. After the failure has been diagnosed and the corrective action is actually complete, verify the current head and live check execution. Only when no implementor-owned work remains and required checks are queued or running should you checkpoint and yield for runtime observation.\n\n")
	}
	if reason, _ := evidence["reason"].(string); reason == "REVIEW_INFORMAL_FEEDBACK" || reason == "REVIEW_CHANGES_REQUESTED" {
		b.WriteString("Address only concrete author changes the response requests. A current-head APPROVED report with no Blocking or Important findings and no author changes requested is an eligible informal approval, not feedback: formal GitHub approval is never required, so do not ask the reviewer or a maintainer for a formal approval, a reviewDecision, or publication work, and do not re-request review for it. Revalidate green CI and mergeability, then execute the normal merge gate.\n\n")
	}
	for _, key := range []string{"outcome", "reason", "next_action", "pull_request", "head_sha", "repository"} {
		v, ok := evidence[key]
		if !ok || v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s: %v\n", key, v)
	}
	if req, ok := evidence["review_request"].(map[string]any); ok && len(req) > 0 {
		if enc, err := json.Marshal(req); err == nil {
			fmt.Fprintf(&b, "- review_request: %s\n", enc)
		}
	}
	return trimTrailingNewlines(b.String()) + "\n"
}

func removeReviewEvidenceSection(content string) string {
	for {
		idx := strings.Index(content, reviewEvidenceHeading)
		if idx < 0 {
			return content
		}
		content = content[:idx] + content[guardSectionEnd(content, idx):]
	}
}

// EnsureReviewTimeoutContext keeps the effective delegated review budget in
// the current task even when a custom template omits REVIEW_TIMEOUT.
func EnsureReviewTimeoutContext(content string, reviewTimeout int) string {
	effective := (&config.Config{ReviewTimeout: reviewTimeout}).EffectiveReviewTimeout()
	line := fmt.Sprintf("%s `%d` seconds", reviewTimeoutContextPrefix, effective)
	lines := strings.Split(content, "\n")
	sectionStart, sectionEnd := runtimeContextSection(lines)
	if sectionStart >= 0 {
		for i := sectionStart + 1; i < sectionEnd; i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), reviewTimeoutContextPrefix) {
				lines[i] = line
				return strings.Join(lines, "\n")
			}
		}
		lines = insertLine(lines, sectionEnd, line)
		return strings.Join(lines, "\n")
	}

	return trimTrailingNewlines(content) + "\n\n" + reviewTimeoutContextHeading + "\n\n" + line + "\n"
}

// runtimeContextSection returns the last runtime-context section so an issue
// body containing similarly named text is never treated as Sandman metadata.
func runtimeContextSection(lines []string) (start, end int) {
	start = -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case reviewTimeoutContextHeading, "## Runtime Context":
			start = i
		}
	}
	if start < 0 {
		return -1, -1
	}

	end = len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "## ") || (strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "##")) {
			end = i
			break
		}
	}
	return start, end
}

func insertLine(lines []string, index int, line string) []string {
	if index == len(lines) && index > 0 && lines[index-1] == "" {
		index--
	}
	lines = append(lines, "")
	copy(lines[index+1:], lines[index:])
	lines[index] = line
	return lines
}

func removeCanonicalGuard(content string) string {
	for {
		idx := strings.Index(content, continuationFreshnessGuardHeading)
		if idx < 0 {
			return content
		}
		end := guardSectionEnd(content, idx)
		content = content[:idx] + content[end:]
	}
}

func removeCanonicalSection(content, heading string) string {
	for {
		idx := strings.Index(content, heading)
		if idx < 0 {
			return content
		}
		content = content[:idx] + content[guardSectionEnd(content, idx):]
	}
}

// guardSectionEnd returns the byte offset that follows the H2 section opened
// by the guard heading at the given offset. The section terminates at the
// next H2 heading (a line beginning with "## "), at the next H1 ("# "), or
// at the end of the content. Anchoring on Markdown headings ensures stale or
// short-bodied guards cannot delete the persisted sections that follow
// them — a regression that previously sliced into the next H2 body.
func guardSectionEnd(content string, headingIdx int) int {
	lineStart := strings.LastIndex(content[:headingIdx], "\n") + 1
	scan := lineStart
	for scan < len(content) {
		nl := strings.IndexByte(content[scan:], '\n')
		var end int
		if nl < 0 {
			end = len(content)
		} else {
			end = scan + nl
		}
		line := content[scan:end]
		trimmed := strings.TrimSpace(line)
		if scan != lineStart {
			if strings.HasPrefix(trimmed, "## ") || (scan+1 < len(content) && strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "##")) {
				return scan
			}
		}
		if nl < 0 {
			return len(content)
		}
		scan = end + 1
	}
	return len(content)
}

func trimTrailingNewlines(content string) string {
	for len(content) > 0 && (content[len(content)-1] == '\n' || content[len(content)-1] == '\r') {
		content = content[:len(content)-1]
	}
	return content
}
