// Tests for issue #1937 slice 0: portalRun.RunDir plumbing. The RunDir
// field is the host-absolute path to the per-row run folder, used by
// slice-1 verdict readers that locate the decision file at
// <runDir>/decision.md. The field is server-only — tagged json:"-",
// so it never reaches the front-end.
package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortal_RunDir_ActiveRowStampsSocketDir pins slice 0b: every active
// row produced from a live portalActiveRun carries RunDir equal to the
// directory holding the active instance's socket (i.e. the batch
// directory on disk for issue-driven batches, or the per-row folder
// for review batches). The value is the same path the daemon would
// hand to os.Stat when looking for decision.md / run.log / run.sock.
func TestPortal_RunDir_ActiveRowStampsSocketDir(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "live-rundir")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), BatchId: "live-rundir"}); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))
	addBatchToIndex(t, repoRoot, "live-rundir", batchDir, []int{42})

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: "abcd-260101120000-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}

	want := batchDir // filepath.Dir(batch.sock) == batchDir
	if got := runs[0].RunDir; got != want {
		t.Errorf("active row RunDir = %q, want %q (filepath.Dir(SocketPath))", got, want)
	}
}

// TestPortal_RunDir_FieldHasNoJSONTag pins AC #2: the RunDir field is
// tagged `json:"-"`, so json.Marshal never serializes it. The test
// marshals a populated portalRun and asserts the JSON payload has no
// `rundir`/`runDir`/`RunDir` key.
func TestPortal_RunDir_FieldHasNoJSONTag(t *testing.T) {
	run := portalRun{
		Key:    "k",
		RunID:  "r",
		RunDir: "/server-only/path/that/never/reaches/the/frontend",
	}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"RunDir", "rundir", "runDir", "run_dir"} {
		if _, ok := raw[key]; ok {
			t.Errorf("JSON output should not contain key %q (server-only field must not reach the front-end): %s", key, string(data))
		}
	}
}

// TestPortal_RunDir_TerminalRowStampsBatchesIndexPath pins slice 0c:
// a terminal event-log row whose batch is in the Batches index carries
// RunDir equal to `<idx.Resolve(batchID).Path>/runs/<runID>`. The
// Batches index is the source of truth for the on-disk location because
// the per-row manifest's path is the index entry's Path at archive time
// (the run folder itself can be relocated between batch and archive).
func TestPortal_RunDir_TerminalRowStampsBatchesIndexPath(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchID := "abcd-260101120000-99"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	runID := "abcd-260101120000-99"
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: runID, Issue: 99, Payload: map[string]any{"branch": "sandman/99-fix", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: runID, Issue: 99, Payload: map[string]any{"status": "success", "branch": "sandman/99-fix", "batch_id": batchID}},
	})
	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{99})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}

	want := filepath.Join(batchDir, "runs", runID)
	if got := runs[0].RunDir; got != want {
		t.Errorf("terminal row RunDir = %q, want %q (idx.Resolve(batchID).Path + runs/<runID>)", got, want)
	}
}

// TestPortal_RunDir_TerminalRowUnresolvableBatchLeavesRunDirEmpty pins
// the negative side of slice 0c: when the terminal row's batch cannot
// be resolved in the Batches index, RunDir stays empty. The caller
// treats empty as Unclear (per the issue brief), so a missing index
// entry must not fabricate a directory the verdict reader can't stat.
func TestPortal_RunDir_TerminalRowUnresolvableBatchLeavesRunDirEmpty(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runID := "abcd-260101120000-77"
	// No addBatchToIndex call — the batch is unknown to the Batches
	// index. The terminal row survives via the event log alone.
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: runID, Issue: 77, Payload: map[string]any{"branch": "sandman/77-fix", "batch_id": "ghost-batch"}},
		{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: runID, Issue: 77, Payload: map[string]any{"status": "success", "branch": "sandman/77-fix", "batch_id": "ghost-batch"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d: %#v", len(runs), runs)
	}
	if got := runs[0].RunDir; got != "" {
		t.Errorf("terminal row RunDir = %q, want empty (unresolvable batch must leave RunDir empty so caller renders Unclear)", got)
	}
}
