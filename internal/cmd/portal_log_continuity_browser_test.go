package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
)

// TestPortalLogContinuityBrowser_TabRoundTrips is the issue #2772 baseline:
// execute the embedded production page and its real tab handlers over HTTP,
// with native fetch/EventSource/rAF and a source-backed snapshot/replay mismatch.
// Unlike buildPortalReproPage/runPortalChromium, it substitutes no browser APIs
// and does not load a file:// page or extract/redefine production functions.
func TestPortalLogContinuityBrowser_TabRoundTrips(t *testing.T) {
	chromium := requirePortalContinuityTool(t, "chromium")
	node := requirePortalContinuityTool(t, "node")
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate continuity browser driver")
	}
	driverPath := filepath.Join(filepath.Dir(thisFile), "portal_log_continuity_browser_driver_test.js")
	driver, err := os.ReadFile(driverPath)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", driverPath).CombinedOutput(); err != nil {
		t.Fatalf("browser driver syntax: %v\n%s", err, out)
	}
	for _, tab := range []string{"events", "details"} {
		t.Run(tab, func(t *testing.T) {
			fixture := newPortalLogContinuityFixture(t, tab, driver)
			// The handler argument is the transport seam for the subsequent
			// saved-log/structured-frame implementation. Keep the page, browser
			// actions, append-only artifact and complete-sequence oracle intact.
			server := httptest.NewServer(fixture.handler(http.HandlerFunc(fixture.serveLegacyStream)))
			defer server.Close()
			result := runPortalContinuityBrowser(t, chromium, server.URL, fixture.results)
			if result.Error != "" {
				t.Fatalf("actual-page browser reproduction: %s", result.Error)
			}
			status := fixture.status()
			if status.Connections != 2 || status.Closed < 1 || !status.ReplayComplete || status.Error != "" {
				t.Fatalf("did not exercise a closed stream and replacement replay: %+v", status)
			}
			initial := fixture.readRange(t, fixture.snapshotStart, fixture.initialEnd)
			if result.InitialLog != initial {
				t.Fatalf("initial production Log pane differs from the saved 64 KiB suffix: %s", portalContinuityDifference(result.InitialLog, initial))
			}
			info, err := os.Stat(fixture.run.LogPath)
			if err != nil {
				t.Fatal(err)
			}
			want := fixture.readRange(t, fixture.snapshotStart, info.Size())
			t.Logf("saved run.log=%d bytes; metadata suffix=[%d,%d); replacement replay=[%d,%d); HTTP streams=%d",
				info.Size(), fixture.snapshotStart, fixture.initialEnd, fixture.replayStart, fixture.initialEnd, status.Connections)
			// Compare every visible byte, including padding, newlines and all
			// records. Short labels below are diagnostics, never the oracle.
			if result.VisibleLog != want {
				t.Fatalf("Log -> %s -> Log violates saved-source continuity: %s\nvisible records: %s\nsource suffix + appended records: %s",
					tab, portalContinuityDifference(result.VisibleLog, want), portalContinuityLabels(result.VisibleLog), portalContinuityLabels(want))
			}
		})
	}
}

func requirePortalContinuityTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err == nil {
		return path
	}
	if os.Getenv("SANDMAN_REQUIRE_PORTAL_BROWSER") == "1" {
		t.Fatalf("SANDMAN_REQUIRE_PORTAL_BROWSER=1 requires %s on PATH: %v", name, err)
	}
	t.Skipf("%s not on PATH; set SANDMAN_REQUIRE_PORTAL_BROWSER=1 to require continuity browser coverage", name)
	return ""
}

type portalContinuityBrowserResult struct {
	InitialLog string `json:"initialLog"`
	VisibleLog string `json:"visibleLog"`
	Error      string `json:"error"`
}

type portalContinuityStreamStatus struct {
	Connections    int    `json:"connections"`
	Closed         int    `json:"closed"`
	ReplayComplete bool   `json:"replayComplete"`
	Error          string `json:"error,omitempty"`
}

type portalLogContinuityFixture struct {
	repoRoot      string
	run           portalRun
	tab           string
	driver        []byte
	initialEnd    int64
	snapshotStart int64
	replayStart   int64
	liveText      string
	writer        *os.File
	replay        chan struct{}
	live          chan struct{}
	results       chan portalContinuityBrowserResult
	mu            sync.Mutex
	streamStatus  portalContinuityStreamStatus
	replayOnce    sync.Once
	liveOnce      sync.Once
}

func newPortalLogContinuityFixture(t *testing.T, tab string, driver []byte) *portalLogContinuityFixture {
	t.Helper()
	const runID = "261007090000-b277-2772"
	const recordBytes = 8 * 1024
	f := &portalLogContinuityFixture{
		repoRoot: t.TempDir(), tab: tab, driver: driver,
		replay: make(chan struct{}), live: make(chan struct{}), results: make(chan portalContinuityBrowserResult, 1),
		liveText: "10:16:37 new live output\n10:16:38 second live output\n",
	}
	runDir := filepath.Join(f.repoRoot, ".sandman", "batches", "261007090000-b277", "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.run = portalRun{
		Key: runID, RunID: runID, Kind: "active", Status: "running", IssueNumber: 2772, IssueLabel: "#2772",
		BatchKey: "261007090000-b277", StartedAt: time.Now(), SourceExists: true,
		LogPath: filepath.Join(runDir, "run.log"), SocketPath: filepath.Join(runDir, "run.sock"),
		Events: []portalEvent{{Type: "run.started", Timestamp: time.Now()}},
	}
	var source strings.Builder
	for i := 0; i < 40; i++ {
		var label string
		switch {
		case i < 8:
			label = fmt.Sprintf("08:00:%02d outside replay %02d", i, i)
		case i == 8:
			label = "09:25:07 old command"
		case i == 9:
			label = "09:27:15 old output"
		case i < 32:
			label = fmt.Sprintf("09:28:%02d old output %02d", i, i)
		case i == 32:
			label = "10:09:32 current command"
		case i < 39:
			label = fmt.Sprintf("10:10:%02d current progress %02d", i, i)
		default:
			label = "10:16:22 current output"
		}
		// Fixed-size complete records make both production history limits
		// record-aligned. Generated padding crosses the renderer's 16 KiB
		// threshold without a large literal or thousands of SSE messages.
		prefix := label + " | "
		source.WriteString(prefix + strings.Repeat(".", recordBytes-len(prefix)-1) + "\n")
	}
	writer, err := os.OpenFile(f.run.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.writer = writer
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := io.WriteString(writer, source.String()); err != nil {
		t.Fatal(err)
	}
	f.initialEnd = int64(source.Len())
	if f.initialEnd <= daemon.MaxBufferSize {
		t.Fatal("saved-log fixture must exceed the production 256 KiB replay limit")
	}
	f.snapshotStart = f.initialEnd - portalReadLimit
	f.replayStart = f.initialEnd - daemon.MaxBufferSize
	f.run.Log = f.readRange(t, f.snapshotStart, f.initialEnd)
	return f
}

func (f *portalLogContinuityFixture) readRange(t *testing.T, start, end int64) string {
	t.Helper()
	data, err := f.rawRange(start, end)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *portalLogContinuityFixture) rawRange(start, end int64) ([]byte, error) {
	file, err := os.Open(f.run.LogPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.NewSectionReader(file, start, end-start))
	if err == nil && int64(len(data)) != end-start {
		err = fmt.Errorf("saved log range [%d,%d) returned %d bytes", start, end, len(data))
	}
	return data, err
}

func (f *portalLogContinuityFixture) status() portalContinuityStreamStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamStatus
}

func (f *portalLogContinuityFixture) handler(stream http.Handler) http.Handler {
	mux := http.NewServeMux()
	// Render through the production template/page-data builder so later
	// embedded frontend modules are automatically exercised by this test.
	page := &portalHandler{repoRoot: f.repoRoot}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		page.handlePage(recorder, r)
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		// Only add a test driver; production scripts and handlers are intact.
		_, _ = io.WriteString(w, strings.Replace(recorder.Body.String(), "</body>", `<script src="/__test/driver.js"></script></body>`, 1))
	})
	mux.HandleFunc("/api/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Query().Get("runKey") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"run": f.run})
		} else {
			// Preload the bounded suffix in metadata, as in the baseline report.
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []portalRun{f.run}})
		}
	})
	mux.HandleFunc("/api/runs/stream", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.streamStatus.Connections++
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.streamStatus.Closed++
			f.mu.Unlock()
		}()
		stream.ServeHTTP(w, r)
	})
	mux.HandleFunc("/__test/driver.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(f.driver)
	})
	mux.HandleFunc("/__test/config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runKey": f.run.Key, "tab": f.tab, "lastLiveLine": "10:16:38 second live output",
		})
	})
	mux.HandleFunc("/__test/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.status())
	})
	mux.HandleFunc("/__test/replay", func(w http.ResponseWriter, r *http.Request) {
		f.replayOnce.Do(func() { close(f.replay) })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/append", func(w http.ResponseWriter, r *http.Request) {
		f.liveOnce.Do(func() {
			_, err := io.WriteString(f.writer, f.liveText)
			if err != nil {
				f.mu.Lock()
				f.streamStatus.Error = err.Error()
				f.mu.Unlock()
			}
			close(f.live)
		})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/result", func(w http.ResponseWriter, r *http.Request) {
		var result portalContinuityBrowserResult
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&result); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.results <- result
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (f *portalLogContinuityFixture) serveLegacyStream(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("runKey") != f.run.Key {
		http.Error(w, "unknown fixture run", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	_, _ = io.WriteString(w, ": saved-log fixture connected\n\n")
	_ = rc.Flush()
	if f.status().Connections == 1 {
		// Isolate tab reattachment from the separately reported first-attach
		// bug: the initial subscription is idle, leaving metadata intact.
		_, _ = io.WriteString(w, "event: replay-complete\ndata: replay-complete\n\n")
		_ = rc.Flush()
		<-r.Context().Done()
		return
	}
	select {
	case <-r.Context().Done():
		return
	case <-f.replay:
	}
	if err := f.sendLegacyRange(w, r, f.replayStart, f.initialEnd, true); err != nil {
		f.recordStreamError(r, err)
		return
	}
	_, _ = io.WriteString(w, "event: replay-complete\ndata: replay-complete\n\n")
	if err := rc.Flush(); err != nil {
		f.recordStreamError(r, err)
		return
	}
	f.mu.Lock()
	f.streamStatus.ReplayComplete = true
	f.mu.Unlock()
	select {
	case <-r.Context().Done():
		return
	case <-f.live:
	}
	if err := f.sendLegacyRange(w, r, f.initialEnd, f.initialEnd+int64(len(f.liveText)), false); err != nil {
		f.recordStreamError(r, err)
		return
	}
	// Keep native EventSource open; EOF would introduce an unrelated reconnect.
	<-r.Context().Done()
}

func (f *portalLogContinuityFixture) recordStreamError(r *http.Request, err error) {
	if r.Context().Err() == nil {
		f.mu.Lock()
		f.streamStatus.Error = err.Error()
		f.mu.Unlock()
	}
}

func (f *portalLogContinuityFixture) sendLegacyRange(w http.ResponseWriter, r *http.Request, start, end int64, paced bool) error {
	data, err := f.rawRange(start, end)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(bytes.NewReader(data))
	rc := http.NewResponseController(w)
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF && line == "" {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", cleanPortalStreamLine(line)); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return err
		}
		if paced {
			// Avoid the independent burst/coalescer-loss bug obscuring tab order.
			timer := time.NewTimer(40 * time.Millisecond)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return r.Context().Err()
			case <-timer.C:
			}
		}
	}
}

func runPortalContinuityBrowser(t *testing.T, chromium, url string, results <-chan portalContinuityBrowserResult) portalContinuityBrowserResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chromium,
		"--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--no-first-run", "--no-default-browser-check", "--disable-background-timer-throttling",
		"--disable-renderer-backgrounding", "--window-size=1360,900",
		"--user-data-dir="+t.TempDir(), url)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(exited)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()
	select {
	case result := <-results:
		return result
	case <-exited:
		t.Fatalf("chromium exited before browser assertions: %v\n%s", exitErr, output.String())
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("actual-page continuity browser timed out: %v\n%s", ctx.Err(), output.String())
	}
	return portalContinuityBrowserResult{}
}

func portalContinuityDifference(got, want string) string {
	common := 0
	for common < len(got) && common < len(want) && got[common] == want[common] {
		common++
	}
	return fmt.Sprintf("first differing byte=%d, visible bytes=%d, source-range bytes=%d", common, len(got), len(want))
}

func portalContinuityLabels(text string) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, line := range lines {
		lines[i], _, _ = strings.Cut(line, " | ")
	}
	return fmt.Sprintf("%q", lines)
}
