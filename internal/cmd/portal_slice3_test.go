package cmd

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortal_Compute_AutoSelectSuccessRow_PopulatesReasonAndKeepsTerminalStatus
// is the slice-3 tracer bullet. It drives a real JSONL log through
// compute() and asserts the resulting row carries Reason, Kind, and
// Status that match the terminal event. This is the smallest vertical
// slice that exercises discovery + projection + JSON encoding together.
func TestPortal_Compute_AutoSelectSuccessRow_PopulatesReasonAndKeepsTerminalStatus(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	autoSelectRunID := "auto-select-1700000000000"

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: autoSelectRunID, Payload: map[string]any{
			"run_kind":   "auto-select",
			"count":      5,
			"query":      "label:ready-for-agent is:open",
			"candidates": []int{1, 2, 3},
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: autoSelectRunID, Payload: map[string]any{
			"run_kind": "auto-select",
			"status":   "success",
			"selected": []int{1, 2},
		}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.RunID != autoSelectRunID {
		t.Fatalf("expected RunID %q, got %q", autoSelectRunID, got.RunID)
	}
	if got.Reason != "auto-select" {
		t.Fatalf("expected Reason %q, got %q", "auto-select", got.Reason)
	}
	if got.Status != "success" {
		t.Fatalf("expected Status %q, got %q", "success", got.Status)
	}
	if got.Kind != "completed" {
		t.Fatalf("expected Kind %q, got %q", "completed", got.Kind)
	}
}

// TestPortal_Compute_ReasonTableForAllRunKinds pins the Reason field
// for every run kind through compute(). The empty-string case for a
// regular issue-driven run is the negative side of the contract: those
// rows must not gain a Reason, otherwise the slice-2 chip rendering
// would light up for runs it should ignore.
func TestPortal_Compute_ReasonTableForAllRunKinds(t *testing.T) {
	type want struct {
		reason string
		status string
		kind   string
		review bool
		prNum  int
		label  string
	}
	cases := []struct {
		name   string
		events []events.Event
		want   want
	}{
		{
			name: "auto-select success",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "auto-select-1", Payload: map[string]any{"run_kind": "auto-select", "candidates": []int{1, 2}}},
				{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 1, 0, 0, time.UTC), RunID: "auto-select-1", Payload: map[string]any{"run_kind": "auto-select", "status": "success", "selected": []int{1, 2}}},
			},
			want: want{reason: "auto-select", status: "success", kind: "completed", label: "auto-select-1"},
		},
		{
			name: "auto-select failure",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "auto-select-2", Payload: map[string]any{"run_kind": "auto-select", "candidates": []int{1}}},
				{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 1, 0, 0, time.UTC), RunID: "auto-select-2", Payload: map[string]any{"run_kind": "auto-select", "status": "failure", "reason": "agent exited 1"}},
			},
			want: want{reason: "auto-select", status: "failure", kind: "completed", label: "auto-select-2"},
		},
		{
			name: "review success",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
				{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: "PR42", Payload: map[string]any{"review": true, "pr_number": 42, "status": "success", "branch": "sandman/review-PR42"}},
			},
			want: want{reason: "review", status: "success", kind: "completed", review: true, prNum: 42, label: "PR42"},
		},
		{
			name: "review failure",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "PR42-fail", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
				{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: "PR42-fail", Payload: map[string]any{"review": true, "pr_number": 42, "status": "failure", "branch": "sandman/review-PR42"}},
			},
			want: want{reason: "review", status: "failure", kind: "completed", review: true, prNum: 42, label: "PR42-fail"},
		},
		{
			name: "review aborted",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "PR42-abort", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
				{Type: "run.aborted", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: "PR42-abort", Payload: map[string]any{"review": true, "pr_number": 42, "status": "aborted", "branch": "sandman/review-PR42"}},
			},
			want: want{reason: "review", status: "aborted", kind: "completed", review: true, prNum: 42, label: "PR42-abort"},
		},
		{
			name: "regular issue-driven run",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "run-42-1", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
				{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: "run-42-1", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
			},
			want: want{reason: "", status: "success", kind: "completed", label: "#42"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
				t.Fatal(err)
			}
			writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), tc.events)

			runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			if len(runs) != 1 {
				t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
			}
			got := runs[0]
			if got.Reason != tc.want.reason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.want.reason)
			}
			if got.Status != tc.want.status {
				t.Fatalf("Status = %q, want %q", got.Status, tc.want.status)
			}
			if got.Kind != tc.want.kind {
				t.Fatalf("Kind = %q, want %q", got.Kind, tc.want.kind)
			}
			if got.Review != tc.want.review {
				t.Fatalf("Review = %v, want %v", got.Review, tc.want.review)
			}
			if got.PRNumber != tc.want.prNum {
				t.Fatalf("PRNumber = %d, want %d", got.PRNumber, tc.want.prNum)
			}
			if got.IssueLabel != tc.want.label {
				t.Fatalf("IssueLabel = %q, want %q", got.IssueLabel, tc.want.label)
			}
		})
	}
}

// TestPortal_StatusOrDefault_PreservesTerminalStatusesForAutoSelectAndReview
// pins statusOrDefault: real terminal statuses from run.finished /
// run.aborted must never be silently rewritten to a synthetic default.
func TestPortal_StatusOrDefault_PreservesTerminalStatusesForAutoSelectAndReview(t *testing.T) {
	v := &portalRunsView{}
	cases := []struct {
		name     string
		status   string
		active   bool
		isReview bool
		want     string
	}{
		{"auto-select success stays success", "success", false, false, "success"},
		{"auto-select failure stays failure", "failure", false, false, "failure"},
		{"review success stays success", "success", false, true, "success"},
		{"review failure stays failure", "failure", false, true, "failure"},
		{"aborted stays aborted", "aborted", false, false, "aborted"},

		// The fallback path still has to work for the genuine "no
		// status key on the finished event" case.
		{"empty status falls back to completed", "", false, false, "completed"},

		// Active-run override takes precedence over the status string.
		{"active review stays reviewing", "", true, true, "reviewing"},
		{"active non-review stays running", "ignored", true, false, "running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := v.statusOrDefault(tc.status, tc.active, tc.isReview)
			if got != tc.want {
				t.Fatalf("statusOrDefault(%q, %v, %v) = %q, want %q", tc.status, tc.active, tc.isReview, got, tc.want)
			}
		})
	}
}

// TestPortal_KindForRun_TerminalAutoSelectAndReviewClassifiedAsCompleted
// pins kindForRun. A finished run is "completed" regardless of run kind.
func TestPortal_KindForRun_TerminalAutoSelectAndReviewClassifiedAsCompleted(t *testing.T) {
	v := &portalRunsView{}
	finishedAt := time.Date(2025, 1, 1, 12, 1, 0, 0, time.UTC)
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	makeFinished := func(payload map[string]any) *events.Event {
		e := events.Event{Timestamp: finishedAt, Payload: payload}
		return &e
	}

	cases := []struct {
		name     string
		runState events.RunState
		want     string
	}{
		{
			name:     "active run is active",
			runState: events.RunState{RunID: "run-active", Started: events.Event{Timestamp: startedAt}},
			want:     "active",
		},
		{
			name: "terminal auto-select is completed",
			runState: events.RunState{
				RunID:    "auto-select-1",
				Started:  events.Event{Timestamp: startedAt, Payload: map[string]any{"run_kind": "auto-select"}},
				Finished: makeFinished(map[string]any{"run_kind": "auto-select", "status": "success"}),
			},
			want: "completed",
		},
		{
			name: "terminal review is completed",
			runState: events.RunState{
				RunID:    "PR42",
				Started:  events.Event{Timestamp: startedAt, Payload: map[string]any{"review": true}},
				Finished: makeFinished(map[string]any{"review": true, "status": "success"}),
			},
			want: "completed",
		},
		{
			name: "terminal issue is completed",
			runState: events.RunState{
				RunID:    "run-42-1",
				Started:  events.Event{Timestamp: startedAt},
				Finished: makeFinished(map[string]any{"status": "success"}),
			},
			want: "completed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := v.kindForRun(tc.runState)
			if got != tc.want {
				t.Fatalf("kindForRun = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPortal_MarkCompletedIfSocketDead_LeavesCompletedRowsAlone pins the
// dead-socket reaper's invariant: a terminal row whose Kind is already
// "completed" is not touched, even if its socket is dead. A regression
// that always overwrites Kind would reanimate the row to "active".
func TestPortal_MarkCompletedIfSocketDead_LeavesCompletedRowsAlone(t *testing.T) {
	v := &portalRunsView{}

	t.Run("active run with dead socket flips to completed", func(t *testing.T) {
		run := portalRun{Kind: "active"}
		sockDir, err := os.MkdirTemp("", "sm-rmsd")
		if err != nil {
			t.Fatal(err)
		}
		sockPath := filepath.Join(sockDir, "d.sock")
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		_ = ln.Close()

		v.markCompletedIfSocketDead(&run, sockPath)

		if run.Kind != "completed" {
			t.Fatalf("Kind = %q, want %q", run.Kind, "completed")
		}
	})

	t.Run("completed run is not touched by dead socket", func(t *testing.T) {
		run := portalRun{Kind: "completed", Status: "success"}
		v.markCompletedIfSocketDead(&run, "")

		if run.Kind != "completed" {
			t.Fatalf("Kind = %q, want %q (reaper must not touch completed rows)", run.Kind, "completed")
		}
		if run.Status != "success" {
			t.Fatalf("Status = %q, want %q", run.Status, "success")
		}
	})
}

// TestPortal_DedupRunGroup_PreservesZeroPriorityTerminalRows pins the
// dedup helper's invariant that two unrelated terminal runs (e.g., a
// recovered failure + a fresh success) sharing an issue number stay
// visible together. The slice-3 contract is enforced upstream in
// compute() (active instances get matched to terminal states for the
// same RunID), not by changing dedupRunGroup.
func TestPortal_DedupRunGroup_PreservesZeroPriorityTerminalRows(t *testing.T) {
	v := &portalRunsView{}
	base := time.Now().Add(-10 * time.Minute)
	group := []portalRun{
		{Key: "success-row", Kind: "completed", Status: "success", IssueNumber: 42, StartedAt: base},
		{Key: "failure-row", Kind: "completed", Status: "failure", IssueNumber: 42, StartedAt: base.Add(1 * time.Minute)},
	}

	result := v.dedupRunGroup(group)

	if len(result) != 2 {
		t.Fatalf("expected 2 surviving rows (unrelated terminal runs), got %d: %#v", len(result), result)
	}
}

// TestPortal_Polling_AutoSelectSuccessPersistsAcrossRunsDirRemoval is
// the slice-3 polling-style integration test for auto-select. It writes
// real JSONL events to a temp file, polls compute() four times (empty,
// after run.started, after run.finished, after daemon exit) and asserts
// the row's terminal status persists.
func TestPortal_Polling_AutoSelectSuccessPersistsAcrossRunsDirRemoval(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	autoSelectRunID := "auto-select-1700000000000"

	// JSONLLogger keeps the file handle open between calls; using a
	// single instance across the polling sequence mirrors what a real
	// portal does (it constructs the logger once and calls Log as the
	// orchestrator emits events).
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	log := &events.JSONLLogger{Path: logPath}

	// Poll 1: empty event log.
	first, err := (&portalRunsView{}).compute(repoRoot, log)
	if err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if len(first) != 0 {
		t.Fatalf("poll 1: expected 0 rows, got %d", len(first))
	}

	// Selection phase emits run.started.
	startedAt := time.Now().Add(-5 * time.Minute)
	if err := log.Log(events.Event{Type: "run.started", Timestamp: startedAt, RunID: autoSelectRunID, Payload: map[string]any{
		"run_kind":   "auto-select",
		"count":      5,
		"query":      "label:ready-for-agent is:open",
		"candidates": []int{1, 2, 3},
	}}); err != nil {
		t.Fatalf("log started: %v", err)
	}

	// Poll 2: row exists with the auto-select reason.
	second, err := (&portalRunsView{}).compute(repoRoot, log)
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("poll 2: expected 1 row, got %d: %#v", len(second), second)
	}
	if second[0].Reason != "auto-select" {
		t.Fatalf("poll 2: Reason = %q, want %q", second[0].Reason, "auto-select")
	}

	// Selection phase emits run.finished with status=success.
	finishedAt := startedAt.Add(2 * time.Minute)
	if err := log.Log(events.Event{Type: "run.finished", Timestamp: finishedAt, RunID: autoSelectRunID, Payload: map[string]any{
		"run_kind": "auto-select",
		"status":   "success",
		"selected": []int{1, 2},
	}}); err != nil {
		t.Fatalf("log finished: %v", err)
	}

	// Poll 3: terminal success, auto-select reason.
	third, err := (&portalRunsView{}).compute(repoRoot, log)
	if err != nil {
		t.Fatalf("poll 3: %v", err)
	}
	if len(third) != 1 {
		t.Fatalf("poll 3: expected 1 row, got %d: %#v", len(third), third)
	}
	if third[0].Status != "success" {
		t.Fatalf("poll 3: Status = %q, want %q", third[0].Status, "success")
	}
	if third[0].Reason != "auto-select" {
		t.Fatalf("poll 3: Reason = %q, want %q", third[0].Reason, "auto-select")
	}
	if third[0].Kind != "completed" {
		t.Fatalf("poll 3: Kind = %q, want %q", third[0].Kind, "completed")
	}

	// Poll 4: terminal success persists after the daemon exits and
	// the run dir is gone — the row is reconstructed from the event
	// log alone.
	fourth, err := (&portalRunsView{}).compute(repoRoot, log)
	if err != nil {
		t.Fatalf("poll 4: %v", err)
	}
	if len(fourth) != 1 {
		t.Fatalf("poll 4: expected 1 row, got %d: %#v", len(fourth), fourth)
	}
	if fourth[0].Status != "success" {
		t.Fatalf("poll 4: Status = %q, want %q", fourth[0].Status, "success")
	}
	if fourth[0].Reason != "auto-select" {
		t.Fatalf("poll 4: Reason = %q, want %q", fourth[0].Reason, "auto-select")
	}
}

// TestPortal_Polling_AutoSelectFailureIsNotRewritten is the negative
// path of the auto-select persistence contract. A real "failure"
// terminal status must not be silently rewritten to "success" or
// "completed".
func TestPortal_Polling_AutoSelectFailureIsNotRewritten(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	autoSelectRunID := "auto-select-1700000000000-fail"
	startedAt := time.Now().Add(-5 * time.Minute)
	finishedAt := startedAt.Add(2 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: autoSelectRunID, Payload: map[string]any{
			"run_kind": "auto-select", "candidates": []int{1},
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: autoSelectRunID, Payload: map[string]any{
			"run_kind": "auto-select", "status": "failure", "reason": "agent exited 1",
		}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.Status != "failure" {
		t.Fatalf("Status = %q, want %q (must not be rewritten to success or completed)", got.Status, "failure")
	}
	if got.Reason != "auto-select" {
		t.Fatalf("Reason = %q, want %q", got.Reason, "auto-select")
	}
}

// TestPortal_Polling_ReviewSuccessPreservesReviewFlagAndReason is the
// polling-style integration test for a review run that finishes
// successfully. The event-log-only path (no run dir) is the simpler
// half of the contract, and the test polls twice to confirm the
// terminal status persists across repeated reads.
func TestPortal_Polling_ReviewSuccessPreservesReviewFlagAndReason(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().Add(-5 * time.Minute)
	finishedAt := startedAt.Add(2 * time.Minute)

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	log := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}
	if err := log.Log(events.Event{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Payload: map[string]any{
		"review": true, "pr_number": 42, "branch": "sandman/review-PR42",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := log.Log(events.Event{Type: "run.finished", Timestamp: finishedAt, RunID: "PR42", Payload: map[string]any{
		"review": true, "pr_number": 42, "status": "success", "branch": "sandman/review-PR42",
	}}); err != nil {
		t.Fatal(err)
	}

	assertTerminal := func(label string, runs []portalRun) {
		if len(runs) != 1 {
			for i, r := range runs {
				t.Logf("%s row %d: Status=%q Kind=%q Reason=%q RunID=%q", label, i, r.Status, r.Kind, r.Reason, r.RunID)
			}
			t.Fatalf("%s: expected 1 row, got %d", label, len(runs))
		}
		got := runs[0]
		if got.Status != "success" {
			t.Fatalf("%s: Status = %q, want %q", label, got.Status, "success")
		}
		if got.Reason != "review" {
			t.Fatalf("%s: Reason = %q, want %q", label, got.Reason, "review")
		}
		if !got.Review {
			t.Fatalf("%s: Review = false, want true", label)
		}
		if got.PRNumber != 42 {
			t.Fatalf("%s: PRNumber = %d, want 42", label, got.PRNumber)
		}
		if got.IssueLabel != "PR42" {
			t.Fatalf("%s: IssueLabel = %q, want %q", label, got.IssueLabel, "PR42")
		}
	}

	// Poll 1: terminal status on first read.
	first, err := (&portalRunsView{}).compute(repoRoot, log)
	if err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	assertTerminal("poll 1", first)

	// Poll 2: terminal status persists on a fresh compute(). The user
	// contract is "indefinitely", so a single read is not enough to
	// pin it.
	second, err := (&portalRunsView{}).compute(repoRoot, log)
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	assertTerminal("poll 2", second)
}

// TestPortal_Polling_ReviewLiveSocketStillShowsTerminalSuccess is the
// slice-3 regression for the persistence contract under the hardest
// case: a review run with a live socket on disk while the event log
// already carries a run.finished with status=success. The terminal
// status must win; the user must not see "reviewing" indefinitely.
func TestPortal_Polling_ReviewLiveSocketStillShowsTerminalSuccess(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(repoRoot, ".sandman", "runs", "PR42")
	sockPath := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	startedAt := time.Now().Add(-5 * time.Minute)
	finishedAt := startedAt.Add(2 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Payload: map[string]any{
			"review": true, "pr_number": 42, "branch": "sandman/review-PR42",
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "PR42", Payload: map[string]any{
			"review": true, "pr_number": 42, "status": "success", "branch": "sandman/review-PR42",
		}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		for i, r := range runs {
			t.Logf("  row %d: Status=%q Kind=%q Reason=%q RunID=%q BatchKey=%q IssueNumber=%d", i, r.Status, r.Kind, r.Reason, r.RunID, r.BatchKey, r.IssueNumber)
		}
		t.Fatalf("expected 1 row, got %d", len(runs))
	}
	got := runs[0]
	if got.Status != "success" {
		t.Fatalf("Status = %q, want %q (terminal status must win over live socket)", got.Status, "success")
	}
	if got.Kind != "completed" {
		t.Fatalf("Kind = %q, want %q (terminal status must win over live socket)", got.Kind, "completed")
	}
	if got.Reason != "review" {
		t.Fatalf("Reason = %q, want %q", got.Reason, "review")
	}
}

// TestPortal_Compute_CompletedRunUnderArchiveDir_MarksArchived is the
// cycle-1 tracer bullet for the Archived field. A completed run whose
// directory has been moved under .sandman/archive/<run-id> must surface
// Archived=true on the corresponding row, and the JSON payload must
// carry the "archived":true key (acceptance criterion #3).
func TestPortal_Compute_CompletedRunUnderArchiveDir_MarksArchived(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "run-id-1-archived"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	archiveDir := filepath.Join(repoRoot, ".sandman", "archive", runID)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if !got.Archived {
		t.Fatalf("Archived = false, want true (run directory exists under .sandman/archive/%s)", runID)
	}
	if got.Kind != "completed" {
		t.Fatalf("Kind = %q, want %q", got.Kind, "completed")
	}

	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"archived":true`) {
		t.Fatalf("JSON payload missing %q: %s", `"archived":true`, payload)
	}
}

// TestPortal_Compute_CompletedRunWithoutArchiveDir_NotArchived is the
// cycle-2 test. A completed run whose archive directory does not exist
// must surface Archived=false and the JSON payload must carry
// "archived":false.
func TestPortal_Compute_CompletedRunWithoutArchiveDir_NotArchived(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "run-id-2-not-archived"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.Archived {
		t.Fatalf("Archived = true, want false (no archive directory was created)")
	}
	if got.Kind != "completed" {
		t.Fatalf("Kind = %q, want %q", got.Kind, "completed")
	}

	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"archived":false`) {
		t.Fatalf("JSON payload missing %q: %s", `"archived":false`, payload)
	}
}

// TestPortal_Compute_ActiveRunNeverArchived is the cycle-3 test. An
// active run (live control socket on disk) must NEVER carry the
// Archived flag, even when an archive directory with the same RunID
// also exists on disk. The live socket keeps the row at Kind="active"
// so the compute() pass skips the archive lookup entirely.
func TestPortal_Compute_ActiveRunNeverArchived(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Keep the RunID short so the resulting .sandman/runs/<id>/run.sock
	// path stays within the typical Linux sun_path limit (108 bytes)
	// inside the test's t.TempDir() prefix.
	const runID = "active-archive"
	startedAt := time.Now().Add(-5 * time.Minute)

	// Live control socket under .sandman/runs/<runID>/run.sock.
	runDir := filepath.Join(repoRoot, ".sandman", "runs", runID)
	sockPath := filepath.Join(runDir, "run.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// The matching archive directory exists on disk. The detector must
	// still NOT mark the row archived.
	archiveDir := filepath.Join(repoRoot, ".sandman", "archive", runID)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}

	// Event log carries only run.started; the run is still live.
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.Kind != "active" {
		t.Fatalf("Kind = %q, want %q (live socket must keep the row active)", got.Kind, "active")
	}
	if got.Archived {
		t.Fatalf("Archived = true, want false (active runs are never marked archived regardless of archive dir presence)")
	}

	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"archived":false`) {
		t.Fatalf("JSON payload missing %q: %s", `"archived":false`, payload)
	}
}
