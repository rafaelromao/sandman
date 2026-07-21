package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortal_LastOutputAt pins the staleness data source. The portal's
// core unmet job is distinguishing an active run that is producing output
// from one that has gone quiet. The run-folder log (<batchDir>/runs/<runID>/run.log)
// is opened with O_APPEND during AgentRun.Execute, so its mtime is the
// cheapest accurate "last output" signal — and, unlike event timestamps,
// it does not flag a healthy but quiet agent as stale.
func TestPortal_LastOutputAt(t *testing.T) {
	staleMtime := time.Date(2025, 6, 17, 12, 0, 0, 0, time.UTC)
	startedAt := time.Date(2025, 6, 17, 11, 55, 0, 0, time.UTC)

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "42.log")
	if err := os.WriteFile(logPath, []byte("[issue-42] 12:00:00 hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logPath, staleMtime, staleMtime); err != nil {
		t.Fatal(err)
	}

	v := &portalRunsView{}

	t.Run("uses saved log mtime when present", func(t *testing.T) {
		run := portalRun{Kind: "active", LogPath: logPath, StartedAt: startedAt}
		if got := v.lastOutputAt(run); !got.Equal(staleMtime) {
			t.Fatalf("lastOutputAt=%v, want log mtime %v", got, staleMtime)
		}
	})

	t.Run("falls back to started at when no log file yet", func(t *testing.T) {
		run := portalRun{Kind: "active", LogPath: filepath.Join(tmp, "missing.log"), StartedAt: startedAt}
		if got := v.lastOutputAt(run); !got.Equal(startedAt) {
			t.Fatalf("lastOutputAt=%v, want startedAt %v", got, startedAt)
		}
	})

	t.Run("falls back to started at when log path empty", func(t *testing.T) {
		run := portalRun{Kind: "active", StartedAt: startedAt}
		if got := v.lastOutputAt(run); !got.Equal(startedAt) {
			t.Fatalf("lastOutputAt=%v, want startedAt %v", got, startedAt)
		}
	})

	t.Run("zero when neither is set", func(t *testing.T) {
		run := portalRun{Kind: "active"}
		if got := v.lastOutputAt(run); !got.IsZero() {
			t.Fatalf("lastOutputAt=%v, want zero", got)
		}
	})

	t.Run("ignores a directory at the log path", func(t *testing.T) {
		dirPath := filepath.Join(tmp, "is-a-dir")
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			t.Fatal(err)
		}
		run := portalRun{Kind: "active", LogPath: dirPath, StartedAt: startedAt}
		if got := v.lastOutputAt(run); !got.Equal(startedAt) {
			t.Fatalf("lastOutputAt=%v, want startedAt fallback %v (dir must not be treated as a log)", got, startedAt)
		}
	})
}

// TestPortal_RunFromState_MultiIssueBatchActive_LogPathUsesOnDiskDirSuffix
// pins the active-batch path: when an active row's batch directory carries
// the "+N" suffix (the on-disk identity produced by runid.NewBatchID) and
// the active instance's BatchID is the per-row RunID for the first issue
// (the index entry id, per ADR-0036), runFromState must point LogPath at
// the per-row log under the on-disk "+N" directory — not at a
// non-existent "<indexEntryId>/runs/<runState.RunID>/run.log" path that
// makes the staleness stat fall back to startedAt and produce a stale
// chip equal to the run duration (issue #1715).
func TestPortal_RunFromState_MultiIssueBatchActive_LogPathUsesOnDiskDirSuffix(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const perRowRunID = "260703095305-fde2-1704"
	const onDiskDir = "260703095305-fde2-1699+6"
	const indexEntryID = "260703095305-fde2-1699"

	// Build the per-row log at the on-disk (+N) path. This is the file
	// the agent writes via O_APPEND during AgentRun.Execute; its mtime
	// is the staleness signal.
	logPath := filepath.Join(repoRoot, ".sandman", "batches", onDiskDir, "runs", perRowRunID, "run.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("output\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 6, 17, 12, 0, 0, 0, time.UTC)
	runState := events.RunState{
		RunID: perRowRunID,
		Started: events.Event{
			Timestamp: startedAt,
			Payload: map[string]any{
				"batch_id": onDiskDir,
				"branch":   "sandman/1704-fix",
			},
		},
	}

	// active carries the index entry id (per-row RunID for first issue,
	// per ADR-0036) — NOT the on-disk dir name with "+N".
	active := &portalActiveRun{
		Key:        indexEntryID,
		Dir:        filepath.Join(repoRoot, ".sandman", "batches", onDiskDir),
		SocketPath: filepath.Join(repoRoot, ".sandman", "batches", onDiskDir, "batch.sock"),
		BatchID:    indexEntryID,
		RunID:      perRowRunID,
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, active, nil, nil, nil)

	if run.LogPath != logPath {
		t.Fatalf("LogPath=%q, want %q (the on-disk per-row log under %s)", run.LogPath, logPath, onDiskDir)
	}
	// The post-loop in compute() sets LastOutputAt from run.LogPath via
	// lastOutputAt. Exercise the same seam here so the test reads as a
	// single behaviour: "given an active row in a multi-issue batch, the
	// staleness signal points at the real per-row log mtime, not startedAt."
	at := (&portalRunsView{}).lastOutputAt(run)
	if at.IsZero() {
		t.Fatal("lastOutputAt is zero; want per-row log mtime")
	}
	if at.Equal(startedAt) {
		t.Fatalf("lastOutputAt=%v equals startedAt; the staleness fallback would render a stale chip equal to the run duration (issue #1715)", at)
	}
}

// TestPortal_RunFromState_ActiveNil_MultiIssueBatch_LogPathFromEventPayload
// pins the regression guard for the active==nil path of runFromState.
// The completed-row branch of compute() calls runFromState with
// active=nil; for a multi-issue batch the LogPath must still come from
// runState.BatchID() (the event payload's batch_id, with "+N") — NOT
// from a hard-coded index-entry-id. This slice is the no-regression
// guard for the active-row fix in Slice 1.
func TestPortal_RunFromState_ActiveNil_MultiIssueBatch_LogPathFromEventPayload(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const perRowRunID = "260703095305-fde2-1704"
	const onDiskDir = "260703095305-fde2-1699+6"

	logPath := filepath.Join(repoRoot, ".sandman", "batches", onDiskDir, "runs", perRowRunID, "run.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("output\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 6, 17, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	runState := events.RunState{
		RunID: perRowRunID,
		Started: events.Event{
			Timestamp: startedAt,
			Payload: map[string]any{
				"batch_id": onDiskDir,
				"branch":   "sandman/1704-fix",
			},
		},
		Finished: &events.Event{Timestamp: finishedAt, Payload: map[string]any{"status": "success"}},
	}

	run := (&portalRunsView{}).runFromState(repoRoot, runState, nil, nil, nil, nil)

	if run.LogPath != logPath {
		t.Fatalf("LogPath=%q, want %q (active==nil path must keep using state.BatchID())", run.LogPath, logPath)
	}
	if run.LastOutputAt != nil {
		t.Fatalf("LastOutputAt=%v, want nil for completed row", run.LastOutputAt)
	}
}

// TestPortal_DiscoverActiveRuns_IssueMultiBatch_RunIDIsPerRow pins the
// discovery path of the active-batch bug: for a multi-issue batch the
// per-row RunID must be resolved from the per-row run.json under the
// on-disk "+N" directory, distinct from the on-disk BatchId. Without
// this fix, active.RunID == active.BatchID and the staleness stat
// falls back to startedAt (issue #1715). Issue #1954 (slice 11) pins
// the public BatchId contract: the manifest BatchId must equal the
// on-disk directory basename (with "+N"), so the portal Batch label
// and Details tab render `<ts>-<sid>-<first>+<additionalCount>`.
func TestPortal_DiscoverActiveRuns_IssueMultiBatch_RunIDIsPerRow(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "pda")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const firstIssue = 1699
	const issueN = 6
	const perRowRunID = "260703095305-fde2-1704"
	const onDiskDir = "260703095305-fde2-1699+6"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", onDiskDir)
	batchSockPath := filepath.Join(batchDir, "batch.sock")
	runDir := filepath.Join(batchDir, "runs", perRowRunID)
	logPath := filepath.Join(runDir, "run.log")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, batchSockPath)

	// Public BatchId (issue #1954): manifest.BatchId equals the
	// on-disk batch directory basename and carries the "+N" suffix.
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		BatchId:   onDiskDir,
		RunKind:   "issue",
		Issues:    []int{1699, 1700, 1701, 1702, 1703, 1704},
		CreatedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("write batch manifest: %v", err)
	}

	// Per-row manifest under the on-disk (+N) dir. Issue matches the
	// runID's embedded issue number (1704).
	if err := daemon.WriteRunManifest(batchDir, perRowRunID, batchindex.RunManifest{
		RunID:     perRowRunID,
		BatchID:   onDiskDir,
		Issue:     1704,
		Branch:    "sandman/1704-fix",
		Kind:      batchindex.KindIssue,
		Status:    batchindex.RunManifestStatusActive,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}

	// Write the per-row log and pin its mtime so the assertion is
	// deterministic.
	if err := os.WriteFile(logPath, []byte("output\n"), 0644); err != nil {
		t.Fatal(err)
	}
	logMtime := time.Now().Add(-30 * time.Second).Round(time.Second)
	if err := os.Chtimes(logPath, logMtime, logMtime); err != nil {
		t.Fatal(err)
	}

	// Index entry: id == on-disk dir with "+N" (the public BatchId),
	// path == on-disk dir. Pre-#1954 this used the per-row RunID of
	// the first issue (no "+N"); post-#1954 the index id matches the
	// public BatchId in both shape and value.
	addBatchToIndex(t, repoRoot, onDiskDir, batchDir, []int{1699, 1700, 1701, 1702, 1703, 1704})

	idx := getPortalRunsIndex(repoRoot)
	active, err := idx.view.discoverActiveRuns(repoRoot, nil)
	if err != nil {
		t.Fatalf("discoverActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active instance, got %d: %#v", len(active), active)
	}
	got := active[0]
	if got.RunID != perRowRunID {
		t.Fatalf("active.RunID=%q, want per-row RunID %q (issue #1715: must not collapse to on-disk BatchId %q)", got.RunID, perRowRunID, onDiskDir)
	}
	if got.BatchID != onDiskDir {
		t.Fatalf("active.BatchID=%q, want %q (public BatchId per ADR-0032)", got.BatchID, onDiskDir)
	}
	if !got.LastOutputAt.Equal(logMtime) {
		t.Fatalf("active.LastOutputAt=%v, want per-row log mtime %v (issue #1715: stat must hit the real per-row log)", got.LastOutputAt, logMtime)
	}
}

// TestPortal_RunFromActiveBatchIssue_StateLessMultiBatch_LogPathFromActiveDir
// pins the state-less path of runFromActiveBatchIssue. When the active
// instance has a live socket but no matching RunState yet (pre-run.started
// window), the function must resolve LogPath via active.Dir (the on-disk
// dir with "+N" suffix) — not via Layout.RunFolder(active.BatchID,
// active.RunID), which would produce a non-existent path and break the
// staleness stat for state-less active rows (issue #1715).
func TestPortal_RunFromActiveBatchIssue_StateLessMultiBatch_LogPathFromActiveDir(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const perRowRunID = "260703095305-fde2-1704"
	const onDiskDir = "260703095305-fde2-1699+6"
	const indexEntryID = "260703095305-fde2-1699"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", onDiskDir)
	logPath := filepath.Join(batchDir, "runs", perRowRunID, "run.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("output\n"), 0644); err != nil {
		t.Fatal(err)
	}

	active := portalActiveRun{
		Key:        indexEntryID,
		Dir:        batchDir,
		SocketPath: filepath.Join(batchDir, "batch.sock"),
		BatchID:    indexEntryID,
		RunID:      perRowRunID,
	}

	// state == nil, blocked == nil, queued == nil: the state-less path
	// through runFromActiveBatchIssue.
	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 1704, nil, nil, nil, "", nil, nil)

	if run.LogPath != logPath {
		t.Fatalf("LogPath=%q, want %q (state-less path must resolve via active.Dir, not active.BatchID)", run.LogPath, logPath)
	}
	at := (&portalRunsView{}).lastOutputAt(run)
	if at.IsZero() {
		t.Fatal("lastOutputAt is zero; want per-row log mtime")
	}
	if at.Equal(run.StartedAt) {
		t.Fatalf("lastOutputAt=%v equals StartedAt; the state-less path would render a stale chip equal to the run duration (issue #1715)", at)
	}
}

// TestPortal_Compute_LeavesLastOutputAtNilForCompletedRows ensures the
// staleness field is omitted for terminal rows so the JSON contract only
// carries it for runs that can actually be stale.
func TestPortal_Compute_LeavesLastOutputAtNilForCompletedRows(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "260618113825-abcd-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "260618113825-abcd-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(runs))
	}
	if runs[0].Kind != "completed" {
		t.Fatalf("expected completed kind, got %q", runs[0].Kind)
	}
	if runs[0].LastOutputAt != nil {
		t.Fatalf("expected LastOutputAt nil for completed row, got %v", *runs[0].LastOutputAt)
	}
}

// TestPortal_RunFromActiveBatchIssue_QueuedRow_StartedAtAnchorsOnQueuedEvent
// pins the active-row StartedAt for a state-less queued row. While the
// issue is queued, the live duration tick is anchored on the run.queued
// event's timestamp — not on the batch manifest's CreatedAt — so the
// duration counter does not include any time before the run entered the
// queue. Stale-time chips continue to be gated by the JS stalenessOf
// predicate and never fire for `queued` rows regardless of StartedAt.
func TestPortal_RunFromActiveBatchIssue_QueuedRow_StartedAtAnchorsOnQueuedEvent(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const perRowRunID = "260703095305-fde2-1704"
	const onDiskDir = "260703095305-fde2-1699+6"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", onDiskDir)
	if err := os.MkdirAll(filepath.Join(batchDir, "runs", perRowRunID), 0755); err != nil {
		t.Fatal(err)
	}

	// manifestCreatedAt represents active.StartedAt — the batch
	// manifest's creation timestamp. This is BEFORE the run was queued
	// and MUST NOT be the duration anchor for a queued row.
	manifestCreatedAt := time.Date(2025, 6, 17, 11, 0, 0, 0, time.UTC)
	queuedAt := time.Date(2025, 6, 17, 11, 30, 0, 0, time.UTC)

	active := portalActiveRun{
		Key:        perRowRunID,
		Dir:        batchDir,
		SocketPath: filepath.Join(batchDir, "batch.sock"),
		BatchID:    onDiskDir,
		RunID:      perRowRunID,
		StartedAt:  manifestCreatedAt,
		ModTime:    manifestCreatedAt,
	}

	// queued != nil, state == nil: the state-less queued path the bug
	// fix targets. In this state, runsFromActiveBatch nulls the state
	// because state.Status() == "queued".
	queued := &events.Event{
		Type:      "run.queued",
		Timestamp: queuedAt,
		RunID:     perRowRunID,
		Issue:     1704,
		Payload:   map[string]any{"branch": "sandman/1704-fix"},
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 1704, nil, nil, queued, "", nil, nil)

	if run.StartedAt.IsZero() {
		t.Fatal("StartedAt is zero; want run.queued.Timestamp so the duration tick is anchored on queue entry")
	}
	if !run.StartedAt.Equal(queuedAt) {
		t.Fatalf("StartedAt=%v, want queued.Timestamp %v (duration must NOT be anchored on active.StartedAt=%v, that would count time before queue entry)", run.StartedAt, queuedAt, manifestCreatedAt)
	}
	if run.StartedAt.Equal(manifestCreatedAt) {
		t.Fatalf("StartedAt=%v equals active.StartedAt (manifest.CreatedAt); the queued row would count pre-queue time in its duration tick", run.StartedAt)
	}
}

// TestPortal_RunFromActiveBatchIssue_StateLessPreQueuedRow_FallsBackToActiveStartedAt
// pins the pre-queued-event window. When the run is so fresh that
// run.queued has not been emitted yet, the row's StartedAt falls back to
// active.StartedAt so the duration tick has a sane anchor instead of
// rendering "—". The fix MUST preserve this fallback when no queued
// event exists.
func TestPortal_RunFromActiveBatchIssue_StateLessPreQueuedRow_FallsBackToActiveStartedAt(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const perRowRunID = "260703095305-fde2-1704"
	const onDiskDir = "260703095305-fde2-1699+6"

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", onDiskDir)
	if err := os.MkdirAll(filepath.Join(batchDir, "runs", perRowRunID), 0755); err != nil {
		t.Fatal(err)
	}

	manifestCreatedAt := time.Date(2025, 6, 17, 11, 0, 0, 0, time.UTC)
	active := portalActiveRun{
		Key:        perRowRunID,
		Dir:        batchDir,
		SocketPath: filepath.Join(batchDir, "batch.sock"),
		BatchID:    onDiskDir,
		RunID:      perRowRunID,
		StartedAt:  manifestCreatedAt,
		ModTime:    manifestCreatedAt,
	}

	run := (&portalRunsView{}).runFromActiveBatchIssue(repoRoot, active, 1704, nil, nil, nil, "", nil, nil)

	if !run.StartedAt.Equal(manifestCreatedAt) {
		t.Fatalf("StartedAt=%v, want active.StartedAt=%v (no queued event -> fall back to active.StartedAt)", run.StartedAt, manifestCreatedAt)
	}
}
