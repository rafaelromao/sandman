package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/runid"
)

func newPortalArchiveHandlerForTest(t *testing.T, repoRoot string) http.Handler {
	t.Helper()
	return newPortalHandler(repoRoot)
}

func postPortalArchive(t *testing.T, handler http.Handler, runID string) (*http.Response, []byte) {
	t.Helper()
	server := startPortalHTTPServer(t, handler)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/archive", strings.NewReader(`{"runId":"`+runID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp, body
}

func postPortalArchiveRaw(t *testing.T, handler http.Handler, rawBody string) (*http.Response, []byte) {
	t.Helper()
	server := startPortalHTTPServer(t, handler)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/archive", strings.NewReader(rawBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp, body
}

// TestPortal_ArchiveEndpointMovesCompletedRunToArchiveDirectory is the
// tracer-bullet slice: a POST with {"runId": "<id>"} for a dead completed
// run returns 200 with {"runId": <id>, "status": "archived"}, and the batch
// directory is relocated from .sandman/batches/ to .sandman/archive/.
func TestPortal_ArchiveEndpointMovesCompletedRunToArchiveDirectory(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-ok-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runID := "abcd-260618113825-archive-ok"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", runID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "marker.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	batchManifest := batchindex.RunManifest{
		BatchID:   runID,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now()},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), runID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var payload struct {
		RunID  string `json:"runId"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal success body: %v: %s", err, body)
	}
	if payload.RunID != runID {
		t.Errorf("expected echoed runId %q, got %q", runID, payload.RunID)
	}
	if payload.Status != "archived" {
		t.Errorf("expected status %q, got %q (body=%s)", "archived", payload.Status, body)
	}

	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Fatalf("expected batch dir %q to be gone, stat err = %v", batchDir, err)
	}
	archivedDir := filepath.Join(repoRoot, ".sandman", "archive", runID)
	info, err := os.Stat(archivedDir)
	if err != nil {
		t.Fatalf("expected archive dir %q to exist, stat err = %v", archivedDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected archive target to be a directory, got mode %s", info.Mode())
	}
	marker, err := os.ReadFile(filepath.Join(archivedDir, "marker.txt"))
	if err != nil {
		t.Fatalf("expected marker file to follow the move: %v", err)
	}
	if string(marker) != "hello" {
		t.Fatalf("expected marker contents to follow the move, got %q", marker)
	}
}

// TestPortal_ArchiveEndpoint_RejectsActiveRun covers slice #2: the liveness
// check fires before the move; the response is 409 with a message the JS
// error banner can surface, and the directory stays under .sandman/batches/.
func TestPortal_ArchiveEndpoint_RejectsActiveRun(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-active-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runID := "abcd-260618113825-archive-active"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", runID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}

	batchManifest := batchindex.RunManifest{
		BatchID:   runID,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now()},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return true }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	originalArchiver := portalRunArchiver
	portalRunArchiver = func(string, string) error {
		t.Fatalf("archiver must not be called for an active run")
		return nil
	}
	t.Cleanup(func() { portalRunArchiver = originalArchiver })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), runID)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for active run, got %d: %s", resp.StatusCode, body)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal error body: %v: %s", err, body)
	}
	if !strings.Contains(strings.ToLower(payload.Error), "active") {
		t.Errorf("expected error message to mention 'active', got %q", payload.Error)
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Fatalf("expected batch dir %q to stay in place for active run, stat err = %v", batchDir, err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".sandman", "archive", runID)); !os.IsNotExist(err) {
		t.Fatalf("expected no archive dir for active run, stat err = %v", err)
	}
}

// TestPortal_ArchiveEndpoint_RejectsAlreadyArchivedRun covers slice #3: a
// pre-existing .sandman/archive/<id> blocks the move and the existing
// archive is left untouched.
func TestPortal_ArchiveEndpoint_RejectsAlreadyArchivedRun(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-dup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runID := "abcd-260618113825-archive-dup"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", runID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	batchManifest := batchindex.RunManifest{
		BatchID:   runID,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now()},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	archiveDir := filepath.Join(repoRoot, ".sandman", "archive", runID)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archiveDir, "previous.txt"), []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	originalArchiver := portalRunArchiver
	portalRunArchiver = func(string, string) error {
		t.Fatalf("archiver must not be called when archive already exists")
		return nil
	}
	t.Cleanup(func() { portalRunArchiver = originalArchiver })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), runID)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for already-archived run, got %d: %s", resp.StatusCode, body)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal error body: %v: %s", err, body)
	}
	if !strings.Contains(strings.ToLower(payload.Error), "archiv") {
		t.Errorf("expected error message to mention 'archive', got %q", payload.Error)
	}

	if _, err := os.Stat(batchDir); err != nil {
		t.Fatalf("expected original batch dir %q to be untouched, stat err = %v", batchDir, err)
	}
	previous, err := os.ReadFile(filepath.Join(archiveDir, "previous.txt"))
	if err != nil {
		t.Fatalf("expected previous archive contents to survive: %v", err)
	}
	if string(previous) != "keep me" {
		t.Fatalf("expected previous archive contents to survive, got %q", previous)
	}
}

// TestPortal_ArchiveEndpoint_Returns404ForMissingRun covers slice #4: a
// run id with no directory under .sandman/runs/ returns 404, and the
// archiver is never called.
func TestPortal_ArchiveEndpoint_Returns404ForMissingRun(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	originalArchiver := portalRunArchiver
	portalRunArchiver = func(string, string) error {
		t.Fatalf("archiver must not be called for a missing run")
		return nil
	}
	t.Cleanup(func() { portalRunArchiver = originalArchiver })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), "abcd-260618113825-archive-missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing run, got %d: %s", resp.StatusCode, body)
	}
}

// TestPortal_ArchiveEndpoint_ValidatesRunID covers input validation: a
// missing or whitespace-only runId returns 400 and never touches the
// filesystem.
func TestPortal_ArchiveEndpoint_ValidatesRunID(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	originalArchiver := portalRunArchiver
	portalRunArchiver = func(string, string) error {
		t.Fatalf("archiver must not be called with empty runId")
		return nil
	}
	t.Cleanup(func() { portalRunArchiver = originalArchiver })

	for _, rawBody := range []string{`{"runId":""}`, `{"runId":"   "}`, `{}`} {
		resp, body := postPortalArchiveRaw(t, newPortalArchiveHandlerForTest(t, repoRoot), rawBody)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for body %q, got %d: %s", rawBody, resp.StatusCode, body)
		}
	}
}

// TestPortal_ArchiveEndpoint_OnlyAffectsPOST ensures the route does not
// accept other HTTP methods (mirrors the abort endpoint's method check).
func TestPortal_ArchiveEndpoint_OnlyAffectsPOST(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalArchiveHandlerForTest(t, repoRoot))
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequest(method, server.URL+"/api/runs/archive", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405 for %s, got %d", method, resp.StatusCode)
		}
	}
}

// TestPortal_ArchiveEndpoint_SurfaceArchivedFlagInRunsAPI covers slice #5
// at the API level: after a successful archive, the next /api/runs poll
// surfaces `archived: true` for that row.
func TestPortal_ArchiveEndpoint_SurfaceArchivedFlagInRunsAPI(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-flag-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runID := "abcd-260618113825-archive-flag"
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", runID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	batchManifest := batchindex.RunManifest{
		BatchID:   runID,
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: started,
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: started, Issues: []int{42}},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: started, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: started.Add(time.Minute), RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	server := startPortalHTTPServer(t, newPortalArchiveHandlerForTest(t, repoRoot))

	before := readPortalRuns(t, server.URL)
	var found *portalRun
	for i := range before {
		if before[i].RunID == runID {
			found = &before[i]
		}
	}
	if found == nil {
		t.Fatalf("expected run %q in /api/runs before archive, got %#v", runID, before)
	}
	if found.Archived {
		t.Fatalf("expected Archived=false before archive, got %#v", found)
	}

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/runs/archive", strings.NewReader(`{"runId":"`+runID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive POST: expected 200, got %d: %s", resp.StatusCode, body)
	}

	after := readPortalRuns(t, server.URL)
	var foundAfter *portalRun
	for i := range after {
		if after[i].RunID == runID {
			foundAfter = &after[i]
		}
	}
	if foundAfter == nil {
		t.Fatalf("expected run %q in /api/runs after archive, got %#v", runID, after)
	}
	if !foundAfter.Archived {
		t.Fatalf("expected Archived=true after archive, got %#v", foundAfter)
	}
}

// TestPortal_ArchiveEndpoint_EndToEndRealRunIDToDirName covers the
// contract that the portal UI's per-row run id (the row RunID the
// events log emits) drives a successful archive even when the per-row
// id differs from the .sandman/batches/<dir> directory name. The
// fixture uses runid.NewBatchID + runid.NewRunID so the two ids
// reflect real-world shapes; a regression where the endpoint silently
// only accepts the batch entry id would 404 in production. The
// archive directory name is the batch entry id (not the per-row id),
// matching today's behaviour.
//
// Uses n=2 (multi-issue) so the public BatchId carries +1 and differs
// from the per-row RunID; single-issue batches use the public BatchId
// = per-row RunID shape (issue #1917 slice 1).
func TestPortal_ArchiveEndpoint_EndToEndRealRunIDToDirName(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := "260618113825"
	shortid := "abcd"
	batchEntryID := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	perRowID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)
	if perRowID == batchEntryID {
		t.Fatalf("fixture invariant: per-row id %q must differ from batch entry id %q", perRowID, batchEntryID)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "log.txt"), []byte("run output"), 0644); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	batchManifest := batchindex.RunManifest{
		BatchID:   batchEntryID,
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: started,
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}
	perRowDir := filepath.Join(batchDir, "runs", perRowID)
	if err := os.MkdirAll(perRowDir, 0755); err != nil {
		t.Fatal(err)
	}
	perRowManifest := batchindex.RunManifest{
		RunID:     perRowID,
		BatchID:   batchEntryID,
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: started,
		Status:    batchindex.RunManifestStatusSuccess,
	}
	if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
		t.Fatal(err)
	}
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: started, Issues: []int{42}},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: started, RunID: perRowID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: started.Add(time.Minute), RunID: perRowID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for per-row id %q, got %d: %s", perRowID, resp.StatusCode, body)
	}

	var payload struct {
		RunID  string `json:"runId"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal success body: %v: %s", err, body)
	}
	if payload.RunID != batchEntryID {
		t.Errorf("expected echoed runId %q (batch entry id), got %q", batchEntryID, payload.RunID)
	}

	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Fatalf("expected batch dir %q to be gone after archive, stat err = %v", batchDir, err)
	}
	archivedDir := filepath.Join(repoRoot, ".sandman", "archive", batchEntryID)
	info, err := os.Stat(archivedDir)
	if err != nil {
		t.Fatalf("expected archive dir %q to exist, stat err = %v", archivedDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected archive target to be a directory, got mode %s", info.Mode())
	}
	log, err := os.ReadFile(filepath.Join(archivedDir, "log.txt"))
	if err != nil {
		t.Fatalf("expected log.txt to follow the move: %v", err)
	}
	if string(log) != "run output" {
		t.Fatalf("expected log contents to follow the move, got %q", log)
	}
}

// TestPortal_ArchiveEndpoint_ResolvesPerRowRunIDToBatchEntryID is the
// tracer-bullet slice for issue #1674: a POST whose runId is the per-row
// run id (the shape the portal UI sends) resolves to the batch index
// entry whose runs/<id>/run.json exists on disk, even when the per-row
// id differs from the batch entry id. The archive dir name uses the
// batch entry id, matching today's behaviour, and the response echoes
// the batch entry id rather than the raw request body.
func TestPortal_ArchiveEndpoint_ResolvesPerRowRunIDToBatchEntryID(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-perrow-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := "260618113825"
	shortid := "abcd"
	batchEntryID := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	perRowID := runid.NewRunID(runid.KindIssue, "43", ts, shortid)
	if perRowID == batchEntryID {
		t.Fatalf("fixture invariant: perRowID %q must differ from batchEntryID %q", perRowID, batchEntryID)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "marker.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	perRowDir := filepath.Join(batchDir, "runs", perRowID)
	if err := os.MkdirAll(perRowDir, 0755); err != nil {
		t.Fatal(err)
	}
	perRowManifest := batchindex.RunManifest{
		RunID:     perRowID,
		BatchID:   batchEntryID,
		Issue:     43,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusSuccess,
	}
	if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
		t.Fatal(err)
	}

	batchManifest := batchindex.RunManifest{
		BatchID:   batchEntryID,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42, 43}},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for per-row id %q, got %d: %s", perRowID, resp.StatusCode, body)
	}

	var payload struct {
		RunID  string `json:"runId"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal success body: %v: %s", err, body)
	}
	if payload.RunID != batchEntryID {
		t.Errorf("expected echoed runId %q (batch entry id), got %q", batchEntryID, payload.RunID)
	}
	if payload.Status != "archived" {
		t.Errorf("expected status %q, got %q (body=%s)", "archived", payload.Status, body)
	}

	if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
		t.Fatalf("expected batch dir %q to be gone, stat err = %v", batchDir, err)
	}
	archivedDir := filepath.Join(repoRoot, ".sandman", "archive", batchEntryID)
	info, err := os.Stat(archivedDir)
	if err != nil {
		t.Fatalf("expected archive dir %q to exist, stat err = %v", archivedDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected archive target to be a directory, got mode %s", info.Mode())
	}
	marker, err := os.ReadFile(filepath.Join(archivedDir, "marker.txt"))
	if err != nil {
		t.Fatalf("expected marker file to follow the move: %v", err)
	}
	if string(marker) != "hello" {
		t.Fatalf("expected marker contents to follow the move, got %q", marker)
	}
}

// TestPortal_ArchiveEndpoint_MultiIssuePerRowIDsResolveToSameEntry locks in
// the multi-issue acceptance criterion: more than one per-row run id in the
// same batch is addressable independently, and each resolves to the same
// batch index entry. Posts the first per-row id, asserts the archive moves
// the batch dir, and then constructs a second batch fixture where the same
// batch is addressed via the second per-row id and verifies the result is
// identical.
func TestPortal_ArchiveEndpoint_MultiIssuePerRowIDsResolveToSameEntry(t *testing.T) {
	ts := "260618113825"
	shortid := "abcd"
	batchEntryID := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	firstPerRowID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)
	secondPerRowID := runid.NewRunID(runid.KindIssue, "43", ts, shortid)
	if firstPerRowID == batchEntryID || secondPerRowID == batchEntryID || firstPerRowID == secondPerRowID {
		t.Fatalf("fixture invariant: ids must all differ (batch=%q first=%q second=%q)", batchEntryID, firstPerRowID, secondPerRowID)
	}

	for _, perRowID := range []string{firstPerRowID, secondPerRowID} {
		t.Run(perRowID, func(t *testing.T) {
			repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-multi-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
			if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
				t.Fatal(err)
			}

			batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
			if err := os.MkdirAll(batchDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(batchDir, "marker.txt"), []byte("hello"), 0644); err != nil {
				t.Fatal(err)
			}

			for _, id := range []string{firstPerRowID, secondPerRowID} {
				perRowDir := filepath.Join(batchDir, "runs", id)
				if err := os.MkdirAll(perRowDir, 0755); err != nil {
					t.Fatal(err)
				}
				issueNum := 42
				if id == secondPerRowID {
					issueNum = 43
				}
				perRowManifest := batchindex.RunManifest{
					RunID:     id,
					BatchID:   batchEntryID,
					Issue:     issueNum,
					Kind:      batchindex.KindIssue,
					CreatedAt: time.Now(),
					Status:    batchindex.RunManifestStatusSuccess,
				}
				if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
					t.Fatal(err)
				}
			}

			batchManifest := batchindex.RunManifest{
				BatchID:   batchEntryID,
				Kind:      batchindex.KindIssue,
				CreatedAt: time.Now(),
				Status:    batchindex.RunManifestStatusActive,
			}
			if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
				t.Fatal(err)
			}

			idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
				{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42, 43}},
			}}
			data, _ := json.Marshal(idx)
			if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
				t.Fatal(err)
			}

			originalProbe := portalRunLivenessProbe
			portalRunLivenessProbe = func(string) bool { return false }
			t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

			resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200 for per-row id %q, got %d: %s", perRowID, resp.StatusCode, body)
			}

			var payload struct {
				RunID  string `json:"runId"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("unmarshal: %v: %s", err, body)
			}
			if payload.RunID != batchEntryID {
				t.Errorf("expected echoed runId %q (batch entry id), got %q", batchEntryID, payload.RunID)
			}
			if payload.Status != "archived" {
				t.Errorf("expected status %q, got %q", "archived", payload.Status)
			}

			if _, err := os.Stat(batchDir); !os.IsNotExist(err) {
				t.Fatalf("expected batch dir %q to be gone, stat err = %v", batchDir, err)
			}
			archivedDir := filepath.Join(repoRoot, ".sandman", "archive", batchEntryID)
			info, err := os.Stat(archivedDir)
			if err != nil {
				t.Fatalf("expected archive dir %q to exist, stat err = %v", archivedDir, err)
			}
			if !info.IsDir() {
				t.Fatalf("expected archive target to be a directory, got mode %s", info.Mode())
			}
		})
	}
}

// for a continue review (review that picked up an existing issue link
// from a follow-up comment): the per-row id carries the linked issue
// subject while the batch entry id is the PR-shaped template. Posting
// the per-row id resolves through runs/<id>/run.json and archives the
// batch dir under the entry id.
func TestPortal_ArchiveEndpoint_ContinueReview(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-creview-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := "260618113825"
	shortid := "abcd"
	batchEntryID := runid.NewBatchID(runid.KindReview, 1, "99", ts, shortid)
	perRowID := runid.NewRunID(runid.KindReview, "42-PR99", ts, shortid)
	if perRowID == batchEntryID {
		t.Fatalf("fixture invariant: perRowID %q must differ from batchEntryID %q", perRowID, batchEntryID)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "marker.txt"), []byte("review-marker"), 0644); err != nil {
		t.Fatal(err)
	}

	perRowDir := filepath.Join(batchDir, "runs", perRowID)
	if err := os.MkdirAll(perRowDir, 0755); err != nil {
		t.Fatal(err)
	}
	perRowManifest := batchindex.RunManifest{
		RunID:     perRowID,
		BatchID:   batchEntryID,
		PR:        99,
		Issue:     42,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusSuccess,
	}
	if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
		t.Fatal(err)
	}

	batchManifest := batchindex.RunManifest{
		BatchID:   batchEntryID,
		PR:        99,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindReview, Status: batchindex.StatusActive, CreatedAt: time.Now(), PR: 99},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for per-row id %q, got %d: %s", perRowID, resp.StatusCode, body)
	}

	var payload struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	if payload.RunID != batchEntryID {
		t.Errorf("expected echoed runId %q (batch entry id), got %q", batchEntryID, payload.RunID)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".sandman", "archive", batchEntryID)); err != nil {
		t.Fatalf("expected archive dir %q to exist, stat err = %v", batchEntryID, err)
	}
}

// TestPortal_ArchiveEndpoint_OrphanReview covers the per-row probe path
// for an orphan review (PR with no linked issue): the per-row id
// includes the issue subject as 0-PR<N> while the batch entry id is
// PR<N>. Posting the per-row id resolves through runs/<id>/run.json.
func TestPortal_ArchiveEndpoint_OrphanReview(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-oreview-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := "260618113825"
	shortid := "abcd"
	batchEntryID := runid.NewBatchID(runid.KindReview, 1, "100", ts, shortid)
	perRowID := runid.NewRunID(runid.KindReview, "0-PR100", ts, shortid)
	if perRowID == batchEntryID {
		t.Fatalf("fixture invariant: perRowID %q must differ from batchEntryID %q", perRowID, batchEntryID)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "marker.txt"), []byte("orphan-review-marker"), 0644); err != nil {
		t.Fatal(err)
	}

	perRowDir := filepath.Join(batchDir, "runs", perRowID)
	if err := os.MkdirAll(perRowDir, 0755); err != nil {
		t.Fatal(err)
	}
	perRowManifest := batchindex.RunManifest{
		RunID:     perRowID,
		BatchID:   batchEntryID,
		PR:        100,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusSuccess,
	}
	if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
		t.Fatal(err)
	}

	batchManifest := batchindex.RunManifest{
		BatchID:   batchEntryID,
		PR:        100,
		Kind:      batchindex.KindReview,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindReview, Status: batchindex.StatusActive, CreatedAt: time.Now(), PR: 100},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for per-row id %q, got %d: %s", perRowID, resp.StatusCode, body)
	}

	if _, err := os.Stat(filepath.Join(repoRoot, ".sandman", "archive", batchEntryID)); err != nil {
		t.Fatalf("expected archive dir %q to exist, stat err = %v", batchEntryID, err)
	}
}

// TestPortal_ArchiveEndpoint_SingleIssueRun covers the per-row probe path
// for a single-issue issue run: even when n=1 the batch entry id carries
// the "+1" suffix (NewBatchID template) while the per-row id drops it.
// The on-disk probe resolves the per-row id through runs/<id>/run.json.
func TestPortal_ArchiveEndpoint_SingleIssueRun(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-single-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := "260618113825"
	shortid := "abcd"
	batchEntryID := runid.NewBatchID(runid.KindIssue, 1, "42", ts, shortid)
	perRowID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}

	perRowDir := filepath.Join(batchDir, "runs", perRowID)
	if err := os.MkdirAll(perRowDir, 0755); err != nil {
		t.Fatal(err)
	}
	perRowManifest := batchindex.RunManifest{
		RunID:     perRowID,
		BatchID:   batchEntryID,
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusSuccess,
	}
	if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
		t.Fatal(err)
	}

	batchManifest := batchindex.RunManifest{
		BatchID:   batchEntryID,
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42}},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".sandman", "archive", batchEntryID)); err != nil {
		t.Fatalf("expected archive dir %q to exist, stat err = %v", batchEntryID, err)
	}
}

// TestPortal_ArchiveEndpoint_ContinueIssueRun covers the --continue flag
// path on a multi-issue issue run: the orchestrator resumes the existing
// batch dir, so the per-row id matches the batch entry id (the
// orchestrator picks the first subject as the resume row). The fast
// path (idx.Resolve) resolves either id form to the same entry.
func TestPortal_ArchiveEndpoint_ContinueIssueRun(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-cissue-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := "260618113825"
	shortid := "abcd"
	batchEntryID := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	perRowID := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	if perRowID != batchEntryID {
		t.Fatalf("fixture invariant: --continue perRowID %q must equal batchEntryID %q", perRowID, batchEntryID)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}

	batchManifest := batchindex.RunManifest{
		BatchID:   batchEntryID,
		Issue:     42,
		Kind:      batchindex.KindIssue,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42, 43}},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".sandman", "archive", batchEntryID)); err != nil {
		t.Fatalf("expected archive dir %q to exist, stat err = %v", batchEntryID, err)
	}
}

// TestPortal_ArchiveEndpoint_AutoSelectRun covers sandman run --auto:
// the per-row id equals the batch entry id (no +N suffix for the auto
// template) and the exact-match path resolves it.
func TestPortal_ArchiveEndpoint_AutoSelectRun(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-auto-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := "260618113825"
	shortid := "abcd"
	batchEntryID := runid.NewBatchID(runid.KindAutoSelect, 3, "", ts, shortid)
	perRowID := runid.NewRunID(runid.KindAutoSelect, "auto-3", ts, shortid)
	if perRowID != batchEntryID {
		t.Fatalf("fixture invariant: perRowID %q must equal batchEntryID %q for auto-select", perRowID, batchEntryID)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}

	batchManifest := batchindex.RunManifest{
		BatchID:   batchEntryID,
		Kind:      batchindex.KindAutoSelect,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}
	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindAutoSelect, Status: batchindex.StatusActive, CreatedAt: time.Now()},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".sandman", "archive", batchEntryID)); err != nil {
		t.Fatalf("expected archive dir %q to exist, stat err = %v", batchEntryID, err)
	}
}

// TestPortal_ArchiveEndpoint_PromptOnlyRun covers sandman run --prompt:
// per issue #1920 (slice 4 of #1916), the per-row RunID equals the
// public BatchId, which is "<shortid>-<ts>-prompt-<userid>" (or
// "<shortid>-<ts>-prompt" without a userid). The on-disk probe resolves
// the per-row id through runs/<runID>/run.json, so the per-row dir
// basename is the same canonical id the batch folder uses.
func TestPortal_ArchiveEndpoint_PromptOnlyRun(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-prompt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ts := "260618113825"
	shortid := "abcd"
	userID := "my-fix-task"
	publicBatchID := runid.NewBatchID(runid.KindPromptOnly, 1, userID, ts, shortid)
	perRowID := runid.NewRunID(runid.KindPromptOnly, userID, ts, shortid)
	// RunID == public BatchId for prompt-only (issue #1920 slice 4).
	if perRowID != publicBatchID {
		t.Fatalf("fixture invariant: perRowID %q must equal publicBatchID %q (RunID == public BatchId for prompt-only)", perRowID, publicBatchID)
	}

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", publicBatchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "marker.txt"), []byte("prompt-marker"), 0644); err != nil {
		t.Fatal(err)
	}

	perRowDir := filepath.Join(batchDir, "runs", perRowID)
	if err := os.MkdirAll(perRowDir, 0755); err != nil {
		t.Fatal(err)
	}
	perRowManifest := batchindex.RunManifest{
		RunID:     perRowID,
		BatchID:   publicBatchID,
		Kind:      batchindex.KindPromptOnly,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusSuccess,
	}
	if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
		t.Fatal(err)
	}

	batchManifest := batchindex.RunManifest{
		BatchID:   publicBatchID,
		Kind:      batchindex.KindPromptOnly,
		CreatedAt: time.Now(),
		Status:    batchindex.RunManifestStatusActive,
	}
	if err := batchindex.WriteManifest(batchDir, batchManifest); err != nil {
		t.Fatal(err)
	}

	idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: publicBatchID, Path: batchDir, Kind: batchindex.KindPromptOnly, Status: batchindex.StatusActive, CreatedAt: time.Now()},
	}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for per-row id %q, got %d: %s", perRowID, resp.StatusCode, body)
	}

	if _, err := os.Stat(filepath.Join(repoRoot, ".sandman", "archive", publicBatchID)); err != nil {
		t.Fatalf("expected archive dir %q to exist, stat err = %v", publicBatchID, err)
	}
	marker, err := os.ReadFile(filepath.Join(repoRoot, ".sandman", "archive", publicBatchID, "marker.txt"))
	if err != nil {
		t.Fatalf("expected marker file to follow the move: %v", err)
	}
	if string(marker) != "prompt-marker" {
		t.Fatalf("expected prompt marker, got %q", marker)
	}
}

// TestPortal_ArchiveEndpoint_PerRowIDPreservesErrorStatuses verifies the
// per-row probe path preserves the same status codes and messages the
// existing tests already lock in when the request body uses the per-row
// id rather than the batch entry id. 404 (no batch), 409 (active),
// 409 (already archived).
func TestPortal_ArchiveEndpoint_PerRowIDPreservesErrorStatuses(t *testing.T) {
	ts := "260618113825"
	shortid := "abcd"
	batchEntryID := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	perRowID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)

	t.Run("404 when per-row id has no batch", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		originalArchiver := portalRunArchiver
		portalRunArchiver = func(string, string) error {
			t.Fatalf("archiver must not be called for a missing run")
			return nil
		}
		t.Cleanup(func() { portalRunArchiver = originalArchiver })

		resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("409 active when per-row id is active", func(t *testing.T) {
		repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-active-probe-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
		if err := os.MkdirAll(batchDir, 0755); err != nil {
			t.Fatal(err)
		}
		manifest := batchindex.RunManifest{
			BatchID:   batchEntryID,
			Kind:      batchindex.KindIssue,
			CreatedAt: time.Now(),
			Status:    batchindex.RunManifestStatusActive,
		}
		if err := batchindex.WriteManifest(batchDir, manifest); err != nil {
			t.Fatal(err)
		}
		perRowDir := filepath.Join(batchDir, "runs", perRowID)
		if err := os.MkdirAll(perRowDir, 0755); err != nil {
			t.Fatal(err)
		}
		perRowManifest := batchindex.RunManifest{
			RunID:     perRowID,
			BatchID:   batchEntryID,
			Issue:     42,
			Kind:      batchindex.KindIssue,
			CreatedAt: time.Now(),
			Status:    batchindex.RunManifestStatusSuccess,
		}
		if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
			t.Fatal(err)
		}
		idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
			{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42, 43}},
		}}
		data, _ := json.Marshal(idx)
		if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
			t.Fatal(err)
		}

		originalProbe := portalRunLivenessProbe
		portalRunLivenessProbe = func(string) bool { return true }
		t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

		originalArchiver := portalRunArchiver
		portalRunArchiver = func(string, string) error {
			t.Fatalf("archiver must not be called for an active run")
			return nil
		}
		t.Cleanup(func() { portalRunArchiver = originalArchiver })

		resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for active run, got %d: %s", resp.StatusCode, body)
		}
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal error body: %v: %s", err, body)
		}
		if !strings.Contains(strings.ToLower(payload.Error), "active") {
			t.Errorf("expected error to mention 'active', got %q", payload.Error)
		}
	})

	t.Run("409 already archived when archive dir exists", func(t *testing.T) {
		repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-dup-probe-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
		if err := os.MkdirAll(batchDir, 0755); err != nil {
			t.Fatal(err)
		}
		manifest := batchindex.RunManifest{
			BatchID:   batchEntryID,
			Kind:      batchindex.KindIssue,
			CreatedAt: time.Now(),
			Status:    batchindex.RunManifestStatusActive,
		}
		if err := batchindex.WriteManifest(batchDir, manifest); err != nil {
			t.Fatal(err)
		}
		perRowDir := filepath.Join(batchDir, "runs", perRowID)
		if err := os.MkdirAll(perRowDir, 0755); err != nil {
			t.Fatal(err)
		}
		perRowManifest := batchindex.RunManifest{
			RunID:     perRowID,
			BatchID:   batchEntryID,
			Issue:     42,
			Kind:      batchindex.KindIssue,
			CreatedAt: time.Now(),
			Status:    batchindex.RunManifestStatusSuccess,
		}
		if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
			t.Fatal(err)
		}
		idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
			{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42, 43}},
		}}
		data, _ := json.Marshal(idx)
		if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
			t.Fatal(err)
		}
		archiveDir := filepath.Join(repoRoot, ".sandman", "archive", batchEntryID)
		if err := os.MkdirAll(archiveDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(archiveDir, "previous.txt"), []byte("keep me"), 0644); err != nil {
			t.Fatal(err)
		}

		originalProbe := portalRunLivenessProbe
		portalRunLivenessProbe = func(string) bool { return false }
		t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

		originalArchiver := portalRunArchiver
		portalRunArchiver = func(string, string) error {
			t.Fatalf("archiver must not be called when archive already exists")
			return nil
		}
		t.Cleanup(func() { portalRunArchiver = originalArchiver })

		resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for already-archived run, got %d: %s", resp.StatusCode, body)
		}
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal error body: %v: %s", err, body)
		}
		if !strings.Contains(strings.ToLower(payload.Error), "archiv") {
			t.Errorf("expected error to mention 'archive', got %q", payload.Error)
		}
	})

	t.Run("500 when archiver fails", func(t *testing.T) {
		repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-500-probe-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
		if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
			t.Fatal(err)
		}

		batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchEntryID)
		if err := os.MkdirAll(batchDir, 0755); err != nil {
			t.Fatal(err)
		}
		manifest := batchindex.RunManifest{
			BatchID:   batchEntryID,
			Kind:      batchindex.KindIssue,
			CreatedAt: time.Now(),
			Status:    batchindex.RunManifestStatusActive,
		}
		if err := batchindex.WriteManifest(batchDir, manifest); err != nil {
			t.Fatal(err)
		}
		perRowDir := filepath.Join(batchDir, "runs", perRowID)
		if err := os.MkdirAll(perRowDir, 0755); err != nil {
			t.Fatal(err)
		}
		perRowManifest := batchindex.RunManifest{
			RunID:     perRowID,
			BatchID:   batchEntryID,
			Issue:     42,
			Kind:      batchindex.KindIssue,
			CreatedAt: time.Now(),
			Status:    batchindex.RunManifestStatusSuccess,
		}
		if err := batchindex.WriteManifest(perRowDir, perRowManifest); err != nil {
			t.Fatal(err)
		}
		idx := batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
			{ID: batchEntryID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{42, 43}},
		}}
		data, _ := json.Marshal(idx)
		if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "batches.json"), data, 0644); err != nil {
			t.Fatal(err)
		}

		originalProbe := portalRunLivenessProbe
		portalRunLivenessProbe = func(string) bool { return false }
		t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

		originalArchiver := portalRunArchiver
		portalRunArchiver = func(string, string) error {
			return fmt.Errorf("synthetic archiver failure")
		}
		t.Cleanup(func() { portalRunArchiver = originalArchiver })

		resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), perRowID)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected 500 for archiver failure, got %d: %s", resp.StatusCode, body)
		}
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal error body: %v: %s", err, body)
		}
		if !strings.Contains(strings.ToLower(payload.Error), "synthetic archiver failure") {
			t.Errorf("expected error to surface archiver message, got %q", payload.Error)
		}
	})
}
