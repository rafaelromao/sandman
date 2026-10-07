package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestLocations_RelocatedBatchAttachPortalArchiveAndRecovery(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(nil, root)
	if err := os.WriteFile(filepath.Join(root, ".git"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(".sandman", "relocated", strings.Repeat("long-", 25))
	b := batchindex.Batch{ID: "public", Path: physical, Status: batchindex.StatusActive, Kind: batchindex.KindIssue, Issues: []int{42, 43}}
	loc := b.Location(layout)
	if err := os.MkdirAll(loc.Dir, 0755); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Minute)
	if err := daemon.WriteManifest(loc.Dir, daemon.BatchManifest{BatchId: b.ID, Issues: b.Issues, CreatedAt: started}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id     string
		issue  int
		status batchindex.RunManifestStatus
	}{{"row-42", 42, batchindex.RunManifestStatusSuccess}, {"row-43", 43, batchindex.RunManifestStatusActive}} {
		if err := daemon.WriteRunManifest(loc.Dir, row.id, batchindex.RunManifest{RunID: row.id, BatchID: b.ID, Issue: row.issue, Kind: b.Kind, Status: row.status, CreatedAt: started}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(loc.Run(row.id).LogPath(), []byte("same-physical-log\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{b}}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	log := &events.JSONLLogger{Path: layout.EventsLogPath}
	list := []events.Event{
		{Type: "run.started", RunID: "row-42", Issue: 42, Timestamp: started, Payload: map[string]any{"batch_id": b.ID}},
		{Type: "run.finished", RunID: "row-42", Issue: 42, Timestamp: started.Add(time.Second), Payload: map[string]any{"status": "success"}},
		{Type: "run.started", RunID: "row-43", Issue: 43, Timestamp: started, Payload: map[string]any{"batch_id": b.ID}},
	}
	for _, e := range list {
		if err := log.Log(e); err != nil {
			t.Fatal(err)
		}
	}
	control := daemon.NewControlSocket(loc.Dir, daemon.NewBroadcaster())
	if err := control.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Stop() })
	t.Chdir(t.TempDir())
	if control.Path() == filepath.Join(loc.Dir, "batch.sock") {
		t.Fatal("fixture needs shortened socket")
	}
	sock, err := findDaemonSocket(root)
	if err != nil || sock != control.Path() {
		t.Fatalf("attach = %q, %v, want %q", sock, err, control.Path())
	}
	rows, err := (&portalRunsView{}).compute(root, log)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.RunID == "row-43" && (row.RunDir != loc.Run("row-43").Dir || row.LogPath != loc.Run("row-43").LogPath()) {
			t.Fatalf("Portal = %+v", row)
		}
	}
	if _, err := archivePortalRunHandler(root, "row-42"); err != nil {
		t.Fatal(err)
	}
	if err := control.Stop(); err != nil {
		t.Fatal(err)
	}
	n, _, err := daemon.RecoverStaleRuns(layout.SandmanDir, list, log)
	if err != nil || n != 1 {
		t.Fatalf("recovery = %d, %v", n, err)
	}
	m, err := daemon.ReadRunManifest(loc.Dir, "row-43")
	if err != nil || m.Status != batchindex.RunManifestStatusAborted {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
}
