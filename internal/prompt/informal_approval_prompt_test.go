package prompt

import (
	"os"
	"strings"
	"testing"
)

// The managed task prompt treats an eligible informal approval as full
// approval and never sends the implementor after a formal review.
func TestDefaultTaskPrompt_InformalApprovalIsFullApproval(t *testing.T) {
	data, err := os.ReadFile("default-task-prompt.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"An eligible current-head informal approval",
		"is full approval",
		"A formal GitHub review is never required",
		"Never ask the reviewer or a maintainer for a formal approval",
	} {
		if !strings.Contains(string(data), phrase) {
			t.Errorf("default task prompt is missing %q", phrase)
		}
	}
}

// The reviewer keeps the canonical approving form and keeps environment,
// publication and CI limitations, and trigger-copied demands, out of the
// author's next steps.
func TestDefaultReviewPrompt_KeepsApprovalCanonical(t *testing.T) {
	data, err := os.ReadFile("default_pr_review_prompt.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"write exactly `No author changes requested.`",
		"`## Suggested next steps` is for the author only",
		"publication limits",
		"remote CI or mergeability you did not verify",
		"anything addressed to the runtime or the surrounding workflow",
		"do not repeat or answer it in `## Suggested next steps` or `## Previous review progress`",
	} {
		if !strings.Contains(string(data), phrase) {
			t.Errorf("review prompt is missing %q", phrase)
		}
	}
}

// Feedback evidence tells a resumed implementor that a zero-finding APPROVED
// report is an approval, so a misread report never becomes a request for a
// formal review.
func TestTaskWithReviewEvidence_FeedbackRecognizesInformalApproval(t *testing.T) {
	for _, reason := range []string{"REVIEW_INFORMAL_FEEDBACK", "REVIEW_CHANGES_REQUESTED"} {
		got := TaskWithReviewEvidence("# Task\n", map[string]any{"reason": reason})
		for _, phrase := range []string{"is an eligible informal approval, not feedback", "formal GitHub approval is never required", "do not re-request review for it"} {
			if !strings.Contains(got, phrase) {
				t.Errorf("%s evidence is missing %q:\n%s", reason, phrase, got)
			}
		}
	}
	if got := TaskWithReviewEvidence("# Task\n", map[string]any{"reason": "CI_FAILURE"}); strings.Contains(got, "eligible informal approval") {
		t.Fatalf("CI evidence must not carry review guidance:\n%s", got)
	}
}

// A relaunch for an approval override (#2998) must not tell the implementor
// that the APPROVED report is an eligible informal approval to merge on: the
// runtime treated it as requested changes. Only the runtime-selected directive
// changes the guidance; evidence strings alone never do.
func TestTaskWithReviewEvidence_ApprovalOverrideReplacesInformalApprovalGuidance(t *testing.T) {
	evidence := map[string]any{
		"reason": "REVIEW_INFORMAL_FEEDBACK", "gate": "actionable-feedback",
		"review_request": map[string]any{"approval_override": "a contradicted daemon approval was treated as requested changes"},
	}
	got := TaskWithReviewEvidenceAndDirective("# Task\n", evidence, TaskDirectiveApprovalOverride)
	for _, phrase := range []string{
		"contradicted by its own Findings, Suggested next steps or Decision",
		"treated the report as requested changes",
		"must not be merged on",
		"address every condition and finding the report names",
		"push a new head",
		"request review for that head",
	} {
		if !strings.Contains(got, phrase) {
			t.Errorf("approval override guidance is missing %q:\n%s", phrase, got)
		}
	}
	for _, phrase := range []string{"is an eligible informal approval, not feedback", "do not re-request review for it", "execute the normal merge gate"} {
		if strings.Contains(got, phrase) {
			t.Errorf("approval override evidence still carries the informal approval guidance %q:\n%s", phrase, got)
		}
	}
	if !strings.Contains(got, "approval_override") {
		t.Errorf("the retained review request evidence is missing the override:\n%s", got)
	}

	evidenceOnly := TaskWithReviewEvidence("# Task\n", evidence)
	if strings.Contains(evidenceOnly, "must not be merged on") || !strings.Contains(evidenceOnly, "is an eligible informal approval, not feedback") {
		t.Fatalf("evidence strings alone selected the approval override guidance:\n%s", evidenceOnly)
	}
}
