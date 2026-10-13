package batch

import (
	"io"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
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

const capturedApprovalWithCIWaitAdvice = `## Summary
The requested changes are addressed in this diff.

## Quality check
### Scope
Changed internal/batch/run_lifecycle.go and internal/batch/review_decision_approval.go.

### Metrics
The metrics signals do not demonstrate a construct-specific readability or change-risk concern in this diff.

### Findings
None. The metrics signals do not demonstrate a construct-specific readability or change-risk concern in this diff.

## Findings

## Suggested next steps
Await the pending required CI checks.

## Decision
**APPROVED**

## Previous review progress
All prior findings are resolved.

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
			{ID: "approval", Body: body, CreatedAt: now.Add(time.Minute)},
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
		status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
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
	status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	if !handled || status != "resume" || extras["gate"] != gateReadyToMerge || extras["reason"] != "REVIEW_APPROVED" {
		t.Fatalf("green CI = (%s, %v, %t), want approval merge resume", status, extras, handled)
	}
	request := extras["review_request"].(map[string]any)
	if request["outcome"] != "approved" || request["informal_feedback"] != nil {
		t.Fatalf("merge evidence = %v, want approval without false feedback", request)
	}
}

func TestInformalApprovalLifecycle_ApprovalWithCIWaitAdviceAndMetricExplanation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"CI wait advice", "## Summary\nChanged worker.go.\n\n## Suggested next steps\nAwait the pending required CI checks.\n\n## Decision\nAPPROVED"},
		{"metric explanation", "## Summary\nChanged worker.go.\n\n## Findings\nNone. The metrics signals do not demonstrate a readability concern.\n\n## Decision\nAPPROVED"},
		{"captured response", capturedApprovalWithCIWaitAdvice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, pr, workDir, now := informalApprovalLifecycleFixture(t, tc.body)
			for i := 0; i < defaultAwaitResumeMax+2; i++ {
				status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
				if !handled || status != "await" || extras["gate"] != "pending" || s.resumeCount != 0 {
					t.Fatalf("pending CI = %s/%v, want await without repair relaunch", status, extras["gate"])
				}
				request := extras["review_request"].(map[string]any)
				if request["operation_resolved"] != true || request["informal_feedback"] != nil {
					t.Fatalf("approval evidence = %v, want resolved approval without feedback", request)
				}
				*now = now.Add(time.Minute)
			}
			pr.StatusCheckRollup = "success"
			status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			if !handled || status != "resume" || extras["gate"] != gateReadyToMerge {
				t.Fatalf("green CI = %s/%v, want approved merge resume", status, extras["gate"])
			}
		})
	}
}

func TestInformalApprovalLifecycle_SeparateFeedbackStillResumesRepair(t *testing.T) {
	s, client, pr, workDir, now := informalApprovalLifecycleFixture(t, approvingMetricsReview)
	client.inline = []github.PRReviewComment{{ID: "feedback", Body: "Please fix the race in internal/batch/run_lifecycle.go before merging.", CommitID: pr.HeadRefOid, CreatedAt: now.Add(-30 * time.Second)}}
	status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	if !handled || status != "resume" || extras["gate"] != gateActionableFeedback {
		t.Fatalf("separate concrete feedback = (%s, %v, %t), want repair resume", status, extras, handled)
	}
	request := extras["review_request"].(map[string]any)
	feedback, ok := request["informal_feedback"].([]informalFeedbackEvidence)
	if !ok || len(feedback) != 1 || feedback[0].ID != "feedback" {
		t.Fatalf("retained feedback = %v, want only the independent requested change", request["informal_feedback"])
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
			status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			request, _ := extras["review_request"].(map[string]any)
			if !handled || status != "await" || request["operation_resolved"] != true {
				t.Fatalf("informal approval with pending CI = (%s, %v, %t), want resolved-review CI await", status, extras, handled)
			}
			pr.StatusCheckRollup = "success"
			status, extras, handled = s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
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
			status, extras, _ := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
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
	status, extras, handled := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
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
			status, extras, _ := s.lifecycleDecisionForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
			if status == "resume" && extras["gate"] == gateReadyToMerge {
				t.Fatal("ambiguous/conditional/quoted decision authorized merge")
			}
		})
	}
}
