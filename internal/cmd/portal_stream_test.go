package cmd

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
)

// Historical test names are preserved for native compatibility selectors. The
// production Portal endpoint now streams the Saved Run Log, independently of
// daemon socket availability, replay windows and timestamp prefix writers.
func TestPortal_RunStream_BridgesControlSocketToSSE(t *testing.T) {
	server, path, logger, runID := savedPortalStreamHTTPFixture(t, "\x1b[32m[row]\x1b[0m 12:00:01 starting work\r\n[row] 12:00:02 \x1b[1;33mwarning\x1b[0m: low disk\n[row] 12:00:03 done\n")
	resp := getSavedPortalStream(t, server.URL, runID)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	frame := readPortalLogFrame(t, reader)
	if frame.Type != "snapshot" || !reflect.DeepEqual(recordTexts(frame), []string{"12:00:01 starting work", "12:00:02 warning: low disk", "12:00:03 done"}) {
		t.Fatalf("saved snapshot = %#v", frame)
	}
	appendSavedLog(t, path, "[row] 12:00:04 live after snapshot\n")
	appendFrame := readPortalLogFrame(t, reader)
	if appendFrame.Type != "append" || appendFrame.Start != frame.End || !reflect.DeepEqual(recordTexts(appendFrame), []string{"12:00:04 live after snapshot"}) {
		t.Fatalf("saved tail = %#v", appendFrame)
	}
	appendSavedLog(t, path, "final fragment")
	if err := logger.Log(events.Event{Type: "run.finished", RunID: runID, Timestamp: time.Now(), Payload: map[string]any{"status": "success"}}); err != nil {
		t.Fatal(err)
	}
	final := readPortalLogFrame(t, reader)
	end := readPortalLogFrame(t, reader)
	if final.Type != "append" || !reflect.DeepEqual(recordTexts(final), []string{"final fragment"}) || end.Type != "end" || end.End != final.End {
		t.Fatalf("final drain = %#v; end = %#v", final, end)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("end must close stream, got %v", err)
	}
}

func TestPortal_RunStream_EmitsHeartbeatOnIdleSocket(t *testing.T) {
	original := portalStreamHeartbeat
	portalStreamHeartbeat = 20 * time.Millisecond
	t.Cleanup(func() { portalStreamHeartbeat = original })
	server, _, _, runID := savedPortalStreamHTTPFixture(t, "quiet\n")
	resp := getSavedPortalStream(t, server.URL, runID)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	if frame := readPortalLogFrame(t, reader); frame.Type != "snapshot" {
		t.Fatalf("initial frame = %#v", frame)
	}
	for n := 0; n < 3; {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == ": keepalive\n" {
			n++
		} else if strings.HasPrefix(line, "data:") {
			t.Fatalf("unfinished EOF emitted a terminal/data event: %s", line)
		}
	}
}

func TestPortal_RunStream_RejectsNonActiveRun(t *testing.T) {
	server, _, logger, runID := savedPortalStreamHTTPFixture(t, "saved terminal output\n")
	resp, err := http.Get(server.URL + "/api/runs/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing runKey: got %d", resp.StatusCode)
	}
	if err := logger.Log(events.Event{Type: "run.finished", RunID: runID, Timestamp: time.Now(), Payload: map[string]any{"status": "success"}}); err != nil {
		t.Fatal(err)
	}
	// Terminal saved artifacts are streamable even with no control socket.
	resp = getSavedPortalStream(t, server.URL, runID)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	frame := readPortalLogFrame(t, reader)
	if frame.Type != "snapshot" || !reflect.DeepEqual(recordTexts(frame), []string{"saved terminal output"}) || readPortalLogFrame(t, reader).Type != "end" {
		t.Fatalf("terminal artifact = %#v", frame)
	}
}

func TestPortal_RunStream_FiltersCrossRunBleed(t *testing.T) {
	server, path, _, runID := savedPortalStreamHTTPFixture(t, "[row] 18:51:00 working on PR\n18:51:05 still on it\n\n")
	// Sibling and batch artifacts must never be used as the per-run source.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(filepath.Dir(path)), "sibling"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(path)), "sibling", "run.log"), []byte("sibling output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resp := getSavedPortalStream(t, server.URL, runID)
	defer resp.Body.Close()
	frame := readPortalLogFrame(t, bufio.NewReader(resp.Body))
	if !reflect.DeepEqual(recordTexts(frame), []string{"18:51:00 working on PR", "18:51:05 still on it", ""}) {
		t.Fatalf("per-run source = %#v", frame)
	}
}

func savedPortalStreamHTTPFixture(t *testing.T, raw string) (*httptest.Server, string, *events.JSONLLogger, string) {
	t.Helper()
	root := t.TempDir()
	batchID, runID := "test-batch", "test-batch-42"
	batchDir := filepath.Join(root, ".sandman", "batches", batchID)
	runDir := filepath.Join(batchDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(batchindex.RunManifest{Issue: 42, RunID: runID, BatchID: batchID})
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{{ID: batchID, Path: batchDir, Kind: "issue", Status: "active", Issues: []int{42}}}}
	if err := idx.Save(filepath.Join(root, ".sandman", "batches.json")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runDir, "run.log")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	logger := &events.JSONLLogger{Path: filepath.Join(root, ".sandman", "events.jsonl")}
	if err := logger.Log(events.Event{Type: "run.started", RunID: runID, Issue: 42, Timestamp: time.Now(), Payload: map[string]any{"batch_id": batchID}}); err != nil {
		t.Fatal(err)
	}
	h := &portalHandler{repoRoot: root}
	server := httptest.NewServer(http.HandlerFunc(h.handleRunStream))
	t.Cleanup(server.Close)
	t.Cleanup(func() { portalRunsIndexes.Delete(root) })
	return server, path, logger, runID
}

func getSavedPortalStream(t *testing.T, baseURL, runID string) *http.Response {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(baseURL + "/api/runs/stream?runKey=" + url.QueryEscape(runID))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("stream HTTP status %d, Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return resp
}
