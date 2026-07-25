//go:build !linux

package cmd

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestDiscoverPortalInstances_LongPathBindsAndDials(t *testing.T) {
	repoRoot := testenv.MkdirShort(t, "sm-portal-")
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")

	// Build a batch dir long enough that batch.sock exceeds the sun_path limit.
	batchDir := batchesDir
	for len(daemon.BatchSocketPath(batchDir)) <= 108 {
		batchDir = filepath.Join(batchDir, strings.Repeat("long-path-segment", 4))
	}
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Start a live control socket at the effective short path.
	ctl := daemon.NewControlSocket(batchDir, daemon.NewBroadcaster())
	if err := ctl.Start(); err != nil {
		t.Fatalf("Start control socket: %v", err)
	}
	defer ctl.Stop()

	// Register an active entry in the batches index.
	layout := paths.NewLayout(nil, repoRoot)
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{
				ID:        "long-batch-1",
				Path:      batchDir,
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: time.Now().Add(-time.Minute),
			},
		},
	}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		t.Fatalf("discoverPortalInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}

	inst := instances[0]
	if inst.Dir != batchDir {
		t.Errorf("Dir = %q, want %q", inst.Dir, batchDir)
	}

	// The effective SocketPath must be a live Unix socket.
	conn, err := net.DialTimeout("unix", inst.SocketPath, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("dial SocketPath: %v", err)
	}
	conn.Close()
}
