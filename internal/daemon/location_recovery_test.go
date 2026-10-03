package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

func TestLocations_RecoveryPublicIDCollisionProtectsLiveRun(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	dead := batchindex.Batch{ID: "dead-public", Path: layout.BatchDir("live-public"), Status: batchindex.StatusActive, Issues: []int{42}, Kind: batchindex.KindIssue}
	live := batchindex.Batch{ID: "live-public", Path: filepath.Join(layout.SandmanDir, "relocated", "live-physical"), Status: batchindex.StatusActive, Issues: []int{42}, Kind: batchindex.KindIssue}
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{dead, live}}
	started := time.Now().Add(-time.Minute)
	for _, b := range idx.Batches {
		if err := os.MkdirAll(b.Path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := WriteManifest(b.Path, BatchManifest{BatchId: b.ID, Issues: b.Issues, CreatedAt: started}); err != nil {
			t.Fatal(err)
		}
		if err := WriteRunManifest(b.Path, b.ID+"-row", batchindex.RunManifest{RunID: b.ID + "-row", BatchID: b.ID, Issue: 42, Status: batchindex.RunManifestStatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	control := NewControlSocket(live.Path, NewBroadcaster())
	if err := control.Start(); err != nil {
		t.Fatal(err)
	}
	defer control.Stop()
	list := []events.Event{
		{Type: "run.started", RunID: dead.ID + "-row", Issue: 42, Timestamp: started, Payload: map[string]any{"batch_id": dead.ID}},
		{Type: "run.started", RunID: live.ID + "-row", Issue: 42, Timestamp: started, Payload: map[string]any{"batch_id": live.ID}},
	}
	log := &recordingEventLog{}
	n, _, err := RecoverStaleRuns(layout.SandmanDir, list, log)
	if err != nil || n != 1 {
		t.Fatalf("recovery = %d, %v", n, err)
	}
	m, err := ReadRunManifest(dead.Path, dead.ID+"-row")
	if err != nil || m.Status != batchindex.RunManifestStatusAborted {
		t.Fatalf("dead row = %+v,%v", m, err)
	}
	m, err = ReadRunManifest(live.Path, live.ID+"-row")
	if err != nil || m.Status != batchindex.RunManifestStatusActive {
		t.Fatalf("live row mutated = %+v,%v", m, err)
	}
}

func TestLocations_RecoveryUpdatesPromptAndReviewPhysicalManifests(t *testing.T) {
	for _, kind := range []batchindex.Kind{batchindex.KindPromptOnly, batchindex.KindReview} {
		t.Run(string(kind), func(t *testing.T) {
			layout := paths.NewLayout(nil, t.TempDir())
			b := batchindex.Batch{ID: "public", Path: filepath.Join(layout.SandmanDir, "moved", string(kind)), Status: batchindex.StatusActive, Kind: kind}
			if err := os.MkdirAll(b.Path, 0755); err != nil {
				t.Fatal(err)
			}
			started := time.Now().Add(-time.Minute)
			if err := WriteManifest(b.Path, BatchManifest{BatchId: b.ID, RunKind: string(kind), CreatedAt: started}); err != nil {
				t.Fatal(err)
			}
			if err := WriteRunManifest(b.Path, "row", batchindex.RunManifest{RunID: "row", BatchID: b.ID, Kind: kind, Status: batchindex.RunManifestStatusActive}); err != nil {
				t.Fatal(err)
			}
			if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{b}}).Save(layout.BatchesIndexPath); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"batch_id": b.ID, "run_kind": string(kind)}
			if kind == batchindex.KindReview {
				payload["review"] = true
				payload["pr_number"] = 99
			}
			list := []events.Event{{Type: "run.started", RunID: "row", Timestamp: started, Payload: payload}}
			n, _, err := RecoverStaleRuns(layout.SandmanDir, list, &recordingEventLog{})
			if err != nil || n != 1 {
				t.Fatalf("recovery = %d,%v", n, err)
			}
			m, err := ReadRunManifest(b.Path, "row")
			if err != nil || m.Status != batchindex.RunManifestStatusAborted {
				t.Fatalf("physical manifest = %+v,%v", m, err)
			}
		})
	}
}
