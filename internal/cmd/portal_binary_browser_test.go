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
}

func TestPortalBuiltBinaryBrowserContinuity(t *testing.T) {
	if _, err := exec.LookPath("chromium"); err != nil {
		t.Skip("chromium not on PATH; focused Linux CI installs it")
	}

	binPath := buildPortalBrowserBinary(t)
	repoDir := shortTempDir(t)
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)
	mainRun, reviewRun, secondRun, logPaths := createPortalBinaryBrowserFixture(t, repoDir)
	port := freePortalBrowserPort(t)
	initialURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	control := &portalBinaryBrowserControl{portalURL: initialURL, signals: make(chan string, 8)}
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
			if name := r.URL.Query().Get("name"); name == "error" {
				control.mu.Lock()
				control.error = r.URL.Query().Get("detail")
				control.mu.Unlock()
			}
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
	for _, marker := range []string{"portal-binary-browser-marker", "live-before-restart", "live-after-restart", "subject-switched"} {
		if !strings.Contains(output, marker) {
			t.Fatalf("browser output missing %q:\n%s", marker, output)
		}
	}
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
		if err := os.WriteFile(logPath, []byte("["+run.id+"] 10:00:00 initial\n"), 0o644); err != nil {
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
    await new Promise((resolve, reject) => { frame.onload = resolve; frame.onerror = reject; });
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
    await clickRow(frame, mainRun);
    await clickTab(frame, 'events');
    await clickTab(frame, 'log');
    await clickTab(frame, 'details');
    await clickTab(frame, 'log');
    await switchSubject(frame, reviewRun);
    await switchSubject(frame, mainRun);
     await clickRow(frame, mainRun);
     await clickRow(frame, secondRun);
     await clickRow(frame, mainRun);
    const source = frame.contentWindow.streamSources && frame.contentWindow.streamSources[mainRun];
    if (typeof frame.contentWindow.stopRunStream === 'function') frame.contentWindow.stopRunStream(mainRun);
    await clickTab(frame, 'log');
    await signal('interactions');
    await waitPhase('restart-ready');
     frame = await loadPortal();
     await clickRow(frame, mainRun);
    await clickTab(frame, 'events');
    await clickTab(frame, 'log');
     await switchSubject(frame, reviewRun);
     await switchSubject(frame, mainRun);
    await waitFor(() => {
      const text = frame.contentDocument.body.textContent || '';
      return text.indexOf('live-before-restart') >= 0 && text.indexOf('live-after-restart') >= 0;
    }, 'post-restart live records');
    const marker = document.createElement('pre');
    marker.id = 'portal-binary-browser-marker';
    marker.textContent = JSON.stringify({
      main: frame.contentDocument.body.textContent || '',
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
		t.Fatalf("timed out waiting for browser signal %q", want)
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
