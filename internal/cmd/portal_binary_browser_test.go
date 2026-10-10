package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

type portalBinaryBrowserControl struct {
	mu        sync.RWMutex
	portalURL string
	phase     string
	error     string
	signals   chan string
	history   []string
	appendAt  map[string]time.Time
	signalAt  map[string]time.Time
}

func TestPortalBuiltBinaryBrowserContinuity(t *testing.T) {
	if _, err := exec.LookPath("chromium"); err != nil {
		t.Skip("chromium not on PATH; focused Linux CI installs it")
	}

	binPath := buildPortalBrowserBinary(t)
	t.Logf("built Portal binary revision: %s", portalBinarySourceRevision(t))
	repoDir := shortTempDir(t)
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)
	mainRun, reviewRun, secondRun, logPaths := createPortalBinaryBrowserFixture(t, repoDir)
	port := freePortalBrowserPort(t)
	initialURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	control := &portalBinaryBrowserControl{
		portalURL: initialURL,
		signals:   make(chan string, 8),
		appendAt:  make(map[string]time.Time),
		signalAt:  make(map[string]time.Time),
	}
	controlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		switch r.URL.Path {
		case "/portal":
			control.mu.RLock()
			url := control.portalURL
			control.mu.RUnlock()
			_, _ = io.WriteString(w, url)
		case "/phase":
			control.mu.RLock()
			phase := control.phase
			control.mu.RUnlock()
			_, _ = io.WriteString(w, phase)
		case "/signal":
			name := r.URL.Query().Get("name")
			control.mu.Lock()
			control.signalAt[name] = time.Now()
			control.history = append(control.history, name)
			if name == "error" {
				control.error = r.URL.Query().Get("detail")
			}
			control.mu.Unlock()
			select {
			case control.signals <- r.URL.Query().Get("name"):
			default:
			}
		case "/wrapper":
			_, _ = io.WriteString(w, portalBrowserWrapper(controlServerURL(r), mainRun, reviewRun, secondRun))
		default:
			http.NotFound(w, r)
		}
	}))
	defer controlServer.Close()

	portalCmd := startPortalBrowserBinary(t, binPath, repoDir, port)
	waitPortalBrowserReady(t, initialURL)
	t.Cleanup(func() {
		if portalCmd.Process != nil {
			_ = portalCmd.Process.Kill()
		}
		_ = portalCmd.Wait()
	})

	chromium := exec.Command(
		"chromium",
		"--headless",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-web-security",
		"--enable-precise-memory-info",
		"--window-size=1360,900",
		"--virtual-time-budget=30000",
		"--dump-dom",
		controlServer.URL+"/wrapper",
	)
	var browserOutput bytes.Buffer
	chromium.Stdout = &browserOutput
	chromium.Stderr = &browserOutput
	if err := chromium.Start(); err != nil {
		t.Fatalf("start Chromium: %v", err)
	}
	browserDone := make(chan error, 1)
	go func() { browserDone <- chromium.Wait() }()

	waitPortalBrowserSignal(t, control, "loaded")
	appendPortalBrowserLog(t, logPaths[mainRun], "["+mainRun+"] 10:00:01 live-before-restart\n")
	appendPortalBrowserLog(t, logPaths[reviewRun], "["+reviewRun+"] 10:00:01 live-before-restart\n")
	appendPortalBrowserLog(t, logPaths[secondRun], "["+secondRun+"] 10:00:01 live-before-restart\n")
	control.mu.Lock()
	control.appendAt["before-restart"] = time.Now()
	control.mu.Unlock()
	waitPortalBrowserSignal(t, control, "visible-before-restart")
	waitPortalBrowserSignal(t, control, "interactions")

	if err := portalCmd.Process.Kill(); err != nil {
		t.Fatalf("stop Portal for forced reconnect: %v", err)
	}
	_ = portalCmd.Wait()
	portalCmd = startPortalBrowserBinary(t, binPath, repoDir, port)
	waitPortalBrowserReady(t, initialURL)
	appendPortalBrowserLog(t, logPaths[mainRun], "["+mainRun+"] 10:00:02 live-after-restart\n")
	appendPortalBrowserLog(t, logPaths[reviewRun], "["+reviewRun+"] 10:00:02 live-after-restart\n")
	appendPortalBrowserLog(t, logPaths[secondRun], "["+secondRun+"] 10:00:02 live-after-restart\n")
	control.mu.Lock()
	control.appendAt["after-restart"] = time.Now()
	control.phase = "restart-ready"
	control.mu.Unlock()

	waitPortalBrowserSignal(t, control, "done")
	select {
	case err := <-browserDone:
		if err != nil {
			t.Fatalf("Chromium failed: %v\n%s", err, browserOutput.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Chromium did not finish after browser continuity marker")
	}
	output := browserOutput.String()
	marker := extractPortalMarker(t, output, "portal-binary-browser-marker")
	var result struct {
		Logs struct {
			Main   string `json:"main"`
			Review string `json:"review"`
			Second string `json:"second"`
		} `json:"logs"`
		Metrics struct {
			DOMNodes         int `json:"domNodes"`
			PreCount         int `json:"preCount"`
			RenderedLogBytes int `json:"renderedLogBytes"`
			RetainedBytes    int `json:"retainedBytes"`
			HeapUsedBytes    int `json:"heapUsedBytes"`
			StreamResources  int `json:"streamResources"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal([]byte(marker), &result); err != nil {
		t.Fatalf("parse built-binary continuity marker: %v\nmarker=%s", err, marker)
	}
	for _, check := range []struct {
		name string
		got  string
		want string
	}{
		{name: "main", got: result.Logs.Main, want: portalBinaryExpectedLog(mainRun, true)},
		{name: "review", got: result.Logs.Review, want: portalBinaryExpectedLog(reviewRun, true)},
		{name: "second", got: result.Logs.Second, want: portalBinaryExpectedLog(secondRun, true)},
	} {
		if check.got != check.want {
			t.Fatalf("built-binary %s log mismatch: got %q, want %q", check.name, check.got, check.want)
		}
	}
	if result.Metrics.DOMNodes <= 0 || result.Metrics.PreCount <= 0 || result.Metrics.RenderedLogBytes <= 0 || result.Metrics.RetainedBytes <= 0 || result.Metrics.StreamResources <= 0 {
		t.Fatalf("invalid built-binary browser metrics: %+v", result.Metrics)
	}
	t.Logf("built-binary browser retention metrics: dom_nodes=%d pre_count=%d rendered_log_bytes=%d retained_bytes=%d heap_used_bytes=%d stream_resources=%d", result.Metrics.DOMNodes, result.Metrics.PreCount, result.Metrics.RenderedLogBytes, result.Metrics.RetainedBytes, result.Metrics.HeapUsedBytes, result.Metrics.StreamResources)
	for _, marker := range []string{"portal-binary-browser-marker", "live-before-restart", "live-after-restart", "subject-switched"} {
		if !strings.Contains(output, marker) {
			t.Fatalf("browser output missing %q:\n%s", marker, output)
		}
	}
	control.mu.RLock()
	beforeLatency := control.signalAt["visible-before-restart"].Sub(control.appendAt["before-restart"])
	afterLatency := control.signalAt["visible-after-restart"].Sub(control.appendAt["after-restart"])
	metricsLine := control.signalAt["metrics"]
	control.mu.RUnlock()
	if beforeLatency <= 0 || afterLatency <= 0 || metricsLine.IsZero() {
		t.Fatalf("browser measurements were not recorded: before=%s after=%s metrics=%v", beforeLatency, afterLatency, !metricsLine.IsZero())
	}
	t.Logf("built binary continuity measurements: append_to_visible_before_restart=%s append_to_visible_after_restart=%s", beforeLatency, afterLatency)
}

func createPortalBinaryBrowserFixture(t *testing.T, repoDir string) (string, string, string, map[string]string) {
	t.Helper()
	batchID := "binary-browser-2772"
	mainRun := batchID + "-impl"
	reviewRun := batchID + "-review"
	secondRun := batchID + "-second"
	batchDir := filepath.Join(repoDir, ".sandman", "batches", batchID)
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatalf("create browser batch: %v", err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{2772, 2773}, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("write browser batch manifest: %v", err)
	}
	logPaths := map[string]string{}
	for _, run := range []struct {
		id     string
		kind   string
		issue  int
		review bool
	}{
		{id: mainRun, kind: "issue", issue: 2772},
		{id: reviewRun, kind: "review", issue: 2772, review: true},
		{id: secondRun, kind: "issue", issue: 2773},
	} {
		runDir := filepath.Join(batchDir, "runs", run.id)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatalf("create browser run %s: %v", run.id, err)
		}
		manifest := map[string]any{"runID": run.id, "batchId": batchID, "kind": run.kind, "status": "running", "issue": run.issue}
		if run.review {
			manifest["review"] = true
			manifest["prNumber"] = 42
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("marshal browser run %s: %v", run.id, err)
		}
		if err := os.WriteFile(filepath.Join(runDir, "run.json"), data, 0o644); err != nil {
			t.Fatalf("write browser run %s: %v", run.id, err)
		}
		logPath := filepath.Join(runDir, "run.log")
		if err := os.WriteFile(logPath, []byte(portalBinaryInitialLog(run.id)), 0o644); err != nil {
			t.Fatalf("write browser log %s: %v", run.id, err)
		}
		logPaths[run.id] = logPath
	}
	layout := paths.NewLayout(nil, repoDir)
	index := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{{ID: batchID, Path: batchDir, Kind: batchindex.KindIssue, Status: batchindex.StatusActive, CreatedAt: time.Now(), Issues: []int{2772, 2773}}}}
	if err := index.Save(layout.BatchesIndexPath); err != nil {
		t.Fatalf("save browser batch index: %v", err)
	}
	logger := &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")}
	for _, event := range []events.Event{
		{Type: "run.started", Timestamp: time.Now(), RunID: mainRun, Issue: 2772, Payload: map[string]any{"batch_id": batchID, "branch": "binary-browser-2772"}},
		{Type: "run.started", Timestamp: time.Now().Add(time.Second), RunID: reviewRun, Issue: 2772, Payload: map[string]any{"batch_id": batchID, "review": true, "pr_number": 42, "branch": "binary-browser-2772"}},
		{Type: "run.started", Timestamp: time.Now().Add(2 * time.Second), RunID: secondRun, Issue: 2773, Payload: map[string]any{"batch_id": batchID, "branch": "binary-browser-2773"}},
	} {
		if err := logger.Log(event); err != nil {
			t.Fatalf("write browser event: %v", err)
		}
	}
	return mainRun, reviewRun, secondRun, logPaths
}

func portalBinaryInitialLog(runID string) string {
	var log strings.Builder
	for i := 0; i < 32; i++ {
		fmt.Fprintf(&log, "[%s] 09:59:%02d history-%02d\n", runID, i, i)
	}
	fmt.Fprintf(&log, "[%s] 10:00:00 initial\n", runID)
	return log.String()
}

func portalBinaryExpectedLog(runID string, afterRestart bool) string {
	var log strings.Builder
	for i := 0; i < 32; i++ {
		fmt.Fprintf(&log, "09:59:%02d history-%02d\n", i, i)
	}
	log.WriteString("10:00:00 initial\n10:00:01 live-before-restart\n")
	if afterRestart {
		log.WriteString("10:00:02 live-after-restart\n")
	}
	return log.String()
}

func portalBrowserWrapper(controlURL, mainRun, reviewRun, secondRun string) string {
	return `<!doctype html><meta charset="utf-8"><body><script>
(async function () {
  const control = ` + strconv.Quote(controlURL) + `;
  const mainRun = ` + strconv.Quote(mainRun) + `;
  const reviewRun = ` + strconv.Quote(reviewRun) + `;
  const secondRun = ` + strconv.Quote(secondRun) + `;
  const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
  async function get(path) { return (await fetch(control + path)).text(); }
  async function signal(name) { await fetch(control + '/signal?name=' + encodeURIComponent(name)); }
  async function waitFor(predicate, label) {
    const deadline = Date.now() + 12000;
    while (Date.now() < deadline) {
      try { if (await predicate()) return; } catch (_) {}
      await sleep(50);
    }
    throw new Error('timed out waiting for ' + label);
  }
  async function loadPortal() {
    const frame = document.querySelector('iframe') || document.body.appendChild(document.createElement('iframe'));
    frame.style.width = '1280px';
    frame.style.height = '800px';
    frame.src = await get('/portal');
    await waitFor(() => frame.contentDocument.querySelector('tr[data-run-key="' + mainRun + '"]'), 'main row');
    return frame;
  }
  function row(frame, key) { return frame.contentDocument.querySelector('tr[data-run-key="' + key + '"]'); }
  async function clickRow(frame, key) {
    await waitFor(() => row(frame, key), 'row ' + key);
    row(frame, key).click();
    await sleep(250);
  }
  async function clickTab(frame, tab) {
    await waitFor(() => frame.contentDocument.querySelector('button[data-action="set-tab"][data-tab="' + tab + '"]'), tab + ' tab');
    frame.contentDocument.querySelector('button[data-action="set-tab"][data-tab="' + tab + '"]').click();
    await sleep(250);
  }
  function expected(runID, afterRestart) {
    const lines = [];
    for (let i = 0; i < 32; i++) {
      lines.push('09:59:' + String(i).padStart(2, '0') + ' history-' + String(i).padStart(2, '0'));
    }
    lines.push('10:00:00 initial');
    lines.push('10:00:01 live-before-restart');
    if (afterRestart) lines.push('10:00:02 live-after-restart');
    return lines.join('\n') + '\n';
  }
  function logText(frame, key) {
    const pre = frame.contentDocument.querySelector('pre[data-scroll-key="' + key + '"]');
    return pre ? pre.getAttribute('data-rendered-log') || '' : '';
  }
  async function assertLog(frame, key, want) {
    await waitFor(() => logText(frame, key) === want, 'complete log ' + key);
    if (logText(frame, key) !== want) throw new Error('log mismatch for ' + key);
  }
  async function activateRun(frame, key, want) {
    await waitFor(() => row(frame, key), 'row ' + key);
    if (!frame.contentDocument.querySelector('pre[data-scroll-key="' + key + '"]')) {
      await clickRow(frame, key);
    }
    await clickTab(frame, 'log');
    await assertLog(frame, key, want);
  }
  async function activateSubject(frame, value, want) {
    await activateRun(frame, mainRun, value === mainRun ? want : expected(mainRun, want.indexOf('live-after-restart') >= 0));
    const select = frame.contentDocument.querySelector('select[data-action="set-subject"]');
    if (!select) throw new Error('subject selector missing');
    if (select.value !== value) {
      select.value = value;
      select.dispatchEvent(new Event('change', { bubbles: true }));
      await sleep(300);
    }
    await clickTab(frame, 'log');
    await assertLog(frame, value, want);
  }
  function browserMetrics(frame) {
    const doc = frame.contentDocument;
    const pres = Array.from(doc.querySelectorAll('pre[data-scroll-key]'));
    const renderedLogBytes = pres.reduce((sum, pre) => sum + (pre.getAttribute('data-rendered-log') || '').length, 0);
    const retainedBytes = pres.reduce((sum, pre) => sum + Number(pre.getAttribute('data-log-retained-bytes') || 0), 0);
    const framePerformance = frame.contentWindow.performance;
    const streamResources = framePerformance.getEntriesByType('resource').filter(entry => entry.name.indexOf('/api/runs/stream') >= 0).length;
    return {
      domNodes: doc.querySelectorAll('*').length,
      preCount: pres.length,
      renderedLogBytes,
      retainedBytes,
      heapUsedBytes: framePerformance.memory ? framePerformance.memory.usedJSHeapSize : 0,
      streamResources,
    };
  }
  async function switchSubject(frame, value) {
    await waitFor(() => frame.contentDocument.querySelector('select[data-action="set-subject"]'), 'subject selector');
    const select = frame.contentDocument.querySelector('select[data-action="set-subject"]');
    select.value = value;
    select.dispatchEvent(new Event('change', { bubbles: true }));
    await sleep(300);
  }
  async function waitPhase(value) {
    await waitFor(async function () { return (await get('/phase')) === value; }, 'phase ' + value);
  }
  try {
    let frame = await loadPortal();
    await signal('loaded');
    await activateRun(frame, mainRun, expected(mainRun, false));
    await clickTab(frame, 'events');
    await clickTab(frame, 'log');
    await clickTab(frame, 'details');
    await clickTab(frame, 'log');
    await assertLog(frame, mainRun, expected(mainRun, false));
    await activateSubject(frame, reviewRun, expected(reviewRun, false));
    await activateSubject(frame, mainRun, expected(mainRun, false));
    await activateRun(frame, secondRun, expected(secondRun, false));
    await activateRun(frame, mainRun, expected(mainRun, false));
    await signal('visible-before-restart');
    await signal('interactions');
    await waitPhase('restart-ready');
    await activateRun(frame, mainRun, expected(mainRun, true));
    await signal('visible-after-restart');
    await activateSubject(frame, reviewRun, expected(reviewRun, true));
    await activateRun(frame, secondRun, expected(secondRun, true));
    await activateRun(frame, mainRun, expected(mainRun, true));
    frame = await loadPortal();
    await activateRun(frame, mainRun, expected(mainRun, true));
    await activateSubject(frame, reviewRun, expected(reviewRun, true));
    await activateRun(frame, secondRun, expected(secondRun, true));
    await activateRun(frame, mainRun, expected(mainRun, true));
    const metrics = browserMetrics(frame);
    if (metrics.domNodes <= 0 || metrics.preCount <= 0 || metrics.renderedLogBytes <= 0 || metrics.retainedBytes <= 0) {
      throw new Error('invalid browser retention metrics: ' + JSON.stringify(metrics));
    }
    await signal('metrics');
    const marker = document.createElement('pre');
    marker.id = 'portal-binary-browser-marker';
    marker.textContent = JSON.stringify({
      logs: {
        main: logText(frame, mainRun),
        review: logText(frame, reviewRun),
        second: logText(frame, secondRun),
      },
      metrics,
      subject: 'subject-switched',
    });
    document.body.appendChild(marker);
    await signal('done');
  } catch (err) {
    document.body.setAttribute('data-browser-error', String(err && err.stack || err));
    await fetch(control + '/signal?name=error&detail=' + encodeURIComponent(String(err && err.stack || err)));
    return;
  }
})();
</script></body>`
}

func controlServerURL(r *http.Request) string {
	return "http://" + r.Host
}

func buildPortalBrowserBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "sandman")
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve browser test source")
	}
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/sandman")
	cmd.Dir = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build Portal browser binary: %v\n%s", err, out)
	}
	return binPath
}

func portalBinarySourceRevision(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve browser test source for revision")
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("resolve Portal binary source revision: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func freePortalBrowserPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate Portal browser port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release Portal browser port: %v", err)
	}
	return port
}

func startPortalBrowserBinary(t *testing.T, binPath, repoDir string, port int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binPath, "portal", "--port", strconv.Itoa(port))
	cmd.Dir = repoDir
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start built Portal binary: %v", err)
	}
	return cmd
}

func waitPortalBrowserReady(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/api/runs")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("built Portal did not become ready at %s", baseURL)
}

func waitPortalBrowserSignal(t *testing.T, control *portalBinaryBrowserControl, want string) {
	t.Helper()
	select {
	case got := <-control.signals:
		if got == "error" {
			control.mu.RLock()
			detail := control.error
			control.mu.RUnlock()
			t.Fatalf("browser reported error while waiting for %q: %s", want, detail)
		}
		if got != want {
			t.Fatalf("browser signal = %q, want %q", got, want)
		}
	case <-time.After(20 * time.Second):
		control.mu.RLock()
		history := append([]string(nil), control.history...)
		phase, browserError := control.phase, control.error
		control.mu.RUnlock()
		t.Fatalf("timed out waiting for browser signal %q: history=%v phase=%q error=%q", want, history, phase, browserError)
	}
}

func appendPortalBrowserLog(t *testing.T, path, value string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open browser saved log: %v", err)
	}
	if _, err := file.WriteString(value); err != nil {
		_ = file.Close()
		t.Fatalf("append browser saved log: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close browser saved log: %v", err)
	}
}
