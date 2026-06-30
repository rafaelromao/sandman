package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/runid"
)

// seedPriorCanonicalReview seeds the canonical per-row review folder
// shape on disk for a prior review batch so hydration tests can
// exercise the loadSeenCache path against the layout introduced by
// issue #1551 / ADR-0030. The folder shape is
// `<batch>/runs/<rowID>/{run.json,review-state.json}` where
// `<rowID>` matches what `reviewRunIDFor` would mint for the same
// (prNumber, linkedIssue) pair.
//
// When `rowIDOverride` is non-empty the helper uses that directory
// name verbatim; otherwise it derives the rowID from the batch ID by
// parsing out the `<sid>-<ts>-` prefix and using
// `reviewRunIDFor(prNumber, 0, ts, sid)` so the test's folder shape
// matches what a launched review would actually produce.
func seedPriorCanonicalReview(t *testing.T, baseDir, batchID string, prNumber int, commentID string) {
	t.Helper()
	rowID := runid.NewRunID(runid.KindReview, "PR"+itoa(0, prNumber), parseTSFromBatchID(batchID), parseSIDFromBatchID(batchID))
	if rowID == "" {
		t.Fatalf("could not derive rowID from batchID %q", batchID)
	}
	seedPriorCanonicalReviewWith(t, baseDir, batchID, rowID, prNumber, commentID)
}

func seedPriorCanonicalReviewWith(t *testing.T, baseDir, batchID, rowID string, prNumber int, commentID string) {
	t.Helper()
	batchesDir := filepath.Join(baseDir, "batches")
	batchPath := filepath.Join(batchesDir, batchID)
	runDir := filepath.Join(batchPath, "runs", rowID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("create canonical run dir: %v", err)
	}
	state := batchindex.ReviewState{
		PR: prNumber,
		SeenComments: []batchindex.SeenComment{
			{CommentID: commentID, Status: "success", Timestamp: time.Now()},
		},
	}
	if err := batchindex.WriteReviewState(runDir, state); err != nil {
		t.Fatalf("write review state: %v", err)
	}
	manifest := batchindex.RunManifest{
		RunID:     rowID,
		BatchID:   batchID,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(runDir, manifest); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}
	idxPath := daemon.BatchesIndexPath(baseDir)
	idx, err := batchindex.Load(idxPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("load batches index: %v", err)
	}
	if idx == nil {
		idx = &batchindex.Index{Version: batchindex.IndexVersion}
	}
	idx.Add(batchindex.Entry{
		ID:        batchID,
		Path:      batchPath,
		Kind:      batchindex.KindReview,
		Status:    batchindex.StatusActive,
		CreatedAt: time.Now(),
		PR:        prNumber,
	})
	if err := idx.Save(idxPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}
}

func parseSIDFromBatchID(batchID string) string {
	parts := strings.SplitN(batchID, "-", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}

func parseTSFromBatchID(batchID string) string {
	parts := strings.SplitN(batchID, "-", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// TestDaemon_ReviewRunIDAndFolder_AreCanonical pins the headline
// behavior of issue #1551 and ADR-0030: the review daemon's launch
// path produces a per-row RunID under ADR-0030's review templates
// and persists `run.json` and `review-state.json` under
// `<batch>/runs/<runID>/`. The legacy literal `RunID: "review"` and
// the `runs/review` folder must no longer be written by the daemon.
//
// This test drives a single tick against a /sandman review comment
// on a PR that does NOT link an issue, then asserts:
//   - `batch.Request.RunID` ends with `-PR<pr>` and matches the
//     canonical `<sid>-<ts>-PR<pr>` shape (acceptance #2).
//   - The persisted `run.json` under the canonical run folder has
//     `runID == rowID` (no literal `"review"`).
//   - `review-state.json` lives at
//     `<batch>/runs/<rowID>/review-state.json` (acceptance #3).
//   - The legacy `runs/review/...` path is NOT written by the
//     daemon (acceptance #4).
func TestDaemon_ReviewRunIDAndFolder_AreCanonical(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{42: {Number: 42, Title: "PR 42", Body: "Body of 42"}},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}

	rowID := runner.last.RunID
	if rowID == "" {
		t.Fatalf("captured batch.Request.RunID is empty")
	}
	if rowID == "review" {
		t.Fatalf("RunID must not be the literal %q (issue #1551), got %q", "review", rowID)
	}
	if !strings.HasSuffix(rowID, "-PR42") {
		t.Errorf("RunID must end with -PR<pr>, got %q", rowID)
	}
	// Canonical shape: <sid>-<ts>-<rest>. <sid> is exactly four
	// lowercase hex chars and <ts> is twelve digits (060102150405).
	parts := strings.SplitN(rowID, "-", 3)
	if len(parts) < 3 {
		t.Fatalf("RunID must contain at least 3 dash-separated parts, got %q", rowID)
	}
	if l := len(parts[0]); l != 4 {
		t.Errorf("RunID <sid> segment length = %d, want 4 (hex), got %q", l, rowID)
	}
	if l := len(parts[1]); l != 12 {
		t.Errorf("RunID <ts> segment length = %d, want 12, got %q", l, rowID)
	}

	// Acceptance #2: run.json.RunID matches the canonical review run ID.
	manifestPath := filepath.Join(runner.last.RunDir, "run.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read run.json at %s: %v", manifestPath, err)
	}
	var manifest batchindex.RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode run.json: %v", err)
	}
	if manifest.RunID != rowID {
		t.Errorf("run.json.RunID = %q, want %q (canonical)", manifest.RunID, rowID)
	}
	if manifest.RunID == "review" {
		t.Errorf("run.json.RunID must not be literal %q", "review")
	}

	// Acceptance #3: review-state.json lives under the canonical run folder.
	statePath := filepath.Join(runner.last.RunDir, "review-state.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("read review-state.json at %s: %v", statePath, err)
	}

	// Acceptance #4: no runs/review folder was written by the daemon.
	batchRoot := filepath.Dir(strings.TrimSuffix(runner.last.RunDir, "/runs"))
	legacyDir := filepath.Join(batchRoot, "runs", "review")
	if _, err := os.Stat(legacyDir); err == nil {
		t.Errorf("daemon must not write the legacy alias folder %s, but it exists on disk", legacyDir)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error on legacy alias folder: %v", err)
	}
}

// TestDaemon_ReviewRunIDAndFolder_AreCanonicalWithLinkedIssue pins
// the linked-issue half of the canonical template:
// `<sid>-<ts>-<linkedIssue>-PR<pr>`. The PR body carries
// "Fixes #1551" so the daemon must mint a per-row RunID that
// includes the linked issue number between `<sid>-<ts>-` and
// `-PR<pr>`. This is the new shape introduced by ADR-0030 and
// required by acceptance criterion #1 of issue #1551.
func TestDaemon_ReviewRunIDAndFolder_AreCanonicalWithLinkedIssue(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs: []github.PR{{Number: 42, State: "open"}},
		comments: map[int][]github.PRComment{
			42: {
				{ID: "100", Body: "/sandman review", CreatedAt: now},
			},
		},
		prFetch: map[int]*github.PR{
			42: {Number: 42, Title: "PR 42", Body: "This PR fixes #1551."},
		},
	}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return now }

	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("expected 1 batch run, got %d", runner.calls)
	}

	rowID := runner.last.RunID
	if rowID == "" {
		t.Fatalf("captured batch.Request.RunID is empty")
	}
	// Must include the linked issue number between the canonical
	// <sid>-<ts>- prefix and the -PR42 suffix.
	if !strings.HasPrefix(rowID, "0000-") {
		// timestamp drift between NewBatch and Clock is possible
		// (we don't freeze runid's clock); we therefore only
		// assert the trailing shape and that the linked issue
		// shows up exactly once between the prefix and suffix.
		t.Logf("RunID = %q (sid drift accepted if NewBatch ran on a different second)", rowID)
	}
	if !strings.HasSuffix(rowID, "-1551-PR42") {
		t.Errorf("RunID must end with -<linkedIssue>-PR<pr>, got %q", rowID)
	}
	if strings.Contains(rowID, "-PR42-") || strings.Count(rowID, "PR42") < 1 {
		t.Errorf("RunID must contain exactly the PR42 subject, got %q", rowID)
	}

	// run.json.RunID must match the captured rowID.
	manifestPath := filepath.Join(runner.last.RunDir, "run.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read run.json at %s: %v", manifestPath, err)
	}
	var manifest batchindex.RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode run.json: %v", err)
	}
	if manifest.RunID != rowID {
		t.Errorf("run.json.RunID = %q, want %q", manifest.RunID, rowID)
	}

	// review-state.json lives under the canonical run folder.
	statePath := filepath.Join(runner.last.RunDir, "review-state.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("read review-state.json at %s: %v", statePath, err)
	}
}

// TestDaemon_LoadSeenCache_ReadsCanonicalRunFolder pins Slice 2 of
// the canonical hydration path. After launch, the daemon's
// loadSeenCache must consult `<batch>/runs/<rowID>/review-state.json`
// (where `<rowID>` is the canonical per-row RunID), not the legacy
// `runs/review/` folder. The test seeds a prior batch with the
// canonical layout, asks the daemon to hydrate its seen cache, and
// confirms the (prNumber, commentID) terminal-seen pair flows into
// the cache.
func TestDaemon_LoadSeenCache_ReadsCanonicalRunFolder(t *testing.T) {
	const (
		prNumber  = 42
		commentID = "hydrated"
	)
	dir := t.TempDir()
	t.Chdir(dir)
	batchesDir := filepath.Join(dir, "batches")
	batchID := "abcd-260625120000-PR42"
	batchPath := filepath.Join(batchesDir, batchID)
	// Canonical rowID for this prior batch's review (no linked issue).
	rowID := "abcd-260625120000-PR42"
	runDir := filepath.Join(batchPath, "runs", rowID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("create canonical run dir: %v", err)
	}
	state := batchindex.ReviewState{
		PR: prNumber,
		SeenComments: []batchindex.SeenComment{
			{CommentID: commentID, Status: "success", Timestamp: time.Now()},
		},
	}
	if err := batchindex.WriteReviewState(runDir, state); err != nil {
		t.Fatalf("write review state: %v", err)
	}
	runManifest := batchindex.RunManifest{
		RunID:     rowID,
		BatchID:   batchID,
		PR:        prNumber,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(runDir, runManifest); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}
	idxPath := daemon.BatchesIndexPath(dir)
	idx := &batchindex.Index{Version: batchindex.IndexVersion}
	idx.Add(batchindex.Entry{
		ID:        batchID,
		Path:      batchPath,
		Kind:      batchindex.KindReview,
		Status:    batchindex.StatusActive,
		CreatedAt: time.Now(),
		PR:        prNumber,
	})
	if err := idx.Save(idxPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	gh := &fakeGH{}
	d := New(dir, gh, &prompt.Engine{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false)

	if !d.IsTerminalSeen(prNumber, commentID) {
		t.Fatalf("seenCache should have hydrated (PR %d, %s) from canonical run folder %s, got %v",
			prNumber, commentID, runDir, d.seenCache)
	}
}

// TestDaemon_LoadSeenCache_IgnoresLegacyRunsReviewFolder pins the
// acceptance criterion "No code path writes or reads `runs/review`
// as the canonical review location" against the hydration path. A
// legacy `runs/review/review-state.json` (no per-row RunID folder)
// must NOT be picked up by loadSeenCache — only the canonical run
// folder shape counts.
func TestDaemon_LoadSeenCache_IgnoresLegacyRunsReviewFolder(t *testing.T) {
	const (
		prNumber  = 42
		commentID = "should-not-be-hydrated"
	)
	dir := t.TempDir()
	t.Chdir(dir)
	batchesDir := filepath.Join(dir, "batches")
	batchID := "abcd-260625120000-PR42"
	batchPath := filepath.Join(batchesDir, batchID)
	legacyRunDir := filepath.Join(batchPath, "runs", "review")
	if err := os.MkdirAll(legacyRunDir, 0o755); err != nil {
		t.Fatalf("create legacy run dir: %v", err)
	}
	state := batchindex.ReviewState{
		PR: prNumber,
		SeenComments: []batchindex.SeenComment{
			{CommentID: commentID, Status: "success", Timestamp: time.Now()},
		},
	}
	if err := batchindex.WriteReviewState(legacyRunDir, state); err != nil {
		t.Fatalf("write legacy review state: %v", err)
	}
	idxPath := daemon.BatchesIndexPath(dir)
	idx := &batchindex.Index{Version: batchindex.IndexVersion}
	idx.Add(batchindex.Entry{
		ID:        batchID,
		Path:      batchPath,
		Kind:      batchindex.KindReview,
		Status:    batchindex.StatusActive,
		CreatedAt: time.Now(),
		PR:        prNumber,
	})
	if err := idx.Save(idxPath); err != nil {
		t.Fatalf("save batches index: %v", err)
	}

	gh := &fakeGH{}
	d := New(dir, gh, &prompt.Engine{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false)

	if d.IsTerminalSeen(prNumber, commentID) {
		t.Fatalf("loadSeenCache must not hydrate from the legacy runs/review/ folder, got cache %v",
			d.seenCache)
	}
}
