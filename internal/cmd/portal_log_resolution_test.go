package cmd

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/runid"
)

// TestPortalRunsView_CompletedBatchCountLogDownload is the tracer bullet for
// issue #1437. A completed issue run whose batch id differs from its run id
// (because the batch id carries the issue-count suffix, including "+") must get
// a log download URL that points at the real batch folder, not a path derived
// from the run id alone.
func TestPortalRunsView_CompletedBatchCountLogDownload(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)

	ts := "260101120000"
	shortid := "abcd"
	batchID := runid.NewBatchID(runid.KindIssue, 3, "42", ts, shortid)
	runID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)

	if batchID == runID {
		t.Fatalf("setup error: batch id %q must differ from run id %q", batchID, runID)
	}
	if !strings.Contains(batchID, "+") {
		t.Fatalf("setup error: batch id %q must contain '+'", batchID)
	}

	rawLogContent := "[issue-42] 10:01:00 first line\n[issue-42] 10:01:05 second line\n"

	startedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "42-fix", "batch_id": batchID}},
	})

	batchDir := filepath.Join(layout.BatchesDir, batchID)
	runDir := filepath.Join(batchDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte(rawLogContent), 0644); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{
				ID:        batchID,
				Path:      batchDir,
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: startedAt,
				Issues:    []int{42},
			},
		},
	}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d: %#v", len(runs), runs)
	}
	gotRun := runs[0]

	if gotRun.RunID != runID {
		t.Errorf("RunID = %q, want %q", gotRun.RunID, runID)
	}
	if gotRun.Archived {
		t.Errorf("expected Archived=false, got true")
	}
	if !gotRun.SourceExists {
		t.Errorf("expected SourceExists=true for real batch folder, got false")
	}
	if gotRun.LogURL == "" {
		t.Fatal("expected non-empty LogURL for completed issue run with log file")
	}

	wantPath := filepath.Join(".sandman", "batches", batchID, "runs", runID, "run.log")
	logURL, err := url.Parse(server.URL + gotRun.LogURL)
	if err != nil {
		t.Fatalf("parse LogURL %q: %v", gotRun.LogURL, err)
	}
	gotPath := logURL.Query().Get("path")
	if gotPath != wantPath {
		t.Fatalf("decoded log path = %q, want %q; LogURL = %q", gotPath, wantPath, gotRun.LogURL)
	}

	logResp, err := http.Get(server.URL + gotRun.LogURL)
	if err != nil {
		t.Fatal(err)
	}
	defer logResp.Body.Close()
	if logResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from log download URL %q, got %d", gotRun.LogURL, logResp.StatusCode)
	}

	body, err := io.ReadAll(logResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != rawLogContent {
		t.Fatalf("log body = %q, want %q", body, rawLogContent)
	}
}

// TestPortalRunsView_ArchivedBatchCountLogDownload covers archived issue runs.
// After the batch directory moves to .sandman/archive/<batch_id>, the portal row
// must still resolve its log URL and source existence from the index entry's
// recorded Path, not from .sandman/batches.
func TestPortalRunsView_ArchivedBatchCountLogDownload(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)

	ts := "260101120000"
	shortid := "abcd"
	batchID := runid.NewBatchID(runid.KindIssue, 3, "42", ts, shortid)
	runID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)

	if batchID == runID {
		t.Fatalf("setup error: batch id %q must differ from run id %q", batchID, runID)
	}
	if !strings.Contains(batchID, "+") {
		t.Fatalf("setup error: batch id %q must contain '+'", batchID)
	}

	rawLogContent := "[issue-42] 10:01:00 archived first line\n[issue-42] 10:01:05 archived second line\n"

	startedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "42-fix", "batch_id": batchID}},
	})

	archiveDir := filepath.Join(layout.ArchiveDir, batchID)
	archiveRunDir := filepath.Join(archiveDir, "runs", runID)
	if err := os.MkdirAll(archiveRunDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archiveRunDir, "run.log"), []byte(rawLogContent), 0644); err != nil {
		t.Fatal(err)
	}

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{
				ID:         batchID,
				Path:       archiveDir,
				Kind:       batchindex.KindIssue,
				Status:     batchindex.StatusArchived,
				CreatedAt:  startedAt,
				ArchivedAt: &finishedAt,
				Issues:     []int{42},
			},
		},
	}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d: %#v", len(runs), runs)
	}
	gotRun := runs[0]

	if gotRun.RunID != runID {
		t.Errorf("RunID = %q, want %q", gotRun.RunID, runID)
	}
	if !gotRun.Archived {
		t.Errorf("expected Archived=true, got false")
	}
	if !gotRun.SourceExists {
		t.Errorf("expected SourceExists=true for archived batch folder, got false")
	}
	if strings.TrimSpace(gotRun.Log) == "" {
		t.Errorf("expected non-empty Log preview from archived run.log")
	}
	if gotRun.LogURL == "" {
		t.Fatal("expected non-empty LogURL for archived issue run with log file")
	}

	wantPath := filepath.Join(".sandman", "archive", batchID, "runs", runID, "run.log")
	logURL, err := url.Parse(server.URL + gotRun.LogURL)
	if err != nil {
		t.Fatalf("parse LogURL %q: %v", gotRun.LogURL, err)
	}
	gotPath := logURL.Query().Get("path")
	if gotPath != wantPath {
		t.Fatalf("decoded log path = %q, want %q; LogURL = %q", gotPath, wantPath, gotRun.LogURL)
	}

	logResp, err := http.Get(server.URL + gotRun.LogURL)
	if err != nil {
		t.Fatal(err)
	}
	defer logResp.Body.Close()
	if logResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from log download URL %q, got %d", gotRun.LogURL, logResp.StatusCode)
	}

	body, err := io.ReadAll(logResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != rawLogContent {
		t.Fatalf("log body = %q, want %q", body, rawLogContent)
	}
}

// TestPortalRunsView_CompletedRunLogResolvesFromSavedFile is the
// real-artifact regression test for #1637 / #1641: drives the portal's
// HTTP /api/runs endpoint end-to-end against a temp repo seeded with
// the bug scenario — a finished run plus a still-connectable batch
// socket — and asserts the row's Log comes from the Saved Run Log, not
// from the live socket's broadcast.
//
// Setup mirrors the PRD: the run is finished (status=success), but a
// sibling run lives in the same batch dir, so the batch daemon's
// batch.sock is still connectable and broadcasting sibling-run
// content. The completed row's Log field must contain the saved log's
// first and last lines and must not contain any of the live socket's
// lines, because the Saved Run Log is the authoritative record of a
// finished AgentRun per CONTEXT.md.
//
// Path coverage: the list endpoint (/api/runs no runKey) goes through
// runFromActiveMatch → runFromState; the keyed endpoint
// (/api/runs?runKey=<runID>) goes through runFromActiveBatchIssue →
// runFromState. Both paths must converge on the saved log for the
// kind=completed row.
func TestPortalRunsView_CompletedRunLogResolvesFromSavedFile(t *testing.T) {
	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return true }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)

	ts := "260101120000"
	shortid := "abcd"
	batchID := runid.NewBatchID(runid.KindIssue, 2, "1597", ts, shortid)
	rowRunID := runid.NewRunID(runid.KindIssue, "1597", ts, shortid)
	siblingRunID := runid.NewRunID(runid.KindIssue, "1598", ts, shortid)

	rawSavedLog := "[" + rowRunID + "] 13:31:05 > build · MiniMax-M3\n" +
		"[" + rowRunID + "] 13:31:30 > first saved line\n" +
		"[" + rowRunID + "] 13:32:00 > middle saved line\n" +
		"[" + rowRunID + "] 13:33:00 > last saved line\n"
	siblingLivePrefix := "[" + siblingRunID + "]"

	startedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: rowRunID, Issue: 1597, Payload: map[string]any{
			"branch":   "1597-fix",
			"batch_id": batchID,
		}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: rowRunID, Issue: 1597, Payload: map[string]any{
			"status":   "success",
			"branch":   "1597-fix",
			"batch_id": batchID,
		}},
	})

	batchDir := filepath.Join(layout.BatchesDir, batchID)
	runDir := filepath.Join(batchDir, "runs", rowRunID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte(rawSavedLog), 0644); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{1597, 1598}, BatchId: batchID, CreatedAt: startedAt}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	sockPath := filepath.Join(batchDir, "batch.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	siblingLive := siblingLivePrefix + " 13:49:33 sibling run live line\n" +
		siblingLivePrefix + " 13:49:35 another sibling live line\n"
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte(siblingLive))
			_ = conn.Close()
		}
	}()

	idx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Batches: []batchindex.Batch{
			{
				ID:        batchID,
				Path:      batchDir,
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusActive,
				CreatedAt: startedAt,
				Issues:    []int{1597, 1598},
			},
		},
	}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	// List endpoint: GET /api/runs (no runKey) goes through
	// runFromActiveMatch → runFromState for this row.
	listRuns := readPortalRuns(t, server.URL)
	var target *portalRun
	for i := range listRuns {
		if listRuns[i].RunID == rowRunID {
			target = &listRuns[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("expected list endpoint to include row %q, got %#v", rowRunID, listRuns)
	}
	assertCompletedRowUsesSavedLog(t, "list", target, rawSavedLog, siblingLivePrefix)

	// Keyed endpoint: GET /api/runs?runKey=<runID> goes through
	// runFromActiveBatchIssue → runFromState for this row. The keyed
	// contract is `{repoRoot, run}` (singular), not a list.
	keyedRun := readPortalRunByKey(t, server.URL, rowRunID)
	if keyedRun == nil {
		t.Fatalf("expected keyed endpoint to return row %q, got nil", rowRunID)
	}
	assertCompletedRowUsesSavedLog(t, "keyed", keyedRun, rawSavedLog, siblingLivePrefix)
}

// readPortalRunByKey hits the /api/runs?runKey=<runID> endpoint and
// returns the singular `run` payload. Returns nil when the endpoint
// returns a 4xx/5xx response (treated as "row not found").
func readPortalRunByKey(t *testing.T, baseURL, runKey string) *portalRun {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/runs?runKey=" + url.QueryEscape(runKey))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var payload struct {
		Run *portalRun `json:"run"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Run
}

func assertCompletedRowUsesSavedLog(t *testing.T, where string, row *portalRun, savedLog, siblingLivePrefix string) {
	t.Helper()
	if row.Kind != "completed" {
		t.Fatalf("%s: expected Kind %q, got %q", where, "completed", row.Kind)
	}
	if row.Status != "success" {
		t.Fatalf("%s: expected Status %q, got %q", where, "success", row.Status)
	}
	if !strings.Contains(row.Log, "13:31:05 > build · MiniMax-M3") {
		t.Fatalf("%s: expected Log to contain saved first line, got %q", where, row.Log)
	}
	if !strings.Contains(row.Log, "13:33:00 > last saved line") {
		t.Fatalf("%s: expected Log to contain saved last line, got %q", where, row.Log)
	}
	if strings.Contains(row.Log, siblingLivePrefix) {
		t.Fatalf("%s: expected Log NOT to contain sibling live prefix %q, got %q", where, siblingLivePrefix, row.Log)
	}
}
