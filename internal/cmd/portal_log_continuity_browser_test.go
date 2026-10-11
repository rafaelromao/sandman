package cmd

import (
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortalLogContinuityBrowser_TabRoundTrips drives the issue #2772 actual
// tab paths and a forced native EventSource reconnect through the production
// saved-log endpoint and embedded page.
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
			registry := newPortalLogRegistry()
			t.Cleanup(registry.Close)
			server := httptest.NewServer(fixture.handler(fixture.savedLogStream(registry)))
			defer server.Close()
			result := runPortalContinuityBrowser(t, chromium, server.URL, fixture.results)
			if result.Error != "" {
				t.Fatalf("actual-page browser reproduction: %s", result.Error)
			}
			status := fixture.status()
			if status.Connections != 55 || status.Closed < 54 || status.Error != "" {
				t.Fatalf("did not exercise native reconnect plus repeated tab replacements: %+v", status)
			}
			if len(status.ResumeCursors) != 55 || status.ResumeCursors[0] != "" || status.ResumeCursors[1] != "" {
				t.Fatalf("expected fresh URL cursors only for initial/native streams: %+v", status.ResumeCursors)
			}
			if len(status.LastEventIDs) != 55 || status.LastEventIDs[1] != result.InitialCursor {
				t.Fatalf("native EventSource reconnect did not send its last received source cursor: %+v", status.LastEventIDs)
			}
			for i := 2; i < len(status.ResumeCursors); i++ {
				if status.ResumeCursors[i] == "" {
					t.Fatalf("tab stream %d did not resume from accepted position: %+v", i+1, status.ResumeCursors)
				}
			}
			if result.StressTransitions != 50 || status.SummaryNotModified == 0 {
				t.Fatalf("actual page did not settle 50 mixed transitions under unchanged 304 polls: transitions=%d 304s=%d", result.StressTransitions, status.SummaryNotModified)
			}
			if result.MaxWarmTransitionMs >= 1000 || result.LiveUpdateMs >= 1000 {
				t.Fatalf("Portal exceeded sub-second warm/live responsiveness: warm-max=%.1fms live=%.1fms samples=%v", result.MaxWarmTransitionMs, result.LiveUpdateMs, result.WarmTransitionMs)
			}
			if result.ModelCacheBytes > 8*1024*1024 || result.ModelRuns > 16 || result.DOMLogNodes > 1000 {
				t.Fatalf("Portal continuity retention is not bounded: model=%d bytes/%d runs, DOM nodes=%d", result.ModelCacheBytes, result.ModelRuns, result.DOMLogNodes)
			}
			initial := fixture.readRange(t, fixture.snapshotStart, fixture.initialEnd)
			if result.InitialLog != initial {
				t.Fatalf("initial production Log pane differs from the saved 256 KiB retained range: %s", portalContinuityDifference(result.InitialLog, initial))
			}
			if result.InitialStart != fixture.snapshotStart || result.InitialEnd != fixture.initialEnd || result.InitialGeneration == "" {
				t.Fatalf("initial accepted range does not identify the saved source: %+v", result)
			}
			info, err := os.Stat(fixture.run.LogPath)
			if err != nil {
				t.Fatal(err)
			}
			sourceFile, err := os.Open(fixture.run.LogPath)
			if err != nil {
				t.Fatal(err)
			}
			finalStart, err := portalLogRetainedStart(sourceFile, info.Size())
			_ = sourceFile.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := fixture.readRange(t, finalStart, info.Size())
			t.Logf("saved run.log=%d bytes; accepted source range=[%d,%d); streams=%d; 304 polls=%d; warm max=%.1fms; live=%.1fms; model=%d bytes/%d runs; DOM nodes=%d",
				info.Size(), finalStart, info.Size(), status.Connections, status.SummaryNotModified,
				result.MaxWarmTransitionMs, result.LiveUpdateMs, result.ModelCacheBytes, result.ModelRuns, result.DOMLogNodes)
			// Compare every visible byte, including padding, newlines and all
			// records. Short labels below are diagnostics, never the oracle.
			if result.VisibleLog != want {
				t.Fatalf("Log -> %s -> Log violates saved-source continuity: %s\nvisible records: %s\nsource suffix + appended records: %s",
					tab, portalContinuityDifference(result.VisibleLog, want), portalContinuityLabels(result.VisibleLog), portalContinuityLabels(want))
			}
			if result.FinalStart != finalStart || result.FinalEnd != info.Size() || result.FinalGeneration != result.InitialGeneration {
				t.Fatalf("final DOM range/generation differs from source: final=[%d,%d) generation=%q, source=[%d,%d) generation=%q",
					result.FinalStart, result.FinalEnd, result.FinalGeneration, finalStart, info.Size(), result.InitialGeneration)
			}
		})
	}
}

func requirePortalContinuityTool(t *testing.T, name string) string {
	t.Helper()
	if name == "chromium" {
		if configured := strings.TrimSpace(os.Getenv("SANDMAN_PORTAL_CHROMIUM")); configured != "" {
			path, err := exec.LookPath(configured)
			if err != nil {
				t.Fatalf("SANDMAN_PORTAL_CHROMIUM is not executable: %v", err)
			}
			return path
		}
	}
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
	InitialLog          string    `json:"initialLog"`
	InitialStart        int64     `json:"initialStart"`
	InitialEnd          int64     `json:"initialEnd"`
	InitialGeneration   string    `json:"initialGeneration"`
	InitialCursor       string    `json:"initialCursor"`
	VisibleLog          string    `json:"visibleLog"`
	FinalStart          int64     `json:"finalStart"`
	FinalEnd            int64     `json:"finalEnd"`
	FinalGeneration     string    `json:"finalGeneration"`
	StressTransitions   int       `json:"stressTransitions"`
	MaxWarmTransitionMs float64   `json:"maxWarmTransitionMs"`
	WarmTransitionMs    []float64 `json:"warmTransitionMs"`
	LiveUpdateMs        float64   `json:"liveUpdateMs"`
	ModelCacheBytes     int       `json:"modelCacheBytes"`
	ModelRuns           int       `json:"modelRuns"`
	DOMLogNodes         int       `json:"domLogNodes"`
	Error               string    `json:"error"`
}

type portalContinuityStreamStatus struct {
	Connections        int      `json:"connections"`
	Closed             int      `json:"closed"`
	ResumeCursors      []string `json:"resumeCursors"`
	LastEventIDs       []string `json:"lastEventIds"`
	SummaryNotModified int      `json:"summaryNotModified"`
	Error              string   `json:"error,omitempty"`
}

type portalLogContinuityFixture struct {
	repoRoot      string
	run           portalRun
	tab           string
	driver        []byte
	initialEnd    int64
	snapshotStart int64
	liveText      string
	writer        *os.File
	results       chan portalContinuityBrowserResult
	mu            sync.Mutex
	streamStatus  portalContinuityStreamStatus
}

func newPortalLogContinuityFixture(t *testing.T, tab string, driver []byte) *portalLogContinuityFixture {
	t.Helper()
	const runID = "261007090000-b277-2772"
	const recordBytes = 8 * 1024
	f := &portalLogContinuityFixture{
		repoRoot: t.TempDir(), tab: tab, driver: driver,
		results:  make(chan portalContinuityBrowserResult, 1),
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
	if f.initialEnd <= portalLogRetainedBytes {
		t.Fatal("saved-log fixture must exceed the production 256 KiB retained-range limit")
	}
	f.snapshotStart = f.initialEnd - portalLogRetainedBytes
	f.run.Log = f.readRange(t, f.snapshotStart, f.initialEnd)
	eventLog := &events.JSONLLogger{Path: filepath.Join(f.repoRoot, ".sandman", "events.jsonl")}
	if err := eventLog.Log(events.Event{Type: "run.started", Timestamp: time.Now(), RunID: runID, Issue: 2772}); err != nil {
		t.Fatal(err)
	}
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
		if r.URL.Query().Get("summary") == "1" {
			w.Header().Set("ETag", `"continuity-fixture"`)
			if r.Header.Get("If-None-Match") == `"continuity-fixture"` {
				f.mu.Lock()
				f.streamStatus.SummaryNotModified++
				f.mu.Unlock()
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		if r.URL.Query().Get("runKey") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"run": f.run})
		} else {
			// Preload the bounded suffix in metadata, as in the baseline report.
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []portalRun{f.run}})
		}
	})
	mux.Handle("/api/runs/stream", f.trackStream(stream))
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
	mux.HandleFunc("/__test/source", func(w http.ResponseWriter, r *http.Request) {
		start, startErr := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
		end, endErr := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
		if startErr != nil || endErr != nil || start < 0 || end < start {
			http.Error(w, "invalid source range", http.StatusBadRequest)
			return
		}
		text, err := f.rawRange(start, end)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(text)
	})
	mux.HandleFunc("/__test/append", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		_, err := io.WriteString(f.writer, f.liveText)
		if err != nil {
			f.streamStatus.Error = err.Error()
		}
		f.mu.Unlock()
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

func (f *portalLogContinuityFixture) trackStream(stream http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.streamStatus.Connections++
		f.streamStatus.ResumeCursors = append(f.streamStatus.ResumeCursors, r.URL.Query().Get("cursor"))
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.streamStatus.Closed++
			f.mu.Unlock()
		}()
		f.mu.Lock()
		dropSnapshot := f.streamStatus.Connections == 1
		f.streamStatus.LastEventIDs = append(f.streamStatus.LastEventIDs, r.Header.Get("Last-Event-ID"))
		f.mu.Unlock()
		if dropSnapshot {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			stream.ServeHTTP(&portalContinuityDropWriter{ResponseWriter: w, cancel: cancel}, r.WithContext(ctx))
			return
		}
		stream.ServeHTTP(w, r)
	})
}

// portalContinuityDropWriter forces a real HTTP disconnect immediately after
// the first snapshot has been flushed. Chromium's native EventSource must
// reconnect with the received SSE id rather than a test-injected callback.
type portalContinuityDropWriter struct {
	http.ResponseWriter
	cancel  context.CancelFunc
	dropped bool
}

func (w *portalContinuityDropWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if !w.dropped && bytes.Contains(p, []byte(`"type":"snapshot"`)) {
		w.dropped = true
		_ = http.NewResponseController(w.ResponseWriter).Flush()
		w.cancel()
	}
	return n, err
}

func (w *portalContinuityDropWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *portalContinuityDropWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (f *portalLogContinuityFixture) savedLogStream(registry *portalLogRegistry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("runKey") != f.run.Key {
			http.Error(w, "unknown fixture run", http.StatusNotFound)
			return
		}
		servePortalLogStream(w, r, f.run, portalLogStreamOptions{
			Scope:    f.repoRoot,
			Registry: registry,
			Terminal: func(context.Context) (bool, error) { return false, nil },
		})
	})
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
