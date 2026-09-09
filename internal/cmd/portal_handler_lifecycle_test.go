package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestRunPortalServerWaitsForStaleCleanupOnShutdown(t *testing.T) {
	previous := portalStaleCleaner
	t.Cleanup(func() { portalStaleCleaner = previous })

	started := make(chan struct{})
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	portalStaleCleaner = func(string) error {
		close(started)
		<-release
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		result <- runPortalServer(ctx, t.TempDir(), 0, "127.0.0.1", &out)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stale cleanup did not start")
	}
	cancel()
	select {
	case err := <-result:
		t.Fatalf("portal server shutdown returned before stale cleanup completed: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	close(release)
	released = true
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runPortalServer returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("portal server shutdown did not return after stale cleanup completed")
	}
}
