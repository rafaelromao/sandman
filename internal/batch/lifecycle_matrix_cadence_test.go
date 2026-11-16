package batch

import (
	"context"
	"testing"
	"time"
)

// The faster observation cadence changes only the interval between
// observations. These cases pin that the lifecycle decision taken at each
// observation is the existing one, at the session-gate seam with the
// production plan.

// CI turns green before the reviewer answers: the request stays waiting, no
// relaunch is spent, and the later current-head answer selects merge work.
func TestLifecycleMatrix_CIBeforeApprovalWaitsThenResumesOnApproval(t *testing.T) {
	s, client, pr, workDir, now := informalApprovalLifecycleFixture(t, approvingMetricsReview)
	answer := client.comments[1]
	client.comments = client.comments[:1] // the request is delivered; nobody answered yet
	pr.StatusCheckRollup = "success"
	s.opts.lifecyclePollPlan = nil // production cadence

	for i := 0; i < defaultAwaitResumeMax+2; i++ {
		verdict := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
		if verdict.Action != lifecycleAwait {
			t.Fatalf("observation %d with green CI and an unanswered request = %s/%v, want await", i, verdict.status(), verdict.Evidence)
		}
		if s.resumeCount != 0 {
			t.Fatalf("observation %d spent the relaunch allowance while waiting for the reviewer", i)
		}
		*now = now.Add(managedObservationInterval)
	}

	answer.CreatedAt = now.Add(-time.Second)
	client.comments = append(client.comments, answer)
	verdict := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	if verdict.Action != lifecycleResume || verdict.Evidence["gate"] != gateReadyToMerge || verdict.Evidence["reason"] != "REVIEW_APPROVED" {
		t.Fatalf("answered request = %s/%v, want approval merge resume", verdict.status(), verdict.Evidence)
	}
}

// An approval bound to a head the pull request has since left never authorizes
// merge work, however green the checks are.
func TestLifecycleMatrix_ApprovalForASupersededHeadDoesNotAuthorizeMerge(t *testing.T) {
	s, _, pr, workDir, now := informalApprovalLifecycleFixture(t, approvingMetricsReview)
	pr.StatusCheckRollup = "success"
	s.opts.lifecyclePollPlan = nil
	if verdict := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run"); verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "REVIEW_APPROVED" {
		t.Fatalf("precondition: current-head approval with green CI = %s/%v, want approval merge resume", verdict.status(), verdict.Evidence)
	}

	pr.HeadRefOid = "next-sha"
	s.opts.currentHead = func(string) (string, error) { return "next-sha", nil }
	*now = now.Add(managedObservationInterval)
	verdict := s.observeLifecycleForTest(t.Context(), workDir, gateTestBranch, "", "same-run")
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "REVIEW_REQUEST_REQUIRED" {
		t.Fatalf("superseded-head observation = %s/%v, want a fresh review request for the new head, never merge work", verdict.status(), verdict.Evidence)
	}
}

// Cancellation during the default foreground wait aborts without another
// observation, on the real timer rather than a fake.
func TestForegroundDefaultWaitIsCancellationAware(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := &runSession{opts: runSessionOptions{}}
	start := time.Now()
	if err := session.waitForLifecyclePoll(ctx, managedObservationInterval); err == nil {
		t.Fatal("cancelled foreground wait returned nil")
	}
	verdict := newLifecycleObserver(session).waitForTransition(ctx, lifecycleObservationRequest{RunID: "run"}, AgentRunResult{}, lifecycleVerdict{Action: lifecycleAwait})
	if verdict.Action != lifecycleAborted {
		t.Fatalf("cancelled foreground observation = %s, want aborted without a further observation", verdict.status())
	}
	if elapsed := time.Since(start); elapsed >= managedObservationInterval/2 {
		t.Fatalf("cancelled foreground wait blocked for %v", elapsed)
	}
}
