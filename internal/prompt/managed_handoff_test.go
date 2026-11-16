package prompt

import (
	"os"
	"strings"
	"testing"
)

const handoffHeading = "## Sandman Managed Handoff Context"

// The handoff section is the single statement of who owns what: the
// implementor owns local verification and publication, the runtime owns
// external observation. Each phrase pins one part of that contract.
var handoffContract = []string{
	"You own local verification and publication; the Sandman runtime owns observation of external CI and review.",
	"a background command that is still running",
	"an unresolved test failure",
	"a commit that is not yet pushed",
	"an incomplete publication, review-request delivery or blind review round",
	"Never skip, interrupt, weaken or abbreviate required verification to hand off sooner.",
	"Early publication of durable progress is allowed",
	"Publishing does not end your turn.",
	"If local work remains after you publish, say so explicitly; the runtime is not observing an external wait for you while you are still working.",
	"record the durable checkpoint in `.sandman/task.md` and end the session immediately",
	"Do not launch redundant verification, and do not poll CI or review from this session",
	"The runtime observes external checks and review, then decides whether to resume you.",
	"local verification in progress, external CI or review awaiting runtime observation, and ready for merge",
	`Never carry an earlier "CI is running" or "review is pending" statement forward as present fact`,
	"label the statement as the last observation with its timestamp",
	"The runtime, not your summary, makes the post-attempt decision.",
}

func TestManagedHandoffContextStatesOwnershipHandoffAndTruthfulReporting(t *testing.T) {
	got := EnsureManagedTaskContext("# Task\n\nImplement the change.\n")
	for _, want := range handoffContract {
		if !strings.Contains(got, want) {
			t.Errorf("managed task context omitted %q:\n%s", want, got)
		}
	}
}

func TestEnsureManagedTaskContextIsIdempotentAndPreservesTheCheckpoint(t *testing.T) {
	content := "# Task\n\n## Checkpoint\n\nKeep this user-owned checkpoint.\n\n" + handoffHeading + "\n\nStale handoff guidance.\n\n## Sandman Failed CI Remediation Context\n\nStale CI guidance.\n"
	once := EnsureManagedTaskContext(content)
	twice := EnsureManagedTaskContext(once)
	thrice := EnsureManagedTaskContext(twice)
	if twice != once || thrice != once {
		t.Fatalf("managed task context is not idempotent:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
	if strings.Count(once, handoffHeading) != 1 || strings.Count(once, ciOwnershipContextHeading) != 1 {
		t.Fatalf("canonical sections must appear exactly once:\n%s", once)
	}
	if !strings.Contains(once, "Keep this user-owned checkpoint.") {
		t.Fatalf("user checkpoint was not preserved:\n%s", once)
	}
	if strings.Contains(once, "Stale handoff guidance.") || strings.Contains(once, "Stale CI guidance.") {
		t.Fatalf("stale canonical guidance survived:\n%s", once)
	}
	if strings.Index(once, ciOwnershipContextHeading) > strings.Index(once, handoffHeading) {
		t.Fatalf("handoff section must follow the failed-CI section so both orders converge:\n%s", once)
	}
	// Composing the failed-CI section alone, as older call sites did, then the
	// managed context again converges to the same text.
	if again := EnsureManagedTaskContext(EnsureCIOwnershipContext(once)); again != once {
		t.Fatalf("mixed composition diverged:\n%s", again)
	}
}

func TestContinuationAndRecoveryPromptsCarryTheHandoffContextOnce(t *testing.T) {
	task := "# Task\n\n## Checkpoint\n\nLocal verification finished; CI was running at 10:56.\n"
	prompts := map[string]string{
		"continuation":                 ContinuationTaskPrompt(task),
		"continuation with timeout":    ContinuationTaskPromptWithReviewTimeout(task, 1800),
		"context recovery":             ContextRecoveryTaskPrompt(task, 1800),
		"context recovery, repeated":   ContextRecoveryTaskPrompt(ContextRecoveryTaskPrompt(task, 1800), 1800),
		"continuation of continuation": ContinuationTaskPrompt(ContinuationTaskPrompt(task)),
	}
	for name, got := range prompts {
		if strings.Count(got, handoffHeading) != 1 {
			t.Errorf("%s: handoff section count != 1:\n%s", name, got)
		}
		for _, want := range handoffContract {
			if !strings.Contains(got, want) {
				t.Errorf("%s omitted %q", name, want)
			}
		}
		if !strings.Contains(got, "CI was running at 10:56.") {
			t.Errorf("%s dropped the saved checkpoint", name)
		}
	}
	if a, b := ContinuationTaskPrompt(task), ContinuationTaskPrompt(ContinuationTaskPrompt(task)); a != b {
		t.Fatalf("continuation prompt is not idempotent:\nonce:\n%s\ntwice:\n%s", a, b)
	}
	if a, b := ContextRecoveryTaskPrompt(task, 1800), ContextRecoveryTaskPrompt(ContextRecoveryTaskPrompt(task, 1800), 1800); a != b {
		t.Fatalf("context recovery prompt is not idempotent:\nonce:\n%s\ntwice:\n%s", a, b)
	}
}

// The saved handoff and wait sentences must not give a second, conflicting
// rule: they defer to the handoff section's definition of owned work.
func TestYieldGuidanceDefersToTheHandoffDefinitionOfOwnedWork(t *testing.T) {
	got := ContinuationTaskPrompt("# Task\n")
	for _, want := range []string{
		"has no agent-owned work left (as defined in the Sandman Managed Handoff Context)",
		"Only after no implementor-owned work remains (as defined in the Sandman Managed Handoff Context)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("yield guidance does not defer to the handoff definition: missing %q", want)
		}
	}
}

// The shipped template carries the same section, so a fresh Task and the
// docs copy never drift from the composed text.
func TestDefaultTaskPromptCarriesTheCanonicalHandoffSection(t *testing.T) {
	data, err := os.ReadFile("default-task-prompt.md")
	if err != nil {
		t.Fatal(err)
	}
	template := string(data)
	start := strings.Index(template, handoffHeading)
	if start < 0 {
		t.Fatal("default task prompt has no managed handoff section")
	}
	section := template[start:guardSectionEnd(template, start)]
	if strings.TrimSpace(section) != managedHandoffContext {
		t.Fatalf("template handoff section drifted from the canonical text:\n%s\nwant:\n%s", section, managedHandoffContext)
	}
	for _, want := range []string{
		"has no agent-owned work left (as defined in the Sandman Managed Handoff Context)",
		"Only after no implementor-owned work remains (as defined in the Sandman Managed Handoff Context)",
	} {
		if !strings.Contains(template, want) {
			t.Errorf("template yield guidance does not defer to the handoff definition: missing %q", want)
		}
	}
	// "Return immediately" never pre-empts owned review work.
	if !strings.Contains(managedHandoffContext, "review-request delivery or blind review round") {
		t.Error("handoff section must keep review delivery and the blind review round implementor-owned")
	}
}

func TestManagedHandoffContextIsComposedOnlyByTheManagedComposition(t *testing.T) {
	if strings.Count(ContinuationTaskPrompt(""), handoffHeading) != 1 {
		t.Fatal("empty-task continuation must still compose exactly one handoff section")
	}
	if rendered := TaskWithReviewEvidence("# Task\n", map[string]any{"reason": "REVIEW_APPROVED"}); strings.Contains(rendered, handoffHeading) {
		t.Fatal("evidence rendering must not add the handoff section by itself")
	}
}
