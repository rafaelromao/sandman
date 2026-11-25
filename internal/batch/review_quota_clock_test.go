package batch

import (
	"os"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/reviewquota"
)

func TestManagedReviewQuotaRecoveryKeepsRemainingBudget(t *testing.T) {
	started := time.Date(2026, 11, 25, 11, 5, 0, 0, time.UTC)
	pausedAt := started.Add(12 * time.Minute)
	recoveredAt := pausedAt.Add(3 * time.Hour)
	pr := continuityProbePR()
	client := &registrationGitHubClient{
		fakeGitHubClient: fakeGitHubClient{prs: map[string]*github.PR{continuityBranch: pr}},
		comments: []github.PRComment{{
			ID: "trigger-1001", Body: "/sandman review", CreatedAt: started.Add(-time.Minute),
		}},
	}
	workDir := t.TempDir()
	layout := paths.NewLayout(nil, workDir)
	newSession := func(now time.Time) *runSession {
		return &runSession{
			issueNumber: 42,
			deps:        runDeps{githubClient: client, layout: layout},
			renderCfg:   prompt.RenderConfig{ReviewCommand: "/sandman review", ReviewTimeout: 1800},
			opts: runSessionOptions{
				now:         func() time.Time { return now },
				currentHead: func(string) (string, error) { return pr.HeadRefOid, nil },
			},
			reviewRegistrationNow:  func() time.Time { return now },
			reviewAttemptStartedAt: started.Add(-2 * time.Minute),
		}
	}
	if err := newSession(started).registerReviewRequest(t.Context(), workDir, pr, pr.HeadRefOid); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.ReviewsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	writeQuota := func(state reviewquota.State) {
		t.Helper()
		if err := atomicfs.WriteAtomicJSON(reviewquota.Path(layout.ReviewsDir()), state, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	observe := func(at time.Time) lifecycleVerdict {
		observer := &lifecycleObserver{runSession: newSession(at), outcomes: continuityOutcomes}
		return observer.decideLifecycleForAttempt(t.Context(), workDir, continuityBranch, "quota-recovery-run",
			true, "success", false, true, nil)
	}

	// A parent restarted during a recorded pause can still observe the same
	// request after its original deadline, even if the daemon is restarting.
	writeQuota(reviewquota.State{Paused: true, StartedAt: pausedAt})
	if verdict := observe(recoveredAt.Add(-time.Second)); verdict.Action != lifecycleAwait {
		t.Fatalf("during recorded pause after restart: got %s (%s), want to keep waiting", verdict.status(), verdict.Evidence["reason"])
	} else if _, _, bounded := lifecycleDeadline(verdict.Evidence); bounded {
		t.Fatalf("paused request still has a wall-clock deadline: %v", verdict.Evidence)
	}

	// The successful probe has just cleared the pause, and the reviewer has
	// not relaunched. A fresh observer must not depend on in-memory pause state.
	writeQuota(reviewquota.State{CompletedPauses: []reviewquota.Pause{{
		StartedAt: pausedAt, RecoveredAt: recoveredAt,
	}}})
	deadline := recoveredAt.Add(18 * time.Minute)
	for _, now := range []time.Time{recoveredAt.Add(4 * time.Second), deadline.Add(-time.Second)} {
		verdict := observe(now)
		if verdict.Action != lifecycleAwait {
			t.Fatalf("at %v after quota recovery: got %s (%s), want the same request to keep waiting", now, verdict.status(), verdict.Evidence["reason"])
		}
		if got, gate, ok := lifecycleDeadline(verdict.Evidence); !ok || gate != gateReviewTimeout || !got.Equal(deadline) {
			t.Fatalf("review deadline = (%v, %s, %t), want %v", got, gate, ok, deadline)
		}
		request, _ := verdict.Evidence["review_request"].(map[string]any)
		if request["trigger_id"] != "trigger-1001" {
			t.Fatalf("retained request trigger = %v, want trigger-1001", request["trigger_id"])
		}
	}
	if verdict := observe(deadline); verdict.Action != lifecycleFailure || verdict.Evidence["reason"] != reviewRequestExpiredReason {
		t.Fatalf("at adjusted deadline: got %s (%s), want REVIEW_REQUEST_EXPIRED", verdict.status(), verdict.Evidence["reason"])
	}
}

func TestDaemonReviewWaitRemainsRecoverableAcrossQuotaBoundary(t *testing.T) {
	start := time.Unix(1000, 0)
	deadline := start.Add(time.Minute)
	evidence := map[string]any{
		"branch": "issue-42", "base_branch": "main", "batch_id": "batch",
		"review_request": confirmedExpiryProbeRequest(),
	}
	request := evidence["review_request"].(map[string]any)
	request["trigger_prefix"] = "/sandman review"
	request["deadline_unix_seconds"] = deadline.Unix()
	layout := paths.NewLayout(nil, t.TempDir())
	location := layout.LocateBatch("batch", "")
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run", Issue: 42, Timestamp: start, Payload: evidence},
		{Type: "run.await", RunID: "run", Issue: 42, Timestamp: start, Payload: evidence},
	}}
	owner, err := newWaitOwner(layout, location, daemon.RunWait{
		Protocol: "run-wait/v1", RunID: "run", BatchID: "batch", Issue: 42,
		Branch: "issue-42", BaseBranch: "main", OperationID: "admission",
	}, func() time.Time { return start }, log)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	if err := owner.checkpoint(RowSpec{RunID: "run"}, false, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	wait, err := daemon.ReadRunWaitAt(layout, location, "run")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate owner loss before it could observe a quota transition. Only
	// normal recovery grace, not the old review deadline, limits re-entry.
	state := events.ProjectRunStates(log.snapshot())[0]
	reconciled, _, valid := reconcileRecoveryWait(wait, state)
	if !valid || !reconciled.RecoverableAt(deadline.Add(time.Second)) ||
		reconciled.RecoverableAt(start.Add(daemon.RunRecoveryGrace)) {
		t.Fatalf("review wait did not retain exactly the normal ownerless grace: %+v, valid=%t", reconciled, valid)
	}
	ciDeadline := deadline.Add(time.Second)
	evidence["ci_wait"] = map[string]any{"deadline_unix_seconds": ciDeadline.Unix()}
	if _, got, ok := lifecycleOwnershipOperation(evidence, "run"); !ok || !got.Equal(ciDeadline) {
		t.Fatalf("review wait changed CI's independent cutoff: %v, bounded=%t", got, ok)
	}
}

func TestManagedReviewClockIgnoresUnrelatedDaemonQuota(t *testing.T) {
	started := time.Unix(1000, 0)
	deadline := started.Add(30 * time.Minute)
	now := deadline.Add(time.Minute)
	layout := paths.NewLayout(nil, t.TempDir())
	if err := os.MkdirAll(layout.ReviewsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicfs.WriteAtomicJSON(reviewquota.Path(layout.ReviewsDir()), reviewquota.State{
		Paused: true, StartedAt: started.Add(time.Minute),
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	session := &runSession{deps: runDeps{layout: layout}}
	for _, prefix := range []string{"/external review", "/sandmanreview"} {
		request := reviewRequestEnvelope{
			TriggerPrefix: prefix, StartedUnixSeconds: int(started.Unix()), DeadlineUnixSeconds: int(deadline.Unix()),
		}
		if got, paused := session.managedReviewClock(request, now); paused || !got.Equal(deadline) {
			t.Fatalf("unrelated review command %q used the daemon quota clock: %v, paused=%t", prefix, got, paused)
		}
	}
}
