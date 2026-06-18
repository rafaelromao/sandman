package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPortalLauncher_LaunchUsesRequestContext pins the core P0 reliability
// fix from issue #1129: launcher.launch must thread the HTTP request context
// through to startPortalCommand, otherwise a client disconnect leaves the
// spawned subprocess running and strands the handler goroutine.
func TestPortalLauncher_LaunchUsesRequestContext(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	launcher, err := newPortalLauncher(repoRoot)
	if err != nil {
		t.Fatalf("newPortalLauncher: %v", err)
	}

	orig := portalStartCommand
	t.Cleanup(func() { portalStartCommand = orig })

	started := make(chan context.Context, 1)
	portalStartCommand = func(ctx context.Context, repoRoot string, args []string) *portalCommandResult {
		started <- ctx
		<-ctx.Done()
		return &portalCommandResult{Err: ctx.Err(), ExitCode: -1}
	}

	ctx, cancel := context.WithCancel(context.Background())
	launchDone := make(chan error, 1)
	go func() {
		_, err := launcher.launch(ctx, []string{"status"})
		launchDone <- err
	}()

	select {
	case gotCtx := <-started:
		if gotCtx == nil {
			t.Fatal("portalStartCommand received a nil context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("launcher.launch never invoked portalStartCommand")
	}

	cancel()

	select {
	case err := <-launchDone:
		if err == nil {
			t.Fatal("expected launch to return an error after context cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("launcher.launch did not return after context cancellation; request ctx was not threaded through")
	}
}

// TestPortalRunsView_ReviewPayloadIntsReuseCmdPayloadInt proves the portal's
// review helpers delegate to the shared cmd-package payloadInt helper instead
// of carrying their own float64/int/int64-only coercion. The regression signal
// is string payloads: payloadInt accepts them, the old inline switch did not.
func TestPortalRunsView_ReviewPayloadIntsReuseCmdPayloadInt(t *testing.T) {
	v := &portalRunsView{}

	if got := v.reviewPRNumber(map[string]any{"pr_number": "42"}); got != 42 {
		t.Fatalf("reviewPRNumber string coercion = %d, want 42", got)
	}
	if got := v.reviewIssueNumber(map[string]any{"issue_number": "107"}); got != 107 {
		t.Fatalf("reviewIssueNumber string coercion = %d, want 107", got)
	}
	if got := v.reviewPRNumber(map[string]any{"pr_number": "not-a-number"}); got != 0 {
		t.Fatalf("reviewPRNumber invalid string = %d, want 0", got)
	}
	if got := v.reviewIssueNumber(map[string]any{"issue_number": "not-a-number"}); got != 0 {
		t.Fatalf("reviewIssueNumber invalid string = %d, want 0", got)
	}
}
