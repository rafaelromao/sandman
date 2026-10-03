package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestRecoverStaleRuns_EventLifecycleSurvivesSnapshotDivergence(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "aborted", "cancelled", "queued", "blocked", "running"} {
		t.Run(outcome, func(t *testing.T) {
			baseDir := t.TempDir()
			batchDir := filepath.Join(baseDir, "batches", "dead")
			created := time.Now().UTC().Add(-time.Minute)
			writeManifestFile(t, batchDir, BatchManifest{Issues: []int{42}, CreatedAt: created})
			snapshot := batchindex.RunManifestStatusActive
			if outcome == "running" {
				snapshot = batchindex.RunManifestStatusSuccess
			}
			if err := WriteRunManifest(batchDir, "row", batchindex.RunManifest{Issue: 42, Status: snapshot}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(baseDir, "events.jsonl")
			log := &events.JSONLLogger{Path: path}
			started := created.Add(time.Second)
			if outcome != "queued" {
				if err := log.Log(events.Event{Type: "run.started", RunID: "row", Issue: 42, Timestamp: started, Payload: map[string]any{"batch_id": "dead"}}); err != nil {
					t.Fatal(err)
				}
			}
			if outcome != "running" {
				kind := "run.finished"
				if outcome != "success" && outcome != "failure" {
					kind = "run." + outcome
				}
				if err := log.Log(events.Event{Type: kind, RunID: "row", Issue: 42, Timestamp: started.Add(time.Second), Payload: map[string]any{"status": outcome, "batch_id": "dead", "terminal_placeholder": outcome == "queued"}}); err != nil {
					t.Fatal(err)
				}
			}
			for pass := 0; pass < 2; pass++ {
				fresh := &events.JSONLLogger{Path: path}
				list, err := fresh.Read()
				if err != nil {
					t.Fatal(err)
				}
				recovered, _, err := RecoverStaleRuns(baseDir, list, fresh)
				wantRecovered := 0
				if outcome == "running" && pass == 0 {
					wantRecovered = 1
				}
				if err != nil || recovered != wantRecovered {
					t.Fatalf("pass %d recovery=%d, %v; want %d", pass, recovered, err, wantRecovered)
				}
				states, err := events.ReadRunStates(fresh)
				if err != nil {
					t.Fatal(err)
				}
				want := outcome
				if outcome == "running" || outcome == "cancelled" {
					want = "aborted"
				}
				if states["row"].Status() != want || !states["row"].IsTerminal() {
					t.Fatalf("pass %d state=%+v; want terminal %s", pass, states["row"], want)
				}
				// A restart with no artifact cannot revise an event outcome.
				if err := os.RemoveAll(batchDir); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRecoverStaleRuns_DiagnosticEventsDoNotAuthorizeCompletion(t *testing.T) {
	baseDir := t.TempDir()
	batchDir := filepath.Join(baseDir, "batches", "dead")
	writeManifestFile(t, batchDir, BatchManifest{Issues: []int{42}})
	if err := WriteRunManifest(batchDir, "row", batchindex.RunManifest{Issue: 42, Status: batchindex.RunManifestStatusSuccess}); err != nil {
		t.Fatal(err)
	}
	log := &events.JSONLLogger{Path: filepath.Join(baseDir, "events.jsonl")}
	if err := log.Log(events.Event{Type: "run.warning", RunID: "row", Issue: 42, Payload: map[string]any{"batch_id": "dead"}}); err != nil {
		t.Fatal(err)
	}
	list, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	recovered, _, err := RecoverStaleRuns(baseDir, list, log)
	if err != nil || recovered != 0 {
		t.Fatalf("diagnostics invented completion: %d, %v", recovered, err)
	}
	states, err := events.ReadRunStates(log)
	if err != nil {
		t.Fatal(err)
	}
	if states["row"].IsTerminal() {
		t.Fatal("unknown lifecycle became terminal")
	}
}
