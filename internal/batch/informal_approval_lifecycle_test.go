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
