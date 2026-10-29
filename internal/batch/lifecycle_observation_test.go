package batch

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
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
			StatusCheckRollup: "failure", CIHasActiveChecks: true, CIActiveFailedWorkflowRunID: "101", MergeStateStatus: "BLOCKED",
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
	untouchedFailure := observer.Observe(t.Context(), request)
	if untouchedFailure.Action != lifecycleResume || untouchedFailure.Evidence["reason"] != "CI_FAILURE" || !untouchedFailure.ciRepairRequired {
		t.Fatalf("an agent exit without a helper result must retain CI remediation ownership: %+v", untouchedFailure)
	}
	request.CIRerunAction = captureCIRerunActionFromOpenCodeToolEvent(t,
		`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`,
		`{"protocol":"ci-rerun/v1","state":"waiting","reason":"workflow-still-running","repository":"owner/repo","pull_request":17,"source_head_sha":"current-sha","tested_sha":"tested-merge-sha","diagnosis":"transient-provider-timeout","run_id":"101","job_id":"987654321","observed_attempt":1}`)
	if request.CIRerunAction == nil {
		t.Fatal("structured helper tool result was not captured")
	}
	blockedRerun := observer.Observe(t.Context(), request)
	if blockedRerun.Action != lifecycleAwait || blockedRerun.Evidence["gate"] != string(lifecycleGatePending) || blockedRerun.Evidence["reason"] != "CI_FAILURE" {
		t.Fatalf("verified helper result should authorize observation of its active workflow prerequisite: %+v", blockedRerun)
	}
	ciWait, ok := blockedRerun.Evidence["ci_wait"].(map[string]any)
	if !ok || ciWait["execution_id"] != "checks-with-pending-sibling" {
		t.Fatalf("prerequisite await lost identified CI execution: %#v", blockedRerun.Evidence)
	}
	prerequisite, ok := blockedRerun.Evidence["ci_rerun_prerequisite"].(map[string]any)
	if !ok || prerequisite["workflow_run_id"] != "101" || prerequisite["head_sha"] != "current-sha" || prerequisite["pull_request"] != 17 {
		t.Fatalf("prerequisite await lost its typed workflow identity: %#v", blockedRerun.Evidence)
	}
	if action, ok := blockedRerun.Evidence["ci_rerun_action"].(map[string]any); !ok || action["job_id"] != "987654321" {
		t.Fatalf("prerequisite await lost the diagnosed helper result: %#v", blockedRerun.Evidence["ci_rerun_action"])
	}
	initialDeadline, ok := ciWait["deadline_unix_seconds"].(int64)
	if !ok {
		t.Fatalf("CI wait deadline has unexpected type %T", ciWait["deadline_unix_seconds"])
	}

	client.prs[gateTestBranch].CIHasActiveChecks = false
	client.prs[gateTestBranch].CIActiveFailedWorkflowRunID = ""
	now = now.Add(2 * time.Minute)
	completedPrerequisite := observer.Observe(t.Context(), request)
	if completedPrerequisite.Action != lifecycleResume || completedPrerequisite.Evidence["gate"] != "ci-failure" {
		t.Fatalf("completed workflow prerequisite must return failed CI to implementation ownership: %+v", completedPrerequisite)
	}
	retained, err := readCIWaitRegistration(filepath.Join(workDir, ".sandman", "state", "17.ci_wait.json"))
	if err != nil || retained.DeadlineUnixSeconds != initialDeadline {
		t.Fatalf("same CI execution deadline changed across the prerequisite: registration=%+v error=%v", retained, err)
	}

	client.prs[gateTestBranch].CIHasActiveChecks = true
	client.prs[gateTestBranch].CIActiveFailedWorkflowRunID = "101"
	client.prs[gateTestBranch].CIExecutionID = "checks-rerun-attempt-2"
	request.CIRerunAction = captureCIRerunActionFromOpenCodeToolEvent(t,
		`sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`,
		`{"protocol":"ci-rerun/v1","state":"confirmed","reason":"selected-job-and-quality-gate-rerun-observed","repository":"owner/repo","pull_request":17,"source_head_sha":"current-sha","tested_sha":"tested-merge-sha","diagnosis":"transient-provider-timeout","run_id":"101","job_id":"987654321","observed_attempt":2}`)
	if request.CIRerunAction == nil {
		t.Fatal("confirmed helper rerun result was not captured")
	}
	confirmedRerun := observer.Observe(t.Context(), request)
	if confirmedRerun.Action != lifecycleAwait || confirmedRerun.Evidence["gate"] != string(lifecycleGatePending) {
		t.Fatalf("confirmed new attempt should authorize waiting while the failure-first rollup remains visible: %+v", confirmedRerun)
	}
	newCI, ok := confirmedRerun.Evidence["ci_wait"].(map[string]any)
	if !ok || newCI["execution_id"] != "checks-rerun-attempt-2" || newCI["deadline_unix_seconds"] != now.Add(ciWaitTimeout).Unix() {
		t.Fatalf("confirmed rerun did not start a deadline for its verified new execution: %#v", confirmedRerun.Evidence)
	}
}

func TestLifecycleObservation_FailedCIWithUnrelatedActiveWorkflowResumesImplementation(t *testing.T) {
	workDir := t.TempDir()
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-with-unrelated-active-workflow",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, MergeStateStatus: "BLOCKED",
		}},
	}
	session := &runSession{
		issueNumber: 42,
		deps:        runDeps{githubClient: client, errorLog: io.Discard},
		opts: runSessionOptions{
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}
	verdict := newLifecycleObserver(session).Observe(t.Context(), lifecycleObservationRequest{
		WorkDir: workDir, Branch: gateTestBranch, RunID: "run-unrelated-active", HostPathsReady: true,
		AttemptStatus: "success", AfterAgentAttempt: true,
	})
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "CI_FAILURE" {
		t.Fatalf("unrelated active workflow deferred implementation-owned CI remediation: %+v", verdict)
	}
}

func TestAwaitingRowLifecycle_ReobservesOnlyTheRetainedWorkflowPrerequisite(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(2_000, 0).UTC()
	ciWait := map[string]any{
		"protocol": ciWaitProtocol, "pull_request": 17, "head_sha": "current-sha", "execution_id": "checks-101",
		"started_unix_seconds": now.Unix(), "deadline_unix_seconds": now.Add(ciWaitTimeout).Unix(),
		"effective_timeout_seconds": int64(ciWaitTimeout / time.Second),
	}
	prerequisite := map[string]any{
		"protocol": "ci-rerun-prerequisite/v1", "repository": "owner/repo", "pull_request": 17, "head_sha": "current-sha", "workflow_run_id": "101",
	}
	action := map[string]any{
		"protocol": "ci-rerun/v1", "state": "waiting", "reason": "workflow-still-running", "repository": "owner/repo",
		"pull_request": 17, "source_head_sha": "current-sha", "tested_sha": "tested-merge-sha",
		"diagnosis": "transient-provider-timeout", "run_id": "101", "job_id": "987654321", "observed_attempt": 1,
	}
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: "run-rerun-prerequisite", Issue: 42, Timestamp: now.Add(-time.Minute), Payload: map[string]any{"branch": gateTestBranch}},
		{Type: "run.await", RunID: "run-rerun-prerequisite", Issue: 42, Timestamp: now, Payload: map[string]any{
			"await": true, "await_reason": "pending", "branch": gateTestBranch,
			"ci_wait": ciWait, "ci_rerun_prerequisite": prerequisite, "ci_rerun_action": action,
		}},
	}}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-101",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, CIActiveFailedWorkflowRunID: "101", MergeStateStatus: "BLOCKED",
		}},
	}
	e := &runExecutor{deps: runDeps{
		githubClient: client, eventLog: log, layout: paths.NewLayout(nil, root), errorLog: io.Discard,
		runSessionOpts: runSessionOptions{
			now:         func() time.Time { return now },
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}}
	row := RowSpec{IssueNumber: 42, RunID: "run-rerun-prerequisite", Branches: map[int]string{42: gateTestBranch}}
	continued := e.observeAwaitingRowLifecycle(t.Context(), row)
	if continued.Action != lifecycleAwait || continued.Evidence["gate"] != string(lifecycleGatePending) || continued.ciRepairRequired {
		t.Fatalf("same identified active workflow prerequisite should remain awaited: %+v", continued)
	}

	client.prs[gateTestBranch].CIActiveFailedWorkflowRunID = ""
	withoutPrerequisite := e.observeAwaitingRowLifecycle(t.Context(), row)
	if withoutPrerequisite.Action != lifecycleResume || withoutPrerequisite.Evidence["reason"] != "CI_FAILURE" || !withoutPrerequisite.ciRepairRequired {
		t.Fatalf("completed or changed workflow prerequisite must return CI repair to the implementation agent: %+v", withoutPrerequisite)
	}
}

func TestCIRerunActionEvidenceIsCurrentAttemptAndMatchesWorkflowIdentity(t *testing.T) {
	valid := ciRerunActionEvidence{
		Protocol: "ci-rerun/v1", State: "waiting", Reason: "workflow-still-running",
		Repository: "owner/repo", PullRequest: 17, SourceHeadSHA: "current-sha", TestedSHA: "tested-merge-sha",
		Diagnosis: "transient-provider-timeout", RunID: "101", JobID: "987654321", ObservedAttempt: 4,
	}
	line, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	prerequisite := map[string]any{
		"protocol": "ci-rerun-prerequisite/v1", "repository": "owner/repo", "pull_request": 17,
		"head_sha": "current-sha", "workflow_run_id": "101",
	}
	command := `sh "$HOME/.agents/skills/sandman/implement/ci-rerun-v1.sh" --repository owner/repo --pull-request 17 --head-sha current-sha --tested-sha tested-merge-sha --run-id 101 --job-id 987654321 --diagnosis transient-provider-timeout`
	current := ciRerunHelperToolEvidence(command, string(line))
	if current == nil || !current.matchesPrerequisite(prerequisite) {
		t.Fatalf("structured current helper result did not match its prerequisite: %+v", current)
	}
	confirmed := valid
	confirmed.State = "confirmed"
	confirmed.Reason = "selected-job-and-quality-gate-rerun-observed"
	confirmed.ObservedAttempt = 5
	if !confirmed.matchesPrerequisite(prerequisite) {
		t.Fatalf("confirmed new execution did not match its prerequisite: %+v", confirmed)
	}
	for name, mutate := range map[string]func(*ciRerunActionEvidence){
		"repository":   func(e *ciRerunActionEvidence) { e.Repository = "other/repo" },
		"pull request": func(e *ciRerunActionEvidence) { e.PullRequest++ },
		"source head":  func(e *ciRerunActionEvidence) { e.SourceHeadSHA = "old-head" },
		"workflow run": func(e *ciRerunActionEvidence) { e.RunID = "102" },
		"zero run id":  func(e *ciRerunActionEvidence) { e.RunID = "0" },
		"job id":       func(e *ciRerunActionEvidence) { e.JobID = "not-a-database-id" },
		"diagnosis":    func(e *ciRerunActionEvidence) { e.Diagnosis = "deterministic-source-failure" },
		"confirmed without a new attempt": func(e *ciRerunActionEvidence) {
			e.State, e.Reason, e.ObservedAttempt = "confirmed", "selected-job-and-quality-gate-rerun-observed", 1
		},
		"reason": func(e *ciRerunActionEvidence) { e.Reason = "rerun-submission-not-confirmed" },
	} {
		t.Run(name, func(t *testing.T) {
			evidence := valid
			mutate(&evidence)
			data, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if action := ciRerunHelperToolEvidence(command, string(data)); action != nil && action.matchesPrerequisite(prerequisite) {
				t.Fatalf("mismatched helper evidence authorized a wait: %+v", evidence)
			}
		})
	}
	for name, suffix := range map[string]string{
		"ordinary echo": `; printf '%s' ` + string(line),
		"pipeline":      ` | cat`,
		"substitution":  `$(printf x)`,
	} {
		t.Run(name, func(t *testing.T) {
			if action := ciRerunHelperToolEvidence(command+suffix, string(line)); action != nil {
				t.Fatalf("composed shell command authorized helper evidence: %+v", action)
			}
		})
	}
}

func TestAwaitingRowLifecycle_FailedCIWithUnrelatedActiveCheckResumesImplementation(t *testing.T) {
	root := t.TempDir()
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{42: {Number: 42, State: "open"}},
		prs: map[string]*github.PR{gateTestBranch: {
			Number: 17, State: "open", HeadRefOid: "current-sha", CIExecutionID: "checks-with-unrelated-active-workflow",
			StatusCheckRollup: "failure", CIHasActiveChecks: true, MergeStateStatus: "BLOCKED",
		}},
	}
	e := &runExecutor{deps: runDeps{
		githubClient: client,
		layout:       paths.NewLayout(nil, root),
		errorLog:     io.Discard,
		runSessionOpts: runSessionOptions{
			currentHead: func(string) (string, error) { return "current-sha", nil },
		},
	}}
	row := RowSpec{IssueNumber: 42, RunID: "run-pending-ci", Branches: map[int]string{42: gateTestBranch}}
	verdict := e.observeAwaitingRowLifecycle(t.Context(), row)
	if verdict.Action != lifecycleResume || verdict.Evidence["reason"] != "CI_FAILURE" || !verdict.ciRepairRequired {
		t.Fatalf("a failed check discovered during a pending-CI wait must return to implementation, not await unrelated active CI: %+v", verdict)
	}
}
