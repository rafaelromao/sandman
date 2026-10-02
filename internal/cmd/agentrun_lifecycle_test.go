package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

func recordTerminalLifecycle(t *testing.T, root string, ids ...string) {
	t.Helper()
	path := filepath.Join(root, ".sandman", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	log := &events.JSONLLogger{Path: path}
	for _, id := range ids {
		if err := log.Log(events.Event{Type: "run.finished", RunID: id, Timestamp: time.Now().UTC(), Payload: map[string]any{"status": "success"}}); err != nil {
			t.Fatal(err)
		}
	}
}

func terminalArchiveDeps(t *testing.T, root string, ids ...string) Dependencies {
	t.Helper()
	recordTerminalLifecycle(t, root, ids...)
	deps := newTestDeps(t)
	deps.EventLog = &events.JSONLLogger{Path: filepath.Join(root, ".sandman", "events.jsonl")}
	return deps
}

// The lifecycle survives stale snapshots, archive relocation and fresh readers.
func TestAgentRunLifecycle_ArchiveRestart(t *testing.T) {
	root := newSandmanDir(t)
	t.Chdir(root)
	id := "260618113825-abcd-42"
	batchDir := filepath.Join(root, ".sandman", "batches", id)
	if err := daemon.WriteRunManifest(batchDir, id, batchindex.RunManifest{
		RunID: id, BatchID: id, Issue: 42, Kind: batchindex.KindIssue,
		Status: batchindex.RunManifestStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	writeBatchIndexForArchive(t, root, []batchindex.Batch{{
		ID: id, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive,
		Issues: []int{42}, Runs: []batchindex.RunRecord{{RunID: id, Status: batchindex.RunRecordStatusActive}},
	}})
	logPath := filepath.Join(root, ".sandman", "events.jsonl")
	log := &events.JSONLLogger{Path: logPath}
	start := time.Now().UTC().Add(-time.Minute)
	for _, event := range []events.Event{
		{Type: "run.started", RunID: id, Issue: 42, Timestamp: start, Payload: map[string]any{"batch_id": id}},
		{Type: "run.finished", RunID: id, Issue: 42, Timestamp: start.Add(time.Second), Payload: map[string]any{"status": "success"}},
	} {
		if err := log.Log(event); err != nil {
			t.Fatal(err)
		}
	}
	assertReaders := func() {
		t.Helper()
		fresh := &events.JSONLLogger{Path: logPath}
		var status, history bytes.Buffer
		s := NewStatusCmd(fresh)
		s.SetOut(&status)
		if err := s.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(status.String(), "No active runs") {
			t.Fatalf("status: %s", &status)
		}
		h := NewHistoryCmd(fresh)
		h.SetOut(&history)
		if err := h.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(history.String(), "#42  success") {
			t.Fatalf("history: %s", &history)
		}
		response := httptest.NewRecorder()
		newPortalHandler(root).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("Portal: %d %s", response.Code, response.Body)
		}
		var payload struct {
			Runs []portalRun `json:"runs"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		for _, row := range payload.Runs {
			if row.RunID == id && row.Status == "success" {
				return
			}
		}
		t.Fatalf("Portal lost terminal run: %+v", payload.Runs)
	}
	assertReaders()
	deps := newTestDeps(t)
	deps.RepoRoot, deps.EventLog = root, log
	archive := NewArchiveCmd(deps)
	archive.SetArgs([]string{"run", id})
	if err := archive.Execute(); err != nil {
		t.Fatal(err)
	}
	assertReaders()
	fresh := &events.JSONLLogger{Path: logPath}
	list, err := fresh.Read()
	if err != nil {
		t.Fatal(err)
	}
	recovered, _, err := daemon.RecoverStaleRuns(filepath.Join(root, ".sandman"), list, fresh)
	if err != nil || recovered != 0 {
		t.Fatalf("restart recovery = %d, %v", recovered, err)
	}
	if err := os.RemoveAll(filepath.Join(root, ".sandman", "archive", id)); err != nil {
		t.Fatal(err)
	}
	assertReaders()
}

func TestArchiveBatch_RequiresEveryAgentRunTerminal(t *testing.T) {
	for _, phase := range []string{"running", "unknown", "capacity-queued", "terminal"} {
		t.Run(phase, func(t *testing.T) {
			root := newSandmanDir(t)
			batchID := "mixed"
			batchDir := filepath.Join(root, ".sandman", "batches", batchID)
			for _, id := range []string{"done", "other"} {
				writeRunDirForArchive(t, batchDir, id, batchindex.RunManifest{Status: batchindex.RunManifestStatusSuccess})
			}
			writeBatchIndexForArchive(t, root, []batchindex.Batch{{ID: batchID, Path: batchDir, Status: batchindex.StatusActive}})
			deps := terminalArchiveDeps(t, root, "done")
			deps.RepoRoot = root
			log := deps.EventLog
			if phase != "unknown" {
				if err := log.Log(events.Event{Type: "run.started", RunID: "other", Payload: map[string]any{"batch_id": batchID}}); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "capacity-queued" {
				if err := log.Log(events.Event{Type: "run.capacity_queued", RunID: "other"}); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "terminal" {
				if err := log.Log(events.Event{Type: "run.queued", RunID: "other"}); err != nil {
					t.Fatal(err)
				}
			}
			cmd := NewArchiveCmd(deps)
			cmd.SetArgs([]string{"batch", batchID})
			err := cmd.Execute()
			if phase == "terminal" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("archived batch with nonterminal/unknown member")
			}
			for _, id := range []string{"done", "other"} {
				if _, err := os.Stat(filepath.Join(batchDir, "runs", id)); err != nil {
					t.Fatalf("member moved: %v", err)
				}
			}
		})
	}
}
