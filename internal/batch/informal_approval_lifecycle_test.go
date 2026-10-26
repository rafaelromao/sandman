package batch

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/reviewpub"
)

const approvingMetricsReview = `## Summary
The selected terminal outcome is applied without legacy re-arbitration.

## Quality check
### Scope
Changed internal/batch/run_lifecycle.go and its focused regression tests.

### Metrics
Cognitive complexity: 70 at internal/batch/run_lifecycle.go:22. The increase is
limited to the adapter branch; no separate readability concern was demonstrated.

### Findings
No quality findings filed. The metric increase is limited to the adapter branch;
no separate readability or change-risk concern was demonstrated.

## Findings

## Suggested next steps
None.

## Decision
**APPROVED**

## Previous review progress
The prior concern in internal/batch/orchestrator_resume.go is resolved.

<!-- sandman-publication:dd4739c73528fd53939c6a0e52d7fb08c5ea79d2a6f2f6c56b3bd1aee86bcf1f -->
`

func informalApprovalLifecycleFixture(t *testing.T, body string) (*runSession, *registrationGitHubClient, *github.PR, string, *time.Time) {
	t.Helper()
	workDir := t.TempDir()
	now := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	pr := &github.PR{Number: 17, State: "open", HeadRefName: gateTestBranch, HeadRefOid: "current-sha", StatusCheckRollup: "pending", MergeStateStatus: "CLEAN"}
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{prs: map[string]*github.PR{gateTestBranch: pr}},
		comments: []github.PRComment{
			{ID: "request", Body: "/sandman review", CreatedAt: now.Add(-time.Minute)},
			{ID: "approval", Body: answeringPublication(body, "request", time.Time{}, pr.HeadRefOid), CreatedAt: now.Add(time.Minute)},
		},
	}
	opts := gateTestRunOptions()
	opts.now = func() time.Time { return now }
	s := &runSession{
		deps:                   runDeps{githubClient: client, layout: paths.NewLayout(nil, workDir), errorLog: io.Discard},
		renderCfg:              prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 1800},
		reviewAttemptStartedAt: now.Add(-2 * time.Minute), opts: opts,
	}
	if err := s.registerReviewRequest(t.Context(), workDir, pr, pr.HeadRefOid); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	return s, client, pr, workDir, &now
}

func TestInformalApprovalLifecycle_MetricsReviewAwaitsCIThenResumesMerge(t *testing.T) {
	s, _, pr, workDir, now := informalApprovalLifecycleFixture(t, approvingMetricsReview)
	for i := 0; i < defaultAwaitResumeMax+2; i++ {
		observedLifecycle74 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
		status, extras, handled := observedLifecycle74.status(), observedLifecycle74.Evidence, observedLifecycle74.handled()
		if !handled || status != "await" || extras["gate"] != "pending" {
			t.Fatalf("pending CI observation %d = (%s, %v, %t), want await/pending after informal approval", i, status, extras["gate"], handled)
		}
		if s.resumeCount != 0 {
			t.Fatal("pending CI consumed the lifecycle relaunch allowance")
		}
		request := extras["review_request"].(map[string]any)
		if request["trigger_id"] != "request" || request["operation_resolved"] != true {
			t.Fatalf("approval did not resolve the original request: %v", request)
		}
		*now = now.Add(time.Minute)
	}
	pr.StatusCheckRollup = "success"
	observedLifecycle88 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	status, extras, handled := observedLifecycle88.status(), observedLifecycle88.Evidence, observedLifecycle88.handled()
	if !handled || status != "resume" || extras["gate"] != gateReadyToMerge || extras["reason"] != "REVIEW_APPROVED" {
		t.Fatalf("green CI = (%s, %v, %t), want approval merge resume", status, extras, handled)
	}
	request := extras["review_request"].(map[string]any)
	if request["outcome"] != "approved" || request["informal_feedback"] != nil {
		t.Fatalf("merge evidence = %v, want approval without false feedback", request)
	}
}

func TestInformalApprovalLifecycle_RealReviewExplanationsAndCIAdvice(t *testing.T) {
	captured := capturedApprovalWithCIAdvice(t)
	for _, tc := range []struct{ name, body string }{
		{"captured review", captured},
		{"CI wait", "## Summary\nChanged worker.go.\n\n## Suggested next steps\nAwait the pending required CI checks.\n\n## Decision\n**APPROVED**"},
		{"negative metric explanation", "## Summary\nChanged worker.go.\n\n## Findings\nNone. The metrics signals do not demonstrate a construct-specific readability or change-risk concern in this diff.\n\n## Decision\n**APPROVED**"},
		{"no author followup", "## Summary\nChanged worker.go.\n\n## Suggested next steps\nNo author follow-up required.\n\n## Decision\n**APPROVED**"},
		{"met criteria and resolved defects", "## Findings\nNone.\n\nVerification against the acceptance criteria:\nThe timeout criterion is met. The previous defect in worker.go is resolved. No defect is unresolved.\n\n## Suggested next steps\nNo author changes requested. No unresolved defects remain.\n\n## Decision\n**APPROVED**"},
		{"negated unmet requirements", "## Findings\nNone.\n\nVerification against the acceptance criteria:\nNo unmet requirements remain in worker.go. No requirements are unmet.\n\n## Decision\n**APPROVED**"},
		{"resolved failed requirement", "## Findings\nNone.\n\nVerification against the acceptance criteria:\nThe previously failed requirement in worker.go is now satisfied. The unresolved defect in worker.go is now fixed.\n\n## Suggested next steps\nNo author changes requested. The failed requirement in worker.go has been satisfied.\n\n## Decision\n**APPROVED**"},
		{"wrapped resolved assessments", "## Findings\nNone.\n\nVerification against the acceptance criteria:\nNo unmet requirements remain\nin worker.go. The previously failed requirement in worker.go\nis now satisfied.\n\n## Suggested next steps\nNo author changes requested. The previously failed requirement in worker.go\nis now satisfied.\n\n## Decision\n**APPROVED**"},
		{"no changes then CI wait", "## Findings\nNone.\n\n## Suggested next steps\nNone. Await the pending CI checks before merging.\n\n## Decision\n**APPROVED**"},
		{"no author changes then CI wait", "## Findings\nNone.\n\n## Suggested next steps\nNo author changes requested; await the required CI checks before merging. A formal GitHub review cannot be submitted from this daemon run.\n\n## Decision\n**APPROVED**"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, pr, workDir, now := informalApprovalLifecycleFixture(t, tc.body)
			for range defaultAwaitResumeMax + 2 {
				observedLifecycle115 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
				status, extras, handled := observedLifecycle115.status(), observedLifecycle115.Evidence, observedLifecycle115.handled()
				if !handled || status != "await" || extras["gate"] != "pending" || s.resumeCount != 0 {
					t.Fatalf("approved review with pending CI = %s/%v (%v), want await without repair launches", status, extras["gate"], extras["reason"])
				}
				*now = now.Add(time.Minute)
			}
			pr.StatusCheckRollup = "success"
			observedLifecycle122 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			status, extras, handled := observedLifecycle122.status(), observedLifecycle122.Evidence, observedLifecycle122.handled()
			if !handled || status != "resume" || extras["gate"] != gateReadyToMerge {
				t.Fatalf("approved review with green CI = %s/%v, want merge resume", status, extras["gate"])
			}
		})
	}
}

// answeringPublication rebinds a captured daemon publication to the fixture's
// own trigger and head, as the daemon would have published it there. Bodies
// that are not daemon publications are returned unchanged.
func answeringPublication(body, triggerID string, triggerUpdatedAt time.Time, head string) string {
	if !reviewpub.Published(body) {
		return body
	}
	return strings.TrimRight(reviewpub.StripMarkers(body), "\n") + "\n\n" + reviewpub.Identity(reviewpub.TriggerKey(triggerID, triggerUpdatedAt), head)
}

func capturedApprovingReports(t *testing.T) map[string]string {
	t.Helper()
	reports := make(map[string]string)
	for _, name := range []string{"approved-daemon-28-first.md", "approved-daemon-28-second.md", "approved-daemon-28-final.md", "approved-sandman-2818.md", "approved-sandman-2886.md",
		"approved-daemon-29-first.md", "approved-daemon-29-second.md", "approved-daemon-29-third.md", "approved-daemon-29-fourth.md", "approved-daemon-29-fifth.md", "approved-daemon-29-final.md", "approved-daemon-30.md"} {
		body, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		reports[name] = string(body)
	}
	return reports
}

func TestInformalApprovalLifecycle_CapturedReports(t *testing.T) {
	for name, body := range capturedApprovingReports(t) {
		t.Run(name, func(t *testing.T) {
			if !reviewDecisionApprovedBody(body) {
				for _, section := range collectReviewApprovalSections(body) {
					if !section.permitsApproval() {
						t.Errorf("rejected %s section: %s", section.kind, section.text)
					}
				}
				t.Fatal("captured approving report rejected")
			}
			s, _, pr, workDir, now := informalApprovalLifecycleFixture(t, body)
			for range defaultAwaitResumeMax + 2 {
				observedLifecycle167 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
				status, extras, handled := observedLifecycle167.status(), observedLifecycle167.Evidence, observedLifecycle167.handled()
				if !handled || status != "await" || extras["gate"] != "pending" || s.resumeCount != 0 {
					t.Fatalf("pending CI = %s/%v, want wait without relaunch", status, extras)
				}
				*now = now.Add(time.Minute)
			}
			pr.StatusCheckRollup = "success"
			observedLifecycle174 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			status, extras, handled := observedLifecycle174.status(), observedLifecycle174.Evidence, observedLifecycle174.handled()
			if !handled || status != "resume" || extras["gate"] != gateReadyToMerge || extras["reason"] != "REVIEW_APPROVED" {
				t.Fatalf("green CI = %s/%v, want approved merge work", status, extras)
			}
		})
	}
}

func TestInformalApprovalLifecycle_FailedCriteriaRemainActionable(t *testing.T) {
	for _, assessment := range []string{
		"The timeout criterion is not met: worker.go drops requests on timeout.",
		"The timeout criterion is **not** yet met: worker.go drops requests on timeout.",
		"The acceptance criteria remain unsatisfied: worker.go drops requests on timeout.",
		"An unmet requirement in worker.go drops requests on timeout.",
		"The timeout criterion has failed: worker.go drops requests on timeout.",
		"The defect is still unresolved: worker.go drops requests on timeout.",
		"There is an unresolved defect in worker.go: requests disappear on timeout.",
		"The implementation remains unsafe: worker.go drops requests on timeout.",
		"The handler is still broken: worker.go drops requests on timeout.",
		"No unmet requirements remain in worker.go, except the timeout criterion.",
		"No unmet requirements remain in worker.go,\nexcept the timeout criterion.",
		"The previously failed requirement in worker.go is now satisfied, but the timeout criterion is not met.",
		"The previously failed requirement in worker.go is now satisfied,\nbut the timeout criterion is not met.",
		"Publication was unavailable\nFix the race in worker.go.",
		"Merge\nwhen the race in worker.go is fixed.",
		"The unresolved defect in worker.go is now fixed; the timeout criterion is not met.",
	} {
		for _, section := range []string{"## Findings\nNone.\n\nVerification against the acceptance criteria:\n", "## Suggested next steps\nNone.\n", "## Spec\n"} {
			t.Run(section+assessment, func(t *testing.T) {
				body := section + assessment + "\n\n## Decision\n**APPROVED**"
				if reviewDecisionApprovedBody(body) {
					t.Fatal("unmet criterion or unresolved defect established approval")
				}
				s, _, pr, workDir, _ := informalApprovalLifecycleFixture(t, body)
				pr.StatusCheckRollup = "success"
				observedLifecycle209 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
				status, extras, handled := observedLifecycle209.status(), observedLifecycle209.Evidence, observedLifecycle209.handled()
				if !handled || status != "resume" || extras["gate"] != gateActionableFeedback || extras["reason"] != informalFeedbackReason {
					t.Fatalf("failed assessment = %s/%v, want actionable feedback", status, extras)
				}
			})
		}
	}
}

func TestInformalApprovalClassifier_CapturedReportsAndContradictions(t *testing.T) {
	for name, body := range capturedApprovingReports(t) {
		t.Run(name, func(t *testing.T) {
			c := informalClassificationForTest(t, func(raw map[string]any) {
				sources := raw["sources"].(map[string]any)
				records := sources["top_level"].([]any)
				records[0].(map[string]any)["body"] = body
			})
			request := informalRequestForTest()
			if got := c.reviewDecisionApprovalEvidenceFor(request, ""); len(got) != 1 {
				t.Fatalf("approval evidence = %v", got)
			}
			if got := c.informalFeedbackEvidenceFor(request, ""); len(got) != 0 {
				t.Fatalf("approving report produced feedback: %v", got)
			}
			changes := strings.Replace(body, "**APPROVED**", "**CHANGES_REQUESTED**", 1)
			if reviewDecisionApprovedBody(changes) || !informalReviewFeedbackConcrete(changes) {
				t.Fatal("a CHANGES_REQUESTED decision did not remain actionable")
			}
			contradictory := strings.Replace(body, "## Decision", "## Findings\n### Important\nFix the race in `worker.go`.\n\n## Decision", 1)
			if reviewDecisionApprovedBody(contradictory) {
				t.Fatal("an APPROVED report with an Important finding established approval")
			}
			if reviewpub.Published(contradictory) == informalReviewFeedbackConcrete(contradictory) {
				// A daemon publication that contradicts itself is unsure, so it
				// waits; an unpublished report keeps the prose feedback rules.
				t.Fatalf("contradictory report feedback = %t for a published=%t body, want feedback only without publication", informalReviewFeedbackConcrete(contradictory), reviewpub.Published(contradictory))
			}
		})
	}
}

func TestInformalApprovalLifecycle_ApprovalPrecedesFeedbackFacts(t *testing.T) {
	for _, checks := range []string{"pending", "success"} {
		t.Run(checks, func(t *testing.T) {
			d := decideImplementationPRLifecycle(implementationPRFacts{
				pr:      &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: checks, MergeStateStatus: "CLEAN"},
				headSHA: "current-sha", requireReview: true,
				retainedEvidence: retainedReviewEvidence{
					outcome: retainedReviewApproval, actionable: true,
					informalFeedback: []informalFeedbackEvidence{{Body: "Fix `worker`."}},
				},
			})
			if checks == "pending" && d.action != lifecycleAwait || checks == "success" && (d.action != lifecycleResume || d.gate != lifecycleGateReady) {
				t.Fatalf("approval lost to feedback: %+v", d)
			}
		})
	}
}

func capturedApprovalWithCIAdvice(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("testdata/approved-with-ci-advice.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestInformalApprovalLifecycle_ApprovalPrecedesSeparateFeedback(t *testing.T) {
	s, client, pr, workDir, now := informalApprovalLifecycleFixture(t, approvingMetricsReview)
	client.inline = []github.PRReviewComment{{ID: "feedback", Body: "Please fix the race in internal/batch/run_lifecycle.go before merging.", CommitID: pr.HeadRefOid, CreatedAt: now.Add(-30 * time.Second)}}
	observedLifecycle280 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	status, extras, handled := observedLifecycle280.status(), observedLifecycle280.Evidence, observedLifecycle280.handled()
	if !handled || status != "await" || extras["gate"] != "pending" {
		t.Fatalf("approved request with pending CI = (%s, %v, %t), want CI wait", status, extras, handled)
	}
	request := extras["review_request"].(map[string]any)
	if request["informal_feedback"] != nil {
		t.Fatalf("approved request retained feedback: %v", request)
	}
	pr.StatusCheckRollup = "success"
	observedLifecycle289 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	status, extras, handled = observedLifecycle289.status(), observedLifecycle289.Evidence, observedLifecycle289.handled()
	if !handled || status != "resume" || extras["gate"] != gateReadyToMerge {
		t.Fatalf("approved request with green CI = (%s, %v, %t), want merge work", status, extras, handled)
	}
}

func TestInformalApprovalLifecycle_OrdinaryAndCommentedApprovalsSuffice(t *testing.T) {
	for _, tc := range []struct {
		name, body, source, commit string
	}{
		{name: "plain LGTM", body: "LGTM"},
		{name: "plain looks good", body: "Looks good to me, thanks!"},
		{name: "plain plus one", body: "+1 👍"},
		{name: "structured decision", body: "## Decision: APPROVED"},
		{name: "published decision", body: "## Decision\n**APPROVED**\n<!-- sandman-publication:dd4739c73528fd53939c6a0e52d7fb08c5ea79d2a6f2f6c56b3bd1aee86bcf1f -->"},
		{name: "descriptive scope in no findings", body: "## Findings\nNo findings. Scope: worker.go. Cognitive complexity: 12.\n\n## Decision\nAPPROVED"},
		{name: "scope filename resembles repair verb", body: "## Findings\nNo findings. Scope: fix.go.\n\n## Decision\nAPPROVED"},
		{name: "resolved historical changes", body: approvingMetricsReview + "The previous CHANGES_REQUESTED and not-approved decision are resolved.\n"},
		{name: "COMMENTED current", body: "LGTM", source: "COMMENTED", commit: "current-sha"},
		{name: "COMMENTED no commit", body: "Looks good", source: "COMMENTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, client, pr, workDir, now := informalApprovalLifecycleFixture(t, tc.body)
			if tc.source != "" {
				client.comments = client.comments[:1]
				client.reviews = []github.PRReview{{ID: "commented", State: tc.source, Body: tc.body, CommitID: tc.commit, CreatedAt: now.Add(-time.Minute)}}
			}
			observedLifecycle316 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			status, extras, handled := observedLifecycle316.status(), observedLifecycle316.Evidence, observedLifecycle316.handled()
			request, _ := extras["review_request"].(map[string]any)
			if !handled || status != "await" || request["operation_resolved"] != true {
				t.Fatalf("informal approval with pending CI = (%s, %v, %t), want resolved-review CI await", status, extras, handled)
			}
			pr.StatusCheckRollup = "success"
			observedLifecycle322 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			status, extras, handled = observedLifecycle322.status(), observedLifecycle322.Evidence, observedLifecycle322.handled()
			if !handled || status != "resume" || extras["gate"] != gateReadyToMerge || extras["reason"] != "REVIEW_APPROVED" {
				t.Fatalf("informal approval with green CI = (%s, %v, %t), want approved merge resume without formal approval", status, extras, handled)
			}
		})
	}
}

func TestInformalApprovalLifecycle_RequestBoundariesAndFormalPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*registrationGitHubClient, *github.PR, time.Time)
	}{
		{"stale COMMENTED", func(c *registrationGitHubClient, pr *github.PR, now time.Time) {
			c.comments = c.comments[:1]
			c.reviews = []github.PRReview{{ID: "stale", State: "COMMENTED", Body: "LGTM", CommitID: "old-sha", CreatedAt: now.Add(-time.Minute)}}
		}},
		{"stale formal APPROVED", func(c *registrationGitHubClient, pr *github.PR, now time.Time) {
			c.reviews = []github.PRReview{{ID: "stale", State: "APPROVED", CommitID: "old-sha", CreatedAt: now.Add(-time.Minute)}}
		}},
		{"unknown formal APPROVED", func(c *registrationGitHubClient, pr *github.PR, now time.Time) {
			c.reviews = []github.PRReview{{ID: "unknown", State: "APPROVED", CreatedAt: now.Add(-time.Minute)}}
		}},
		{"formal requested changes", func(c *registrationGitHubClient, pr *github.PR, now time.Time) {
			c.reviews = []github.PRReview{{ID: "changes", State: "CHANGES_REQUESTED", Body: "Fix worker.go", CommitID: pr.HeadRefOid, CreatedAt: now.Add(-time.Minute)}}
		}},
		{"out of window", func(c *registrationGitHubClient, pr *github.PR, now time.Time) {
			c.comments[1].CreatedAt = now.Add(30 * time.Minute)
		}},
		{"unanswered new request", func(c *registrationGitHubClient, pr *github.PR, now time.Time) {
			c.comments = append(c.comments, github.PRComment{ID: "new-request", Body: "/sandman review", CreatedAt: now.Add(-30 * time.Second)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, client, pr, workDir, now := informalApprovalLifecycleFixture(t, approvingMetricsReview)
			tc.mutate(client, pr, *now)
			pr.StatusCheckRollup = "success"
			verdict := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			status, extras := verdict.status(), verdict.Evidence
			if status == "resume" && extras["gate"] == gateReadyToMerge {
				t.Fatal("ineligible or contradicted approval authorized merge")
			}
		})
	}
}

func TestInformalApprovalLifecycle_OnTimeResponseSurvivesDelayedReentry(t *testing.T) {
	s, client, pr, workDir, now := informalApprovalLifecycleFixture(t, approvingMetricsReview)
	client.comments[1].CreatedAt = time.Date(2026, 10, 13, 2, 30, 0, 0, time.UTC)
	// A response exactly at the original deadline is eligible even when first
	// observed by a fresh executor after capacity delay or process downtime.
	*now = time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)
	pr.StatusCheckRollup = "success"
	observedLifecycle374 := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	status, extras, handled := observedLifecycle374.status(), observedLifecycle374.Evidence, observedLifecycle374.handled()
	if !handled || status != "resume" || extras["gate"] != gateReadyToMerge {
		t.Fatalf("late observation of an on-time approval = (%s, %v, %t), want merge resume", status, extras, handled)
	}
	registration, err := readFileReviewRegistration(paths.NewLayout(nil, workDir).PRReviewRegistrationPath(pr.Number))
	if err != nil {
		t.Fatal(err)
	}
	if registration.Request.TriggerID != "request" || registration.Request.DeadlineUnixSeconds != int(client.comments[1].CreatedAt.Unix()) {
		t.Fatal("late observation changed the request identity or original deadline")
	}
}

func TestInformalApprovalLifecycle_AmbiguousDecisionsCannotAuthorizeMerge(t *testing.T) {
	for _, body := range []string{
		"## Decision\nAPPROVED once the race in worker.go is fixed",
		"## Decision\nNot LGTM",
		"## Decision\n> APPROVED",
		"## Decision\n`APPROVED`",
		"## Findings\nFix the race in worker.go before merging.\n\n## Decision\nAPPROVED",
		"## Findings\n### Important\nFix the race in worker.go before merging.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\nFix the race in worker.go before merging.\n\n## Decision\nAPPROVED",
		"## Findings\nNone. Not good: this remains racy.\n\n## Decision\nAPPROVED",
		"## Findings\nNone. This remains racy.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nThis remains racy.\n\n## Decision\nAPPROVED",
		"## Scope\nworker.go\n### Findings\nImportant: Fix the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Metrics\n### Decision\nCHANGES_REQUESTED\n\n## Decision\nAPPROVED",
		"## Findings\nNone. The code is unsafe.\n\n## Decision\nAPPROVED",
		"## Findings\nNone. The metrics reveal a correctness concern.\n\n## Decision\nAPPROVED",
		"## Findings\nNone. The metrics signals are limited to a data race.\n\n## Decision\nAPPROVED",
		"## Findings\nNone. The metric increase is limited to the adapter branch with a data race.\n\n## Decision\nAPPROVED",
		"## Findings\nNone. The metrics do not demonstrate a resolution of a correctness concern.\n\n## Decision\nAPPROVED",
		"## Findings\nNone. The metrics signals do not demonstrate a concern. Not good: this remains racy.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nAwait the pending CI checks; fix the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNo author follow-up required except fixing worker.go.\n\n## Decision\nAPPROVED",
		"## Findings\nNone in the API layer.\n### Important\nFix the race in worker.go before merging.\n\n## Decision\nAPPROVED",
		"## Findings: Fix the race in worker.go before merging.\n\n## Decision: APPROVED",
		"## Suggested next steps: Fix the race in worker.go before merging.\n\n## Decision: APPROVED",
		"## Findings - Fix the race in worker.go.\n\n## Decision: APPROVED",
		"## Suggested next steps - Fix the race in worker.go.\n\n## Decision: APPROVED",
		"## Findings\nNo minor issues; please fix `worker` race and concurrency issues.\n\n## Decision\nAPPROVED",
		"## Findings\nNo style issues; please fix the race on line 42 to resolve the correctness issues.\n\n## Decision\nAPPROVED",
		"## Decision\nAPPROVED — fix the race in worker.go before merging.",
		"## Decision\nAPPROVED - fix the race in worker.go.",
		"## Decision\nAPPROVED\n### Merge conditions\nFix the race in worker.go before merging.",
		"## Findings\nNone.\n\n## Decision\nAPPROVED\n\n## Merge conditions\nFix the race in worker.go before merging.",
		"## Findings\nNone.\n\n## Decision\nAPPROVED\n\n## Merge conditions: Fix the race in worker.go before merging.",
		"## Findings\nNo style findings.\nScope: in worker.go, fix the race.\n\n## Decision\nAPPROVED",
		"## Findings\nNo style findings.\nScope: worker.go and fix the race.\n\n## Decision\nAPPROVED",
		"## Findings\nNo style findings.\nScope: worker.go and address the race.\n\n## Decision\nAPPROVED",
		"## Findings\nNo style findings.\nScope: worker.go and `fix` the race.\n\n## Decision\nAPPROVED",
		"## Decision\nAPPROVED\n\n## Merge conditions\n`fix` the race in worker.go.",
		"## Decision\nAPPROVED\n\n## Merge conditions\nMerge only after fixing the race in worker.go.",
		"## Summary\nNot approved: worker.go still has a race.\n\n## Decision\nAPPROVED",
		"## Scope\nworker.go: approval is contingent on fixing the race before merging.\n\n## Findings\nNone.\n\n## Decision\nAPPROVED",
		"## Metrics\nCognitive complexity: 12. Please fix the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Tools used\nworker.go: merge only after fixing the race.\n\n## Decision\nAPPROVED",
		"## Scope\nworker.go: CHANGES_REQUESTED\n\n## Findings\nNone.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\nMerge only after fixing the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n### Decision\nCHANGES_REQUESTED\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\nCHANGES_REQUESTED\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n`Fix` the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n``Fix`` the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n**`Fix`** the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\nYou `must` repair worker.go.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n- `Important`: worker.go races.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n- _Important_: worker.go races.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n- ***Important***: worker.go races.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n* Important: worker.go races.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n+ **Important**: worker.go races.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n1. Important: worker.go races.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\nPublication was unavailable. Important: worker.go races.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\nThis is **not** approved: worker.go still races.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\nApproval is **subject** to repairing worker.go.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n### Merge conditions\nFix worker.go before merging.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone. Await the pending CI checks before merging; fix the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone. Merge only; after fixing the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNo author changes requested. Documentation is informational; fix the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone. **Fix** the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone. _Fix the race_ in worker.go.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone.\n+ **Fix** the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone.\n1) **Fix** the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone.\n- Important: worker.go races.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone. Publication was unavailable.\n- Important: worker.go races.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNo author changes requested. Publication was unavailable. Await the pending CI checks before merging.\n* **Blocking**: worker.go races.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone.\n* **Blocking**: worker.go races.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone.\n### Important\nworker.go races.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone. This is **not** approved: worker.go races.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNone. _CHANGES_REQUESTED_: worker.go races.\n\n## Decision\nAPPROVED",
		"## Findings\nNone.\n\nVerification against the acceptance criteria:\n__NOT_APPROVED__: worker.go races.\n\n## Decision\nAPPROVED",
		"## Scope\nworker.go: approval is **subject** to repairing the race.\n\n## Decision\nAPPROVED",
		"## Suggested next steps\nNo author changes requested. You **must** repair worker.go.\n\n## Decision\nAPPROVED",
		"## Findings\nNone. **Fix** the race in worker.go.\n\n## Decision\nAPPROVED",
		"## Scope\nworker.go: **Fix** the race.\n\n## Decision\nAPPROVED",
		"## Decision\nAPPROVED subject to fixing the race in worker.go.",
		"## Decision\n'APPROVED'",
		"No LGTM",
		"Previously approved",
		"Is this approved?",
		"## Decision\nAPPROVED\nBut fix the race in worker.go first.",
		"## Previous review progress\n### Decision\nAPPROVED",
	} {
		t.Run(body, func(t *testing.T) {
			s, _, pr, workDir, _ := informalApprovalLifecycleFixture(t, body)
			pr.StatusCheckRollup = "success"
			verdict := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			status, extras := verdict.status(), verdict.Evidence
			if status == "resume" && extras["gate"] == gateReadyToMerge {
				t.Fatal("ambiguous/conditional/quoted decision authorized merge")
			}
		})
	}
}
