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
	// Without runs/ directory, dirIDs is empty so the event-backed
	// member for issue 1 still gets a synthetic row from
	// synthesizedDeadBatchRows. The cross-batch dedup strip pass (issue
	// #1886) drops that duplicate because issue 1 is already covered
	// by an event-backed row.
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs (1 event-backed + 2 synthesized), got %d: %#v", len(runs), runs)
	}

	byIssue := map[int][]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = append(byIssue[run.IssueNumber], run)
	}
	if got := len(byIssue[1]); got != 1 {
		t.Fatalf("expected 1 row for issue 1 (event-backed only, duplicate synthesized row suppressed), got %d: %#v", got, byIssue[1])
	}
	if run := byIssue[1][0]; run.Status != "success" {
		t.Fatalf("expected issue 1 to remain event-backed success, got %#v", run)
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
	// distinguish batch identity, so synthesizedDeadBatchRows still
	// fabricates a row for issue 1. The cross-batch dedup strip pass
	// (issue #1886) drops that duplicate because issue 1 is already
	// covered by the event-backed row.
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs (1 event + 2 synthesized, runs/ dir missing), got %d: %#v", len(runs), runs)
	}
	byIssue := map[int][]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = append(byIssue[run.IssueNumber], run)
	}
	if got := len(byIssue[1]); got != 1 {
		t.Fatalf("expected 1 row for issue 1 (event-backed only, duplicate synthesized row suppressed), got %d: %#v", got, byIssue[1])
	}
	if run := byIssue[1][0]; run.Status != "success" {
		t.Fatalf("expected issue 1 to remain event-backed success, got %#v", run)
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

// TestPortal_DeadBatchesReuseIssueNumberWithEventBackedSuppression pins
// the cross-batch dedup contract from issue #1886: when two dead
// batches claim the same issue, a synthetic aborted row for the issue
// must be suppressed if any other batch carries an event-backed row
// for that issue, regardless of BatchKey. Without the suppression the
// portal surfaces a 0s aborted row with no log over the real row.
//
// The companion case where no batch has any events for the issue is
// covered by TestPortal_DeadBatchSynthesizesOnlyMissingManifestMembers
// and TestPortal_DeadBatchSynthesizesAllManifestMembersWithNoIssueEvents.
func TestPortal_DeadBatchesReuseIssueNumberWithEventBackedSuppression(t *testing.T) {
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
	if len(runs) != 1 {
		t.Fatalf("expected 1 row for issue 42 (event-backed success only, dead-2 synthesized row suppressed), got %d: %#v", len(runs), runs)
	}
	row := runs[0]
	if row.IssueNumber != 42 {
		t.Fatalf("expected issue 42, got %#v", row)
	}
	if row.Status != "success" {
		t.Fatalf("expected event-backed success row, got %#v", row)
	}
	if row.BatchKey != "dead-1" {
		t.Fatalf("expected row anchored to event-backed batch dead-1, got BatchKey %q", row.BatchKey)
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

// TestPortal_CrossBatchSynthesisSuppressedWhenIssueAlreadyCovered pins
// the cross-batch dedup behaviour for issue #1886: a dead-batch
// synthesized aborted row must be suppressed when any other batch (live
// or dead) already carries an event-backed row for the same issue. The
// common case is an orphaned "ghost" batch directory that was created
// but never received a run, while the real run lives in a newer batch.
// Without this suppression the portal surfaces a 0s aborted row with
// no log path over the real row, because dedup only collapsed rows
// within the same BatchKey.
func TestPortal_CrossBatchSynthesisSuppressedWhenIssueAlreadyCovered(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	realStart := time.Now().Add(-15 * time.Minute)
	realDir := filepath.Join(repoRoot, ".sandman", "batches", "fb4a-real")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(realDir, daemon.BatchManifest{Issues: []int{1857}, CreatedAt: realStart}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "fb4a-real", realDir, []int{1857})
	if err := daemon.WriteRunManifest(realDir, "fb4a-real-1857", batchindex.RunManifest{Issue: 1857, BatchID: "fb4a-real", CreatedAt: realStart.Add(1 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	ghostStart := time.Now().Add(-30 * time.Minute)
	ghostDir := filepath.Join(repoRoot, ".sandman", "batches", "2569-ghost")
	if err := os.MkdirAll(ghostDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(ghostDir, daemon.BatchManifest{Issues: []int{1855, 1857}, CreatedAt: ghostStart}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "2569-ghost", ghostDir, []int{1855, 1857})

	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: realStart.Add(1 * time.Minute), RunID: "fb4a-real-1857", Issue: 1857, Payload: map[string]any{"branch": "sandman/1857-fix", "batch_id": "fb4a-real"}},
		{Type: "run.finished", Timestamp: realStart.Add(2 * time.Minute), RunID: "fb4a-real-1857", Issue: 1857, Payload: map[string]any{"status": "success", "branch": "sandman/1857-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	byIssue := map[int][]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = append(byIssue[run.IssueNumber], run)
	}

	if rows := byIssue[1857]; len(rows) != 1 {
		t.Fatalf("expected exactly 1 row for issue 1857 (event-backed only, ghost synthesized row suppressed), got %d: %#v", len(rows), rows)
	}
	row := byIssue[1857][0]
	if row.Status != "success" {
		t.Fatalf("expected issue 1857 row to be the event-backed success row, got %#v", row)
	}
	if row.BatchKey != "fb4a-real" {
		t.Fatalf("expected issue 1857 row to be anchored to real batch fb4a-real, got %q", row.BatchKey)
	}

	if rows := byIssue[1855]; len(rows) != 1 {
		t.Fatalf("expected exactly 1 row for issue 1855 (ghost-synthesized only), got %d: %#v", len(rows), rows)
	}
	ghost := byIssue[1855][0]
	if ghost.Kind != "completed" || ghost.Status != "aborted" || ghost.BatchKey != "2569-ghost" {
		t.Fatalf("expected issue 1855 to keep the ghost-batch synthesized aborted row, got %#v", ghost)
	}
}

// TestPortal_CrossBatchSynthesisKeepsReviewRunDistinct is the regression
// guard for the cross-batch dedup fix: a review run on the same issue
// must not suppress the ghost-batch synthesized aborted row, because
// review and implementation runs are different work. Without this guard
// the dedup pass would treat every review row as a covered issue and
// drop legitimate dead-batch placeholders for unimplemented work.
func TestPortal_CrossBatchSynthesisKeepsReviewRunDistinct(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reviewAt := time.Now().Add(-15 * time.Minute)
	reviewBatchDir := filepath.Join(repoRoot, ".sandman", "batches", "review-42")
	if err := os.MkdirAll(reviewBatchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(reviewBatchDir, daemon.BatchManifest{Issues: []int{42}, RunKind: "review", CreatedAt: reviewAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "review-42", reviewBatchDir, []int{42})
	if err := daemon.WriteRunManifest(reviewBatchDir, "review-42-42", batchindex.RunManifest{Issue: 42, BatchID: "review-42", CreatedAt: reviewAt.Add(1 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	deadAt := time.Now().Add(-30 * time.Minute)
	deadDir := filepath.Join(repoRoot, ".sandman", "batches", "dead-impl")
	if err := os.MkdirAll(deadDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(deadDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: deadAt}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "dead-impl", deadDir, []int{42})

	logPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	writePortalLog(t, logPath, []events.Event{
		{Type: "run.started", Timestamp: reviewAt.Add(1 * time.Minute), RunID: "review-42-42", Issue: 42, Payload: map[string]any{"review": true, "pr_number": 99, "branch": "sandman/review-99"}},
		{Type: "run.finished", Timestamp: reviewAt.Add(2 * time.Minute), RunID: "review-42-42", Issue: 42, Payload: map[string]any{"review": true, "status": "success", "pr_number": 99, "branch": "sandman/review-99"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: logPath})
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}

	byIssue := map[int][]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = append(byIssue[run.IssueNumber], run)
	}

	rows := byIssue[42]
	if len(rows) != 2 {
		t.Fatalf("expected exactly 2 rows for issue 42 (review + ghost synthesized), got %d: %#v", len(rows), rows)
	}
	var sawReview, sawSynth bool
	for _, run := range rows {
		if run.Review {
			sawReview = true
		}
		if !run.Review && run.Kind == "completed" && run.Status == "aborted" && run.BatchKey == "dead-impl" {
			sawSynth = true
		}
	}
	if !sawReview {
		t.Fatal("expected review row for issue 42")
	}
	if !sawSynth {
		t.Fatal("expected ghost-batch synthesized aborted row for issue 42 (review run must not suppress synthesis)")
	}
}
