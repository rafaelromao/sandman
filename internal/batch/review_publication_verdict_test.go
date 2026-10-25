package batch

import (
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/reviewpub"
)

// A daemon CHANGES_REQUESTED verdict is repair evidence even when its prose
// carries no mechanical code anchor.
func TestReviewPublicationVerdict_DaemonChangesRequestedIsFeedback(t *testing.T) {
	body := "## Summary\nThe retry loop loses the request.\n\n## Suggested next steps\nKeep the request across retries.\n\n## Decision\n**CHANGES_REQUESTED**\n\n" +
		reviewpub.Marker(reviewpub.ChangesRequested) + "\n" + reviewpub.Identity("request", "current-sha")
	s, _, pr, workDir, _ := informalApprovalLifecycleFixture(t, body)
	pr.StatusCheckRollup = "success"
	status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	if !handled || status != "resume" || extras["gate"] != gateActionableFeedback {
		t.Fatalf("daemon changes requested = %s/%v, want actionable feedback", status, extras)
	}
}

// An unknown daemon verdict never becomes repair evidence: when unsure, the
// run keeps waiting on its request instead of relaunching the implementor.
func TestReviewPublicationVerdict_UnknownDaemonVerdictIsNeverFeedback(t *testing.T) {
	body := "## Summary\nSee `worker.go:12` for the retry loop.\n\n## Decision\nPending a second look at worker.go.\n\n" + reviewpub.Identity("request", "current-sha")
	s, _, pr, workDir, _ := informalApprovalLifecycleFixture(t, body)
	pr.StatusCheckRollup = "success"
	status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	if handled && status == "resume" && extras["gate"] == gateActionableFeedback {
		t.Fatalf("unknown daemon verdict = %s/%v, must not relaunch for feedback", status, extras)
	}
}

// A publication bound to another head is not a response to this request: an
// approval of an older head cannot approve the current one.
func TestReviewPublicationVerdict_PublicationForAnotherHeadIsStale(t *testing.T) {
	body := "## Decision\n**APPROVED**\n\n" + reviewpub.Marker(reviewpub.Approved) + "\n" + reviewpub.Identity("request", "older-sha")
	s, client, pr, workDir, _ := informalApprovalLifecycleFixture(t, "placeholder")
	client.comments[1].Body = body
	pr.StatusCheckRollup = "success"
	status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	if !handled || status != "await" || extras["reason"] == "REVIEW_APPROVED" {
		t.Fatalf("approval of another head = %s/%v, want the current request to keep waiting", status, extras)
	}
}

// The daemon keys a trigger by its revision. The runtime recomputes that key
// from the live comment, so the daemon's review of an edited trigger matches.
func TestReviewPublicationVerdict_EditedTriggerRevisionMatches(t *testing.T) {
	s, client, pr, workDir, _ := informalApprovalLifecycleFixture(t, "placeholder")
	edited := client.comments[0].CreatedAt.Add(30 * time.Second).Truncate(time.Second)
	client.comments[0].UpdatedAt = edited
	client.comments[1].Body = "## Decision\n**APPROVED**\n\n" + reviewpub.Marker(reviewpub.Approved) + "\n" +
		reviewpub.Identity(reviewpub.TriggerKey("request", edited), "current-sha")
	pr.StatusCheckRollup = "success"
	status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	if !handled || status != "resume" || extras["reason"] != "REVIEW_APPROVED" {
		t.Fatalf("review of the edited trigger = %s/%v, want approval", status, extras)
	}
}

// Human and third-party approvals carry no publication identity and keep the
// prose rules, including the paraphrases the real daemon reports used.
func TestReviewPublicationVerdict_ProseApprovalsWithoutPublication(t *testing.T) {
	reports := capturedApprovingReports(t)
	for _, name := range []string{"approved-daemon-29-first.md", "approved-daemon-29-second.md", "approved-daemon-29-third.md", "approved-daemon-29-fourth.md", "approved-daemon-29-fifth.md", "approved-daemon-29-final.md", "approved-daemon-30.md"} {
		t.Run(name, func(t *testing.T) {
			prose := reviewpub.StripMarkers(reports[name])
			if !reviewDecisionApprovedBody(prose) {
				for _, section := range collectReviewApprovalSections(prose) {
					if !section.permitsApproval() {
						t.Errorf("rejected %s section: %s", section.kind, section.text)
					}
				}
				t.Fatal("paraphrased approving report rejected without its publication identity")
			}
		})
	}
	for _, human := range []string{"LGTM, approved.", "Looks good to me. Approved."} {
		if !reviewDecisionApprovedBody(human) {
			t.Fatalf("human approval %q rejected", human)
		}
	}
}

// Wording variants of the no-change declaration and remarks addressed to the
// runtime are approvals; genuine contradictions still are not.
func TestReviewPublicationVerdict_ProseVariantsAndContradictions(t *testing.T) {
	report := func(summary, steps string) string {
		return "## Summary\n" + summary + "\n\n## Findings\nNone.\n\n## Suggested next steps\n" + steps + "\n\n## Decision\n**APPROVED**\n"
	}
	for _, tc := range []struct{ name, body string }{
		{"no code changes", report("Reviewed head abc.", "No code changes requested.")},
		{"no author code changes", report("Reviewed head abc.", "No author code changes requested.")},
		{"no implementation changes", report("Reviewed head abc.", "No implementation changes requested.")},
		{"runtime CI remark", report("Reviewed head abc.", "No code changes requested. The surrounding workflow should verify required remote CI before merging.")},
		{"runtime publication remark", report("Reviewed head abc.", "No author code changes requested. The surrounding runtime must handle the formal-review publication gap and verify remote CI. No authorized formal-review publication integration is available to this reviewer.")},
		{"workflow formal review remark", report("Reviewed head abc.", "No author changes requested. The surrounding workflow should publish a formal APPROVED review and verify remote CI before merging.")},
		{"negated summary", report("Approval is retained; no code changes requested.", "None.")},
		{"negated implementation summary", report("No implementation changes requested.", "None.")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !reviewDecisionApprovedBody(tc.body) {
				for _, section := range collectReviewApprovalSections(tc.body) {
					if !section.permitsApproval() {
						t.Errorf("rejected %s section: %s", section.kind, section.text)
					}
				}
				t.Fatal("approving variant rejected")
			}
		})
	}
	for _, tc := range []struct{ name, body string }{
		{"not approved", report("This is not approved yet.", "None.")},
		{"changes requested", report("Changes requested for the retry loop.", "None.")},
		{"explicit state", report("Verdict CHANGES_REQUESTED.", "None.")},
		{"conditional", report("Reviewed head abc.", "Approved once worker.go is fixed.")},
		{"author directive", report("Reviewed head abc.", "No code changes requested. Author should fix worker.go before merging.")},
		{"severity", "## Findings\n- Important: worker.go races.\n\n## Decision\n**APPROVED**\n"},
		{"blocking severity", "## Findings\n- Blocking: worker.go drops the request.\n\n## Decision\n**APPROVED**\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if reviewDecisionApprovedBody(tc.body) {
				t.Fatal("contradiction accepted as approval")
			}
		})
	}
}
