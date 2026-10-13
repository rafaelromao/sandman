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
None.

## Findings

## Suggested next steps
None.

## Decision
**APPROVED**

## Previous review progress
The prior concern in internal/batch/orchestrator_resume.go is resolved.
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
		"## Decision\nAPPROVED\nBut fix the race in worker.go first.",
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
