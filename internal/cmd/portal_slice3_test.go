package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
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
			want: want{reason: "review", status: "success", kind: "completed", review: true, prNum: 42, label: "Review of #42"},
		},
		{
			name: "review failure",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC), RunID: "PR42-fail", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
				{Type: "run.finished", Timestamp: time.Date(2025, time.January, 1, 12, 5, 0, 0, time.UTC), RunID: "PR42-fail", Payload: map[string]any{"review": true, "pr_number": 42, "status": "failure", "branch": "sandman/review-PR42"}},
			},
			want: want{reason: "review", status: "failure", kind: "completed", review: true, prNum: 42, label: "Review of #42"},
		},
		{
			name: "review aborted",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC), RunID: "PR42-abort", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
				{Type: "run.aborted", Timestamp: time.Date(2025, time.January, 1, 12, 5, 0, 0, time.UTC), RunID: "PR42-abort", Payload: map[string]any{"review": true, "pr_number": 42, "status": "aborted", "branch": "sandman/review-PR42"}},
			},
			want: want{reason: "review", status: "aborted", kind: "completed", review: true, prNum: 42, label: "Review of #42"},
		},
		{
			name: "regular issue-driven run",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "abcd-260618113825-issue-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
				{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: "abcd-260618113825-issue-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
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
		name         string
		status       string
		active       bool
		isReview     bool
		isAutoSelect bool
		want         string
	}{
		{"auto-select success stays success", "success", false, false, false, "success"},
		{"auto-select failure stays failure", "failure", false, false, false, "failure"},
		{"review success stays success", "success", false, true, false, "success"},
		{"review failure stays failure", "failure", false, true, false, "failure"},
		{"aborted stays aborted", "aborted", false, false, false, "aborted"},

		// The fallback path still has to work for the genuine "no
		// status key on the finished event" case.
		{"empty status falls back to completed", "", false, false, false, "completed"},

		// Active-run override takes precedence over the status string.
		{"active review stays reviewing", "", true, true, false, "reviewing"},
		{"active auto-select stays auto-selecting", "", true, false, true, "auto-selecting"},
		{"active non-review stays running", "ignored", true, false, false, "running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := v.statusOrDefault(tc.status, tc.active, tc.isReview, tc.isAutoSelect)
			if got != tc.want {
				t.Fatalf("statusOrDefault(%q, %v, %v, %v) = %q, want %q", tc.status, tc.active, tc.isReview, tc.isAutoSelect, got, tc.want)
			}
		})
	}
}

func TestPortal_Compute_AggregatesChildReviewsOntoIssueRow(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	terminalReviewAt := startedAt.Add(2 * time.Minute)
	reviewRunDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42-live")
	createUnixRunSocket(t, filepath.Join(reviewRunDir, "batch.sock"))
	if err := daemon.WriteManifest(reviewRunDir, daemon.BatchManifest{Issues: []int{1}, CreatedAt: startedAt, BatchId: "PR42-live"}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "PR42-live", reviewRunDir, []int{1})
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "issue-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: "issue-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "status": "success"}},
		{Type: "run.started", Timestamp: startedAt.Add(30 * time.Second), RunID: "PR42-live", Issue: 1, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
		{Type: "run.started", Timestamp: startedAt.Add(90 * time.Second), RunID: "PR43-done", Issue: 1, Payload: map[string]any{"review": true, "pr_number": 43, "branch": "sandman/review-PR43"}},
		{Type: "run.finished", Timestamp: terminalReviewAt, RunID: "PR43-done", Issue: 1, Payload: map[string]any{"review": true, "pr_number": 43, "branch": "sandman/review-PR43", "status": "success"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var issueRow *portalRun
	var reviewRows int
	var groupedReviewRows int
	for i := range runs {
		run := &runs[i]
		if run.IssueNumber != 1 {
			continue
		}
		if run.Review {
			reviewRows++
			if run.GroupedReview {
				groupedReviewRows++
			}
			continue
		}
		issueRow = run
	}
	if issueRow == nil {
		t.Fatalf("expected issue row for #1, got %#v", runs)
	}
	if issueRow.RunID != "issue-1" {
		t.Fatalf("expected canonical issue row runID issue-1, got %q", issueRow.RunID)
	}
	if issueRow.Status != "reviewing" {
		t.Fatalf("expected parent status reviewing while child review is live (aggregateReviewChildren flips all parents), got %q", issueRow.Status)
	}
	if issueRow.ReviewCount != 2 {
		t.Fatalf("expected review count 2, got %d", issueRow.ReviewCount)
	}
	if issueRow.ReviewVerdict != "Approved" {
		t.Fatalf("expected latest terminal review verdict Approved, got %q", issueRow.ReviewVerdict)
	}
	if reviewRows != 2 {
		t.Fatalf("expected 2 review child rows, got %d from %#v", reviewRows, runs)
	}
	if groupedReviewRows != 2 {
		t.Fatalf("expected 2 grouped review child rows, got %d from %#v", groupedReviewRows, runs)
	}
}

// TestPortal_Compute_CanonicalParentIdentityPreservedWithReviewChildren
// is the backend regression test for issue #1525 acceptance criterion #6:
// when compute() projects an issue group that has both a canonical
// implementation row and review child rows, the canonical parent's
// BatchKey, RunID, IssueTitle, and StartedAt must remain pinned to the
// implementation run's own identity. Review aggregation (ReviewCount,
// ReviewVerdict) may enrich the parent row, but it must never overwrite
// the parent's own identity fields.
func TestPortal_Compute_CanonicalParentIdentityPreservedWithReviewChildren(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	parentBranch := "sandman/1-canonical"
	reviewBranch := "sandman/review-PR42"
	terminalReviewAt := startedAt.Add(2 * time.Minute)

	// Active review batch for issue #1. Only one batch is registered so
	// the review child is materialized once, matching the production
	// shape where the active batch owns the live review runs.
	reviewRunDir := filepath.Join(repoRoot, ".sandman", "batches", "batch-review")
	createUnixRunSocket(t, filepath.Join(reviewRunDir, "batch.sock"))
	if err := daemon.WriteManifest(reviewRunDir, daemon.BatchManifest{Issues: []int{1}, CreatedAt: startedAt, BatchId: "batch-review"}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "batch-review", reviewRunDir, []int{1})

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{
			Type:      "run.started",
			Timestamp: startedAt,
			RunID:     "issue-1",
			Issue:     1,
			Payload: map[string]any{
				"branch":      parentBranch,
				"issue_title": "Fix login bug",
			},
		},
		{
			Type:      "run.finished",
			Timestamp: startedAt.Add(1 * time.Minute),
			RunID:     "issue-1",
			Issue:     1,
			Payload: map[string]any{
				"branch": parentBranch,
				"status": "success",
			},
		},
		{
			Type:      "run.started",
			Timestamp: startedAt.Add(30 * time.Second),
			RunID:     "PR42-live",
			Issue:     1,
			Payload: map[string]any{
				"review":      true,
				"pr_number":   42,
				"branch":      reviewBranch,
				"issue_title": "Review PR42 — Fix login bug",
			},
		},
		{
			Type:      "run.finished",
			Timestamp: terminalReviewAt,
			RunID:     "PR43-done",
			Issue:     1,
			Payload: map[string]any{
				"review":    true,
				"pr_number": 43,
				"branch":    "sandman/review-PR43",
				"status":    "success",
			},
		},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var parentRow *portalRun
	var reviewChildCount int
	for i := range runs {
		run := &runs[i]
		if run.IssueNumber != 1 {
			continue
		}
		if run.Review {
			reviewChildCount++
			continue
		}
		parentRow = run
	}
	if parentRow == nil {
		t.Fatalf("expected canonical issue row for #1, got %#v", runs)
	}
	if reviewChildCount != 2 {
		t.Fatalf("expected 2 review child rows, got %d from %#v", reviewChildCount, runs)
	}
	if parentRow.RunID != "issue-1" {
		t.Fatalf("expected canonical parent RunID issue-1, got %q", parentRow.RunID)
	}
	if parentRow.IssueTitle != "Fix login bug" {
		t.Fatalf("expected canonical parent IssueTitle Fix login bug, got %q", parentRow.IssueTitle)
	}
	if !parentRow.StartedAt.Equal(startedAt) {
		t.Fatalf("expected canonical parent StartedAt %v, got %v", startedAt, parentRow.StartedAt)
	}
	// Issue #1525 AC2: the canonical parent's BatchKey must stay pinned to
	// the implementation run's own identity, never overwritten by review
	// aggregation. For an event-log-reconstructed parent the BatchKey
	// falls back to the empty string because no active batch is matched.
	if parentRow.BatchKey != "" {
		t.Fatalf("expected canonical parent BatchKey empty (event-log-only), got %q", parentRow.BatchKey)
	}
	if parentRow.ReviewCount != 2 {
		t.Fatalf("expected canonical parent ReviewCount 2, got %d", parentRow.ReviewCount)
	}
}

// TestPortal_TerminalReviewChild_ParentNotStuck is the slice-1 regression
// test for the bug where a review child with a terminal run.finished event
// keeps Kind=="active" because the socket is still alive on disk. This caused
// the parent aggregate to incorrectly stay on "reviewing" even though the child
// had a terminal event-log status. The fix: gate the live flag on Status
// instead of Kind so terminal status wins.
func TestPortal_TerminalReviewChild_ParentNotStuck(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	reviewFinishedAt := startedAt.Add(2 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42")
	sockPath := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addBatchToIndex(t, repoRoot, "PR42", runDir, []int{})

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "issue-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.started", Timestamp: startedAt.Add(30 * time.Second), RunID: "PR42", Issue: 1, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
		{Type: "run.finished", Timestamp: reviewFinishedAt, RunID: "PR42", Issue: 1, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42", "status": "success"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var issueRow *portalRun
	var reviewRows int
	for i := range runs {
		run := &runs[i]
		if run.IssueNumber != 1 {
			continue
		}
		if run.Review {
			reviewRows++
			continue
		}
		issueRow = run
	}
	if issueRow == nil {
		t.Fatalf("expected issue row for #1, got %#v", runs)
	}
	if reviewRows != 1 {
		t.Fatalf("expected 1 review child row, got %d", reviewRows)
	}
	if issueRow.Status != "running" {
		t.Fatalf("Status = %q, want %q (parent must return to running when all review children have terminal event-log entries, even with live socket)", issueRow.Status, "running")
	}
}

// TestPortal_TerminalReviewLiveSocket_PreservesStatus is the regression
// test for the bug where a review child with a terminal run.finished event
// but a live socket had its Status overwritten to "reviewing". The fix:
// pass active=false to statusOrDefault when runState.Status() is non-empty.
func TestPortal_TerminalReviewLiveSocket_PreservesStatus(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	reviewFinishedAt := startedAt.Add(2 * time.Minute)

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42")
	sockPath := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addBatchToIndex(t, repoRoot, "PR42", runDir, []int{})

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "PR42", Issue: 1, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
		{Type: "run.finished", Timestamp: reviewFinishedAt, RunID: "PR42", Issue: 1, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42", "status": "success"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d: %#v", len(runs), runs)
	}
	run := &runs[0]
	if run.Status != "success" {
		t.Fatalf("Status = %q, want %q (terminal event-log status must be preserved even with live socket)", run.Status, "success")
	}
}

// TestPortal_ParentSuccWithLiveChild_FlipsToReviewing verifies
// that aggregateReviewChildren flips even a terminal parent's status
// to "reviewing" when a live review child exists. The frontend was
// previously re-deriving this; now the backend is the sole source of truth.
func TestPortal_ParentSuccWithLiveChild_FlipsToReviewing(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	runDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42")
	sockPath := filepath.Join(runDir, "batch.sock")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addBatchToIndex(t, repoRoot, "PR42", runDir, []int{1})

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "issue-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: "issue-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "status": "success"}},
		{Type: "run.started", Timestamp: startedAt.Add(30 * time.Second), RunID: "PR42", Issue: 1, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var issueRow *portalRun
	for i := range runs {
		if runs[i].IssueNumber == 1 && !runs[i].Review {
			issueRow = &runs[i]
			break
		}
	}
	if issueRow == nil {
		t.Fatalf("expected issue row for #1, got %#v", runs)
	}
	if issueRow.Status != "reviewing" {
		t.Fatalf("Status = %q, want %q (parent status must be flipped to reviewing when live review child exists)", issueRow.Status, "reviewing")
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
			runState: events.RunState{RunID: "abcd-260618113825-active", Started: events.Event{Timestamp: startedAt}},
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
				RunID:    "abcd-260618113825-issue-42",
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
		if got.IssueLabel != "Review of #42" {
			t.Fatalf("%s: IssueLabel = %q, want %q", label, got.IssueLabel, "Review of #42")
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
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42")
	sockPath := filepath.Join(runDir, "batch.sock")
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

	const runID = "abcd-260618113825-id-1-archived"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	archiveDir := filepath.Join(repoRoot, ".sandman", "archive", runID)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}
	addArchivedBatchToIndex(t, repoRoot, runID, archiveDir, []int{42})

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
	if !got.Archived {
		t.Fatalf("Archived = false, want true (index entry status=archived, entry.Path=%s)", archiveDir)
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

	const runID = "abcd-260618113825-id-2-not-archived"
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
	if got.SourceExists {
		t.Fatalf("SourceExists = true, want false (no source directory was created)")
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
	if !strings.Contains(string(payload), `"sourceExists":false`) {
		t.Fatalf("JSON payload missing %q: %s", `"sourceExists":false`, payload)
	}
}

// TestPortal_Compute_CompletedRunWithBatchDir_DoesNotCountAsSourceExists covers
// the cleanup after the legacy batch-dir fallback was removed: a completed row
// whose batch directory still exists but whose per-run source directory does
// not should not advertise SourceExists.
func TestPortal_Compute_CompletedRunWithBatchDir_DoesNotCountAsSourceExists(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "abcd-260618113825-issue-42"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "batch-42")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(runDir, "batch.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt, BatchId: runID}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	addBatchToIndex(t, repoRoot, runID, runDir, []int{42})

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
	if got.SourceExists {
		t.Fatalf("SourceExists = true, want false (batch dir fallback removed for %s)", filepath.Base(runDir))
	}
}

// TestPortal_Compute_CompletedRunWithDeadBatchDir_ReportsSourceExists is the
// regression for historical completed rows: when the batch directory is still
// on disk but the daemon is gone, the portal should recover the batch dir name
// from the manifest so SourceExists stays true for the per-run source folder.
func TestPortal_Compute_CompletedRunWithDeadBatchDir_ReportsSourceExists(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "abcd-260618113825-issue-42"
	runDir := filepath.Join(repoRoot, ".sandman", "batches", "batch-42")
	if err := os.MkdirAll(filepath.Join(runDir, "runs", runID), 0755); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	if err := daemon.WriteManifest(runDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt, BatchId: runID}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	addBatchToIndex(t, repoRoot, runID, runDir, []int{42})

	batchName := filepath.Base(runDir)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix", "batch_id": batchName}},
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
	if got.BatchKey != filepath.Base(runDir) {
		t.Fatalf("BatchKey = %q, want %q", got.BatchKey, filepath.Base(runDir))
	}
	if !got.SourceExists {
		t.Fatalf("SourceExists = false, want true (per-run source directory exists under %s)", filepath.Base(runDir))
	}
}

// TestPortal_Compute_CompletedRunWithSourceDirOnly_DoesNotCountAsSourceExists
// covers the cleanup after removing the legacy batch-dir fallback. A completed
// row whose batch directory exists but whose per-run source directory does not
// should not advertise SourceExists.
func TestPortal_Compute_CompletedRunWithSourceDirOnly_DoesNotCountAsSourceExists(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "abcd-260618113825-archive-source-present"
	runDir := filepath.Join(repoRoot, ".sandman", "batches", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
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
	if got.Kind != "completed" {
		t.Fatalf("Kind = %q, want %q", got.Kind, "completed")
	}
	if got.Archived {
		t.Fatalf("Archived = true, want false (run is still under .sandman/runs/%s)", runID)
	}
	if got.SourceExists {
		t.Fatalf("SourceExists = true, want false (legacy batch-dir-only source fallback removed for %s)", runID)
	}

	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"sourceExists":false`) {
		t.Fatalf("JSON payload missing %q: %s", `"sourceExists":false`, payload)
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
	runDir := filepath.Join(repoRoot, ".sandman", "batches", runID)
	sockPath := filepath.Join(runDir, "batch.sock")
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

func TestPortal_Compute_ActiveIndexEntryWithArchiveDir_NotArchived(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "abcd-260618113825-id-active-with-archive-dir"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	runDir := filepath.Join(repoRoot, ".sandman", "batches", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir batch dir: %v", err)
	}
	addBatchToIndex(t, repoRoot, runID, runDir, []int{42})

	archiveDir := filepath.Join(repoRoot, ".sandman", "archive", runID)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir spurious archive: %v", err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.Archived {
		t.Fatalf("Archived = true, want false (index entry status=active even though archive dir %s exists on disk)", archiveDir)
	}
	if got.Kind != "completed" {
		t.Fatalf("Kind = %q, want %q", got.Kind, "completed")
	}
}

func TestPortal_Compute_OrphanedActiveRunFromDeadBatch_Demoted(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runID := "abcd-260618113825-issue-42"
	batchID := "abcd-260618113825-999-1"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	runDir := filepath.Join(batchDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}

	manifest := daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "batch.json"), manifestData, 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	runManifest := map[string]any{"run_id": runID, "issue": 42, "branch": "sandman/42-fix"}
	runManifestData, err := json.Marshal(runManifest)
	if err != nil {
		t.Fatalf("marshal run manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), runManifestData, 0644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{42})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.Kind != "completed" {
		t.Fatalf("Kind = %q, want %q (orphaned active row from dead batch must be demoted)", got.Kind, "completed")
	}
	if got.Status != "aborted" {
		t.Fatalf("Status = %q, want %q", got.Status, "aborted")
	}
	if got.IssueNumber != 42 {
		t.Fatalf("IssueNumber = %d, want 42", got.IssueNumber)
	}
}

func TestPortal_Compute_DeadBatchWithStaleRunSock_StillDemoted(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runID := "abcd-260618113825-issue-42"
	batchID := "abcd-260618113825-999-1"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	runDir := filepath.Join(batchDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}

	manifest := daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "batch.json"), manifestData, 0644); err != nil {
		t.Fatalf("write batch.json: %v", err)
	}

	runManifest := map[string]any{"run_id": runID, "issue": 42, "branch": "sandman/42-fix"}
	runManifestData, err := json.Marshal(runManifest)
	if err != nil {
		t.Fatalf("marshal run manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), runManifestData, 0644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}

	runSockPath := filepath.Join(runDir, "run.sock")
	ln, err := net.Listen("unix", runSockPath)
	if err != nil {
		t.Fatalf("create stale run.sock: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close stale run.sock listener: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(runSockPath) })

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{42})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.Kind != "completed" {
		t.Fatalf("Kind = %q, want %q (stale run.sock must not block demotion)", got.Kind, "completed")
	}
	if got.Status != "aborted" {
		t.Fatalf("Status = %q, want %q", got.Status, "aborted")
	}
	if got.IssueNumber != 42 {
		t.Fatalf("IssueNumber = %d, want 42", got.IssueNumber)
	}
}

func TestPortal_Compute_DeadBatchQueuedRow_StaysQueued(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	deadBatchID := "batch-dead"
	deadBatchDir := filepath.Join(repoRoot, ".sandman", "batches", deadBatchID)
	if err := os.MkdirAll(deadBatchDir, 0755); err != nil {
		t.Fatalf("mkdir batch dir: %v", err)
	}
	if err := daemon.WriteManifest(deadBatchDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt, BatchId: deadBatchID}); err != nil {
		t.Fatalf("write dead manifest: %v", err)
	}

	runs := []portalRun{
		{Key: "queued", RunID: "queued-run-42", Status: "queued", Kind: "active", BatchKey: deadBatchID, StartedAt: startedAt},
		{Key: "running", RunID: "running-run-42", Status: "running", Kind: "active", BatchKey: deadBatchID, StartedAt: startedAt},
	}
	got := (&portalRunsView{}).demoteOrphanedActiveRunsFromDeadBatches(repoRoot, runs)
	if got[0].Kind != "active" || got[0].Status != "queued" {
		t.Fatalf("queued row = %#v, want active/queued", got[0])
	}
	if got[1].Kind != "completed" || got[1].Status != "aborted" {
		t.Fatalf("running row = %#v, want completed/aborted", got[1])
	}
}

func TestPortal_Compute_LiveParentAndDeadReviewChild_DoesNotAggregateReviewing(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	liveParentAt := startedAt.Add(5 * time.Minute)
	deadReviewAt := startedAt

	liveBatchID := "batch-live"
	liveRunID := "run-live-issue-1"
	liveBatchDir := filepath.Join(repoRoot, ".sandman", "batches", liveBatchID)
	liveRunDir := filepath.Join(liveBatchDir, "runs", liveRunID)
	if err := os.MkdirAll(liveRunDir, 0755); err != nil {
		t.Fatalf("mkdir live run dir: %v", err)
	}
	createUnixRunSocket(t, filepath.Join(liveBatchDir, "batch.sock"))
	if err := daemon.WriteManifest(liveBatchDir, daemon.BatchManifest{Issues: []int{1}, CreatedAt: liveParentAt, BatchId: liveBatchID}); err != nil {
		t.Fatalf("write live manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(liveRunDir, "run.json"), []byte(`{"run_id":"`+liveRunID+`","issue":1,"branch":"sandman/1-fix"}`), 0644); err != nil {
		t.Fatalf("write live run.json: %v", err)
	}
	addBatchToIndex(t, repoRoot, liveBatchID, liveBatchDir, []int{1})

	deadBatchID := "batch-dead"
	deadReviewID := "review-1"
	deadBatchDir := filepath.Join(repoRoot, ".sandman", "batches", deadBatchID)
	deadRunDir := filepath.Join(deadBatchDir, "runs", deadReviewID)
	if err := os.MkdirAll(deadRunDir, 0755); err != nil {
		t.Fatalf("mkdir dead run dir: %v", err)
	}
	if err := daemon.WriteManifest(deadBatchDir, daemon.BatchManifest{Issues: []int{1}, CreatedAt: deadReviewAt, BatchId: deadBatchID}); err != nil {
		t.Fatalf("write dead manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deadRunDir, "run.json"), []byte(`{"run_id":"`+deadReviewID+`","issue":1,"branch":"sandman/review-42"}`), 0644); err != nil {
		t.Fatalf("write dead run.json: %v", err)
	}
	addBatchToIndex(t, repoRoot, deadBatchID, deadBatchDir, []int{1})

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: liveParentAt, RunID: liveRunID, Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.started", Timestamp: deadReviewAt, RunID: deadReviewID, Issue: 1, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-42"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	var parent *portalRun
	var review *portalRun
	for i := range runs {
		r := &runs[i]
		if r.IssueNumber != 1 {
			continue
		}
		if r.Review {
			review = r
			continue
		}
		if r.RunID == liveRunID {
			parent = r
		}
	}
	if parent == nil {
		t.Fatalf("expected live parent row, got %#v", runs)
	}
	if parent.Status != "running" {
		t.Fatalf("parent Status = %q, want %q", parent.Status, "running")
	}
	if review == nil {
		t.Fatalf("expected review child row, got %#v", runs)
	}
	if review.Kind != "completed" || review.Status != "aborted" {
		t.Fatalf("review child = %#v, want completed/aborted after demotion", review)
	}
	for _, run := range runs {
		if run.IssueNumber == 1 && run.Status == "reviewing" {
			t.Fatalf("unexpected reviewing row after demotion: %#v", run)
		}
	}
}

func TestPortal_Compute_MultipleDeadBatches_IndependentIssueSets(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	type batch struct {
		batchID  string
		runID    string
		issueNum int
	}
	batches := []batch{
		{batchID: "batch-001", runID: "run-001-issue-1", issueNum: 1},
		{batchID: "batch-002", runID: "run-002-issue-2", issueNum: 2},
		{batchID: "batch-003", runID: "run-003-issue-3", issueNum: 3},
	}

	var evts []events.Event
	for _, b := range batches {
		evts = append(evts, events.Event{
			Type: "run.started", Timestamp: startedAt, RunID: b.runID, Issue: b.issueNum,
			Payload: map[string]any{"branch": fmt.Sprintf("sandman/%d-fix", b.issueNum)},
		})
	}
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), evts)

	for _, b := range batches {
		batchDir := filepath.Join(repoRoot, ".sandman", "batches", b.batchID)
		runDir := filepath.Join(batchDir, "runs", b.runID)
		if err := os.MkdirAll(runDir, 0755); err != nil {
			t.Fatalf("mkdir run dir for %s: %v", b.batchID, err)
		}
		manifest := daemon.BatchManifest{Issues: []int{b.issueNum}, CreatedAt: startedAt}
		manifestData, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("marshal manifest for %s: %v", b.batchID, err)
		}
		if err := os.WriteFile(filepath.Join(batchDir, "batch.json"), manifestData, 0644); err != nil {
			t.Fatalf("write batch.json for %s: %v", b.batchID, err)
		}
		runManifest := map[string]any{"run_id": b.runID, "issue": b.issueNum}
		runManifestData, err := json.Marshal(runManifest)
		if err != nil {
			t.Fatalf("marshal run manifest for %s: %v", b.batchID, err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "run.json"), runManifestData, 0644); err != nil {
			t.Fatalf("write run.json for %s: %v", b.batchID, err)
		}
		addBatchToIndex(t, repoRoot, b.batchID, batchDir, []int{b.issueNum})
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 rows, got %d: %#v", len(runs), runs)
	}
	for _, b := range batches {
		found := false
		for i := range runs {
			if runs[i].IssueNumber == b.issueNum {
				found = true
				if runs[i].Kind != "completed" {
					t.Fatalf("issue %d: Kind = %q, want %q (orphaned row from dead batch must be demoted)", b.issueNum, runs[i].Kind, "completed")
				}
				if runs[i].Status != "aborted" {
					t.Fatalf("issue %d: Status = %q, want %q", b.issueNum, runs[i].Status, "aborted")
				}
			}
		}
		if !found {
			t.Fatalf("expected row for issue %d, not found", b.issueNum)
		}
	}
}
