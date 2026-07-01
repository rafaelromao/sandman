package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

// TestPortal_ResolveReviewRunFromCanonicalFolder_Active pins the canonical
// portal behavior for an ACTIVE review run: the active row's RunID must
// equal the per-row RunID written to runs/<rowID>/run.json by the review
// daemon (issue #1551), not the batches-index Entry.ID (the batch dir name).
//
// The previous portal behavior collapsed both onto the batch id, which broke
// the eventsByRun lookup for the canonical review RunID and made the log
// resolver point at a non-existent per-row folder. This slice is the
// tracer bullet that proves the discovery side of the portal speaks the
// canonical schema.
func TestPortal_ResolveReviewRunFromCanonicalFolder_Active(t *testing.T) {
	// Unix sockets reject paths longer than 108 bytes; t.TempDir() puts
	// us under /tmp/TestPortal_ResolveReviewRunFromCanonicalFolder_Active...
	// so a long-batchid + long-rowid quickly blows the budget. Use a
	// short tmpdir under /tmp and short identifiers like the rest of
	// the suite does.
	repoRoot, err := os.MkdirTemp("/tmp", "pra")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchID := "sid-2606181138-PR42"
	canonicalRowID := "sid-2606181138-1066-PR42"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	batchSockPath := filepath.Join(batchDir, "batch.sock")
	runDir := filepath.Join(batchDir, "runs", canonicalRowID)
	runSockPath := filepath.Join(runDir, "run.sock")

	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, batchSockPath)
	createUnixRunSocket(t, runSockPath)

	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(42),
		Issues:    []int{},
		CreatedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	if err := daemon.WriteRunManifest(batchDir, canonicalRowID, batchindex.RunManifest{
		RunID:     canonicalRowID,
		BatchID:   batchID,
		PR:        42,
		Kind:      batchindex.KindReview,
		Status:    batchindex.RunManifestStatusActive,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:        batchID,
				Path:      batchDir,
				Kind:      batchindex.KindReview,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now().Add(-time.Minute),
				PR:        42,
			},
		},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-2 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: canonicalRowID, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
	})

	idx := getPortalRunsIndex(repoRoot)
	active, err := idx.discoverActiveRuns(map[string][]portalEvent{
		canonicalRowID: {
			{Type: "run.started", Timestamp: startedAt, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
		},
	})
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d: %#v", len(active), active)
	}

	got := active[0]
	if got.RunID != canonicalRowID {
		t.Fatalf("active review RunID=%q, want canonical %q (must not collapse to batch id %q)", got.RunID, canonicalRowID, batchID)
	}
	if got.Key != canonicalRowID {
		t.Fatalf("active review Key=%q, want canonical %q (must not collapse to batch id)", got.Key, canonicalRowID)
	}
	if got.PRNumber != 42 {
		t.Fatalf("active review PRNumber=%d, want 42 (must be resolved from eventsByRun[<canonical rowID>])", got.PRNumber)
	}
	if got.BatchID != batchID {
		t.Fatalf("active review BatchID=%q, want %q", got.BatchID, batchID)
	}
}

// TestPortal_ResolveReviewRunFromCanonicalFolder_Completed pins the behavior
// for a COMPLETED review run: the portal surfaces the canonical row RunID
// (read from runs/<rowID>/run.json) even after the batch.sock listener is
// gone, because findBatchDirForRun consults the same on-disk run.json shape.
func TestPortal_ResolveReviewRunFromCanonicalFolder_Completed(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "prc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchID := "sid-2606181138-PR42"
	canonicalRowID := "sid-2606181138-1066-PR42"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	runDir := filepath.Join(batchDir, "runs", canonicalRowID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(42),
		Issues:    []int{},
		CreatedAt: time.Now().Add(-3 * time.Minute),
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	if err := daemon.WriteRunManifest(batchDir, canonicalRowID, batchindex.RunManifest{
		RunID:     canonicalRowID,
		BatchID:   batchID,
		PR:        42,
		Kind:      batchindex.KindReview,
		Status:    batchindex.RunManifestStatusSuccess,
		CreatedAt: time.Now().Add(-3 * time.Minute),
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte("["+canonicalRowID+"] 12:00:00 saved review log line\n"), 0644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:        batchID,
				Path:      batchDir,
				Kind:      batchindex.KindReview,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now().Add(-3 * time.Minute),
				PR:        42,
			},
		},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-3 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: canonicalRowID, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: canonicalRowID, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true, "batch_id": batchID}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.RunID != canonicalRowID {
		t.Fatalf("completed review RunID=%q, want canonical %q", got.RunID, canonicalRowID)
	}
	if got.Key != canonicalRowID {
		t.Fatalf("completed review Key=%q, want canonical %q", got.Key, canonicalRowID)
	}
	if got.Kind != "completed" {
		t.Fatalf("completed review Kind=%q, want %q", got.Kind, "completed")
	}
	if got.Status != "success" {
		t.Fatalf("completed review Status=%q, want %q", got.Status, "success")
	}
	if !got.Review {
		t.Fatal("completed review Review=false, want true")
	}
	if got.PRNumber != 42 {
		t.Fatalf("completed review PRNumber=%d, want 42", got.PRNumber)
	}
	wantLogPath := filepath.Join(runDir, "run.log")
	if got.LogPath != wantLogPath {
		t.Fatalf("completed review LogPath=%q, want %q (per-row log, not batch root)", got.LogPath, wantLogPath)
	}
	if !strings.Contains(got.Log, "saved review log line") {
		t.Fatalf("completed review Log missing saved content, got %q", got.Log)
	}
}

// TestPortal_ResolveReviewRunFromCanonicalFolder_EventLogOnly pins the
// event-log-only path: no runs/<rowID>/ folder on disk, only the canonical
// run.started/run.finished events in events.jsonl. The portal surfaces a
// single completed row whose RunID equals the canonical row RunID (read from
// the event log's RunID field, never from a folder-name heuristic).
func TestPortal_ResolveReviewRunFromCanonicalFolder_EventLogOnly(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pre")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	canonicalRowID := "sid-2606181138-PR42"
	startedAt := time.Now().Add(-5 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: canonicalRowID, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: canonicalRowID, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row from event log only, got %d: %#v", len(runs), runs)
	}
	got := runs[0]
	if got.RunID != canonicalRowID {
		t.Fatalf("event-log-only review RunID=%q, want canonical %q", got.RunID, canonicalRowID)
	}
	if got.Kind != "completed" {
		t.Fatalf("event-log-only review Kind=%q, want completed", got.Kind)
	}
	if got.Status != "success" {
		t.Fatalf("event-log-only review Status=%q, want success", got.Status)
	}
	if !got.Review {
		t.Fatal("event-log-only review Review=false, want true")
	}
	if got.PRNumber != 42 {
		t.Fatalf("event-log-only review PRNumber=%d, want 42", got.PRNumber)
	}
}

// TestPortal_NoRunsReviewLiteralInPortalCode enforces acceptance criterion
// "no portal code depends on runs/review for review discovery" (issue #1550,
// ADR-0030 correction). The literal `"review"` must not appear in any
// portal source file (portal*.go) as a folder name concatenated onto
// `runs/`. The `payload["review"]` flag and the `"review"` reason chip
// remain part of the event contract and are intentionally excluded from
// this assertion.
func TestPortal_NoRunsReviewLiteralInPortalCode(t *testing.T) {
	portalFiles, err := filepath.Glob(filepath.Join("internal", "cmd", "portal*.go"))
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		`filepath.Join(..., "runs", "review"`,
		`path.Join(..., "runs", "review"`,
		`"runs/review"`,
		`'runs/review'`,
		`"runs" + "/review"`,
		`"runs/" + "review"`,
		`'runs/' + "review"`,
	}
	for _, file := range portalFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(data)
		for _, needle := range forbidden {
			if strings.Contains(text, needle) {
				t.Errorf("%s still contains forbidden runs/review path literal %q; remove the alias (issue #1550)", file, needle)
			}
		}
	}
}

func intPtr(v int) *int {
	return &v
}

// TestPortal_ReviewAggregation_HonorsCanonicalRowID pins slice 4: a parent
// implementation row and a child review row for the same IssueNumber must
// continue to aggregate after the child resolves to its canonical row RunID
// (rather than the batchId). The review row's status flows to the parent's
// `ReviewCount`/`ReviewVerdict` exactly as it did before the fix; only the
// row keys move from batchId-shaped to rowId-shaped.
func TestPortal_ReviewAggregation_HonorsCanonicalRowID(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 1066
	canonicalReviewRowID := "sid-2606181138-1066-PR42"
	startedAt := time.Now().Add(-10 * time.Minute)

	// Parent impl row for the linked issue + child review row in the
	// linked PR. The original issue event uses the issue-row's own RunID;
	// the review event uses the canonical per-row RunID (ADR-0030 shape).
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "impl-1066", Issue: issueNumber, Payload: map[string]any{"branch": "sandman/1066-fix", "issue_number": issueNumber, "batch_id": "impl-1066"}},
		{Type: "run.finished", Timestamp: startedAt.Add(8 * time.Minute), RunID: "impl-1066", Issue: issueNumber, Payload: map[string]any{"status": "success", "branch": "sandman/1066-fix", "issue_number": issueNumber, "batch_id": "impl-1066"}},
		{Type: "run.started", Timestamp: startedAt.Add(2 * time.Minute), RunID: canonicalReviewRowID, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42, "issue_number": issueNumber, "batch_id": "sid-2606181138-PR42"}},
		{Type: "run.finished", Timestamp: startedAt.Add(7 * time.Minute), RunID: canonicalReviewRowID, Payload: map[string]any{"status": "success", "branch": "sandman/review-PR42", "review": true, "pr_number": 42, "issue_number": issueNumber, "batch_id": "sid-2606181138-PR42"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) < 1 {
		t.Fatalf("expected at least 1 row, got %d: %#v", len(runs), runs)
	}

	var parent *portalRun
	var review *portalRun
	for i := range runs {
		switch {
		case runs[i].IssueNumber == issueNumber && !runs[i].Review:
			parent = &runs[i]
		case runs[i].Review:
			review = &runs[i]
		}
	}
	if parent == nil {
		t.Fatalf("expected parent impl row for issue %d, got %#v", issueNumber, runs)
	}
	if review == nil {
		t.Fatalf("expected review row (Review=true), got %#v", runs)
	}
	if review.RunID != canonicalReviewRowID {
		t.Fatalf("review row RunID=%q, want canonical %q", review.RunID, canonicalReviewRowID)
	}
	if review.Status != "success" {
		t.Fatalf("review row Status=%q, want success", review.Status)
	}
	if parent.ReviewCount == 0 {
		t.Fatalf("parent ReviewCount=%d, want >=1 (aggregation must include canonical-row-id'd review)", parent.ReviewCount)
	}
	if parent.ReviewVerdict == "" {
		t.Fatalf("parent ReviewVerdict=%q, want non-empty (Approved or Changes requested)", parent.ReviewVerdict)
	}
}

// TestPortal_ReviewAggregation_LiveReviewSocketPreservesIssueIdentity
// pins the issue #1597 regression: when a live review socket is discovered
// without batch-manifest issue membership, the portal must recover the review
// row's issue_number before grouping so the canonical implementation row stays
// visible and the review row stays reachable by its real RunID.
func TestPortal_ReviewAggregation_LiveReviewSocketPreservesIssueIdentity(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pri")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 139
	const batchID = "sid-2606181138-PR42"
	const canonicalReviewRowID = "sid-2606181138-139-PR42"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))

	startedAt := time.Now().Add(-4 * time.Minute)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(42),
		Issues:    []int{},
		CreatedAt: startedAt,
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        42,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	parentStarted := startedAt.Add(-6 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: parentStarted, RunID: "impl-139", Issue: issueNumber, Payload: map[string]any{"branch": "sandman/139-fix", "issue_number": issueNumber, "issue_title": "Fix issue 139", "batch_id": "impl-139"}},
		{Type: "run.finished", Timestamp: parentStarted.Add(5 * time.Minute), RunID: "impl-139", Issue: issueNumber, Payload: map[string]any{"status": "success", "branch": "sandman/139-fix", "issue_number": issueNumber, "issue_title": "Fix issue 139", "batch_id": "impl-139"}},
		{Type: "run.started", Timestamp: startedAt, RunID: canonicalReviewRowID, Issue: issueNumber, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42, "issue_number": issueNumber, "batch_id": batchID}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var parent *portalRun
	var review *portalRun
	for i := range runs {
		switch {
		case runs[i].IssueNumber == issueNumber && !runs[i].Review:
			parent = &runs[i]
		case runs[i].RunID == canonicalReviewRowID:
			review = &runs[i]
		}
	}
	if parent == nil {
		t.Fatalf("expected canonical parent row for issue %d, got %#v", issueNumber, runs)
	}
	if review == nil {
		t.Fatalf("expected live review row with RunID %q, got %#v", canonicalReviewRowID, runs)
	}
	if parent.Key != "impl-139" || parent.RunID != "impl-139" {
		t.Fatalf("expected canonical parent identity to stay anchored on impl-139, got %#v", parent)
	}
	if parent.BatchKey != "impl-139" {
		t.Fatalf("expected canonical parent BatchKey impl-139, got %q", parent.BatchKey)
	}
	if parent.IssueTitle != "Fix issue 139" {
		t.Fatalf("expected canonical parent title to stay intact, got %q", parent.IssueTitle)
	}
	if !parent.StartedAt.Equal(parentStarted) {
		t.Fatalf("expected canonical parent StartedAt %s, got %s", parentStarted, parent.StartedAt)
	}
	if parent.Status != "success" {
		t.Fatalf("expected canonical parent status to stay anchored on its own result, got %q", parent.Status)
	}
	if parent.ReviewCount != 1 {
		t.Fatalf("expected canonical parent ReviewCount 1, got %d", parent.ReviewCount)
	}
	if review.IssueNumber != issueNumber {
		t.Fatalf("expected live review IssueNumber %d, got %d", issueNumber, review.IssueNumber)
	}
	if !review.Review {
		t.Fatalf("expected live review row to remain review=true, got %#v", review)
	}
	if !review.GroupedReview {
		t.Fatalf("expected live review row to be grouped, got %#v", review)
	}
	if review.Kind != "active" || review.Status != "reviewing" {
		t.Fatalf("expected live review row to remain active/reviewing, got %#v", review)
	}
}

// TestPortal_DiscoverActiveRuns_ReviewRunFolderPreservesIssueIdentity pins the
// active-socket side of the #1597 regression: the portal must recover a live
// review's issue number from the review run folder before any event-log
// grouping happens, so the batch can still attach to the canonical issue row
// even if the review event stream has not yet been replayed.
func TestPortal_DiscoverActiveRuns_ReviewRunFolderPreservesIssueIdentity(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pri")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const issueNumber = 139
	const batchID = "sid-2606181138-PR42"
	const canonicalReviewRowID = "sid-2606181138-139-PR42"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))

	startedAt := time.Now().Add(-4 * time.Minute)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(42),
		Issues:    []int{},
		CreatedAt: startedAt,
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}
	if err := daemon.WriteRunManifest(batchDir, canonicalReviewRowID, batchindex.RunManifest{
		RunID:     canonicalReviewRowID,
		BatchID:   batchID,
		PR:        42,
		Kind:      batchindex.KindReview,
		CreatedAt: startedAt,
		Status:    batchindex.RunManifestStatusActive,
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        42,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	active, err := (&portalRunsView{}).discoverActiveRuns(repoRoot, map[string][]portalEvent{})
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d: %#v", len(active), active)
	}
	if got := active[0].IssueNumber; got != issueNumber {
		t.Fatalf("expected active IssueNumber %d, got %d", issueNumber, got)
	}
	if got := active[0].RunID; got != canonicalReviewRowID {
		t.Fatalf("expected active RunID %q, got %q", canonicalReviewRowID, got)
	}
	if got := active[0].IssueNumbers; len(got) != 1 || got[0] != issueNumber {
		t.Fatalf("expected active IssueNumbers [%d], got %#v", issueNumber, got)
	}
}

// TestPortal_BadgeFlipSurvivesEventLogReplay pins slice #1527 acceptance
// criterion 6: refreshing the portal page or replaying the run index from
// events.jsonl only must produce the same badge behavior. Two issues back
// to back, each with a canonical parent impl row and a live or completed
// review child, must surface as portalRow with the right pair of (Kind,
// Status) so visibleRunForIssueGroup's flip/no-flip branch makes the same
// decision deterministically across recomputes. After the second compute,
// the rows are marshaled into the portal.html script so the visible-run
// helper itself picks the right badge for each issue group — the determinism
// check is on the badge output, not only on the intermediate row state.
func TestPortal_BadgeFlipSurvivesEventLogReplay(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pbf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const liveIssue = 1001
	const doneIssue = 1002
	const canonicalReviewRowID = "sid-2606181138-1001-PR42"

	startedAt := time.Now().Add(-15 * time.Minute)

	// Issue 1001: parent completed + review child still live (active, reviewing).
	// Issue 1002: parent completed + review child completed (success).
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		// Parent issue 1001 finished before review started.
		{Type: "run.started", Timestamp: startedAt, RunID: "impl-1001", Issue: liveIssue, Payload: map[string]any{"branch": "sandman/1001-fix", "issue_number": liveIssue, "batch_id": "impl-1001"}},
		{Type: "run.finished", Timestamp: startedAt.Add(5 * time.Minute), RunID: "impl-1001", Issue: liveIssue, Payload: map[string]any{"status": "success", "branch": "sandman/1001-fix", "issue_number": liveIssue, "batch_id": "impl-1001"}},
		// Review for #1001 — started, never finished in this window.
		{Type: "run.started", Timestamp: startedAt.Add(2 * time.Minute), RunID: canonicalReviewRowID, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42, "issue_number": liveIssue, "batch_id": "sid-2606181138-PR42"}},

		// Parent issue 1002 finished before review started.
		{Type: "run.started", Timestamp: startedAt.Add(8 * time.Minute), RunID: "impl-1002", Issue: doneIssue, Payload: map[string]any{"branch": "sandman/1002-fix", "issue_number": doneIssue, "batch_id": "impl-1002"}},
		{Type: "run.finished", Timestamp: startedAt.Add(12 * time.Minute), RunID: "impl-1002", Issue: doneIssue, Payload: map[string]any{"status": "success", "branch": "sandman/1002-fix", "issue_number": doneIssue, "batch_id": "impl-1002"}},
		// Review for #1002 — fully completed (terminal verdict).
		{Type: "run.started", Timestamp: startedAt.Add(9 * time.Minute), RunID: "sid-2606181138-1002-PR43", Payload: map[string]any{"branch": "sandman/review-PR43", "review": true, "pr_number": 43, "issue_number": doneIssue, "batch_id": "sid-2606181138-PR43"}},
		{Type: "run.finished", Timestamp: startedAt.Add(13 * time.Minute), RunID: "sid-2606181138-1002-PR43", Payload: map[string]any{"status": "success", "branch": "sandman/review-PR43", "review": true, "issue_number": doneIssue, "batch_id": "sid-2606181138-PR43"}},
	})

	logger := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}

	// Run compute twice to simulate a page refresh / replay. The output
	// must be identical (acceptance criterion 6: deterministic replay).
	firstRuns, err := (&portalRunsView{}).compute(repoRoot, logger)
	if err != nil {
		t.Fatalf("first compute: %v", err)
	}
	secondRuns, err := (&portalRunsView{}).compute(repoRoot, logger)
	if err != nil {
		t.Fatalf("second compute: %v", err)
	}
	if len(firstRuns) != len(secondRuns) {
		t.Fatalf("replay length mismatch: first=%d second=%d", len(firstRuns), len(secondRuns))
	}

	byIssue := func(issue int, review bool) *portalRun {
		for i := range firstRuns {
			if firstRuns[i].IssueNumber == issue && firstRuns[i].Review == review {
				return &firstRuns[i]
			}
		}
		return nil
	}

	// Issue 1001 review child must be live (Kind=active, Status=reviewing)
	// so visibleRunForIssueGroup will flip the parent badge.
	liveReview := byIssue(liveIssue, true)
	if liveReview == nil {
		t.Fatalf("expected live review child for issue %d, got %#v", liveIssue, firstRuns)
	}
	if liveReview.Kind != "active" {
		t.Fatalf("live review Kind=%q, want active", liveReview.Kind)
	}
	if liveReview.Status != "reviewing" {
		t.Fatalf("live review Status=%q, want reviewing", liveReview.Status)
	}

	// Issue 1002 review child must be terminal (Kind=completed, Status=success)
	// so visibleRunForIssueGroup keeps the parent badge as the parent's own status.
	doneReview := byIssue(doneIssue, true)
	if doneReview == nil {
		t.Fatalf("expected completed review child for issue %d, got %#v", doneIssue, firstRuns)
	}
	if doneReview.Kind != "completed" {
		t.Fatalf("completed review Kind=%q, want completed", doneReview.Kind)
	}
	if doneReview.Status != "success" {
		t.Fatalf("completed review Status=%q, want success", doneReview.Status)
	}

	// Parent rows must carry reviewCount/reviewVerdict metadata
	// (the aggregation logic from slice #1525) so the visible run is the
	// canonical parent even when the review is live.
	for _, issue := range []int{liveIssue, doneIssue} {
		parent := byIssue(issue, false)
		if parent == nil {
			t.Fatalf("expected parent row for issue %d", issue)
		}
		if parent.ReviewCount == 0 {
			t.Fatalf("issue %d parent ReviewCount=0, want >=1", issue)
		}
	}

	// Drive the visible-run helper against the second-compute row set so
	// the determinism check lands on the actual badge output, not just
	// the intermediate row state. Each issue's parent and review rows are
	// marshaled into JS as plain objects with the canonical event-log
	// shape (Kind, Status, IssueNumber, Review, RunID, BatchKey).
	marshal := func(r portalRun) string {
		return "{ key: " + strconv.Quote(r.Key) +
			", kind: " + strconv.Quote(r.Kind) +
			", status: " + strconv.Quote(r.Status) +
			", issueNumber: " + strconv.Itoa(r.IssueNumber) +
			", review: " + strconv.FormatBool(r.Review) +
			", runId: " + strconv.Quote(r.RunID) +
			", batchKey: " + strconv.Quote(r.BatchKey) +
			" }"
	}

	for _, issue := range []int{liveIssue, doneIssue} {
		var jsRows []string
		for _, run := range secondRuns {
			if run.IssueNumber == issue {
				jsRows = append(jsRows, marshal(run))
			}
		}
		if len(jsRows) == 0 {
			t.Fatalf("issue %d missing from second compute", issue)
		}
		js := `const runs = [` + strings.Join(jsRows, ",") + `];
const visible = visibleRunForIssueGroup(` + strconv.Itoa(issue) + `, runs);
if (!visible) throw new Error('expected visible row for issue ` + strconv.Itoa(issue) + `');
if (visible.key !== 'impl-` + strconv.Itoa(issue) + `') throw new Error('expected canonical parent key impl-` + strconv.Itoa(issue) + `, got ' + JSON.stringify(visible.key));
if (visible.runId !== 'impl-` + strconv.Itoa(issue) + `') throw new Error('expected canonical parent runId, got ' + JSON.stringify(visible.runId));
if (visible.review) throw new Error('expected review=false on canonical visible parent, got ' + JSON.stringify(visible.review));
if (visible.batchKey !== 'impl-` + strconv.Itoa(issue) + `') throw new Error('expected canonical parent batchKey, got ' + JSON.stringify(visible.batchKey));
` + badgeAssertion(issue)
		runPortalHTMLScript(t, js)
	}
}

// TestPortal_DiscoverActiveRuns_ReviewIdentityFromBatchDirPreservesIssueIdentity
// pins the residual #1615 regression: when a live review batch has no
// parseable `runs/<run-id>` child folder (so reviewRunIdentityForBatchDir yields
// nothing), the portal must still recover the review's issue number from the
// active review identity itself — the resolved runID, the batchID, or the
// batches-index instance name, all of which encode `<issue>-PR<number>`.
// Without this, the review row falls through with IssueNumber=0 and renders as
// a standalone passthrough row instead of grouping under the canonical issue
// row.
func TestPortal_DiscoverActiveRuns_ReviewIdentityFromBatchDirPreservesIssueIdentity(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pri")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// The batch directory itself encodes the issue+PR (matching the live
	// shape observed in the wild, e.g. `d42a-260701172902-137-PR1614`).
	// There is intentionally NO `runs/<...>-137-PR1614/` child folder, so
	// the folder-based recovery path cannot fire.
	const issueNumber = 137
	const batchID = "d42a-260701172902-137-PR1614"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))

	startedAt := time.Now().Add(-4 * time.Minute)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   batchID,
		RunKind:   "review",
		PR:        intPtr(1614),
		Issues:    []int{},
		CreatedAt: startedAt,
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	layout := paths.NewLayout(nil, repoRoot)
	batchesIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{{
			ID:        batchID,
			Path:      batchDir,
			Kind:      batchindex.KindReview,
			Status:    batchindex.StatusActive,
			CreatedAt: startedAt,
			PR:        1614,
		}},
	}
	if err := batchesIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	// No events: the recovery must rely on the encoded identity, not on a
	// replayed review run.started event.
	active, err := (&portalRunsView{}).discoverActiveRuns(repoRoot, map[string][]portalEvent{})
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d: %#v", len(active), active)
	}
	if got := active[0].IssueNumber; got != issueNumber {
		t.Fatalf("expected active IssueNumber %d recovered from the review identity, got %d", issueNumber, got)
	}
	if got := active[0].PRNumber; got != 1614 {
		t.Fatalf("expected active PRNumber 1614, got %d", got)
	}
	if got := active[0].IssueNumbers; len(got) != 1 || got[0] != issueNumber {
		t.Fatalf("expected active IssueNumbers [%d], got %#v", issueNumber, got)
	}
}

func badgeAssertion(issue int) string {
	if issue == 1001 {
		return `if (visible.status !== 'reviewing') throw new Error('issue 1001 visible badge must flip to reviewing (live review in group, AC1), got ' + JSON.stringify(visible.status));
if (visible.liveReview !== true) throw new Error('issue 1001 expected liveReview=true flag, got ' + JSON.stringify(visible.liveReview));
`
	}
	return `if (visible.status !== 'success') throw new Error('issue ` + strconv.Itoa(issue) + ` visible badge must revert to parent status (no live review, AC3), got ' + JSON.stringify(visible.status));
if (visible.liveReview) throw new Error('issue ` + strconv.Itoa(issue) + ` expected liveReview=false flag, got ' + JSON.stringify(visible.liveReview));
`
}
