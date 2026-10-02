package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
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
	assertReaders := func(archived bool, available bool) {
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
		// A new Portal process has no per-repository snapshot cache.
		portalRunsIndexes.Delete(root)
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
				if row.Archived != archived || row.SourceExists != available {
					t.Fatalf("artifact facts: want %v/%v; row=%+v", archived, available, row)
				}
				if row.FinishedAt == nil || !row.FinishedAt.Equal(start.Add(time.Second)) {
					t.Fatalf("finish changed: %+v", row)
				}
				return
			}
		}
		t.Fatalf("Portal lost terminal run: %+v", payload.Runs)
	}
	assertReaders(false, true)
	deps := newTestDeps(t)
	deps.RepoRoot, deps.EventLog = root, log
	archive := NewArchiveCmd(deps)
	archive.SetArgs([]string{"run", id})
	if err := archive.Execute(); err != nil {
		t.Fatal(err)
	}
	assertReaders(true, true)
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
	assertReaders(true, false)
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

func TestCleanAll_EventTerminalityAndArtifactOwnership(t *testing.T) {
	for _, scenario := range []string{"terminal-stale-snapshot", "unknown", "capacity-queued", "event-only-capacity-member", "read-error", "wrong-owner"} {
		t.Run(scenario, func(t *testing.T) {
			deps := newRunDepsAuto(t, &fakeBatchRunner{})
			root, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			batchDir := filepath.Join(root, ".sandman", "batches", "batch")
			manifest := batchindex.RunManifest{RunID: "row", BatchID: "batch", Kind: batchindex.KindIssue, Status: batchindex.RunManifestStatusSuccess, Branch: "42-work", WorktreePath: filepath.Join(root, ".sandman", "worktrees", "42-work")}
			if scenario == "terminal-stale-snapshot" {
				manifest.Status = batchindex.RunManifestStatusActive
			}
			if scenario == "wrong-owner" {
				manifest.BatchID = "elsewhere"
			}
			if err := daemon.WriteRunManifest(batchDir, "row", manifest); err != nil {
				t.Fatal(err)
			}
			writeBatchIndexForArchive(t, root, []batchindex.Batch{{ID: "batch", Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive}})
			log := &fakeEventLog{events: []events.Event{{Type: "run.finished", RunID: "row", Payload: map[string]any{"status": "success"}}}}
			if scenario == "unknown" {
				log.events = nil
			}
			if scenario == "capacity-queued" {
				log.events = []events.Event{{Type: "run.started", RunID: "row"}, {Type: "run.capacity_queued", RunID: "row"}}
			}
			if scenario == "event-only-capacity-member" {
				log.events = append(log.events,
					events.Event{Type: "run.started", RunID: "missing-row", Payload: map[string]any{"batch_id": "batch"}},
					events.Event{Type: "run.capacity_queued", RunID: "missing-row", Payload: map[string]any{"batch_id": "batch"}},
				)
			}
			if scenario == "read-error" {
				log.err = errors.New("lifecycle unavailable")
			}
			deps.EventLog, deps.GitRunner = log, &fakeGitRunner{}
			deps.RepoRoot = root
			deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(root, ".sandman", "worktrees")}}
			deps.RunActivityProbe = func(string) bool { return false }
			cmd := NewCleanCmd(deps)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--all"})
			err = cmd.Execute()
			if scenario == "read-error" {
				if err == nil {
					t.Fatal("read failure did not fail closed")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(batchDir)
			if scenario == "terminal-stale-snapshot" {
				if !os.IsNotExist(statErr) {
					t.Fatalf("terminal artifacts not reclaimed: %v", statErr)
				}
			} else if statErr != nil {
				t.Fatalf("ineligible artifacts removed: %v", statErr)
			}
		})
	}
}

func TestPortal_LifecycleIsNotInferredFromDeadArtifacts(t *testing.T) {
	root := newSandmanDir(t)
	batchID := "260618113825-abcd-42+1"
	id := "260618113825-abcd-42"
	batchDir := filepath.Join(root, ".sandman", "batches", batchID)
	created := time.Now().UTC().Add(-time.Minute)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42, 43}, CreatedAt: created, RunTS: "260618113825", RunShortID: "abcd"}); err != nil {
		t.Fatal(err)
	}
	writeRunDirForArchive(t, batchDir, id, batchindex.RunManifest{Status: batchindex.RunManifestStatusSuccess})
	writeBatchIndexForArchive(t, root, []batchindex.Batch{{ID: batchID, Path: batchDir, Status: batchindex.StatusActive, Issues: []int{42, 43}}})
	list := []events.Event{{Type: "run.started", RunID: id, Issue: 42, Timestamp: created.Add(time.Second), Payload: map[string]any{"batch_id": batchID}}}
	rows, err := (&portalRunsView{}).computeFromEvents(root, list)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]portalRun)
	for _, row := range rows {
		seen[row.IssueNumber] = row
	}
	if row := seen[42]; row.Status != "running" || row.FinishedAt != nil {
		t.Fatalf("dead artifacts invented terminal outcome: %+v", row)
	}
	if row := seen[43]; row.Status != "unknown" || row.FinishedAt != nil {
		t.Fatalf("missing events invented lifecycle: %+v", row)
	}
}

func TestArchive_EventAuthorityAcrossBoundaries(t *testing.T) {
	for _, boundary := range []string{"run", "batch", "older-than", "stale", "http"} {
		t.Run(boundary, func(t *testing.T) {
			for _, phase := range []string{"terminal-stale-snapshot", "unknown", "capacity-queued", "terminal-queued", "read-error"} {
				t.Run(phase, func(t *testing.T) {
					root := newSandmanDir(t)
					id := "260618113825-abcd-42"
					batchDir := filepath.Join(root, ".sandman", "batches", id)
					status := batchindex.RunManifestStatusSuccess
					if phase == "terminal-stale-snapshot" {
						status = batchindex.RunManifestStatusActive
					}
					writeRunDirForArchive(t, batchDir, id, batchindex.RunManifest{Issue: 42, Status: status, CreatedAt: time.Now().Add(-time.Hour)})
					writeBatchIndexForArchive(t, root, []batchindex.Batch{{ID: id, Path: batchDir, Status: batchindex.StatusActive, Runs: []batchindex.RunRecord{{RunID: id, Status: batchindex.RunRecordStatusActive}}}})
					logPath := filepath.Join(root, ".sandman", "events.jsonl")
					log := &events.JSONLLogger{Path: logPath}
					terminal := phase == "terminal-stale-snapshot" || phase == "terminal-queued"
					switch phase {
					case "terminal-stale-snapshot":
						recordTerminalLifecycle(t, root, id)
					case "terminal-queued":
						if err := log.Log(events.Event{Type: "run.queued", RunID: id, Issue: 42}); err != nil {
							t.Fatal(err)
						}
					case "capacity-queued":
						if err := log.Log(events.Event{Type: "run.started", RunID: id, Issue: 42}); err != nil {
							t.Fatal(err)
						}
						if err := log.Log(events.Event{Type: "run.capacity_queued", RunID: id, Issue: 42}); err != nil {
							t.Fatal(err)
						}
					case "read-error":
						if err := os.Mkdir(logPath, 0755); err != nil {
							t.Fatal(err)
						}
					}
					if boundary == "http" {
						response := httptest.NewRecorder()
						newPortalHandler(root).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/archive", strings.NewReader(`{"runId":"`+id+`"}`)))
						want := http.StatusConflict
						if terminal {
							want = http.StatusOK
						}
						if phase == "read-error" {
							want = http.StatusInternalServerError
						}
						if response.Code != want {
							t.Fatalf("HTTP=%d %s, want %d", response.Code, response.Body, want)
						}
					} else {
						deps := newTestDeps(t)
						deps.RepoRoot, deps.EventLog = root, log
						cmd := NewArchiveCmd(deps)
						cmd.SetOut(&bytes.Buffer{})
						cmd.SetErr(&bytes.Buffer{})
						args := []string{boundary, id}
						if boundary == "older-than" {
							args[1] = "0"
						}
						if boundary == "stale" {
							args = []string{boundary}
						}
						cmd.SetArgs(args)
						err := cmd.Execute()
						wantError := phase == "read-error" || (!terminal && (boundary == "run" || boundary == "batch"))
						if (err != nil) != wantError {
							t.Fatalf("archive error=%v, want error=%v", err, wantError)
						}
					}
					_, err := os.Stat(filepath.Join(batchDir, "runs", id))
					if terminal {
						if !os.IsNotExist(err) {
							t.Fatalf("terminal Run not archived: %v", err)
						}
					} else if err != nil {
						t.Fatalf("ineligible Run moved: %v", err)
					}
				})
			}
		})
	}
}

func TestPortal_QueuedTerminalityControlsReviewPromotion(t *testing.T) {
	for _, phase := range []string{"run.queued", "run.capacity_queued"} {
		t.Run(phase, func(t *testing.T) {
			root := newSandmanDir(t)
			at := time.Now().UTC()
			list := []events.Event{
				{Type: "run.started", RunID: "parent", Issue: 42, Timestamp: at, Payload: map[string]any{"batch_id": "implementation"}},
				{Type: phase, RunID: "parent", Issue: 42, Timestamp: at.Add(time.Second), Payload: map[string]any{"batch_id": "implementation"}},
				{Type: "run.started", RunID: "review", Issue: 42, Timestamp: at.Add(2 * time.Second), Payload: map[string]any{"batch_id": "review-batch", "review": true, "pr_number": 99}},
			}
			rows, err := (&portalRunsView{}).computeFromEvents(root, list)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.RunID != "parent" {
					continue
				}
				want := "queued"
				if phase == "run.capacity_queued" {
					want = "reviewing"
				}
				if row.Status != want {
					t.Fatalf("phase %s parent=%+v, want %s", phase, row, want)
				}
				return
			}
			t.Fatal("missing implementation parent")
		})
	}
}

func TestArchiveRun_MissingSnapshotDoesNotChangeTerminality(t *testing.T) {
	root := newSandmanDir(t)
	id := "row"
	batchDir := filepath.Join(root, ".sandman", "batches", "batch")
	runDir := filepath.Join(batchDir, "runs", id)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeBatchIndexForArchive(t, root, []batchindex.Batch{{ID: "batch", Path: batchDir, Status: batchindex.StatusActive, Runs: []batchindex.RunRecord{{RunID: id, Status: batchindex.RunRecordStatusActive}}}})
	deps := terminalArchiveDeps(t, root, id)
	deps.RepoRoot = root
	cmd := NewArchiveCmd(deps)
	cmd.SetArgs([]string{"run", id})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".sandman", "archive", "batch", "runs", id)); err != nil {
		t.Fatal(err)
	}
}

func TestPortal_TerminalQueuedReviewRetainsOutcome(t *testing.T) {
	root := newSandmanDir(t)
	id := "260618113825-abcd-42-PR99"
	batchID := "260618113825-abcd-PR99"
	at := time.Now().UTC()
	list := []events.Event{{Type: "run.queued", RunID: id, Issue: 42, Timestamp: at, Payload: map[string]any{"batch_id": batchID, "review": true, "pr_number": 99}}}
	view := &portalRunsView{}
	rows, err := view.computeWithActiveRuns(root, list, view.groupEventsByRun(list), []portalActiveRun{{
		Key: id, RunID: id, BatchID: batchID, IssueNumber: 42, IssueNumbers: []int{42}, PRNumber: 99, StartedAt: at,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "queued" || rows[0].FinishedAt == nil {
		t.Fatalf("review queue outcome revised: %+v", rows)
	}
}
