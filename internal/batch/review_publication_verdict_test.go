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
	observedLifecycle17 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	status, extras, handled := observedLifecycle17.status(), observedLifecycle17.Evidence, observedLifecycle17.handled()
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
	observedLifecycle29 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	status, extras, handled := observedLifecycle29.status(), observedLifecycle29.Evidence, observedLifecycle29.handled()
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
	observedLifecycle42 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	status, extras, handled := observedLifecycle42.status(), observedLifecycle42.Evidence, observedLifecycle42.handled()
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
	observedLifecycle57 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	status, extras, handled := observedLifecycle57.status(), observedLifecycle57.Evidence, observedLifecycle57.handled()
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

// Issue #2939: every real approving report is an approval in each form the
// runtime can meet it — published with the daemon's approved verdict, as the
// pre-verdict daemon published it, and as plain prose with no publication
// identity. Fixes that patch prose phrasing one report at a time keep
// regressing; this corpus is the regression net for all of them.
func TestReviewPublicationVerdict_ApprovingReportCorpus(t *testing.T) {
	reports := capturedApprovingReports(t)
	if _, ok := reports["approved-sandman-tests-33.md"]; !ok {
		t.Fatal("corpus lacks the #2939 report (sandman-tests PR #33, comment 6514897469)")
	}
	for name, report := range reports {
		prose := reviewpub.StripMarkers(report)
		for form, body := range map[string]string{
			"published": reviewpub.Body(prose, "request", "current-sha"),
			"legacy":    prose + "\n\n" + reviewpub.Identity("request", "current-sha"),
			"prose":     prose,
		} {
			t.Run(name+"/"+form, func(t *testing.T) {
				if !reviewDecisionApprovedBody(body) {
					for _, section := range collectReviewApprovalSections(prose) {
						if !section.permitsApproval() {
							t.Errorf("rejected %s section: %s", section.kind, section.text)
						}
					}
					t.Fatal("real approving report rejected")
				}
			})
		}
		if publishedApprovalContradicted(reviewpub.Body(prose, "request", "current-sha")) {
			t.Errorf("%s: published approval reads as contradicted", name)
		}
	}
}

// Issue #2939: only the verdict sections (Findings, Suggested next steps and
// Decision) may contradict a published daemon approval. Descriptive sections
// describe the change under review and never veto it, whatever their prose.
func TestReviewPublicationVerdict_DescriptiveSectionsNeverVetoPublishedApproval(t *testing.T) {
	verdict := "\n\n## Findings\nNone.\n\n## Suggested next steps\nNo author changes requested.\n\n## Decision\n**APPROVED**\n"
	for _, tc := range []struct{ name, section string }{
		{"summary code behaviour", "## Summary\n`scheduled.ts` writes `shard:{i}` only if every fetch succeeds, then merges all shards."},
		{"summary verified fix", "## Summary\nThe new commit keeps image URLs only if they are http(s)."},
		{"summary unmet history", "## Summary\nThe timeout criterion was unmet in the previous head; this head meets it."},
		{"spec severity count", "## Spec\n1 finding; worst: Important. The timeout criterion is unmet."},
		{"standards conditional", "## Standards\nThe helper retries only after a transient failure."},
		{"quality metrics", "## Quality check\n### Metrics\n- Cognitive complexity: ~8. Must stay under 15.\n\n### Tools used\nManual review."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			published := reviewpub.Body(tc.section+verdict, "request", "current-sha")
			if publishedApprovalContradicted(published) || !reviewDecisionApprovedBody(published) {
				t.Fatal("descriptive section vetoed the daemon's published approval")
			}
		})
	}
}

// Issue #2939: the prose fallback for bodies without a verdict does not read a
// condition describing code behaviour as a conditional approval. A condition
// that governs the approval or the merge still contradicts it.
func TestReviewPublicationVerdict_ProseConditionsGoverningApproval(t *testing.T) {
	report := func(summary string) string {
		return "## Summary\n" + summary + "\n\n## Findings\nNone.\n\n## Suggested next steps\nNone.\n\n## Decision\n**APPROVED**\n"
	}
	if !reviewDecisionApprovedBody(report("The worker writes the shard only if every fetch succeeds, then merges all shards.")) {
		t.Fatal("a description of code behaviour read as a conditional approval")
	}
	for _, summary := range []string{
		"Merge only after the race in worker.go is fixed.",
		"Only if the race is fixed should this be merged.",
		"Approve only once the timeout test passes.",
	} {
		if reviewDecisionApprovedBody(report(summary)) {
			t.Fatalf("conditional approval %q accepted", summary)
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

// A daemon approval whose author-facing prose contradicts it is not an
// approval, even though the daemon stamped its APPROVED verdict. It is unsure,
// so the Run waits instead of merging or relaunching for feedback.
func TestReviewPublicationVerdict_ContradictedDaemonApprovalWaits(t *testing.T) {
	for _, tc := range []struct{ name, decision string }{
		{"runtime merge prohibition", "## Suggested next steps\nThe surrounding workflow must not merge until the race in worker.go:12 is fixed.\n\n## Decision\n**APPROVED**\n"},
		{"direct imperative", "## Suggested next steps\nFix the race in `worker.go:12`.\n\n## Decision\n**APPROVED**\n"},
		{"decision condition", "## Decision\n**APPROVED**\nOnly after the race is fixed.\n"},
		{"unmet criterion finding", "## Findings\nThe timeout criterion is unmet.\n\n## Decision\n**APPROVED**\n"},
		{"author directive", "## Summary\nReviewed head abc.\n\n## Suggested next steps\nAuthor should fix worker.go before merging.\n\n## Decision\n**APPROVED**\n"},
		{"not approved yet in next steps", "## Summary\nReviewed head abc.\n\n## Suggested next steps\nThis is not approved yet.\n\n## Decision\n**APPROVED**\n"},
		{"nested important finding", "## Summary\nReviewed head abc.\n\n## Findings\n### Spec\n- Important: the timeout criterion is unmet in worker.go.\n\n## Decision\n**APPROVED**\n"},
		{"blocking finding", "## Findings\n- Blocking: `worker.go:12` drops the request on retry.\n\n## Decision\n**APPROVED**\n"},
		{"changes requested finding", "## Findings\nChanges requested for the retry loop.\n\n## Decision\n**APPROVED**\n"},
		{"opposing verdict state in summary", "## Summary\nVerdict: CHANGES_REQUESTED.\n\n## Decision\n**APPROVED**\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			published := reviewpub.Body(tc.decision, "request", "current-sha")
			if reviewDecisionApprovedBody(published) {
				t.Fatal("contradicted daemon approval was accepted")
			}
			s, client, pr, workDir, _ := informalApprovalLifecycleFixture(t, "placeholder")
			client.comments[1].Body = published
			pr.StatusCheckRollup = "success"
			observedLifecycle153 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			status, extras, handled := observedLifecycle153.status(), observedLifecycle153.Evidence, observedLifecycle153.handled()
			if handled && status == "resume" && (extras["reason"] == "REVIEW_APPROVED" || extras["gate"] == gateActionableFeedback) {
				t.Fatalf("contradicted daemon approval = %s/%v, want neither merge work nor a feedback relaunch", status, extras)
			}
		})
	}
	// Harmless runtime and publication remarks do not contradict it.
	for name, body := range capturedApprovingReports(t) {
		if publishedApprovalContradicted(body) {
			t.Errorf("captured approval %s reads as contradicted", name)
		}
	}
}
