package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestMissingManifestIssues_ReturnsUnseenIssuesInManifestOrder(t *testing.T) {
	manifest := daemon.BatchManifest{Issues: []int{1, 2, 3}}
	seen := map[int]struct{}{1: {}}

	got := missingManifestIssues(manifest, seen)
	want := []int{2, 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missingManifestIssues() = %v, want %v", got, want)
	}
}

func TestMissingManifestIssues_ReturnsAllIssuesAndSkipsExcludedKinds(t *testing.T) {
	if got := missingManifestIssues(daemon.BatchManifest{Issues: []int{4, 5}}, nil); !reflect.DeepEqual(got, []int{4, 5}) {
		t.Fatalf("missingManifestIssues() with no seen issues = %v, want %v", got, []int{4, 5})
	}
	for _, runKind := range []string{"auto-select", "review"} {
		if got := missingManifestIssues(daemon.BatchManifest{Issues: []int{4, 5}, RunKind: runKind}, nil); len(got) != 0 {
			t.Fatalf("missingManifestIssues() for runKind %q = %v, want empty", runKind, got)
		}
	}
}

func TestPortal_DeadBatchSynthesizesNeverStartedMembers(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{1, 2, 3}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", batchDir, []int{1, 2, 3})
	if err := daemon.WriteRunManifest(batchDir, "run-1", batchindex.RunManifest{Issue: 1, BatchID: "dead-1", CreatedAt: startedAt.Add(1 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "batch_id": "dead-1"}},
		{Type: "run.finished", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d: %#v", len(runs), runs)
	}
	byIssue := map[int]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}
	if run := byIssue[1]; run.Kind != "completed" || run.Status != "success" {
		t.Fatalf("expected issue 1 to stay completed success, got %#v", run)
	}
	for _, issue := range []int{2, 3} {
		run := byIssue[issue]
		if run.Kind != "completed" || run.Status != "aborted" {
			t.Fatalf("expected synthesized issue %d to be completed aborted, got %#v", issue, run)
		}
	}
}

func TestPortal_DeadBatchSynthesizesAllManifestMembersWithNoIssueEvents(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{1, 2, 3}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", batchDir, []int{1, 2, 3})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 synthesized rows, got %d: %#v", len(runs), runs)
	}
	for _, run := range runs {
		if run.Kind != "completed" || run.Status != "aborted" || run.BatchKey != "dead-1" {
			t.Fatalf("expected dead batch synthesized aborted row, got %#v", run)
		}
	}
}

func TestPortal_DeadBatchSynthesizesOnlyMissingManifestMembers(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{1, 2, 3}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", batchDir, []int{1, 2, 3})

	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	// Without runs/ directory, dirIDs is empty and the event-backed
	// member for issue 1 also gets a synthesized row.
	if len(runs) != 4 {
		t.Fatalf("expected 4 runs (1 event-backed + 3 synthesized), got %d: %#v", len(runs), runs)
	}

	byIssue := map[int][]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = append(byIssue[run.IssueNumber], run)
	}
	if got := len(byIssue[1]); got != 2 {
		t.Fatalf("expected 2 rows for issue 1 (event + synthesized), got %d: %#v", got, byIssue[1])
	}
	for _, run := range byIssue[1] {
		if run.Kind == "completed" && run.Status == "success" && run.BatchKey == "" {
			continue // event-backed row
		}
		if run.Kind == "completed" && run.Status == "aborted" && run.BatchKey == "dead-1" {
			continue // synthesized row (runs/ missing)
		}
		t.Fatalf("unexpected row for issue 1: %#v", run)
	}
	for _, issue := range []int{2, 3} {
		if got := len(byIssue[issue]); got != 1 {
			t.Fatalf("expected exactly 1 synthesized row for issue %d, got %d: %#v", issue, got, byIssue[issue])
		}
		run := byIssue[issue][0]
		if run.Kind != "completed" || run.Status != "aborted" || run.BatchKey != "dead-1" {
			t.Fatalf("expected issue %d to synthesize as dead-batch completed aborted row, got %#v", issue, run)
		}
	}
}

func TestPortal_DeadBatchSynthesizesNeverStartedMembersWithoutRunsTree(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{1, 2, 3}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", batchDir, []int{1, 2, 3})
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	// Without runs/ directory, dirIDs is empty and synthesis cannot
	// distinguish batch identity. The event-backed member coexists
	// with its synthesized row as a known limitation.
	if len(runs) != 4 {
		t.Fatalf("expected 4 runs (1 event + 3 synthesized, runs/ dir missing), got %d: %#v", len(runs), runs)
	}
	byIssue := map[int][]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = append(byIssue[run.IssueNumber], run)
	}
	if got := len(byIssue[1]); got != 2 {
		t.Fatalf("expected 2 rows for issue 1 (event + synthesized, runs/ missing), got %d: %#v", got, byIssue[1])
	}
	var sawEventIss1, sawSynthIss1 bool
	for _, run := range byIssue[1] {
		if run.Kind == "completed" && run.Status == "success" && run.BatchKey == "" {
			sawEventIss1 = true
		}
		if run.Kind == "completed" && run.Status == "aborted" && run.BatchKey == "dead-1" {
			sawSynthIss1 = true
		}
	}
	if !sawEventIss1 {
		t.Fatal("expected event-backed issue 1 to have a completed success row")
	}
	if !sawSynthIss1 {
		t.Fatal("expected issue 1 to also have a synthesized row (runs/ missing)")
	}
	for _, issue := range []int{2, 3} {
		if got := len(byIssue[issue]); got != 1 {
			t.Fatalf("expected 1 synthesized row for issue %d, got %d: %#v", issue, got, byIssue[issue])
		}
		run := byIssue[issue][0]
		if run.Kind != "completed" || run.Status != "aborted" {
			t.Fatalf("expected synthesized issue %d to be completed aborted, got %#v", issue, run)
		}
	}
}

func TestPortal_DeadBatchesReuseIssueNumberWithoutSuppressingLaterSynthesis(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	firstStart := time.Now().Add(-30 * time.Minute)
	firstDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(firstDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(firstDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: firstStart}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", firstDir, []int{42})
	if err := daemon.WriteRunManifest(firstDir, "run-42-old", batchindex.RunManifest{Issue: 42, BatchID: "dead-1", CreatedAt: firstStart.Add(1 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	secondStart := time.Now().Add(-10 * time.Minute)
	secondDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-2")
	if err := os.MkdirAll(secondDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(secondDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: secondStart}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-2", secondDir, []int{42})

	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: firstStart.Add(1 * time.Minute), RunID: "run-42-old", Issue: 42, Payload: map[string]any{"branch": "sandman/42-old", "batch_id": "dead-1"}},
		{Type: "run.finished", Timestamp: firstStart.Add(2 * time.Minute), RunID: "run-42-old", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-old"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 rows for issue 42 across two dead batches, got %d: %#v", len(runs), runs)
	}
	var sawSuccess, sawAborted bool
	for _, run := range runs {
		if run.IssueNumber != 42 {
			t.Fatalf("expected issue 42 in both rows, got %#v", run)
		}
		switch run.Status {
		case "success":
			sawSuccess = true
		case "aborted":
			sawAborted = true
		default:
			t.Fatalf("unexpected status for reused issue 42: %#v", run)
		}
	}
	if !sawSuccess || !sawAborted {
		t.Fatalf("expected both success and synthesized aborted rows, got %#v", runs)
	}
}

func TestPortal_DeadBatchUsesBatchSkewWindowForSeenIssues(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", batchDir, []int{42})
	if err := daemon.WriteRunManifest(batchDir, "run-42", batchindex.RunManifest{Issue: 42, BatchID: "dead-1", CreatedAt: startedAt.Add(1 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(-500 * time.Millisecond), RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix", "batch_id": "dead-1"}},
		{Type: "run.finished", Timestamp: startedAt.Add(500 * time.Millisecond), RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	if runs[0].Status != "success" {
		t.Fatalf("expected near-boundary issue to stay success, got %#v", runs[0])
	}
}

func TestPortal_LiveBatchKeepsNeverStartedMemberQueued(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "live-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42, 43}, CreatedAt: time.Now().Add(-10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))
	addBatchToIndex(t, repoRoot, "live-1", batchDir, []int{42, 43})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 live rows, got %d: %#v", len(runs), runs)
	}
	for _, run := range runs {
		if run.Kind != "active" || run.Status != "queued" {
			t.Fatalf("expected live never-started member to stay active queued, got %#v", run)
		}
	}
}

func TestPortal_MixedLiveDeadAndOrphanRowsStayDistinct(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-20 * time.Minute)
	liveDir := filepath.Join(repoRoot, ".sandman", "batches", "live-1")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(liveDir, daemon.BatchManifest{Issues: []int{7}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(liveDir, "batch.sock"))
	addBatchToIndex(t, repoRoot, "live-1", liveDir, []int{7})

	deadDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(deadDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(deadDir, daemon.BatchManifest{Issues: []int{1, 2}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", deadDir, []int{1, 2})
	if err := daemon.WriteRunManifest(deadDir, "run-1", batchindex.RunManifest{Issue: 1, BatchID: "dead-1", CreatedAt: startedAt.Add(1 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "batch_id": "dead-1"}},
		{Type: "run.finished", Timestamp: startedAt.Add(2 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
		{Type: "run.started", Timestamp: startedAt.Add(3 * time.Minute), RunID: "orphan-99", Issue: 99, Payload: map[string]any{"branch": "sandman/99-fix"}},
		{Type: "run.aborted", Timestamp: startedAt.Add(4 * time.Minute), RunID: "orphan-99", Issue: 99, Payload: map[string]any{"status": "aborted", "branch": "sandman/99-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 4 {
		t.Fatalf("expected 4 rows, got %d: %#v", len(runs), runs)
	}
	byIssue := map[int]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}
	if run := byIssue[7]; run.Kind != "active" || run.Status != "queued" {
		t.Fatalf("expected live issue 7 to remain active queued, got %#v", run)
	}
	if run := byIssue[1]; run.Kind != "completed" || run.Status != "success" {
		t.Fatalf("expected issue 1 to stay completed success, got %#v", run)
	}
	if run := byIssue[2]; run.Kind != "completed" || run.Status != "aborted" {
		t.Fatalf("expected dead batch issue 2 to synthesize as completed aborted, got %#v", run)
	}
	if run := byIssue[99]; run.Kind != "completed" || run.Status != "aborted" {
		t.Fatalf("expected orphan issue 99 to remain completed aborted, got %#v", run)
	}
}

func TestPortal_DeadBatchSynthesisIgnoresReviewRuns(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-1")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42, 43}, CreatedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-1", batchDir, []int{42, 43})

	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt.Add(1 * time.Minute), RunID: "review-42", Issue: 42, Payload: map[string]any{"review": true, "branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(2 * time.Minute), RunID: "review-42", Issue: 42, Payload: map[string]any{"review": true, "status": "success", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 rows (1 review + 2 synthesized), got %d: %#v", len(runs), runs)
	}
	var sawReview, sawDead42, sawDead43 bool
	for _, run := range runs {
		if run.IssueNumber == 42 && run.Review {
			sawReview = true
		}
		if run.IssueNumber == 42 && !run.Review && run.Kind == "completed" && run.Status == "aborted" && run.BatchKey == "dead-1" {
			sawDead42 = true
		}
		if run.IssueNumber == 43 && run.Kind == "completed" && run.Status == "aborted" && run.BatchKey == "dead-1" {
			sawDead43 = true
		}
	}
	if !sawReview {
		t.Fatal("expected review row for issue 42")
	}
	if !sawDead42 {
		t.Fatal("expected synthesized aborted row for dead batch issue 42 (review run must not suppress synthesis)")
	}
	if !sawDead43 {
		t.Fatal("expected synthesized aborted row for dead batch issue 43")
	}
}

// TestPortal_Compute_ReviewOnlyRunRemainsInComputedSet is the regression
// guard for behaviour #6 of issue #1526: a review-only run (one with no
// implementation run anywhere in the same batch) must appear in the
// portalRunsView computed set with Review=true and PRNumber set, so the
// frontend can render the explicit review-only orphan row instead of
// filtering it out as a stray review.
func TestPortal_Compute_ReviewOnlyRunRemainsInComputedSet(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "review-1472", Issue: 1472, Payload: map[string]any{"review": true, "branch": "sandman/1472-fix", "pr_number": 1508}},
		{Type: "run.finished", Timestamp: startedAt.Add(2 * time.Minute), RunID: "review-1472", Issue: 1472, Payload: map[string]any{"review": true, "status": "success", "branch": "sandman/1472-fix", "pr_number": 1508}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected exactly one computed run for review-only issue, got %d: %#v", len(runs), runs)
	}
	run := runs[0]
	if !run.Review {
		t.Fatalf("expected computed run to be marked Review, got %#v", run)
	}
	if run.PRNumber != 1508 {
		t.Fatalf("expected computed run to carry PRNumber=1508, got %#v", run)
	}
	if run.RunID != "review-1472" {
		t.Fatalf("expected computed run to preserve RunID review-1472, got %#v", run)
	}
	if run.IssueNumber != 1472 {
		t.Fatalf("expected computed run to carry IssueNumber=1472, got %#v", run)
	}
}
