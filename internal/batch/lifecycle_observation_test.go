package batch

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestLifecycleObservation_VerifiedMergeWinsOverRetainedReview(t *testing.T) {
	workDir := t.TempDir()
	writeTimedOutReviewRequest(t, workDir)
	now := time.Unix(2_000, 0)
	session := &runSession{
		issueNumber: 42,
		deps: runDeps{
			githubClient: &fakeGitHubClient{
				issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
				prs: map[string]*github.PR{gateTestBranch: {
					Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefOid: "current-sha",
					StatusCheckRollup: "failure", MergeStateStatus: "CONFLICTING",
				}},
			},
			errorLog: io.Discard,
		},
		opts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	var observer lifecycleObservation = newLifecycleObserver(session)

	verdict := observer.Observe(context.Background(), lifecycleObservationRequest{
		WorkDir: workDir, Branch: gateTestBranch, RunID: "run-test", HostPathsReady: true, AttemptStatus: "success",
	})
	if verdict.Action != lifecycleSuccess || len(verdict.Evidence) != 0 {
		t.Fatalf("verified merge verdict = (%v, %#v), want success with no failure evidence", verdict.Action, verdict.Evidence)
	}
}

func TestLifecycleObservation_VerdictTable(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	tests := []struct {
		name       string
		pr         *github.PR
		prepare    func(*testing.T, string)
		wantAction lifecycleAction
		wantKind   lifecycleVerdictKind
		wantGate   string
		wantWait   string
	}{
		{
			name: "await current-head CI with execution identity and deadline",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-2",
				StatusCheckRollup: "pending", MergeStateStatus: "BLOCKED"},
			wantAction: lifecycleAwait,
			wantWait:   "ci_wait",
		},
		{
			name: "await confirmed review with request identity and deadline",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success",
				ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED"},
			prepare: func(t *testing.T, workDir string) {
				writeTimedOutReviewRequest(t, workDir)
				writeCanonicalRegistrationForTest(t, workDir)
			},
			wantAction: lifecycleAwait,
			wantWait:   "review_request",
		},
		{
			name: "resume current-head CI remediation",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "failure",
				MergeStateStatus: "CLEAN"},
			wantAction: lifecycleResume,
			wantGate:   "ci-failure",
		},
		{
			name: "resume approved clean PR for merge work",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success",
				ReviewDecision: "APPROVED", MergeStateStatus: "CLEAN"},
			wantAction: lifecycleResume,
			wantGate:   gateReadyToMerge,
		},
		{
			name: "verified merge completion",
			pr: &github.PR{Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefOid: "current-sha",
				StatusCheckRollup: "failure", MergeStateStatus: "CONFLICTING"},
			wantAction: lifecycleSuccess,
			wantKind:   lifecycleVerdictCompletion,
		},
		{
			name:       "closed without merge",
			pr:         &github.PR{Number: 17, State: "closed", HeadRefOid: "current-sha"},
			wantAction: lifecycleFailure,
			wantKind:   lifecycleVerdictClosedPR,
		},
		{
			name: "resume merge conflict remediation",
			pr: &github.PR{Number: 17, State: "open", HeadRefOid: "current-sha", StatusCheckRollup: "success",
				MergeStateStatus: "CONFLICTING"},
			wantAction: lifecycleResume,
			wantGate:   "merge-conflict",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			if tt.prepare != nil {
				tt.prepare(t, workDir)
			}
			session := &runSession{
				issueNumber: 42,
				deps: runDeps{
					githubClient: &fakeGitHubClient{
						issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
						prs:    map[string]*github.PR{gateTestBranch: tt.pr},
					},
					errorLog: io.Discard,
				},
				opts: runSessionOptions{
					now:         func() time.Time { return now },
					currentHead: func(string) (string, error) { return "current-sha", nil },
				},
			}
			observer := lifecycleObservation(newLifecycleObserver(session))
			verdict := observer.Observe(context.Background(), lifecycleObservationRequest{
				WorkDir: workDir, Branch: gateTestBranch, RunID: "run-test", HostPathsReady: true, AttemptStatus: "success",
			})
			if verdict.Action != tt.wantAction || verdict.Kind != tt.wantKind {
				t.Fatalf("verdict = (%v, kind %v, %#v), want action %v kind %v", verdict.Action, verdict.Kind, verdict.Evidence, tt.wantAction, tt.wantKind)
			}
			if tt.wantGate != "" && stringEvidence(verdict.Evidence, "gate") != tt.wantGate {
				t.Fatalf("verdict gate = %q, want %q; evidence=%#v", stringEvidence(verdict.Evidence, "gate"), tt.wantGate, verdict.Evidence)
			}
			if tt.wantWait != "" {
				wait, ok := verdict.Evidence[tt.wantWait].(map[string]any)
				if verdict.Action != lifecycleAwait || !ok || wait["deadline_unix_seconds"] == nil {
					t.Fatalf("await evidence %q = %#v, want an identity-bound deadline", tt.wantWait, verdict.Evidence[tt.wantWait])
				}
				if tt.wantWait == "ci_wait" && wait["execution_id"] != "checks-2" {
					t.Fatalf("CI execution identity = %#v, want checks-2", wait["execution_id"])
				}
				if tt.wantWait == "review_request" && (wait["trigger_id"] == nil || wait["head_sha"] != "current-sha") {
					t.Fatalf("review request identity = %#v, want current trigger and head", wait)
				}
			}
		})
	}
}

func TestLifecycleObservation_FailedCIWithActiveSiblingKeepsRuntimePrerequisiteWait(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	workDir := t.TempDir()
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-with-pending-sibling",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, MergeStateStatus: "BLOCKED",
		}},
	}
	log := &spyEventLog{}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, eventLog: log, errorLog: io.Discard},
		opts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	observer := newLifecycleObserver(session)
	request := lifecycleObservationRequest{
		WorkDir: workDir, Branch: gateTestBranch, RunID: "run-cannot-rerun-yet", HostPathsReady: true,
		AttemptStatus: "success",
	}

	entry := observer.Observe(t.Context(), request)
	if entry.Action != lifecycleResume || entry.Evidence["gate"] != "ci-failure" {
		t.Fatalf("initial entry must launch the implementation agent to diagnose/repair despite active siblings: %+v", entry)
	}

	request.AfterAgentAttempt = true
	blockedRerun := observer.Observe(t.Context(), request)
	if blockedRerun.Action != lifecycleAwait || blockedRerun.Evidence["gate"] != string(lifecycleGatePending) || blockedRerun.Evidence["reason"] != "CI_FAILURE" {
		t.Fatalf("after the agent diagnoses the failure, the runtime must own observation of the active workflow prerequisite: %+v", blockedRerun)
	}
	ciWait, ok := blockedRerun.Evidence["ci_wait"].(map[string]any)
	if !ok || ciWait["execution_id"] != "checks-with-pending-sibling" {
		t.Fatalf("prerequisite await lost identified CI execution: %#v", blockedRerun.Evidence)
	}
	initialDeadline, ok := ciWait["deadline_unix_seconds"].(int64)
	if !ok {
		t.Fatalf("CI wait deadline has unexpected type %T", ciWait["deadline_unix_seconds"])
	}

	client.prs[gateTestBranch].CIHasActiveChecks = false
	now = now.Add(2 * time.Minute)
	completedPrerequisite := observer.Observe(t.Context(), request)
	if completedPrerequisite.Action != lifecycleResume || completedPrerequisite.Evidence["gate"] != "ci-failure" {
		t.Fatalf("completed workflow prerequisite must return failed CI to implementation ownership: %+v", completedPrerequisite)
	}
	retained, err := readCIWaitRegistration(filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json"))
	if err != nil || retained.DeadlineUnixSeconds != initialDeadline {
		t.Fatalf("same CI execution deadline changed across the prerequisite: registration=%+v error=%v", retained, err)
	}
}
