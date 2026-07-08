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

// TestPortal_RunDir_ActiveRowStampsPerRowFolder pins slice 0b: every active
// row produced from a live portalActiveRun carries RunDir equal to the
// per-row run folder on disk — `<batchDir>/runs/<runID>`. For
// issue-driven batches whose live socket is `<batchDir>/batch.sock`,
// `filepath.Dir(SocketPath)` yields the batch directory, not the per-row
// folder; `activeRunDir` collapses both shapes (issue-driven and review
// batches) into the single canonical per-row folder so the verdict
// reader's `<RunDir>/decision.md` lookup hits the same location as
// `paths.Layout.DecisionFile(batchID, runID)` for terminal rows.
func TestPortal_RunDir_ActiveRowStampsPerRowFolder(t *testing.T) {
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

	want := filepath.Join(batchDir, "runs", "live-rundir")
	if got := runs[0].RunDir; got != want {
		t.Errorf("active row RunDir = %q, want %q (per-row folder under batchDir/runs/<active.RunID>)", got, want)
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

// TestPortal_RunDir_SynthesizedDeadBatchRowStampsBatchRunDir pins slice
// 0d: when a dead batch has issues missing from the event log, the
// synthesized aborted row carries RunDir equal to
// `<deadBatch.RunDir>/runs/<runID>`. The DeadBatch.RunDir is already
// known to the synthesize path (it comes from the index entry's Path),
// so RunDir stamps directly without re-resolving through the index.
func TestPortal_RunDir_SynthesizedDeadBatchRowStampsBatchRunDir(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchID := "dead-rundir"
	runTS := "260101120000"
	runShortID := "abcd"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		Issues:     []int{55, 66},
		CreatedAt:  time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		BatchId:    batchID,
		RunTS:      runTS,
		RunShortID: runShortID,
	}); err != nil {
		t.Fatal(err)
	}
	// Issue 55 ran to completion via the event log; issue 66 did NOT,
	// so synthesizedDeadBatchRows will produce a row for issue 66 only.
	runID55 := "abcd-260101120000-55"
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: runID55, Issue: 55, Payload: map[string]any{"branch": "sandman/55-fix", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: runID55, Issue: 55, Payload: map[string]any{"status": "success", "branch": "sandman/55-fix", "batch_id": batchID}},
	})
	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{55, 66})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	var synth *portalRun
	for i := range runs {
		if runs[i].IssueNumber == 66 && runs[i].Kind == "completed" && runs[i].Status == "aborted" {
			synth = &runs[i]
			break
		}
	}
	if synth == nil {
		t.Fatalf("expected synthesized dead-batch row for issue 66, got %#v", runs)
	}

	want := filepath.Join(batchDir, "runs", synth.RunID)
	if got := synth.RunDir; got != want {
		t.Errorf("synthesized row RunDir = %q, want %q (batch.RunDir + runs/<runID>)", got, want)
	}
}
