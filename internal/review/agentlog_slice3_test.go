package review

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
)

// TestRegisterPendingReview_StoresRunLogPath pins B10: the launch
// goroutine's call to registerPendingReview stores the run-log path on
// the entry so the next tick's promotePendingReviews step can grep it
// for bot-authored bodies. The path defaults to
// filepath.Dir(reviewState)+"/run.log" when the caller passes the
// empty string (the rehydration default).
func TestRegisterPendingReview_StoresRunLogPath(t *testing.T) {
	gh := &fakeGH{}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC) }

	statePath := filepath.Join(d.BaseDir, "fake-run", "review-state.json")
	customLog := filepath.Join(d.BaseDir, "fake-run", "run.log")

	d.registerPendingReview(7, "trigger-1", d.now(), statePath, customLog, nil)

	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	entries := d.pendingReviews[7]
	if len(entries) != 1 {
		t.Fatalf("expected 1 pending entry, got %d", len(entries))
	}
	if entries[0].runLogPath != customLog {
		t.Errorf("runLogPath: got %q, want %q", entries[0].runLogPath, customLog)
	}
}

// TestRegisterPendingReview_DefaultsRunLogPath pins the rehydration
// default: when the caller passes an empty runLogPath, the field is
// populated with filepath.Dir(reviewState)+"/run.log" so the
// bounded-retry grace path always has a log to read.
func TestRegisterPendingReview_DefaultsRunLogPath(t *testing.T) {
	gh := &fakeGH{}
	runner := &capturedRequest{}
	d, _, _ := newDaemonForTest(t, gh, runner, &config.Config{
		DefaultReviewAgent: "opencode",
		DefaultReviewModel: "opencode/foo",
	})
	d.Clock = func() time.Time { return time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC) }

	statePath := filepath.Join(d.BaseDir, "fake-run", "review-state.json")
	d.registerPendingReview(7, "trigger-1", d.now(), statePath, "", nil)

	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	entries := d.pendingReviews[7]
	if len(entries) != 1 {
		t.Fatalf("expected 1 pending entry, got %d", len(entries))
	}
	want := filepath.Join(filepath.Dir(statePath), "run.log")
	if entries[0].runLogPath != want {
		t.Errorf("runLogPath: got %q, want default %q", entries[0].runLogPath, want)
	}
}
