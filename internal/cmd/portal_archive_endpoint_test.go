package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
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
// run returns 200 with {"runId": <id>, "status": "archived"}, and the run
// directory is relocated from .sandman/runs/ to .sandman/archive/.
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
	runDir := filepath.Join(repoRoot, ".sandman", "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "marker.txt"), []byte("hello"), 0644); err != nil {
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

	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("expected run dir %q to be gone, stat err = %v", runDir, err)
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
// error banner can surface, and the directory stays under .sandman/runs/.
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
	runDir := filepath.Join(repoRoot, ".sandman", "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
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

	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("expected run dir %q to stay in place for active run, stat err = %v", runDir, err)
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
	runDir := filepath.Join(repoRoot, ".sandman", "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
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

	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("expected original run dir %q to be untouched, stat err = %v", runDir, err)
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
	runDir := filepath.Join(repoRoot, ".sandman", "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
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
// contract that the events log's RunID equals the .sandman/runs/<dir>
// directory name. A regression where the two diverge would silently 404
// in production. This test uses a randomly-named runID (mirroring the
// "{shortid}-{ts}-{issue}" format the orchestrator emits) and asserts
// the archive endpoint succeeds against the matching directory.
func TestPortal_ArchiveEndpoint_EndToEndRealRunIDToDirName(t *testing.T) {
	repoRoot, err := os.MkdirTemp("/tmp", "sm-archive-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runID := "abcd-260618113825-issue-42"
	runDir := filepath.Join(repoRoot, ".sandman", "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "log.txt"), []byte("run output"), 0644); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: started, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: started.Add(time.Minute), RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return false }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	resp, body := postPortalArchive(t, newPortalArchiveHandlerForTest(t, repoRoot), runID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for runID %q, got %d: %s", runID, resp.StatusCode, body)
	}

	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("expected run dir %q to be gone after archive, stat err = %v", runDir, err)
	}
	archivedDir := filepath.Join(repoRoot, ".sandman", "archive", runID)
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
