package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

// B-tag vocabulary for issue #2109 (review canonical row):
//   B6 — live review carries reviewLive on JSON wire; omitempty when terminal

// TestPortal_Compute_LiveReviewCarriesReviewLiveJSON pins issue #2109
// (B6): when compute() projects a parent implementation row whose sibling
// review child is still in flight, the JSON wire must carry
// "reviewLive": true so the JS renderRunMeta helper can render
// "N review(s) - In Progress". The omitempty contract is also pinned: a
// terminal review scenario (no in-flight child) must NOT carry the field on
// the wire. The parent's own Status field is unchanged by this slice — the
// existing badge-flip / terminal-preservation invariant is untouched.
func TestPortal_Compute_LiveReviewCarriesReviewLiveJSON(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	implStartedAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	implFinishedAt := implStartedAt.Add(20 * time.Minute)
	reviewStartedAt := implStartedAt.Add(21 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: implStartedAt, RunID: "impl-2109", Issue: 2109, Payload: map[string]any{
			"branch":   "sandman/2109-fix",
			"batch_id": "impl-2109",
			"status":   "running",
		}},
		{Type: "run.finished", Timestamp: implFinishedAt, RunID: "impl-2109", Issue: 2109, Payload: map[string]any{
			"branch":   "sandman/2109-fix",
			"batch_id": "impl-2109",
			"status":   "success",
		}},
		{Type: "run.started", Timestamp: reviewStartedAt, RunID: "PR2109-review", Issue: 2109, Payload: map[string]any{
			"branch":       "sandman/review-PR2109",
			"batch_id":     "PR2109-review",
			"review":       true,
			"pr_number":    2109,
			"issue_number": 2109,
		}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var implRow *portalRun
	for i := range runs {
		if runs[i].IssueNumber == 2109 && !runs[i].Review {
			implRow = &runs[i]
			break
		}
	}
	if implRow == nil {
		t.Fatalf("expected impl row for #2109, got %#v", runs)
	}
	if !implRow.ReviewLive {
		t.Fatalf("impl row ReviewLive=false, want true (in-flight sibling review must stamp ReviewLive via aggregateReviewChildren, issue #2109)")
	}
	if implRow.Status != "success" {
		t.Fatalf("impl row Status=%q, want %q (terminal parent Status must NOT be flipped to \"reviewing\" — existing badge-flip invariant preserved)", implRow.Status, "success")
	}

	// Wire contract: the JSON marshalling must carry the explicit
	// "reviewLive": true key (not omitted via omitempty) so the JS
	// counter line can branch on it. The Status field stays "success".
	data, err := json.Marshal(implRow)
	if err != nil {
		t.Fatalf("marshal impl row: %v", err)
	}
	payload := string(data)
	if !strings.Contains(payload, `"reviewLive":true`) {
		t.Errorf("impl row JSON missing \"reviewLive\":true (live review signal must ride the wire), got %s", payload)
	}
	if !strings.Contains(payload, `"status":"success"`) {
		t.Errorf("impl row JSON missing \"status\":\"success\" (terminal parent Status preserved), got %s", payload)
	}
}

// TestPortal_Compute_TerminalReviewOmitsReviewLiveJSON pins issue #2109
// B6 omitempty contract: when every sibling review child is
// terminal (no in-flight child), the JSON wire must NOT carry the
// "reviewLive" key. The Verdict field rides instead.
func TestPortal_Compute_TerminalReviewOmitsReviewLiveJSON(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	implStartedAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	implFinishedAt := implStartedAt.Add(20 * time.Minute)
	reviewStartedAt := implStartedAt.Add(21 * time.Minute)
	reviewFinishedAt := implStartedAt.Add(25 * time.Minute)

	reviewBatchID := "PR2109-review-term"
	reviewRunID := "PR2109-review"
	reviewBatchDir := filepath.Join(repoRoot, ".sandman", "batches", reviewBatchID)
	reviewRunDir := filepath.Join(reviewBatchDir, "runs", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0755); err != nil {
		t.Fatalf("mkdir review run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reviewRunDir, "decision.md"), []byte("## Decision\n**CHANGES_REQUESTED**\n"), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	// Register the review batch in the Batches index so runFromState can
	// stamp RunDir from the index entry (issue #1938)
	// #1938), letting reviewVerdictFromDecisionFile locate decision.md.
	sliceLayout := paths.NewLayout(nil, repoRoot)
	sliceIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{
				ID:        reviewBatchID,
				Path:      reviewBatchDir,
				Kind:      batchindex.KindReview,
				Status:    batchindex.StatusArchived,
				CreatedAt: implStartedAt,
				PR:        2109,
			},
		},
	}
	if err := sliceIdx.Save(sliceLayout.BatchesIndexPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: implStartedAt, RunID: "impl-2109", Issue: 2109, Payload: map[string]any{
			"branch":   "sandman/2109-fix",
			"batch_id": "impl-2109",
			"status":   "running",
		}},
		{Type: "run.finished", Timestamp: implFinishedAt, RunID: "impl-2109", Issue: 2109, Payload: map[string]any{
			"branch":   "sandman/2109-fix",
			"batch_id": "impl-2109",
			"status":   "success",
		}},
		{Type: "run.started", Timestamp: reviewStartedAt, RunID: reviewRunID, Issue: 2109, Payload: map[string]any{
			"branch":       "sandman/review-PR2109",
			"batch_id":     reviewBatchID,
			"review":       true,
			"pr_number":    2109,
			"issue_number": 2109,
		}},
		{Type: "run.finished", Timestamp: reviewFinishedAt, RunID: reviewRunID, Issue: 2109, Payload: map[string]any{
			"branch":   "sandman/review-PR2109",
			"batch_id": reviewBatchID,
			"review":   true,
			"status":   "success",
		}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var implRow *portalRun
	for i := range runs {
		if runs[i].IssueNumber == 2109 && !runs[i].Review {
			implRow = &runs[i]
			break
		}
	}
	if implRow == nil {
		t.Fatalf("expected impl row for #2109, got %#v", runs)
	}
	if implRow.ReviewLive {
		t.Fatalf("impl row ReviewLive=true, want false (no in-flight sibling review; field must stay zero so omitempty drops it)")
	}
	if implRow.ReviewVerdict != "Changes requested" {
		t.Fatalf("impl row ReviewVerdict=%q, want %q (terminal review verdict must still stamp from decision.md)", implRow.ReviewVerdict, "Changes requested")
	}

	data, err := json.Marshal(implRow)
	if err != nil {
		t.Fatalf("marshal impl row: %v", err)
	}
	payload := string(data)
	if strings.Contains(payload, `"reviewLive"`) {
		t.Errorf("impl row JSON must NOT carry \"reviewLive\" when no sibling review is in flight (omitempty contract), got %s", payload)
	}
	if !strings.Contains(payload, `"reviewVerdict":"Changes requested"`) {
		t.Errorf("impl row JSON missing \"reviewVerdict\":\"Changes requested\", got %s", payload)
	}
}
