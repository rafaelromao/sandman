package paths

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocations_EffectiveSocketsKeepArtifactDirectories(t *testing.T) {
	layout := NewLayout(nil, filepath.Join(t.TempDir(), strings.Repeat("long-", 25)))
	b := layout.LocateBatch("public", filepath.Join(layout.SandmanDir, "batches", "physical+1"))
	r := b.Run("row-43")
	if err := os.MkdirAll(r.Dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, socket := range []string{b.SocketPath(), r.SocketPath(), layout.ReviewSocketPath()} {
		if !strings.HasPrefix(socket, "/tmp/sandman-") {
			t.Fatalf("not shortened: %q", socket)
		}
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
	if r.Dir != filepath.Join(b.Dir, "runs", "row-43") || r.LogPath() != filepath.Join(r.Dir, "run.log") || r.ManifestPath() != filepath.Join(r.Dir, "run.json") {
		t.Fatalf("artifact paths changed: %+v", r)
	}
	if b.ID != "public" || filepath.Base(b.Dir) != "physical+1" {
		t.Fatalf("identity conflated: %+v", b)
	}
}
