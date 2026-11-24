package batch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestCIWaitEvidencePersistsDeadlinePerHead(t *testing.T) {
	workDir := t.TempDir()
	s := &runSession{}
	pr := &github.PR{Number: 17, HeadRefOid: "first-head"}

	first, err := s.ciWaitEvidence(context.Background(), workDir, pr, "first-head")
	if err != nil {
		t.Fatalf("first CI wait evidence: %v", err)
	}
	second, err := s.ciWaitEvidence(context.Background(), workDir, pr, "first-head")
	if err != nil {
		t.Fatalf("second CI wait evidence: %v", err)
	}
	firstWait := first["ci_wait"].(map[string]any)
	secondWait := second["ci_wait"].(map[string]any)
	if firstWait["deadline_unix_seconds"] != secondWait["deadline_unix_seconds"] {
		t.Fatalf("same-head deadline renewed: first=%v second=%v", firstWait, secondWait)
	}

	pr.HeadRefOid = "second-head"
	reset, err := s.ciWaitEvidence(context.Background(), workDir, pr, "second-head")
	if err != nil {
		t.Fatalf("reset CI wait evidence: %v", err)
	}
	resetWait := reset["ci_wait"].(map[string]any)
	if resetWait["head_sha"] != "second-head" {
		t.Fatalf("head SHA = %v, want second-head", resetWait["head_sha"])
	}
	path := filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json")
	registration, err := readCIWaitRegistration(path)
	if err != nil {
		t.Fatalf("read persisted CI wait: %v", err)
	}
	if registration.HeadSHA != "second-head" || registration.EffectiveTimeoutSecs != int64(ciWaitTimeout/time.Second) {
		t.Fatalf("registration = %#v", registration)
	}
}

func TestCIWaitEvidenceKeepsVerifiedActionsGenerationMonotonicAcrossRestart(t *testing.T) {
	workDir := t.TempDir()
	now := time.Unix(20_000, 0).UTC()
	session := &runSession{opts: runSessionOptions{now: func() time.Time { return now }}}
	pr := &github.PR{
		Number: 17, HeadRefOid: "current-head", BaseRefName: "main",
		StatusCheckRollup: "failure", CIExecutionID: "stale-rollup-identity",
	}
	execution := func(runID, runNumber int64, attempt int, status, conclusion string) *github.ManagedCIExecution {
		return &github.ManagedCIExecution{
			Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "current-head",
			WorkflowID: 273743359, Event: "pull_request", RunID: runID, RunNumber: runNumber, Attempt: attempt,
			Status: status, Conclusion: conclusion,
		}
	}

	first, err := session.ciWaitEvidence(context.Background(), workDir, pr, pr.HeadRefOid, execution(100, 5288, 1, "queued", ""))
	if err != nil {
		t.Fatal(err)
	}
	firstWait := first["ci_wait"].(map[string]any)
	if firstWait["execution_id"] != "actions:273743359:100:1" {
		t.Fatalf("first execution identity = %#v", firstWait["execution_id"])
	}
	firstDeadline := firstWait["deadline_unix_seconds"]
	now = now.Add(5 * time.Minute)
	restarted := &runSession{opts: runSessionOptions{now: func() time.Time { return now }}}
	same, err := restarted.ciWaitEvidence(context.Background(), workDir, pr, pr.HeadRefOid, execution(100, 5288, 1, "in_progress", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := same["ci_wait"].(map[string]any)["deadline_unix_seconds"]; got != firstDeadline {
		t.Fatalf("restart renewed the same Actions execution deadline: first=%v got=%v", firstDeadline, got)
	}

	stale, err := restarted.ciWaitEvidence(context.Background(), workDir, pr, pr.HeadRefOid, execution(99, 5287, 1, "completed", "success"))
	if err != nil {
		t.Fatal(err)
	}
	staleWait := stale["ci_wait"].(map[string]any)
	if staleWait["execution_id"] != "actions:273743359:100:1" || staleWait["deadline_unix_seconds"] != firstDeadline {
		t.Fatalf("stale Actions observation rolled back the retained generation: %#v", staleWait)
	}

	now = now.Add(2 * time.Minute)
	higherAttempt, err := restarted.ciWaitEvidence(context.Background(), workDir, pr, pr.HeadRefOid, execution(100, 5288, 2, "queued", ""))
	if err != nil {
		t.Fatal(err)
	}
	higherAttemptWait := higherAttempt["ci_wait"].(map[string]any)
	if higherAttemptWait["execution_id"] != "actions:273743359:100:2" ||
		higherAttemptWait["deadline_unix_seconds"] != now.Add(ciWaitTimeout).Unix() {
		t.Fatalf("verified higher attempt did not start a new bounded generation: %#v", higherAttemptWait)
	}

	now = now.Add(3 * time.Minute)
	newer, err := restarted.ciWaitEvidence(context.Background(), workDir, pr, pr.HeadRefOid, execution(101, 5289, 1, "queued", ""))
	if err != nil {
		t.Fatal(err)
	}
	newerWait := newer["ci_wait"].(map[string]any)
	if newerWait["execution_id"] != "actions:273743359:101:1" ||
		newerWait["deadline_unix_seconds"] != now.Add(ciWaitTimeout).Unix() {
		t.Fatalf("verified newer execution did not start its own bounded generation: %#v", newerWait)
	}
	registration, err := readCIWaitRegistration(filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json"))
	if err != nil {
		t.Fatal(err)
	}
	if registration.ActionsExecution == nil || registration.ActionsExecution.Repository != "owner/repo" ||
		registration.ActionsExecution.PullRequest != 17 || registration.ActionsExecution.BaseRefName != "main" ||
		registration.ActionsExecution.HeadSHA != "current-head" || registration.ActionsExecution.WorkflowID != 273743359 ||
		registration.ActionsExecution.Event != "pull_request" ||
		registration.ActionsExecution.RunID != 101 || registration.ActionsExecution.RunNumber != 5289 ||
		registration.ActionsExecution.Attempt != 1 {
		t.Fatalf("persisted Actions identity is incomplete: %+v", registration.ActionsExecution)
	}
}

func TestCIWaitEvidenceLinksStaleAncestorAfterTwoVerifiedReplacementsAndRestart(t *testing.T) {
	workDir := t.TempDir()
	now := time.Unix(25_000, 0).UTC()
	session := &runSession{opts: runSessionOptions{now: func() time.Time { return now }}}
	pr := &github.PR{
		Number: 17, HeadRefOid: "head", BaseRefName: "main", StatusCheckRollup: "failure",
		CIExecutionID: "rollup-a", CIFailedWorkflow: &github.CIFailedWorkflow{RunID: "100"},
	}
	execution := func(runID, runNumber int64) *github.ManagedCIExecution {
		return &github.ManagedCIExecution{
			Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "head",
			WorkflowID: 9, Event: "pull_request", RunID: runID, RunNumber: runNumber, Attempt: 1,
			Status: "queued",
		}
	}
	if _, err := session.ciWaitEvidence(context.Background(), workDir, pr, "head", execution(101, 51)); err != nil {
		t.Fatal(err)
	}
	pr.CIExecutionID = "rollup-b"
	pr.CIFailedWorkflow = &github.CIFailedWorkflow{RunID: "101"}
	now = now.Add(5 * time.Minute)
	if _, err := session.ciWaitEvidence(context.Background(), workDir, pr, "head", execution(102, 52)); err != nil {
		t.Fatal(err)
	}

	pr.CIExecutionID = "rollup-a"
	pr.CIFailedWorkflow = &github.CIFailedWorkflow{RunID: "100"}
	restarted := &runSession{opts: runSessionOptions{now: func() time.Time { return now.Add(time.Minute) }}}
	evidence, err := restarted.ciWaitEvidence(context.Background(), workDir, pr, "head")
	if err != nil {
		t.Fatal(err)
	}
	wait := evidence["ci_wait"].(map[string]any)
	if wait["execution_id"] != "actions:9:102:1" ||
		!managedCIReplacementStillResolves(evidence, pr, "owner/repo") {
		t.Fatalf("stale ancestor read lost the verified A→B→C replacement chain: %#v", evidence)
	}
	registration, err := readCIWaitRegistration(filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(registration.SourceWorkflowRunIDs) != 2 ||
		registration.SourceWorkflowRunIDs[0] != 100 || registration.SourceWorkflowRunIDs[1] != 101 {
		t.Fatalf("persisted replacement ancestry = %v, want [100 101]", registration.SourceWorkflowRunIDs)
	}
}

func TestCIWaitEvidenceDoesNotUseReplacementForDifferentUnverifiedRollup(t *testing.T) {
	workDir := t.TempDir()
	now := time.Unix(30_000, 0).UTC()
	session := &runSession{opts: runSessionOptions{now: func() time.Time { return now }}}
	pr := &github.PR{
		Number: 17, HeadRefOid: "head", BaseRefName: "main", StatusCheckRollup: "failure",
		CIExecutionID: "source-rollup-1",
	}
	execution := &github.ManagedCIExecution{
		Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "head",
		WorkflowID: 9, Event: "pull_request", RunID: 101, RunNumber: 51, Attempt: 1, Status: "queued",
	}
	if _, err := session.ciWaitEvidence(context.Background(), workDir, pr, "head", execution); err != nil {
		t.Fatal(err)
	}
	pr.CIExecutionID = "source-rollup-2"
	pr.CIFailedWorkflow = &github.CIFailedWorkflow{RunID: "202"}
	now = now.Add(4 * time.Minute)
	evidence, err := session.ciWaitEvidence(context.Background(), workDir, pr, "head")
	if err != nil {
		t.Fatal(err)
	}
	wait, ok := evidence["ci_wait"].(map[string]any)
	if !ok || wait["execution_id"] != "actions:9:101:1" ||
		managedCIReplacementStillResolves(evidence, pr, "owner/repo") {
		t.Fatalf("older replacement authorized a different unverified failing rollup: %#v", evidence)
	}
	registration, err := readCIWaitRegistration(filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json"))
	if err != nil {
		t.Fatal(err)
	}
	if registration.ActionsExecution == nil || registration.ActionsExecution.RunID != 101 ||
		registration.DeadlineUnixSeconds != now.Add(-4*time.Minute).Add(ciWaitTimeout).Unix() {
		t.Fatalf("unverified rollup changed the retained Actions generation: %+v", registration)
	}
}

func TestCIWaitEvidenceDoesNotRegressCompletedExecutionOnStaleRead(t *testing.T) {
	workDir := t.TempDir()
	now := time.Unix(40_000, 0).UTC()
	session := &runSession{opts: runSessionOptions{now: func() time.Time { return now }}}
	pr := &github.PR{Number: 17, HeadRefOid: "head", BaseRefName: "main"}
	execution := func(status, conclusion string) *github.ManagedCIExecution {
		return &github.ManagedCIExecution{
			Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "head",
			WorkflowID: 9, Event: "pull_request", RunID: 101, RunNumber: 51, Attempt: 1,
			Status: status, Conclusion: conclusion,
		}
	}
	if _, err := session.ciWaitEvidence(context.Background(), workDir, pr, "head", execution("queued", "")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	completed, err := session.ciWaitEvidence(context.Background(), workDir, pr, "head", execution("completed", "failure"))
	if err != nil {
		t.Fatal(err)
	}
	completedWait := completed["ci_wait"].(map[string]any)
	deadline := completedWait["deadline_unix_seconds"]
	now = now.Add(time.Minute)
	stale, err := session.ciWaitEvidence(context.Background(), workDir, pr, "head", execution("in_progress", ""))
	if err != nil {
		t.Fatal(err)
	}
	staleWait := stale["ci_wait"].(map[string]any)
	observed := staleWait["actions_execution"].(map[string]any)
	if observed["status"] != "completed" || observed["conclusion"] != "failure" ||
		staleWait["deadline_unix_seconds"] != deadline {
		t.Fatalf("stale same-attempt read regressed the terminal observation: %#v", staleWait)
	}
}

func TestCIWaitEvidenceRejectsContradictoryPersistedActionsStatus(t *testing.T) {
	workDir := t.TempDir()
	timeout := int64(ciWaitTimeout / time.Second)
	registration := ciWaitRegistration{
		Protocol: ciWaitProtocol, PullRequest: 17, HeadSHA: "head",
		ExecutionID: "actions:9:101:1",
		ActionsExecution: &ciWaitActionsExecution{
			Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "head",
			WorkflowID: 9, Event: "pull_request", RunID: 101, RunNumber: 51, Attempt: 1,
			Status: "queued", Conclusion: "success", Replacement: true,
		},
		StartedUnixSeconds: 100, DeadlineUnixSeconds: 100 + timeout, EffectiveTimeoutSecs: timeout,
	}
	data, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	evidence, err := (&runSession{}).ciWaitEvidence(
		context.Background(), workDir,
		&github.PR{Number: 17, HeadRefOid: "head", BaseRefName: "main"},
		"head",
	)
	if err == nil || evidence != nil {
		t.Fatalf("contradictory persisted status authorized CI wait: evidence=%#v err=%v", evidence, err)
	}
}

func TestCIWaitEvidenceRejectsPersistedNonReplacementActionsExecution(t *testing.T) {
	workDir := t.TempDir()
	timeout := int64(ciWaitTimeout / time.Second)
	registration := ciWaitRegistration{
		Protocol: ciWaitProtocol, PullRequest: 17, HeadSHA: "head",
		ExecutionID: "actions:9:101:1",
		ActionsExecution: &ciWaitActionsExecution{
			Repository: "owner/repo", PullRequest: 17, BaseRefName: "main", HeadSHA: "head",
			WorkflowID: 9, Event: "pull_request", RunID: 101, RunNumber: 51, Attempt: 1,
			Status: "queued", Replacement: false,
		},
		StartedUnixSeconds: 100, DeadlineUnixSeconds: 100 + timeout, EffectiveTimeoutSecs: timeout,
	}
	data, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	evidence, err := (&runSession{}).ciWaitEvidence(
		context.Background(), workDir,
		&github.PR{Number: 17, HeadRefOid: "head", BaseRefName: "main"},
		"head",
	)
	if err == nil || evidence != nil {
		t.Fatalf("persisted non-replacement Actions execution authorized CI wait: evidence=%#v err=%v", evidence, err)
	}
}

func TestManagedCIReplacementStillResolvesRejectsContradictoryActionsStatus(t *testing.T) {
	pr := &github.PR{Number: 17, HeadRefOid: "head", BaseRefName: "main"}
	evidence := map[string]any{
		"ci_wait": map[string]any{
			"protocol": ciWaitProtocol, "head_sha": "head", "execution_id": "actions:9:101:1",
			"actions_execution": map[string]any{
				"repository": "owner/repo", "pull_request": 17, "base_ref_name": "main",
				"head_sha": "head", "workflow_id": 9, "event": "pull_request",
				"run_id": 101, "run_number": 51, "attempt": 1,
				"status": "queued", "conclusion": "success", "replacement": true,
			},
		},
	}
	if managedCIReplacementStillResolves(evidence, pr, "owner/repo") {
		t.Fatalf("contradictory retained status authorized replacement wait")
	}
}

func TestManagedCIReplacementStillResolvesRequiresReplacementAndActiveStatus(t *testing.T) {
	tests := []struct {
		name          string
		replacement   bool
		status        string
		conclusion    string
		pendingChecks bool
		want          bool
	}{
		{name: "queued replacement", replacement: true, status: "queued", want: true},
		{name: "running replacement", replacement: true, status: "in_progress", want: true},
		{name: "waiting replacement", replacement: true, status: "waiting"},
		{name: "requested replacement", replacement: true, status: "requested"},
		{name: "pending replacement", replacement: true, status: "pending"},
		{name: "passed replacement with stale rollup", replacement: true, status: "completed", conclusion: "success", want: true},
		{name: "failed replacement", replacement: true, status: "completed", conclusion: "failure"},
		{name: "failed replacement with visible pending checks", replacement: true, status: "completed", conclusion: "failure", pendingChecks: true},
		{name: "non-replacement queued execution", status: "queued"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failedRunID := "100"
			if test.pendingChecks {
				failedRunID = "101"
			}
			pr := &github.PR{
				Number: 17, HeadRefOid: "head", BaseRefName: "main", StatusCheckRollup: "failure",
				CIExecutionID:     "source-rollup",
				CIHasActiveChecks: test.pendingChecks,
				CIFailedWorkflow:  &github.CIFailedWorkflow{RunID: failedRunID},
			}
			evidence := map[string]any{
				"ci_wait": map[string]any{
					"protocol": ciWaitProtocol, "head_sha": "head", "execution_id": "actions:9:101:1",
					"source_execution_id": "source-rollup", "source_workflow_run_id": int64(100),
					"actions_execution": map[string]any{
						"repository": "owner/repo", "pull_request": 17, "base_ref_name": "main",
						"head_sha": "head", "workflow_id": 9, "event": "pull_request",
						"run_id": 101, "run_number": 51, "attempt": 1, "status": test.status,
						"conclusion": test.conclusion, "replacement": test.replacement,
					},
				},
			}
			if got := managedCIReplacementStillResolves(evidence, pr, "owner/repo"); got != test.want {
				t.Fatalf("replacement wait authorization = %t, want %t", got, test.want)
			}
		})
	}
}

func TestCIWaitEvidenceCancellationWhileLockContendedDoesNotPersist(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json")
	ready := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLock := func() { releaseOnce.Do(func() { close(release) }) }
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withOperationLock(context.Background(), path, func() error {
			close(ready)
			<-release
			return nil
		})
	}()
	<-ready
	defer func() {
		releaseLock()
		if err := <-holderDone; err != nil {
			t.Errorf("release CI wait lock: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := (&runSession{}).ciWaitEvidence(ctx, workDir, &github.PR{Number: 17, HeadRefOid: "head"}, "head")
		result <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("contended CI observation error = %v, want context cancellation", err)
		}
	case <-time.After(250 * time.Millisecond):
		releaseLock()
		<-holderDone
		t.Fatal("contended CI observation ignored cancellation")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("canceled CI observation wrote state: err=%v", err)
	}
}

func TestLifecycleDeadlineUsesCIWaitWhenNoReviewRequest(t *testing.T) {
	deadline := time.Now().Add(time.Minute).Truncate(time.Second)
	got, gate, ok := lifecycleDeadline(map[string]any{
		"ci_wait": map[string]any{"deadline_unix_seconds": deadline.Unix()},
	})
	if !ok || gate != gateCIWaitTimeout || !got.Equal(deadline) {
		t.Fatalf("CI deadline = (%v, %q, %t), want (%v, %q, true)", got, gate, ok, deadline, gateCIWaitTimeout)
	}
}

func TestLifecycleDeadlineUsesEarlierCIWaitAlongsideReview(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	got, gate, ok := lifecycleDeadline(map[string]any{
		"review_request": map[string]any{"deadline_unix_seconds": float64(now.Add(2 * time.Minute).Unix())},
		"ci_wait":        map[string]any{"deadline_unix_seconds": now.Add(time.Minute).Unix()},
	})
	if !ok || gate != gateCIWaitTimeout || !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("deadline = (%v, %q, %t), want earlier CI deadline", got, gate, ok)
	}
}

func TestLifecycleDeadlineUsesConfirmedReviewRegistration(t *testing.T) {
	deadline := time.Now().Add(time.Minute).Truncate(time.Second)
	diagnostic := reviewRegistrationDiagnostic(&reviewRequestRegistration{
		Request: reviewRequestEnvelope{DeadlineUnixSeconds: int(deadline.Unix())},
	})
	got, gate, ok := lifecycleDeadline(diagnostic)
	if !ok || gate != gateReviewTimeout || !got.Equal(deadline) {
		t.Fatalf("registration deadline = (%v, %q, %t), want (%v, %q, true)", got, gate, ok, deadline, gateReviewTimeout)
	}
}
