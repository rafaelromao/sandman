package cmd

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
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
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix", "batch_id": batchID}},
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
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix", "batch_id": batchID}},
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
		Entries: []batchindex.Entry{
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
