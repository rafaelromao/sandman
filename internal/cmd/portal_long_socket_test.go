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
	"github.com/rafaelromao/sandman/internal/socketpath"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// longPathDir returns a directory under repoRoot/<batches...> whose
// joined <dir>/batch.sock logical path exceeds the host sun_path limit
// (104) so the resolver maps it to a short /tmp filesystem path on
// every platform. The directory itself is created.
func longPathDir(t *testing.T, repoRoot string) string {
	t.Helper()
	batchesDir := filepath.Join(repoRoot, ".sandman", "batches")
	batchDir := batchesDir
	for {
		logical := filepath.Join(batchDir, "batch.sock")
		if len(logical) > 104 && socketpath.Path(logical) != logical {
			break
		}
		batchDir = filepath.Join(batchDir, strings.Repeat("long-path-segment-", 4))
	}
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatalf("mkdir long batch dir: %v", err)
	}
	return batchDir
}

func TestDiscoverPortalInstances_LongPathBindsAndDials(t *testing.T) {
	repoRoot := testenv.MkdirShort(t, "sm-portal-")
	batchDir := longPathDir(t, repoRoot)

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

	// The effective SocketPath must be a live Unix socket (liveness
	// probe, not just bind-time success).
	conn, err := net.DialTimeout("unix", inst.SocketPath, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("dial SocketPath: %v", err)
	}
	conn.Close()

	// The effective path must be the short /tmp filesystem path; the
	// resolver must not have leaked the long logical path or any
	// abstract-socket name into the discovery result.
	if !strings.HasPrefix(inst.SocketPath, "/tmp/") {
		t.Errorf("SocketPath = %q, want /tmp/ prefix (effective path)", inst.SocketPath)
	}
	if strings.HasPrefix(inst.SocketPath, "@") {
		t.Errorf("SocketPath = %q, must not be an abstract-socket name", inst.SocketPath)
	}
}

func TestDiscoverPortalInstances_LongPathExcludesDeadBatch(t *testing.T) {
	repoRoot := testenv.MkdirShort(t, "sm-portal-")
	batchDir := longPathDir(t, repoRoot)

	// No live socket here — the batch should be considered dead and
	// not appear in the discovery result, even when its batch.sock
	// logical path is overlong.
	layout := paths.NewLayout(nil, repoRoot)
	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{
				ID:        "long-batch-dead",
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
	for _, inst := range instances {
		if inst.Dir == batchDir {
			t.Errorf("dead long-path batch %q must not appear in discovery", batchDir)
		}
	}
}
