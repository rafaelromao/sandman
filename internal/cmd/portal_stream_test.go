package cmd

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
)

// startFakeRunDaemon listens on sockPath as a stand-in for a live daemon's
// Control Socket. On each accepted connection it writes the given lines
// (raw, including any ANSI/control bytes), then closes the connection after
// the supplied delay. Returns the accept count so the test can assert the
// bridge actually connected.
func startFakeRunDaemon(t *testing.T, sockPath string, lines []string, closeDelay time.Duration) *int32 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepts int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&accepts, 1)
			go func(c net.Conn) {
				defer func() {
					if closeDelay > 0 {
						// Keep the socket alive briefly so the portal's snapshot
						// liveness probe sees the row as active.
						time.Sleep(closeDelay)
					}
					_ = c.Close()
				}()
				for _, line := range lines {
					_, _ = c.Write([]byte(line))
				}
			}(conn)
		}
	}()
	return &accepts
}

// TestPortal_RunStream_BridgesControlSocketToSSE drives the full handler:
// a fake daemon writes ANSI-tagged lines over run.sock, the SSE bridge
// strips the ANSI and emits one cleaned event per line, and the stream
// ends cleanly when the daemon closes the socket.
func TestPortal_RunStream_BridgesControlSocketToSSE(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-5 * time.Minute)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42")
	runID := "PR42"
	runFolder := filepath.Join(batchDir, "runs", runID)
	sockPath := filepath.Join(runFolder, "run.sock")
	if err := os.MkdirAll(runFolder, 0755); err != nil {
		t.Fatal(err)
	}
	runManifest := batchindex.RunManifest{Issue: 42}
	runManifestData, _ := json.Marshal(runManifest)
	if err := os.WriteFile(filepath.Join(runFolder, "run.json"), runManifestData, 0644); err != nil {
		t.Fatal(err)
	}
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Entries: []batchindex.Entry{
		{ID: runID, Path: batchDir, Kind: "batch", Status: "active", Issues: []int{42}},
	}}
	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := os.MkdirAll(filepath.Dir(idxPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}
	startFakeRunDaemon(t, sockPath, []string{
		"\x1b[32m[issue-42]\x1b[0m 12:00:01 starting work\r\n",
		"[issue-42] 12:00:02 \x1b[1;33mwarning\x1b[0m: low disk\n",
		"[issue-42] 12:00:03 done\n",
	}, 200*time.Millisecond)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runKey := readPortalRuns(t, server.URL)[0].Key
	getPortalRunsIndex(repoRoot).Invalidate()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/runs/stream?runKey="+url.QueryEscape(runKey), nil)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream content-type, got %q", ct)
	}

	events := readSSEEvents(t, resp.Body)
	want := []string{
		"12:00:01 starting work",
		"12:00:02 warning: low disk",
		"12:00:03 done",
	}
	if len(events) < len(want) {
		t.Fatalf("expected at least %d events, got %d: %v", len(want), len(events), events)
	}
	for i, w := range want {
		if events[i] != w {
			t.Fatalf("event %d: got %q, want %q (ANSI/control bytes must be stripped)", i, events[i], w)
		}
	}
}

// TestPortal_RunStream_RejectsNonActiveRun asserts the endpoint refuses to
// stream a terminal run (no live socket) with 409, and a missing runKey
// with 400.
func TestPortal_RunStream_RejectsNonActiveRun(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-issue-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "abcd-260618113825-issue-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	t.Run("missing runKey", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/api/runs/stream")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing runKey, got %d", resp.StatusCode)
		}
	})

	t.Run("completed run is not streamable", func(t *testing.T) {
		runKey := readPortalRuns(t, server.URL)[0].Key
		resp, err := http.Get(server.URL + "/api/runs/stream?runKey=" + url.QueryEscape(runKey))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for a completed run, got %d", resp.StatusCode)
		}
	})
}

// readSSEEvents reads "data: <line>\n\n" frames until EOF, returning the
// data payloads. Used to assert the bridge's wire format.
func readSSEEvents(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading sse stream: %v", err)
	}
	return out
}
