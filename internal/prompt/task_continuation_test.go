package prompt

import (
	"strings"
	"testing"
)

// TestContinuationTaskPrompt_PreservesOriginalTaskTemplate documents the
// --continue invariant: when an issue's .sandman/task.md was originally
// rendered from default-task-prompt.md (and therefore starts with "# Task"
// and contains ## Issue Context, ## Runtime Context, and ## Execution
// Checklist), the resume prompt must preserve the file's verbatim content. The
// earlier round-trip through ParseTask → BuildTaskPrompt rewrote the file
// into a different scaffold (## Completed / ## Pending / ## Blockers /
// ## Key Decisions / ## Next Step), destroying the user-facing Execution
// Checklist and breaking the in-place checklist semantics described in
// default-task-prompt.md.
func TestContinuationTaskPrompt_PreservesOriginalTaskTemplate(t *testing.T) {
	original := `# Task

Implement GitHub issue #1193: Uniform log prefix -- always [<runID>]

## Issue Context

Slice 2 of issue #1193.

## Runtime Context

- You are running inside a Sandman-created worktree.
- Current branch: '1193-slice-2-uniform-log-prefix-always-runid'
- Source branch: '1193-slice-2-uniform-log-prefix-always-runid'
- Base branch: 'main'
- Review command: '/sandman review'

The worktree MUST be checked out on '1193-slice-2-uniform-log-prefix-always-runid' when the run finishes. Do not switch to 'main' or any other branch before exiting.

## Execution Checklist

- [x] Create branch
- [x] Plan (sandman-plan)
- [x] Implement (sandman-implement: execute TDD + commit + self-review + back-merge + create PR)
- [ ] PR-Review (sandman-review-request for one stateless trigger)
- [ ] PR-Merge (sandman-pr-merge)

After completing each item, update '.sandman/task.md' in place by checking that item off.
`

	got := ContinuationTaskPrompt(original)

	if !strings.HasPrefix(got, original) {
		t.Fatalf("expected continuation prompt to preserve the verbatim task.md content before the freshness guard.\n\n--- diff (first 40 lines) ---\nwant prefix:\n%s\n\ngot:\n%s", firstLines(original, 40), firstLines(got, 40))
	}
	if !strings.Contains(got, "## Continuation Freshness Guard") {
		t.Fatalf("expected continuation prompt to append the mandatory freshness guard, got:\n%s", got)
	}
}

// TestContinuationTaskPrompt_RevalidatesPreservedBlockers verifies that a
// historical blocker remains available as evidence but cannot be mistaken for
// current state on a later retry.
func TestContinuationTaskPrompt_RevalidatesPreservedBlockers(t *testing.T) {
	withBlockers := `# Task

Implement GitHub issue #1193.

## Execution Checklist

- [x] Create branch
- [x] Plan
- [x] Implement

## Blockers

- PR #1208 remains open, awaiting unrelated CI failure to be resolved before merge.

## AFK Rule

For asynchronous CI, review, or workflow gates, poll for the full documented budget and preserve current-head state across waits.

## Next Step

Wait for CI to be green.
`

	got := ContinuationTaskPrompt(withBlockers)

	if !strings.Contains(got, "## Blockers") {
		t.Fatalf("expected continuation prompt to preserve ## Blockers section, got:\n%s", got)
	}
	if !strings.Contains(got, "PR #1208 remains open, awaiting unrelated CI failure") {
		t.Fatalf("expected continuation prompt to preserve ## Blockers content, got:\n%s", got)
	}
	if !strings.Contains(got, "## Execution Checklist") {
		t.Fatalf("expected continuation prompt to preserve ## Execution Checklist, got:\n%s", got)
	}
	legacyPolling := "For asynchronous CI, review, or workflow gates, poll for the full documented budget and preserve current-head state across waits."
	if !strings.Contains(got, legacyPolling) {
		t.Fatalf("expected continuation prompt to preserve historical polling instruction as evidence, got:\n%s", got)
	}
	if strings.Index(got, "## Continuation Freshness Guard") < strings.Index(got, legacyPolling) {
		t.Fatalf("freshness guard must follow the historical polling instruction so it can supersede it, got:\n%s", got)
	}
	for _, phrase := range []string{
		"Treat every persisted blocker and next action as historical evidence",
		"Re-check its authoritative live source",
		"checkpoint the head and checks in .sandman/task.md",
		"This overrides persisted instructions to keep polling",
		"Sandman's runtime owns admitted non-terminal waits",
		"without an authorized resolver, fail with a specific next action instead of waiting",
		"Outside a Sandman-managed run, poll within the documented budget",
		"Never stop or exit solely because an earlier attempt recorded a blocker",
	} {
		if !strings.Contains(got, phrase) {
			t.Errorf("expected continuation freshness rule %q, got:\n%s", phrase, got)
		}
	}
	if blocker, guard := strings.Index(got, "## Blockers"), strings.Index(got, "## Continuation Freshness Guard"); guard <= blocker {
		t.Fatalf("freshness guard must follow persisted blockers so it overrides stale next actions: blocker=%d guard=%d", blocker, guard)
	}
}

func TestContinuationTaskPrompt_DoesNotDuplicateFreshnessGuard(t *testing.T) {
	once := ContinuationTaskPrompt("# Task\n\n## Blockers\n\n- CI timed out.\n")
	twice := ContinuationTaskPrompt(once)

	if twice != once {
		t.Fatalf("expected freshness guard injection to be idempotent\nonce:\n%s\ntwice:\n%s", once, twice)
	}
	if got := strings.Count(twice, "## Continuation Freshness Guard"); got != 1 {
		t.Fatalf("freshness guard count = %d, want 1", got)
	}
}

func TestContinuationTaskPrompt_RemainsSingleAfterManyContinuations(t *testing.T) {
	original := "# Task\n\n## Blockers\n\n- CI timed out.\n"
	got := original
	for i := 0; i < 5; i++ {
		got = ContinuationTaskPrompt(got)
	}

	if count := strings.Count(got, "## Continuation Freshness Guard"); count != 1 {
		t.Fatalf("freshness guard count = %d, want 1 after 5 continuations:\n%s", count, got)
	}
	if !strings.HasPrefix(got, original) {
		t.Fatalf("task content must survive multiple continuations:\n%s", got)
	}
	if !strings.Contains(got, "## Blockers\n\n- CI timed out.") {
		t.Fatalf("blocker text must survive multiple continuations:\n%s", got)
	}
}

func TestContinuationTaskPrompt_DoesNotDuplicateWhenAlreadyInInput(t *testing.T) {
	withGuard := "# Task\n\n## Continuation Freshness Guard\n\nCustom body.\n"
	got := ContinuationTaskPrompt(withGuard)

	if count := strings.Count(got, "## Continuation Freshness Guard"); count != 1 {
		t.Fatalf("freshness guard count = %d, want 1 (input already contained the guard):\n%s", count, got)
	}
	if !strings.Contains(got, "Treat every persisted blocker and next action as historical evidence") {
		t.Fatalf("canonical guard body must replace the stale custom copy:\n%s", got)
	}
	if strings.Contains(got, "Custom body.") {
		t.Fatalf("stale guard body must be removed:\n%s", got)
	}
}

func TestContinuationTaskPrompt_MovesFreshnessGuardAfterLaterBlocker(t *testing.T) {
	prior := ContinuationTaskPrompt("# Task\n\n## Next Step\n\nRun tests.\n")
	prior += "\n## Blockers\n\n- CI timed out.\n\n## Next Step\n\nStop because CI timed out.\n"

	got := ContinuationTaskPrompt(prior)

	if count := strings.Count(got, "## Continuation Freshness Guard"); count != 1 {
		t.Fatalf("freshness guard count = %d, want 1", count)
	}
	if blocker, guard := strings.LastIndex(got, "## Blockers"), strings.LastIndex(got, "## Continuation Freshness Guard"); guard <= blocker {
		t.Fatalf("freshness guard must be moved after later blockers: blocker=%d guard=%d\n%s", blocker, guard, got)
	}
}

func TestContinuationTaskPrompt_PreservesArbitraryH2AfterGuard(t *testing.T) {
	task := "# Task\n\n## Blockers\n\n- CI timed out.\n\n## Custom Section\n\nPersisted note.\n"
	got := ContinuationTaskPrompt(ContinuationTaskPrompt(task))

	if !strings.Contains(got, "## Custom Section\n\nPersisted note.") {
		t.Fatalf("expected ## Custom Section preserved verbatim, got:\n%s", got)
	}
}

func TestContinuationTaskPrompt_PreservesH1AndH3Content(t *testing.T) {
	task := "# Task\n\n### Subheading\n\nKeep me.\n\n## Blockers\n\n- stale\n"
	got := ContinuationTaskPrompt(ContinuationTaskPrompt(task))

	if !strings.Contains(got, "### Subheading\n\nKeep me.") {
		t.Fatalf("expected H3 content preserved verbatim, got:\n%s", got)
	}
}

func TestContinuationTaskPrompt_DetectsContinuedGuardWithExtraTrailingText(t *testing.T) {
	prior := ContinuationTaskPrompt("# Task\n\n## Blockers\n\n- stale\n") + "\n## Continuation Freshness Guard\nOld copy.\n"
	got := ContinuationTaskPrompt(prior)

	if strings.Contains(got, "Old copy.") {
		t.Fatalf("expected canonical guard to replace a stale copy, got:\n%s", got)
	}
	if !strings.Contains(got, "Treat every persisted blocker and next action as historical evidence") {
		t.Fatalf("expected canonical guard body, got:\n%s", got)
	}
}

func TestContinuationTaskPrompt_ShortStaleGuardPreservesFollowingSections(t *testing.T) {
	prior := "# Task\n\n## Continuation Freshness Guard\nOld copy.\n\n## Custom Section\n\nPersisted note.\n"
	got := ContinuationTaskPrompt(prior)

	if !strings.Contains(got, "## Custom Section\n\nPersisted note.") {
		t.Fatalf("expected ## Custom Section preserved after stale guard, got:\n%s", got)
	}
	if strings.Contains(got, "Old copy.") {
		t.Fatalf("expected stale guard body to be removed, got:\n%s", got)
	}
	if !strings.Contains(got, "Treat every persisted blocker and next action as historical evidence") {
		t.Fatalf("expected canonical guard body appended, got:\n%s", got)
	}
}

func TestContinuationTaskPrompt_StaleGuardPreservesExecutionChecklist(t *testing.T) {
	prior := "# Task\n\n## Continuation Freshness Guard\nCustom body.\n\n## Execution Checklist\n\n- [x] Continue.\n"
	got := ContinuationTaskPrompt(prior)

	if !strings.Contains(got, "## Execution Checklist\n\n- [x] Continue.") {
		t.Fatalf("expected Execution Checklist preserved after stale guard, got:\n%s", got)
	}
	if strings.Contains(got, "Custom body.") {
		t.Fatalf("expected stale guard body to be removed, got:\n%s", got)
	}
}

func TestContinuationTaskPrompt_StaleGuardPreservesNextStep(t *testing.T) {
	prior := "# Task\n\n## Continuation Freshness Guard\nCustom body.\n\n## Blockers\n\n- stale\n\n## Next Step\n\n- run tests\n"
	got := ContinuationTaskPrompt(prior)

	if !strings.Contains(got, "## Blockers\n\n- stale") {
		t.Fatalf("expected ## Blockers preserved after stale guard, got:\n%s", got)
	}
	if !strings.Contains(got, "## Next Step\n\n- run tests") {
		t.Fatalf("expected ## Next Step preserved after stale guard, got:\n%s", got)
	}
}

// TestContinuationTaskPrompt_EmptyTaskFallsBackToTemplate verifies that the
// empty-file path (when .sandman/task.md does not exist) still produces a
// usable resume prompt — it should use the embedded DefaultPrompt as a
// fallback so the agent has the original Execution Checklist to work from.
func TestContinuationTaskPrompt_EmptyTaskFallsBackToTemplate(t *testing.T) {
	got := ContinuationTaskPrompt("")

	if !strings.Contains(got, "# Task") {
		t.Fatalf("expected fallback to include '# Task' heading from default-task-prompt.md, got:\n%s", firstLines(got, 20))
	}
	if !strings.Contains(got, "## Execution Checklist") {
		t.Fatalf("expected fallback to include '## Execution Checklist' from default-task-prompt.md, got:\n%s", firstLines(got, 40))
	}
}

func TestEnsureReviewTimeoutContextMaintainsCanonicalBlock(t *testing.T) {
	custom := "# Task\n\nDo the work.\n"
	withContext := EnsureReviewTimeoutContext(custom, 600)
	if !strings.Contains(withContext, "## Sandman Runtime Context") {
		t.Fatalf("expected canonical runtime context, got:\n%s", withContext)
	}
	if !strings.Contains(withContext, "Delegated review response timeout: `600` seconds") {
		t.Fatalf("expected effective timeout in runtime context, got:\n%s", withContext)
	}

	updated := EnsureReviewTimeoutContext(withContext, 240)
	if strings.Contains(updated, "`600` seconds") {
		t.Fatalf("stale timeout remained after refresh:\n%s", updated)
	}
	if !strings.Contains(updated, "Delegated review response timeout: `240` seconds") {
		t.Fatalf("updated timeout missing after refresh:\n%s", updated)
	}
}

func TestEnsureReviewTimeoutContextPreservesIssueBodyText(t *testing.T) {
	content := "# Task\n\n## Issue Context\n\nThe issue mentions - Delegated review response timeout: `900` seconds.\n\n## Runtime Context\n\n- Current branch: `feature`\n"

	got := EnsureReviewTimeoutContext(content, 600)

	if !strings.Contains(got, "The issue mentions - Delegated review response timeout: `900` seconds.") {
		t.Fatalf("issue body text was changed:\n%s", got)
	}
	if !strings.Contains(got, "## Runtime Context\n\n- Current branch: `feature`\n- Delegated review response timeout: `600` seconds") {
		t.Fatalf("runtime context was not refreshed:\n%s", got)
	}
}

func TestContinuationTaskPromptWithReviewTimeoutRefreshesCurrentContext(t *testing.T) {
	prior := "# Task\n\n## Runtime Context\n\n- Delegated review response timeout: `900` seconds\n\n## Notes\n\nKeep this.\n"
	got := ContinuationTaskPromptWithReviewTimeout(prior, 600)

	if strings.Contains(got, "`900` seconds") {
		t.Fatalf("stale review timeout remained:\n%s", got)
	}
	if !strings.Contains(got, "Delegated review response timeout: `600` seconds") {
		t.Fatalf("current review timeout missing:\n%s", got)
	}
	if !strings.Contains(got, "Current AgentRun delegated review response timeout is `600` seconds") {
		t.Fatalf("freshness context did not state current timeout:\n%s", got)
	}
	if !strings.Contains(got, "## Notes\n\nKeep this.") {
		t.Fatalf("arbitrary task content was not preserved:\n%s", got)
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n... (truncated)"
}

func TestTaskWithReviewEvidence_NoEvidenceReturnsContentVerbatim(t *testing.T) {
	content := "# Task\n\n## Next Step\n\nDo the work.\n"
	got := TaskWithReviewEvidence(content, nil)
	if got != content {
		t.Fatalf("expected byte-identical content without evidence, got:\n%s", got)
	}
	got = TaskWithReviewEvidence(content, map[string]any{})
	if got != content {
		t.Fatalf("expected byte-identical content with empty evidence, got:\n%s", got)
	}
}

func TestTaskWithReviewEvidence_AppendsCanonicalSection(t *testing.T) {
	content := "# Task\n\n## Next Step\n\nDo the work.\n"
	got := TaskWithReviewEvidence(content, map[string]any{
		"outcome":      "ready-to-merge",
		"reason":       "REVIEW_APPROVED",
		"next_action":  "merge the pull request",
		"pull_request": 17,
		"head_sha":     "abc123",
	})
	for _, want := range []string{"## Review Evidence", "- outcome: ready-to-merge", "- reason: REVIEW_APPROVED", "- pull_request: 17", "- head_sha: abc123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in evidence prompt:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "## Next Step") {
		t.Fatalf("expected preserved task content to survive evidence attachment:\n%s", got)
	}
}

func TestTaskWithReviewEvidence_ReplacesPreviousSection(t *testing.T) {
	content := "# Task\n\n## Review Evidence\n\n- outcome: pending\n\n## Next Step\n\nDo the work.\n"
	got := TaskWithReviewEvidence(content, map[string]any{"outcome": "actionable-feedback", "reason": "REVIEW_CHANGES_REQUESTED"})
	if strings.Contains(got, "- outcome: pending") {
		t.Fatalf("expected stale evidence section to be replaced:\n%s", got)
	}
	if !strings.Contains(got, "- outcome: actionable-feedback") {
		t.Fatalf("expected latest evidence to be attached:\n%s", got)
	}
	if !strings.Contains(got, "## Next Step") {
		t.Fatalf("expected preserved task content to survive evidence replacement:\n%s", got)
	}
}

func TestTaskWithReviewEvidence_EmbedsReviewRequest(t *testing.T) {
	content := "# Task\n"
	got := TaskWithReviewEvidence(content, map[string]any{
		"outcome": "actionable-feedback",
		"reason":  "REVIEW_CHANGES_REQUESTED",
		"review_request": map[string]any{
			"repository":   "owner/repo",
			"pull_request": float64(17),
			"head_sha":     "abc123",
			"outcome":      "changes_requested",
		},
	})
	if !strings.Contains(got, "review_request: {") {
		t.Fatalf("expected embedded review_request JSON in evidence prompt:\n%s", got)
	}
}

func TestTaskWithReviewEvidence_AcceptedInformalApprovalDoesNotRequireFormalReview(t *testing.T) {
	got := TaskWithReviewEvidence("# Task\n\n## Next Step\nWait for formal GitHub approval.\n", map[string]any{
		"reason": "REVIEW_APPROVED",
		"review_request": map[string]any{
			"outcome": "approved", "trigger_id": "confirmed-request", "head_sha": "current-sha",
		},
	})
	for _, required := range []string{
		"informal approval is sufficient",
		"formal GitHub approval is not required",
		"do not request a replacement review solely",
		"Revalidate",
	} {
		if !strings.Contains(got, required) {
			t.Fatalf("accepted informal approval prompt omitted %q:\n%s", required, got)
		}
	}
	feedback := TaskWithReviewEvidence("# Task\n", map[string]any{"reason": "REVIEW_CHANGES_REQUESTED"})
	if strings.Contains(feedback, "informal approval is sufficient") {
		t.Fatal("feedback-only continuation acquired an approval instruction")
	}
}

func TestTaskWithReviewEvidence_CIFailureKeepsRemediationOwned(t *testing.T) {
	content := `# Task

## Checkpoint

The implementation is complete on current-head-sha.

## CI Findings

- opencode-canary timed out after 60 seconds.
- build-macos is still pending.
- Current-head review is approved.

## Blockers

- CI_PROVIDER_TIMEOUT_NO_RETRY_BUDGET

## Next Step

Leave CI observation to the runtime and end the agent session.
`
	evidence := map[string]any{
		"reason":       "CI_FAILURE",
		"gate":         "ci-failure",
		"outcome":      "ci-failure",
		"pull_request": 2930,
		"head_sha":     "current-head-sha",
	}
	withoutDirective := TaskWithReviewEvidence(content, evidence)
	if strings.Contains(withoutDirective, "Diagnose the concrete failure") {
		t.Fatal("CI evidence strings alone selected corrective prompt instructions")
	}
	got := TaskWithReviewEvidenceAndDirective(content, evidence, TaskDirectiveCIRemediation)

	for _, required := range []string{
		"A failed required check on the current pull request head is implementation work you own.",
		"Diagnose the concrete failure and either make and validate the narrow repair or initiate an authorized, bounded rerun of the identified current-head execution.",
		"Pending sibling checks, review delivery, or approval do not transfer this work to the runtime.",
		"The canary's no-retry rule applies within one job invocation; it does not prohibit a bounded rerun of the existing failed execution.",
		"Do not hand off an untouched failure.",
	} {
		if !strings.Contains(got, required) {
			t.Errorf("CI failure continuation omitted corrective authority %q:\n%s", required, got)
		}
	}
	for _, preserved := range []string{
		"The implementation is complete on current-head-sha.",
		"CI_PROVIDER_TIMEOUT_NO_RETRY_BUDGET",
		"Leave CI observation to the runtime and end the agent session.",
	} {
		if !strings.Contains(got, preserved) {
			t.Errorf("historical task content %q was not preserved:\n%s", preserved, got)
		}
	}
	if strings.Index(got, "Do not hand off an untouched failure.") < strings.Index(got, "Leave CI observation to the runtime") {
		t.Fatalf("corrective authority must follow and supersede the saved handoff instruction:\n%s", got)
	}
}

func TestEnsureCIOwnershipContextIsIdempotentAndPreservesTaskCheckpoint(t *testing.T) {
	content := "# Task\n\n## Checkpoint\n\nKeep this user-owned checkpoint.\n\n## Sandman Failed CI Remediation Context\n\nOld runtime guidance.\n"
	once := EnsureCIOwnershipContext(content)
	twice := EnsureCIOwnershipContext(once)
	if twice != once {
		t.Fatalf("CI ownership composition is not idempotent:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
	if strings.Count(twice, "## Sandman Failed CI Remediation Context") != 1 {
		t.Fatalf("canonical CI section count != 1:\n%s", twice)
	}
	if !strings.Contains(twice, "Keep this user-owned checkpoint.") {
		t.Fatalf("user checkpoint was not preserved:\n%s", twice)
	}
	if strings.Contains(twice, "Old runtime guidance.") {
		t.Fatalf("stale canonical CI guidance was not replaced:\n%s", twice)
	}
}
