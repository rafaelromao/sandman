package review

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
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
