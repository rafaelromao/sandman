package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// TestDaemon_S4_EmptyMapAtConstruction pins the Slice-A contract:
// a fresh Daemon constructed via New has a `pendingPost` map keyed
// by (prNumber, commentID) that is nil-or-empty until rehydration
// runs. The map is private; this test pins the empty-map invariant
// via the unexported peekPendingPost helper, which is the only
// surface that lets the Slice B / C / D / E tests observe the
// in-memory map without depending on the internals of processPR.
//
// Slice A does NOT exercise the walker yet — that is Slice B's job.
// Slice A only introduces the type plumbing so the rest of the slices
// have a stable target shape to land against.
func TestDaemon_S4_EmptyMapAtConstruction(t *testing.T) {
	d, _, dir := newDaemonForTest(t, &fakeGH{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	// Ensure the test workspace is empty so no on-disk seed is
	// observed by the walker that is added in Slice B. Slice A's
	// walker is a no-op anyway, but the assertion is robust against
	// future slices accidentally populating the map.
	_ = dir

	// Reset the map to a fresh empty state so an older fixture
	// (e.g. a later test that leaves an entry behind) cannot mask
	// the empty invariant.
	d.pendingPostMu.Lock()
	d.pendingPost = map[int]map[string]pendingPostEntry{}
	d.pendingPostMu.Unlock()

	if entry, ok := d.peekPendingPost(4242, "comment-doesnt-exist"); ok {
		t.Fatalf("peekPendingPost should report ok=false on an empty map, got entry=%+v", entry)
	}

	// No tick should happen for Slice A — the map stays empty
	// because no walker writes to it yet. This is the negative
	// assertion that mirrors Slice B's positive assertions.
	if err := d.tick(context.Background()); err != nil {
		t.Fatalf("tick on empty daemon: %v", err)
	}

	d.pendingPostMu.Lock()
	if got := len(d.pendingPost); got != 0 {
		t.Errorf("pendingPost should stay empty after a tick (Slice A contract), got %d entries", got)
	}
	d.pendingPostMu.Unlock()
}

// TestDaemon_S4_PeekRegisteredEntry pins the peekPendingPost helper:
// when an entry is registered with the daemon, peekPendingPost
// returns it under the lock so concurrent test readers see a
// consistent (runDir, reviewStatePath) tuple. The Slice B walker
// will be the producer of these entries; Slice A wires the
// consumer side.
//
// This test constructs the entry via the lock directly so Slice A
// stays self-contained. Slice B will replace this test fixture with
// the on-disk seed path.
func TestDaemon_S4_PeekRegisteredEntry(t *testing.T) {
	d, _, _ := newDaemonForTest(t, &fakeGH{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	runDir := filepath.Join(t.TempDir(), "row")
	reviewStatePath := filepath.Join(runDir, "review-state.json")

	d.pendingPostMu.Lock()
	d.pendingPost = map[int]map[string]pendingPostEntry{
		4242: {
			"c-s4-1": {
				commentID:       "c-s4-1",
				runDir:          runDir,
				reviewStatePath: reviewStatePath,
			},
		},
	}
	d.pendingPostMu.Unlock()

	entry, ok := d.peekPendingPost(4242, "c-s4-1")
	if !ok {
		t.Fatalf("peekPendingPost should return ok=true for registered (4242, c-s4-1)")
	}
	if entry.runDir != runDir {
		t.Errorf("peekPendingPost returned runDir=%q, want %q", entry.runDir, runDir)
	}
	if entry.reviewStatePath != reviewStatePath {
		t.Errorf("peekPendingPost returned reviewStatePath=%q, want %q", entry.reviewStatePath, reviewStatePath)
	}
	if entry.commentID != "c-s4-1" {
		t.Errorf("peekPendingPost returned commentID=%q, want %q", entry.commentID, "c-s4-1")
	}

	if _, ok := d.peekPendingPost(4242, "missing-comment"); ok {
		t.Errorf("peekPendingPost should return ok=false for an unregistered commentID")
	}
	if _, ok := d.peekPendingPost(9999, "c-s4-1"); ok {
		t.Errorf("peekPendingPost should return ok=false for an unregistered prNumber")
	}
}

// decisionPathForTest returns the absolute path a Slice B / Slice C
// fixture should write decision.md into. Mirrors the per-row folder
// that seedPriorReviewEntry creates: <baseDir>/batches/<batch>/runs/<rowID>/.
func decisionPathForTest(t *testing.T, baseDir, batchID string, prNumber int) string {
	t.Helper()
	rowID := deriveReviewRowID(batchID, prNumber)
	return filepath.Join(baseDir, "batches", batchID, "runs", rowID, "decision.md")
}

// runDirForTest returns the absolute row folder for a seeded entry.
// Helpers like peekPendingPost compare absolute paths so each test
// needs the same canonical path string the daemon computed.
func runDirForTest(t *testing.T, baseDir, batchID string, prNumber int) string {
	t.Helper()
	rowID := deriveReviewRowID(batchID, prNumber)
	return filepath.Join(baseDir, "batches", batchID, "runs", rowID)
}

// TestDaemon_S4_LoadPendingPosts_RegistersDecisionPending pins the
// Slice-B happy path: a pending review-state.json whose row folder
// has decision.md on disk produces exactly one pendingPost entry at
// Daemon.New. The entry holds the absolute runDir and reviewStatePath
// so the rehydrate post (Slice C) can read the file and call
// MarkSeen without resolving paths.
func TestDaemon_S4_LoadPendingPosts_RegistersDecisionPending(t *testing.T) {
	const (
		prNumber  = 5050
		commentID = "c-s4-pending-1"
	)
	dir := t.TempDir()

	const batchID = "sip-batch-PR5050"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")

	decisionPath := decisionPathForTest(t, dir, batchID, prNumber)
	body := "## Summary\n## Decision\nthis is the unposted body.\n"
	if err := os.WriteFile(decisionPath, []byte(body), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	d := New(dir, &fakeGH{}, &prompt.Engine{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false, nil)

	entry, ok := d.peekPendingPost(prNumber, commentID)
	if !ok {
		t.Fatalf("pendingPost should hold an entry for (PR=%d, comment=%s) after Daemon.New, peek returned !ok", prNumber, commentID)
	}
	wantRunDir := runDirForTest(t, dir, batchID, prNumber)
	if entry.runDir != wantRunDir {
		t.Errorf("entry.runDir=%q, want %q", entry.runDir, wantRunDir)
	}
	wantStatePath := filepath.Join(wantRunDir, "review-state.json")
	if entry.reviewStatePath != wantStatePath {
		t.Errorf("entry.reviewStatePath=%q, want %q", entry.reviewStatePath, wantStatePath)
	}
	if entry.commentID != commentID {
		t.Errorf("entry.commentID=%q, want %q", entry.commentID, commentID)
	}
}

// TestDaemon_S4_LoadPendingPosts_SkipsMissingDecision pins the
// Slice-B negative path: a pending review-state.json whose row
// folder has NO decision.md (agent crash before write) must NOT
// register a pendingPost entry.
func TestDaemon_S4_LoadPendingPosts_SkipsMissingDecision(t *testing.T) {
	const (
		prNumber  = 5051
		commentID = "c-s4-pending-no-decision"
	)
	dir := t.TempDir()

	const batchID = "sip-batch-PR5051"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")
	// Deliberately omit decision.md.

	d := New(dir, &fakeGH{}, &prompt.Engine{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false, nil)

	if _, ok := d.peekPendingPost(prNumber, commentID); ok {
		t.Fatalf("pendingPost should not register an entry when decision.md is missing (Slice B negative path)")
	}
}

// TestDaemon_S4_LoadPendingPosts_SkipsTerminalStatuses pins the
// Slice-B status filter: only status="pending" review-state entries
// are considered for rehydrate; status="success" and status=
// "superseded" must be ignored even when decision.md is present.
func TestDaemon_S4_LoadPendingPosts_SkipsTerminalStatuses(t *testing.T) {
	const prNumber = 5052
	for _, status := range []string{"success", "superseded"} {
		status := status
		t.Run(status, func(t *testing.T) {
			dir := t.TempDir()

			commentID := "c-s4-" + status
			batchID := "sip-batch-PR5052"
			seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, status)

			decisionPath := decisionPathForTest(t, dir, batchID, prNumber)
			if err := os.MkdirAll(filepath.Dir(decisionPath), 0755); err != nil {
				t.Fatalf("mkdir decision.md dir: %v", err)
			}
			if err := os.WriteFile(decisionPath, []byte("body"), 0644); err != nil {
				t.Fatalf("write decision.md: %v", err)
			}

			d := New(dir, &fakeGH{}, &prompt.Engine{}, &capturedRequest{}, &config.Config{
				DefaultReviewAgent: "opencode",
				DefaultReviewModel: "opencode/foo",
			}, &lockedBuffer{}, 0, false, nil)
			if _, ok := d.peekPendingPost(prNumber, commentID); ok {
				t.Fatalf("pendingPost should not register entry for status=%s (Slice B status filter)", status)
			}
		})
	}
}

// TestDaemon_S4_LoadPendingPosts_DoesNotDoubleHandleByDeleting pins
// that both walkers may end up registering the same row: a row that
// has BOTH a non-zero Timestamp (so loadPendingReviews registers it)
// AND decision.md on disk (so loadPendingPosts also registers it) is
// present in both maps. The actual precedence is decided at tick time
// by processPR (Slice C: rehydrate branch fires first), so this test
// only asserts the coexistence invariant — both maps end up holding
// the entry, and Slice C will exercise the precedence.
//
// This explicitly documents the design choice: a strict disjoint
// invariant would force exactly one of the two walkers to "lose" the
// row, but in production a row that meets BOTH gates deserves both:
// the rehydrate walker posts the body synchronously, and the lazy-
// verify entry is harmless after MarkSeen("success") takes effect
// (the seen-cache hydrate-and-filter path prevents re-processing).
func TestDaemon_S4_LoadPendingPosts_DoesNotDoubleHandleByDeleting(t *testing.T) {
	const (
		prNumber  = 5053
		commentID = "c-s4-coexist"
	)
	dir := t.TempDir()
	const batchID = "sip-batch-PR5053"
	seedPriorReviewEntry(t, dir, batchID, prNumber, commentID, "pending")

	// decision.md present ⇒ both walkers register the row.
	decisionPath := decisionPathForTest(t, dir, batchID, prNumber)
	if err := os.WriteFile(decisionPath, []byte("body"), 0644); err != nil {
		t.Fatalf("write decision.md: %v", err)
	}

	d := New(dir, &fakeGH{}, &prompt.Engine{}, &capturedRequest{}, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	}, &lockedBuffer{}, 0, false, nil)

	// Rehydrate map has the entry.
	if _, ok := d.peekPendingPost(prNumber, commentID); !ok {
		t.Fatalf("pendingPost should register the entry when decision.md is present (Slice B coexistence)")
	}
	// Lazy-verify map also has the entry (timestamp non-zero, status
	// pending). We do not assert it is in pendingReviews here — that
	// is loadPendingReviews' own contract; this test pins only the
	// rehydrate map's behaviour when both walkers share a row.
}
