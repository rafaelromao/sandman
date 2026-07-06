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
	"github.com/rafaelromao/sandman/internal/testenv"
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
			want: want{reason: "review", status: "success", kind: "completed", review: true, prNum: 42, label: "Review of PR 42"},
		},
		{
			name: "review failure",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC), RunID: "PR42-fail", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
				{Type: "run.finished", Timestamp: time.Date(2025, time.January, 1, 12, 5, 0, 0, time.UTC), RunID: "PR42-fail", Payload: map[string]any{"review": true, "pr_number": 42, "status": "failure", "branch": "sandman/review-PR42"}},
			},
			want: want{reason: "review", status: "failure", kind: "completed", review: true, prNum: 42, label: "Review of PR 42"},
		},
		{
			name: "review aborted",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC), RunID: "PR42-abort", Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
				{Type: "run.aborted", Timestamp: time.Date(2025, time.January, 1, 12, 5, 0, 0, time.UTC), RunID: "PR42-abort", Payload: map[string]any{"review": true, "pr_number": 42, "status": "aborted", "branch": "sandman/review-PR42"}},
			},
			want: want{reason: "review", status: "aborted", kind: "completed", review: true, prNum: 42, label: "Review of PR 42"},
		},
		{
			name: "regular issue-driven run",
			events: []events.Event{
				{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "abcd-260618113825-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
				{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: "abcd-260618113825-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
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
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: "abcd-260618113825-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "status": "success"}},
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
	if issueRow.RunID != "abcd-260618113825-1" {
		t.Fatalf("expected canonical issue row runID issue-1, got %q", issueRow.RunID)
	}
	if issueRow.Status != "reviewing" {
		t.Fatalf("expected parent status reviewing (badge-flip restored by aggregateReviewChildren for live review child), got %q", issueRow.Status)
	}
	if issueRow.ReviewCount != 2 {
		t.Fatalf("expected parent ReviewCount 2 (both review children aggregate onto the canonical parent), got %d", issueRow.ReviewCount)
	}
	if reviewRows != 2 {
		t.Fatalf("expected 2 review child rows, got %d from %#v", reviewRows, runs)
	}
}

// TestPortal_Compute_TerminalReviewWithApprovedMarker_PreservesParentStatus
// pins the restored #1897 behavior for a terminal review child whose saved
// run.log carries the **APPROVED** marker: aggregateReviewChildren stamps
// ReviewVerdict="Approved" onto the parent row from the saved log (during
// compute, before the summary endpoint strips Log). The parent's own
// terminal run.finished status ("success") is preserved because the sibling
// review is terminal, not live — no badge-flip fires.
func TestPortal_Compute_TerminalReviewWithApprovedMarker_PreservesParentStatus(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-99", Issue: 99, Payload: map[string]any{"branch": "sandman/99-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: "abcd-260618113825-99", Issue: 99, Payload: map[string]any{"branch": "sandman/99-fix", "status": "success"}},
		{Type: "run.started", Timestamp: startedAt.Add(30 * time.Second), RunID: "PR99-review", Issue: 99, Payload: map[string]any{"review": true, "pr_number": 99, "branch": "sandman/review-PR99"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "PR99-review", Issue: 99, Payload: map[string]any{"review": true, "pr_number": 99, "branch": "sandman/review-PR99", "status": "success"}},
	})

	reviewRunDir := filepath.Join(repoRoot, ".sandman", "batches", "PR99-review", "runs", "PR99-review")
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reviewLog := "[PR99-review] 12:01:00 ## Summary\r\n" +
		"[PR99-review] 12:01:00 LGTM.\r\n" +
		"[PR99-review] 12:01:30 ## Decision\r\n" +
		"[PR99-review] 12:01:30 **APPROVED**\r\n"
	if err := os.WriteFile(filepath.Join(reviewRunDir, "run.log"), []byte(reviewLog), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var parent *portalRun
	for i := range runs {
		if runs[i].IssueNumber == 99 && !runs[i].Review {
			parent = &runs[i]
			break
		}
	}
	if parent == nil {
		t.Fatalf("expected parent issue row, got %#v", runs)
	}
	if parent.Status != "success" {
		t.Fatalf("parent Status=%q, want %q (terminal run.finished preserved; no live review child to flip the badge)", parent.Status, "success")
	}
	if parent.ReviewVerdict != "Approved" {
		t.Fatalf("parent ReviewVerdict=%q, want %q (restored server-side stamping from saved review run.log ## Decision marker)", parent.ReviewVerdict, "Approved")
	}
	if parent.ReviewCount != 1 {
		t.Fatalf("parent ReviewCount=%d, want 1", parent.ReviewCount)
	}
}

// TestPortal_Compute_PreservesParentStatusWithTrailingQuoteRunLogShape
// is the post-#1825 regression for issue #1767's saved-log shape. The
// bug was the portal's verdict projection anchoring the marker line
// and missing the **APPROVED** marker when a trailing `"` followed it
// (the `gh pr comment --body "..."` shell artifact). After the
// cross-batch aggregation removal, the parent row's own terminal
// status is preserved regardless of any review verdict projection.
// The reviewVerdictFromRunLog helper is kept as forward-compat and
// tolerates the trailing `"`; its direct unit-test coverage lives in
// TestPortal_ReviewVerdictFromRunLog below.
//
// The reproduction here mirrors the exact log tail captured locally
// from `4f35-260704130316-1755-PR1763/run.log` (work item
// 18dc-260704125005-1755 / issue #1755 "Embed sandman-index as a
// sub-skill of sandman"). The test asserts that with the trailing `"`
// tolerated, the parent row surfaces "Approved"; without the fix, the
// helper falls through and the parent row stays "Unclear".
func TestPortal_Compute_PreservesParentStatusWithTrailingQuoteRunLogShape(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	implRunID := "18dc-260704125005-1755"
	implBatchID := "18dc-260704125005-1755"
	reviewRunID := "4f35-260704130316-1755-PR1763"
	reviewBatchID := "4f35-260704130316-1755-PR1763"

	startedAt := time.Date(2026, 7, 4, 12, 50, 5, 0, time.UTC)
	reviewFinishedAt := startedAt.Add(1*time.Hour + 3*time.Minute + 11*time.Second)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: implRunID, Issue: 1755, Payload: map[string]any{
			"branch":      "sandman/1763-embed-sandman-index-as-sub-skill",
			"batch_id":    implBatchID,
			"base_branch": "main",
		}},
		{Type: "run.finished", Timestamp: startedAt.Add(30 * time.Minute), RunID: implRunID, Issue: 1755, Payload: map[string]any{
			"branch":   "sandman/1763-embed-sandman-index-as-sub-skill",
			"batch_id": implBatchID,
			"status":   "success",
		}},
		{Type: "run.started", Timestamp: startedAt.Add(1*time.Hour + 3*time.Minute + 11*time.Second), RunID: reviewRunID, Payload: map[string]any{
			"branch":       "sandman/review-1763-4878138837",
			"batch_id":     reviewBatchID,
			"review":       true,
			"pr_number":    1763,
			"issue_number": 1755,
			"base_branch":  "main",
		}},
		{Type: "run.finished", Timestamp: reviewFinishedAt, RunID: reviewRunID, Payload: map[string]any{
			"branch":    "sandman/review-1763-4878138837",
			"batch_id":  reviewBatchID,
			"review":    true,
			"pr_number": 1763,
			"status":    "success",
		}},
	})

	// Saved run.log for the review run, mirroring the actual
	// production log captured at 4f35-260704130316-1755-PR1763/run.log.
	// The final line is `**APPROVED**"` — the trailing `"` is the
	// bash closing quote that `gh pr comment --body "..."` leaves
	// behind on the marker line.
	reviewLog := "[4f35-260704130316-1755-PR1763] 13:02:40 ## Summary\r\n" +
		"[4f35-260704130316-1755-PR1763] 13:02:40 LGTM.\r\n" +
		"[4f35-260704130316-1755-PR1763] 13:03:10 ## Findings\r\n" +
		"[4f35-260704130316-1755-PR1763] 13:03:10 None.\r\n" +
		"[4f35-260704130316-1755-PR1763] 13:03:16 ## Decision\r\n" +
		"[4f35-260704130316-1755-PR1763] 13:03:16 **APPROVED**\"\r\n"
	reviewRunDir := filepath.Join(repoRoot, ".sandman", "batches", reviewBatchID, "runs", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir review run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reviewRunDir, "run.log"), []byte(reviewLog), 0644); err != nil {
		t.Fatalf("write review run.log: %v", err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var implRow, reviewRow *portalRun
	for i := range runs {
		run := &runs[i]
		switch {
		case run.IssueNumber == 1755 && !run.Review:
			implRow = run
		case run.Review && run.RunID == reviewRunID:
			reviewRow = run
		}
	}
	if implRow == nil {
		t.Fatalf("expected impl row for issue #1755, got %#v", runs)
	}
	if reviewRow == nil {
		t.Fatalf("expected review row %q, got %#v", reviewRunID, runs)
	}
	if !strings.Contains(reviewRow.Log, "**APPROVED**\"") {
		t.Fatalf("review row Log missing the trailing-quote marker: %q", reviewRow.Log)
	}
	if reviewRow.Status != "success" {
		t.Fatalf("review row Status=%q, want success (the agent exits 0 even when it approved)", reviewRow.Status)
	}
	if implRow.Status != "success" {
		t.Fatalf("impl row Status=%q, want success (terminal run.finished preserved)", implRow.Status)
	}
}

// TestPortal_Compute_PreservesParentStatusWithTrailingQuoteAndPipeRunLogShape
// is the post-#1825 regression for issue #1792's saved-log shape. The
// bug was the portal's verdict projection (as of #1767) anchoring the
// marker line to `"?\s*$` and missing the **APPROVED** marker when a
// trailing `" 2>&1 | tail -5` followed it. After the cross-batch
// aggregation removal, the parent row's own terminal status is
// preserved regardless of any review verdict projection. The
// reviewVerdictFromRunLog helper is kept as forward-compat and
// tolerates the broader debris; its direct unit-test coverage lives
// in TestPortal_ReviewVerdictFromRunLog below.
//
// The reproduction here mirrors the actual log tail captured locally
// from `d9f0-260704185852-1779-PR1789/run.log` (work item
// a9d3-260704183525-1779 "[slice 0] e2e gate hygiene" / issue #1779 /
// PR #1789). The test asserts that with the broader marker rule in
// place, the parent row surfaces "Approved"; without it, the helper
// falls through and the parent row stays "Unclear".
func TestPortal_Compute_PreservesParentStatusWithTrailingQuoteAndPipeRunLogShape(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	implRunID := "a9d3-260704183525-1779"
	implBatchID := "a9d3-260704183525-1779"
	reviewRunID := "d9f0-260704185852-1779-PR1789"
	reviewBatchID := "d9f0-260704185852-1779-PR1789"

	startedAt := time.Date(2026, 7, 4, 18, 35, 25, 0, time.UTC)
	reviewFinishedAt := startedAt.Add(26*time.Minute + 35*time.Second)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: implRunID, Issue: 1779, Payload: map[string]any{
			"branch":      "sandman/1789-slice-0-e2e-gate-hygiene",
			"batch_id":    implBatchID,
			"base_branch": "main",
		}},
		{Type: "run.finished", Timestamp: startedAt.Add(22 * time.Minute), RunID: implRunID, Issue: 1779, Payload: map[string]any{
			"branch":   "sandman/1789-slice-0-e2e-gate-hygiene",
			"batch_id": implBatchID,
			"status":   "success",
		}},
		{Type: "run.started", Timestamp: startedAt.Add(25 * time.Minute), RunID: reviewRunID, Payload: map[string]any{
			"branch":       "sandman/review-1789-4878138837",
			"batch_id":     reviewBatchID,
			"review":       true,
			"pr_number":    1789,
			"issue_number": 1779,
			"base_branch":  "main",
		}},
		{Type: "run.finished", Timestamp: reviewFinishedAt, RunID: reviewRunID, Payload: map[string]any{
			"branch":    "sandman/review-1789-4878138837",
			"batch_id":  reviewBatchID,
			"review":    true,
			"pr_number": 1789,
			"status":    "success",
		}},
	})

	// Saved run.log for the review run, mirroring the actual
	// production log captured at d9f0-260704185852-1779-PR1789/run.log.
	// The final line is `**APPROVED**" 2>&1 | tail -5` — the trailing
	// `"` is the bash closing quote of `gh pr comment --body "..."`
	// and the `2>&1 | tail -5` is the redirect-and-pipe trailer the
	// agent (or the operator) chained onto the same `gh pr comment`
	// invocation. Both pieces of debris must be tolerated by the
	// marker rule for the parent issue row to surface "Approved".
	reviewLog := "[d9f0-260704185852-1779-PR1789] 18:58:30 ## Summary\r\n" +
		"[d9f0-260704185852-1779-PR1789] 18:58:30 Reviewed the e2e gate hygiene slice.\r\n" +
		"[d9f0-260704185852-1779-PR1789] 18:59:50 ## Findings\r\n" +
		"[d9f0-260704185852-1779-PR1789] 18:59:50 None — slice is clean and the helper is in place.\r\n" +
		"[d9f0-260704185852-1779-PR1789] 19:00:55 ## Decision\r\n" +
		"[d9f0-260704185852-1779-PR1789] 19:01:06 **APPROVED**\" 2>&1 | tail -5\r\n"
	reviewRunDir := filepath.Join(repoRoot, ".sandman", "batches", reviewBatchID, "runs", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir review run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reviewRunDir, "run.log"), []byte(reviewLog), 0644); err != nil {
		t.Fatalf("write review run.log: %v", err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var implRow, reviewRow *portalRun
	for i := range runs {
		run := &runs[i]
		switch {
		case run.IssueNumber == 1779 && !run.Review:
			implRow = run
		case run.Review && run.RunID == reviewRunID:
			reviewRow = run
		}
	}
	if implRow == nil {
		t.Fatalf("expected impl row for issue #1779, got %#v", runs)
	}
	if reviewRow == nil {
		t.Fatalf("expected review row %q, got %#v", reviewRunID, runs)
	}
	if !strings.Contains(reviewRow.Log, `**APPROVED**" 2>&1 | tail -5`) {
		t.Fatalf("review row Log missing the trailing-quote+pipe marker: %q", reviewRow.Log)
	}
	if reviewRow.Status != "success" {
		t.Fatalf("review row Status=%q, want success (the agent exits 0 even when it approved)", reviewRow.Status)
	}
	if implRow.Status != "success" {
		t.Fatalf("impl row Status=%q, want success (terminal run.finished preserved)", implRow.Status)
	}
}

// TestPortal_Compute_CanonicalParentIdentityPreservedWithReviewChildren
// is the backend regression test for issue #1525 acceptance criterion #6:
// when compute() projects an issue group that has both a canonical
// implementation row and review child rows, the canonical parent's
// BatchKey, RunID, IssueTitle, StartedAt, and Status must remain
// pinned to the implementation run's own identity. The cross-batch
// review aggregation that previously stamped ReviewCount/ReviewVerdict
// onto the parent row was removed in issue #1825, so the parent's
// own terminal run.finished status is the only value that survives.
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
			RunID:     "abcd-260618113825-1",
			Issue:     1,
			Payload: map[string]any{
				"branch":      parentBranch,
				"issue_title": "Fix login bug",
			},
		},
		{
			Type:      "run.finished",
			Timestamp: startedAt.Add(1 * time.Minute),
			RunID:     "abcd-260618113825-1",
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
	if parentRow.RunID != "abcd-260618113825-1" {
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
	if parentRow.Status != "reviewing" {
		t.Fatalf("expected canonical parent Status reviewing (badge-flip restored by aggregateReviewChildren for live review child), got %q", parentRow.Status)
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
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
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

// TestPortal_ParentSuccWithLiveChild_FlipsToReviewing pins the restored
// badge-flip (#1897, originally #1527/#1609): a terminal parent impl row
// (success) with a live (active) sibling review child must show status
// "reviewing" on the parent row, projected by aggregateReviewChildren.
// #1825 deleted that projection and left the parent stuck on "success";
// the restoration flips it back to "reviewing" while the review is live.
func TestPortal_ParentSuccWithLiveChild_FlipsToReviewing(t *testing.T) {
	repoRoot := testenv.MkdirShort(t, "pswl")
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
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: "abcd-260618113825-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "status": "success"}},
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
		t.Fatalf("Status = %q, want %q (parent badge flips to reviewing via aggregateReviewChildren for live review child)", issueRow.Status, "reviewing")
	}
	if issueRow.ReviewCount != 1 {
		t.Fatalf("expected parent ReviewCount 1, got %d", issueRow.ReviewCount)
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
				RunID:    "abcd-260618113825-42",
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

// TestPortal_KindForRun_BlockedStateReturnsCompleted pins the unit-level
// behavior for issue #1699: a wait-state run whose event-fold
// projection lands on Status() == "blocked" must be classified as
// "completed", not "active". The active-row chrome (purple tint,
// row-added highlight, "Active Batches" filter) is gated on Kind ==
// "active" in portal.html / portal_diff.js / portal_runs_view.go's
// sort predicate; classifying a blocked run as "active" pollutes
// that chrome and surfaces the row in the active filter even
// though its daemon is gone (the issue's symptom).
//
// The blocked status here comes from a run.blocked event being
// projected as the run's terminal fold (Finished != nil, Status()
// == "blocked"), which is exactly what the event log presents for
// a row whose batch has died — see portal_runs_view.go around the
// runFrom* constructors.
func TestPortal_KindForRun_BlockedStateReturnsCompleted(t *testing.T) {
	v := &portalRunsView{}
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(1 * time.Minute)

	runState := events.RunState{
		RunID:    "blocked-run-42",
		Started:  events.Event{Timestamp: startedAt, Payload: map[string]any{"branch": "sandman/42-fix"}},
		Finished: &events.Event{Timestamp: finishedAt, Payload: map[string]any{"blocked_by": []int{99}, "status": "blocked"}},
	}

	if got := v.kindForRun(runState); got != "completed" {
		t.Fatalf("kindForRun(blocked) = %q, want %q", got, "completed")
	}
}

// TestPortal_KindForRun_QueuedStateReturnsCompleted pins the unit-level
// behavior for issue #1699: a wait-state run whose event-fold
// projection lands on Status() == "queued" must be classified as
// "completed", not "active". The queued status comes from a
// run.queued event projected as the run's terminal fold (Finished
// != nil, Status() == "queued") — the same fold the event log
// presents for an orphan queued member whose batch has died.
func TestPortal_KindForRun_QueuedStateReturnsCompleted(t *testing.T) {
	v := &portalRunsView{}
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(1 * time.Minute)

	runState := events.RunState{
		RunID:    "queued-run-42",
		Started:  events.Event{Timestamp: startedAt, Payload: map[string]any{"branch": "sandman/42-fix"}},
		Finished: &events.Event{Timestamp: finishedAt, Payload: map[string]any{"blocked_by": []int{99}, "status": "queued"}},
	}

	if got := v.kindForRun(runState); got != "completed" {
		t.Fatalf("kindForRun(queued) = %q, want %q", got, "completed")
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
		if got.IssueLabel != "Review of PR 42" {
			t.Fatalf("%s: IssueLabel = %q, want %q", label, got.IssueLabel, "Review of PR 42")
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

	const runID = "abcd-260618113825-42"
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

	const runID = "abcd-260618113825-42"
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

	runID := "abcd-260618113825-42"
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

	runID := "abcd-260618113825-42"
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
	liveRunID := "run-live-1"
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
		{batchID: "batch-001", runID: "run-001-1", issueNum: 1},
		{batchID: "batch-002", runID: "run-002-2", issueNum: 2},
		{batchID: "batch-003", runID: "run-003-3", issueNum: 3},
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

// TestPortal_Compute_ReviewVerdictFromDecisionMarkerOnSavedRunLog
// is the slice-1 tracer bullet for issue #1729. The bug: today's
// portal infers the parent row's review verdict from the review run's
// exit status ("success" → "Approved"). But the review agent always
// exits 0 because it posts a single `gh pr comment` and exits cleanly,
// regardless of what it decided. The actual reviewer decision lives
// only in the posted comment body, written to the saved run.log as a
// "## Decision" section with a literal marker.
//
// This test pins the new behaviour: when the saved run.log for a
// review run contains the `**CHANGES_REQUESTED**` marker in its
// `## Decision` section, the parent issue row's ReviewVerdict must be
// the literal string "Changes requested" — not "Approved".
func TestPortal_Compute_ReviewVerdictFromDecisionMarkerOnSavedRunLog(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2026, 7, 3, 13, 50, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	reviewRunID := "556c-260703135044-1719-PR1726"
	reviewBatchID := "556c-260703135044-1719-PR1726"
	implRunID := "c5ed-260703133706-1719"
	implBatchID := "c5ed-260703133706-1719"

	// Implementation run for issue #1719. The review is launched
	// against the PR that this implementation produced, so the review's
	// IssueNumber resolves to 1719 in the event log (Issue field is
	// empty here because the review event payload carries
	// review=true/pr_number=, mirroring events.jsonl:180-181).
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(-13 * time.Minute), RunID: implRunID, Issue: 1719, Payload: map[string]any{
			"branch":      "sandman/1719-ci-add-macos-latest",
			"batch_id":    implBatchID,
			"base_branch": "main",
		}},
		{Type: "run.finished", Timestamp: startedAt, RunID: implRunID, Issue: 1719, Payload: map[string]any{
			"branch":   "sandman/1719-ci-add-macos-latest",
			"batch_id": implBatchID,
			"status":   "success",
		}},
		{Type: "run.started", Timestamp: startedAt, RunID: reviewRunID, Payload: map[string]any{
			"branch":       "sandman/review-1726-4878138837",
			"batch_id":     reviewBatchID,
			"review":       true,
			"pr_number":    1726,
			"issue_number": 1719,
			"base_branch":  "main",
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: reviewRunID, Payload: map[string]any{
			"branch":    "sandman/review-1726-4878138837",
			"batch_id":  reviewBatchID,
			"review":    true,
			"pr_number": 1726,
			"status":    "success",
		}},
	})

	// Saved run.log for the review run, mirroring the actual
	// production log (events.jsonl:181 → run.finished status="success",
	// but the agent wrote `**CHANGES_REQUESTED**` inside the
	// `## Decision` section of the posted comment before exiting).
	// The slice-1 fix must read THIS, not the status.
	reviewLog := "[556c-260703135044-1719-PR1726] 13:51:47 ## Findings\r\n" +
		"[556c-260703135044-1719-PR1726] 13:51:47 - macOS job is red on this PR.\r\n" +
		"[556c-260703135044-1719-PR1726] 13:52:10 ## Decision\r\n" +
		"[556c-260703135044-1719-PR1726] 13:52:11 **CHANGES_REQUESTED**\r\n"
	reviewRunDir := filepath.Join(repoRoot, ".sandman", "batches", reviewBatchID, "runs", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir review run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reviewRunDir, "run.log"), []byte(reviewLog), 0644); err != nil {
		t.Fatalf("write review run.log: %v", err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var implRow, reviewRow *portalRun
	for i := range runs {
		run := &runs[i]
		switch {
		case run.IssueNumber == 1719 && !run.Review:
			implRow = run
		case run.Review && run.RunID == reviewRunID:
			reviewRow = run
		}
	}
	if implRow == nil {
		t.Fatalf("expected impl row for issue #1719, got %#v", runs)
	}
	if reviewRow == nil {
		t.Fatalf("expected review row %q, got %#v", reviewRunID, runs)
	}
	if !strings.Contains(reviewRow.Log, "CHANGES_REQUESTED") {
		t.Fatalf("review row Log missing CHANGES_REQUESTED marker: %q", reviewRow.Log)
	}
	if reviewRow.Status != "success" {
		t.Fatalf("review row Status=%q, want success (the agent exits 0 even when it requested changes)", reviewRow.Status)
	}
	if implRow.Status != "success" {
		t.Fatalf("impl row Status=%q, want success (terminal run.finished preserved after aggregateReviewChildren removal)", implRow.Status)
	}
}

// TestPortal_ReviewVerdictFromRunLog pins the boundary of the
// reviewVerdictFromRunLog helper directly (slice-1 extraction test).
// It exercises the markers the prompt today produces and the
// out-of-scope spelling variants that must NOT be coerced. The
// integration tests above already assert the projection flows through
// compute(); this test pins the helper's behaviour so that future
// prompt-edits that introduce new marker spellings fail loudly
// instead of silently disagreeing with production.
func TestPortal_ReviewVerdictFromRunLog(t *testing.T) {
	cases := []struct {
		name    string
		logText string
		want    string
		wantOK  bool
	}{
		{
			name:    "CHANGES_REQUESTED inside ## Decision is recognised",
			logText: "13:52:10 ## Decision\n13:52:11 **CHANGES_REQUESTED**\n",
			want:    "Changes requested",
			wantOK:  true,
		},
		{
			name:    "APPROVED inside ## Decision is recognised",
			logText: "13:52:10 ## Decision\n13:52:11 **APPROVED**\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			name:    "marker outside Decision section is ignored",
			logText: "13:50:00 ## Summary\n13:50:01 **CHANGES_REQUESTED** is unrelated prose\n13:51:00 ## Decision\n13:51:01 **APPROVED**\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			name:    "no Decision section yields no verdict",
			logText: "13:50:00 ## Summary\n13:50:01 LGTM\n",
			want:    "",
			wantOK:  false,
		},
		{
			name:    "Decision section with no marker yields no verdict",
			logText: "13:50:00 ## Decision\n13:50:01 the author will iterate\n",
			want:    "",
			wantOK:  false,
		},
		{
			name:    "spelling variant with space (CHANGES REQUESTED) is not coerced",
			logText: "13:50:00 ## Decision\n13:50:01 **CHANGES REQUESTED**\n",
			want:    "",
			wantOK:  false,
		},
		{
			// Issue #1792: the lenient marker rule intentionally
			// accepts a trailing period, so this spelling variant
			// is no longer rejected. The remaining safety net
			// against "mid-line prose" is the alphanum guard
			// (see "marker inside Decision followed by prose" below),
			// not the trailing-period rule. The case stays in the
			// table to pin the flip explicitly.
			name:    "spelling variant with trailing period is recognised (lenient regex flip)",
			logText: "13:50:00 ## Decision\n13:50:01 **CHANGES_REQUESTED**.\n",
			want:    "Changes requested",
			wantOK:  true,
		},
		{
			name:    "lowercase spelling is not coerced",
			logText: "13:50:00 ## Decision\n13:50:01 **changes_requested**\n",
			want:    "",
			wantOK:  false,
		},
		{
			name: "## Decisions (plural heading) does not match the bare Decision section",
			logText: "13:50:00 ## Decisions\n" +
				"13:50:01 **APPROVED**\n" +
				"13:51:00 ## Decision\n" +
				"13:51:01 **CHANGES_REQUESTED**\n",
			want:   "Changes requested",
			wantOK: true,
		},
		{
			name: "## Decision Tree heading does not match",
			logText: "13:50:00 ## Decision Tree\n" +
				"13:50:01 **APPROVED**\n" +
				"13:51:00 ## Decision\n" +
				"13:51:01 **APPROVED**\n",
			want:   "Approved",
			wantOK: true,
		},
		{
			name:    "## Decision Summary heading does not match",
			logText: "13:50:00 ## Decision Summary\n13:50:01 **APPROVED**\n13:51:00 ## Decision\n13:51:01 **CHANGES_REQUESTED**\n",
			want:    "Changes requested",
			wantOK:  true,
		},
		{
			name:    "## Decisionmid-sentence does not match",
			logText: "13:50:00 some prose mentioning ## Decision in a sentence\n13:50:01 **APPROVED**\n",
			want:    "",
			wantOK:  false,
		},
		{
			name:    "case-insensitive Decision heading match",
			logText: "13:50:00 ## decision\n13:50:01 **APPROVED**\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			name:    "empty log yields no verdict",
			logText: "",
			want:    "",
			wantOK:  false,
		},
		{
			name:    "log with timestamps and runID prefix is parsed",
			logText: "[abcd-260618113825-PR42] 13:52:10 ## Decision\n[abcd-260618113825-PR42] 13:52:11 **CHANGES_REQUESTED**\n",
			want:    "Changes requested",
			wantOK:  true,
		},
		{
			name:    "APPROVED with trailing double-quote (gh pr comment --body) is recognised",
			logText: "13:52:10 ## Decision\n13:52:11 **APPROVED**\"\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			name:    "CHANGES_REQUESTED with trailing double-quote (gh pr comment --body) is recognised",
			logText: "13:52:10 ## Decision\n13:52:11 **CHANGES_REQUESTED**\"\n",
			want:    "Changes requested",
			wantOK:  true,
		},
		{
			name:    "runID-prefixed APPROVED with trailing double-quote is recognised",
			logText: "[4f35-260704130316-1755-PR1763] 13:03:16 ## Decision\n[4f35-260704130316-1755-PR1763] 13:03:16 **APPROVED**\"\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			// Issue #1792: production log line at 19:01:06 in
			// d9f0-260704185852-1779-PR1789/run.log is
			// `**APPROVED**" 2>&1 | tail -5` — the bash shell left
			// the closing quote of `gh pr comment --body "..."`
			// AND the redirect-and-pipe trailer on the same line
			// as the marker. The lenient regex `[^a-zA-Z]*$`
			// tolerates both as trailing non-letter garbage.
			name:    "APPROVED with trailing shell-pipe debris (2>&1 | tail -5) is recognised",
			logText: "13:50:00 ## Decision\n13:50:01 **APPROVED**\" 2>&1 | tail -5\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			// Single-quote trailer — also a frequent shell artifact
			// when the operator hand-types the gh invocation.
			name:    "APPROVED with trailing single quote is recognised",
			logText: "13:50:00 ## Decision\n13:50:01 **APPROVED**'\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			// Full d9f0-…-PR1789 log-line shape: the runID timestamp
			// prefix is on the same line as the marker.
			name:    "runID-prefixed APPROVED with trailing shell-pipe debris is recognised",
			logText: "[d9f0-260704185852-1779-PR1789] 19:01:06 ## Decision\n[d9f0-260704185852-1779-PR1789] 19:01:06 **APPROVED**\" 2>&1 | tail -5\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			// Issue #1792: with the lenient regex, the second `"` is
			// just more trailing non-letter garbage, so the marker
			// is accepted. Pinning this so a future tightening of
			// the regex back to `"?\s*$` would have to update this
			// case explicitly.
			name:    "APPROVED with double trailing quote is recognised (lenient regex flips)",
			logText: "13:50:00 ## Decision\n13:50:01 **APPROVED**\"\"\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			// Issue #1792: trailing period-after-quote — both chars
			// are non-letters, so the lenient regex accepts. Pinning
			// this for the same reason as the previous case.
			name:    "APPROVED with quote+period trailer is recognised (lenient regex flips)",
			logText: "13:50:00 ## Decision\n13:50:01 **APPROVED**\".\n",
			want:    "Approved",
			wantOK:  true,
		},
		{
			// Issue #1792: the alphanum rule is the only remaining
			// safety net for "mid-line prose" once the lenient regex
			// accepts trailing non-letters. A letter immediately
			// after the marker is still rejected, so the helper
			// keeps ignoring a Decision-line marker that is part of
			// a larger prose sentence.
			name:    "marker inside Decision followed by prose is not coerced (alphanum guard)",
			logText: "13:50:00 ## Decision\n13:50:01 **APPROVED** is unrelated prose\n",
			want:    "",
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := reviewVerdictFromRunLog(tc.logText)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}
