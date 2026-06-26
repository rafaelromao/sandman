package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/runid"
)

// TestPortalRunsView_UnavailableFlagFromBatchIndex covers slice #1 of #1312:
// a row whose batchindex entry is marked StatusUnavailable appears in
// /api/runs JSON with `unavailable: true` and `archived: false`.
func TestPortalRunsView_UnavailableFlagFromBatchIndex(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)

	// Use realistic runid output: batch ID and per-row RunID differ,
	// exercising the sourceDirID-based lookup.
	ts := "20250101T100000Z"
	shortid := "abcd"
	batchID := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	rowRunID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)

	startedAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	writePortalLog(t, filepath.Join(layout.EventsLogPath), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: rowRunID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: rowRunID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	// Create a dead batch directory with a runs/ subdirectory so that
	// BatchKey gets populated from the row-to-batch reverse lookup.
	batchDir := filepath.Join(layout.BatchesDir, batchID)
	if err := os.MkdirAll(filepath.Join(batchDir, "runs", rowRunID), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "manifest.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Seed the batch index with a StatusUnavailable entry keyed by the
	// batch ID. The row's RunID differs (no +N suffix), so the lookup
	// must go through BatchKey → sourceDirID to find the match.
	batchIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:        batchID,
				Path:      batchDir,
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusUnavailable,
				CreatedAt: startedAt,
				Issues:    []int{42},
			},
		},
	}
	if err := batchIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	if len(payload.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(payload.Runs))
	}
	got := payload.Runs[0]
	if !got.Unavailable {
		t.Fatalf("expected Unavailable=true for batchindex StatusUnavailable entry, got %+v", got)
	}
	if got.Archived {
		t.Fatalf("expected Archived=false for unavailable entry, got %+v", got)
	}

	// Round-trip the run through JSON to confirm the wire contract carries
	// the field too. The /api/runs handler is what the portal actually
	// reads; this guards against a struct change that hides the field.
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(wire, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["unavailable"] != true {
		t.Fatalf("expected wire JSON to carry unavailable=true, got %v", raw["unavailable"])
	}
	if _, ok := raw["archived"]; !ok {
		t.Fatalf("expected wire JSON to carry archived key (always-present contract), got %v", raw)
	}
	if raw["archived"] != false {
		t.Fatalf("expected wire JSON to carry archived=false, got %v", raw["archived"])
	}
}

// TestPortalRunsView_IssueRunLogFromCorrectRunFolder is the regression test
// for #1416: for an issue run where batchID ≠ runID, run.Log must equal the
// contents of <batchDir>/runs/<runID>/run.log, not <batchDir>/runs/<batchID>/run.log.
// The portal previously used the same string for both path segments, causing
// log-loss and flag inconsistencies for issue batches.
//
// The batch directory is named shortid-ts (e.g. abcd-20250101T100000Z) while
// per-run folders are shortid-ts-subject (e.g. abcd-20250101T100000Z-42).
// This matches the structure proven in orchestrator_test.go:1983:
// batches/<sid>-<ts>/runs/<sid>-<ts>-42/run.log
func TestPortalRunsView_IssueRunLogFromCorrectRunFolder(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)

	ts := "20250101T100000Z"
	shortid := "abcd"
	batchID := shortid + "-" + ts
	rowRunID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)

	rawLogContent := "[issue-42] 10:01:00 Test output line 1\n[issue-42] 10:01:05 Test output line 2\n"
	wantLogContent := "10:01:00 Test output line 1\n10:01:05 Test output line 2\n"

	startedAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	writePortalLog(t, filepath.Join(layout.EventsLogPath), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: rowRunID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: rowRunID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	batchDir := filepath.Join(layout.BatchesDir, batchID)
	runDir := filepath.Join(batchDir, "runs", rowRunID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "manifest.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte(rawLogContent), 0644); err != nil {
		t.Fatal(err)
	}

	batchIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
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
	if err := batchIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	if len(payload.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(payload.Runs))
	}
	got := payload.Runs[0]

	if got.Log != wantLogContent {
		t.Fatalf("run.Log = %q, want %q (log content from correct <batchDir>/runs/<runID>/run.log)", got.Log, wantLogContent)
	}

	if !got.SourceExists {
		t.Fatalf("expected SourceExists=true for real <batchDir>/runs/<runID> folder, got %+v", got)
	}

	if got.Archived {
		t.Fatalf("expected Archived=false for active batch, got %+v", got)
	}
}

// TestPortalRunsView_IssueRunLogDownload returns 200 — this is the regression test
// for #1417: the /api/logs download URL for a completed issue run must
// return 200 and the raw file, not 404. The bug was that runFromState
// derived batchID from runID via batchIDFromRunID, which produced the wrong
// path for issue runs where batchID ≠ runID.
func TestPortalRunsView_IssueRunLogDownload(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)

	ts := "20250101T100000Z"
	shortid := "abcd"
	batchID := shortid + "-" + ts
	rowRunID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)

	rawLogContent := "[issue-42] 10:01:00 Test output line 1\n[issue-42] 10:01:05 Test output line 2\n"

	startedAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	writePortalLog(t, filepath.Join(layout.EventsLogPath), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: rowRunID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: rowRunID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	batchDir := filepath.Join(layout.BatchesDir, batchID)
	runDir := filepath.Join(batchDir, "runs", rowRunID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "manifest.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte(rawLogContent), 0644); err != nil {
		t.Fatal(err)
	}

	batchIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
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
	if err := batchIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /api/runs, got %d", resp.StatusCode)
	}
	var payload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	if len(payload.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(payload.Runs))
	}
	gotRun := payload.Runs[0]

	if gotRun.LogURL == "" {
		t.Fatal("expected non-empty LogURL for completed issue run with log file")
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
		t.Fatalf("log download body = %q, want %q (raw unstripped content)", string(body), rawLogContent)
	}
}
