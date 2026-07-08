package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
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
	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
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
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
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
		"\x1b[32m[" + runID + "]\x1b[0m 12:00:01 starting work\r\n",
		"[" + runID + "] 12:00:02 \x1b[1;33mwarning\x1b[0m: low disk\n",
		"[" + runID + "] 12:00:03 done\n",
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

// TestPortal_RunStream_EmitsHeartbeatOnIdleSocket pins the keepalive
// behavior: when the bridged Control Socket has no new data, the SSE
// bridge must emit a `: keepalive` SSE comment at the heartbeat cadence
// so intermediate proxies and the browser's idle reaper do not close
// a healthy tail. Without it, a quiet agent (one that pauses between
// commands) silently disconnects and the Log tab freezes until the user
// re-selects the row.
func TestPortal_RunStream_EmitsHeartbeatOnIdleSocket(t *testing.T) {
	originalHeartbeat := portalStreamHeartbeat
	portalStreamHeartbeat = 100 * time.Millisecond
	t.Cleanup(func() { portalStreamHeartbeat = originalHeartbeat })

	repoRoot, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoRoot) })
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

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
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: runID, Path: batchDir, Kind: "batch", Status: "active", Issues: []int{42}},
	}}
	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := os.MkdirAll(filepath.Dir(idxPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	// Fake daemon writes a single line, then stays connected without
	// writing more. The bridge should emit the line, then start
	// emitting heartbeat comments.
	startFakeRunDaemon(t, sockPath, []string{
		"[" + runID + "] 12:00:00 starting work\n",
	}, 2*time.Second)

	startedAt := time.Now().Add(-5 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Payload: map[string]any{"branch": "sandman/review-PR42", "review": true, "pr_number": 42}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	runKey := readPortalRuns(t, server.URL)[0].Key
	getPortalRunsIndex(repoRoot).Invalidate()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/runs/stream?runKey="+url.QueryEscape(runKey), nil)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var keepalives int
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), ": keepalive") {
			keepalives++
			if keepalives >= 3 {
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	t.Fatalf("expected at least 3 heartbeat comments within 5s, got %d (idle bridge silently disconnected)", keepalives)
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
		{Type: "run.started", Timestamp: startedAt, RunID: "260618113825-abcd-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: "260618113825-abcd-42", Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
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

// TestPortal_RunStream_FiltersCrossRunBleed is the regression for issue #1544.
// In a mixed batch every issue row shares the same batch.sock; the daemon
// prefixes each line with `[<runID>] ` so the bridge can attribute output
// to a specific run. The SSE stream for a single row must emit only that
// row's lines and drop sibling and unlabeled lines before they reach the
// browser, while preserving live tailing, ordering, and the existing
// ANSI/control-byte cleanup.
func TestPortal_RunStream_FiltersCrossRunBleed(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	batchID := "run-mixed-1"
	issueA := 860
	issueB := 854
	runIDA := fmt.Sprintf("%s-%d", batchID, issueA)
	runIDB := fmt.Sprintf("%s-%d", batchID, issueB)

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{
		Issues:    []int{issueA, issueB},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	batchSock := filepath.Join(batchDir, "batch.sock")
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: batchID, Path: batchDir, Kind: "issue", Status: "active", Issues: []int{issueA, issueB}},
	}}
	idxPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	if err := os.MkdirAll(filepath.Dir(idxPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Save(idxPath); err != nil {
		t.Fatal(err)
	}

	// Live-tail fixture: a fake daemon writes five lines in order —
	// an ANSI-coloured A line, the B (sibling) line, a plain A line,
	// a second B line, and an unlabeled banner. The A lines bracket
	// the sibling output so any naive "first match wins" or buffering
	// implementation would be caught by the ordering assertion below.
	// The ANSI line also exercises the path where the `[<runID>]`
	// label is wrapped in colour codes — the filter must still
	// recognise it, and cleanPortalStreamLine must strip the codes
	// from the emitted event.
	lines := []string{
		"\x1b[32m[" + runIDA + "]\x1b[0m 18:51:00 working on PR\n",
		fmt.Sprintf("[%s] 18:51:04 sibling work\n", runIDB),
		fmt.Sprintf("[%s] 18:51:05 still on it\n", runIDA),
		fmt.Sprintf("[%s] 18:51:06 another sibling line\n", runIDB),
		"unprefixed banner line that must be dropped\n",
	}
	startFakeRunDaemon(t, batchSock, lines, 200*time.Millisecond)

	// Replace the liveness probe so the index entry stays "active"
	// without a real daemon to back the batch.sock path.
	originalProbe := portalRunLivenessProbe
	portalRunLivenessProbe = func(string) bool { return true }
	t.Cleanup(func() { portalRunLivenessProbe = originalProbe })

	startedAt := time.Now()
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runIDA, Issue: issueA},
		{Type: "run.started", Timestamp: startedAt, RunID: runIDB, Issue: issueB},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	// Resolve the runKey for issue A (the row whose stream we are about
	// to assert on). The mixed-batch row is keyed by its RunID, which
	// is `<batchID>-<issue>` (set from the run.started event).
	wantRunKey := runIDA
	runs := readPortalRuns(t, server.URL)
	var matched *portalRun
	for i := range runs {
		if runs[i].Key == wantRunKey {
			matched = &runs[i]
			break
		}
	}
	if matched == nil {
		t.Fatalf("expected run key %q in runs, got %#v", wantRunKey, runs)
	}
	if matched.RunID != runIDA {
		t.Fatalf("expected resolved RunID %q, got %q", runIDA, matched.RunID)
	}

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/runs/stream?runKey="+url.QueryEscape(wantRunKey), nil)
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

	got := readSSEEvents(t, resp.Body)
	want := []string{
		"18:51:00 working on PR",
		"18:51:05 still on it",
	}
	// Assert exact equality — no extra events, no reordering. This
	// pins live-tailing, ordering, and ANSI stripping simultaneously:
	// a future "buffer then emit" refactor that reorders tail output
	// would fail this check, as would any leak from sibling or
	// unprefixed lines.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SSE events mismatch:\n got:  %q\n want: %q", got, want)
	}
}
