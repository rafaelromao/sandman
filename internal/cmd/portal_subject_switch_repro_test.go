package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestPortalReviewSubjectSwitch_Repro(t *testing.T) {
	if _, err := exec.LookPath("chromium"); err != nil {
		t.Skip("chromium not on PATH; skipping portal repro")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	dir := filepath.Dir(currentFile)
	htmlPath := filepath.Join(dir, "portal.html")
	statePath := filepath.Join(dir, "portal_state.js")
	scrollPath := filepath.Join(dir, "portal_scroll.js")
	diffPath := filepath.Join(dir, "portal_diff.js")

	html, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read portal html: %v", err)
	}
	stateJS, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read portal state: %v", err)
	}
	scrollJS, err := os.ReadFile(scrollPath)
	if err != nil {
		t.Fatalf("read portal scroll: %v", err)
	}
	diffJS, err := os.ReadFile(diffPath)
	if err != nil {
		t.Fatalf("read portal diff: %v", err)
	}

	const logLineCount = 6000
	logLines := make([]string, logLineCount)
	for i := range logLines {
		logLines[i] = "review log line " + strconv.Itoa(i+1) + " xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	}
	bigLog := strings.Join(logLines, "\n")

	parent := map[string]any{
		"key":         "issue-1",
		"runId":       "issue-1",
		"kind":        "active",
		"status":      "reviewing",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"reviewCount": 1,
		"log":         bigLog,
	}
	child := map[string]any{
		"key":         "PR42",
		"runId":       "PR42",
		"kind":        "completed",
		"status":      "success",
		"issueLabel":  "PR42",
		"issueNumber": 1,
		"prNumber":    42,
		"review":      true,
		"log":         bigLog,
	}
	runsJSON, err := json.Marshal([]map[string]any{parent, child})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"issue-1","tabs":{"issue-1":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := string(html)
	page = strings.ReplaceAll(page, "{{.SupportedThemesJSON}}", `['sandman']`)
	page = strings.ReplaceAll(page, "{{.PortalStateJS}}", string(stateJS))
	page = strings.ReplaceAll(page, "{{.PortalScrollJS}}", string(scrollJS))
	page = strings.ReplaceAll(page, "{{.PortalDiffJS}}", string(diffJS))
	page = strings.ReplaceAll(page, "{{.ThemeOptionsHTML}}", `<option value="sandman">Sandman</option>`)
	page = strings.ReplaceAll(page, "{{.RefreshPath}}", "/api/runs")
	page = strings.ReplaceAll(page, "{{.PortalAbortSupported}}", "false")
	page = strings.ReplaceAll(page, "{{.PortalStateStorageKey}}", "sandman.portal.view-state.v1")
	page = strings.ReplaceAll(page, "{{.PollInterval}}", "600000")

	injection := fmt.Sprintf(`<script>
    try { sessionStorage.setItem('sandman.portal.view-state.v1', %s); } catch (err) {}
    window.requestAnimationFrame = function (cb) { return setTimeout(function () { cb(performance.now()); }, 0); };
    window.cancelAnimationFrame = function (id) { clearTimeout(id); };
    window.__portalChangeCalls = 0;
    var __origAddEventListener = EventTarget.prototype.addEventListener;
    EventTarget.prototype.addEventListener = function (type, listener, options) {
      if (this && this.getAttribute && this.getAttribute('id') === 'runs-body' && type === 'change' && typeof listener === 'function') {
        var wrapped = function () {
          window.__portalChangeCalls += 1;
          return listener.apply(this, arguments);
        };
        return __origAddEventListener.call(this, type, wrapped, options);
      }
      return __origAddEventListener.call(this, type, listener, options);
    };
    window.fetch = async function () {
      return {
        ok: true,
        status: 200,
        json: async function () { return { runs: %s }; },
        text: async function () { return ''; },
      };
    };
    window.__portalSaveCalls = [];
    var __origSave = window.SandmanPortalState && window.SandmanPortalState.save;
    if (__origSave) {
      window.SandmanPortalState.save = function (value) {
        window.__portalSaveCalls.push(value);
        return __origSave.call(this, value);
      };
    }
    window.__portalDiffDurations = [];
    window.__portalDiffCalls = 0;
    var __origDiffRuns = window.SandmanPortalDiff && window.SandmanPortalDiff.diffRuns;
    if (__origDiffRuns) {
      window.SandmanPortalDiff.diffRuns = function () {
        var start = performance.now();
        try {
          return __origDiffRuns.apply(this, arguments);
        } finally {
          window.__portalDiffDurations.push(performance.now() - start);
          window.__portalDiffCalls += 1;
        }
      };
    }
    setTimeout(function () {
      var select = document.querySelector('select[data-action="set-subject"]');
      if (!select) throw new Error('missing subject selector');
      select.value = 'PR42';
      select.dispatchEvent(new Event('change', { bubbles: true }));
    }, 50);
    setTimeout(function () {
      var pre = document.createElement('pre');
      pre.id = 'portal-repro';
      pre.textContent = JSON.stringify({ changes: window.__portalChangeCalls, saves: window.__portalSaveCalls, calls: window.__portalDiffCalls, durations: window.__portalDiffDurations });
      document.body.appendChild(pre);
    }, 2000);
    const apiPath = "/api/runs";`, strconv.Quote(stateJSON), string(runsJSON))
	page = strings.Replace(page, "<script>\n    const apiPath = \"/api/runs\";", injection, 1)

	outPath := filepath.Join(t.TempDir(), "portal-repro.html")
	if err := os.WriteFile(outPath, []byte(page), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cmd := exec.Command(
		"chromium",
		"--headless",
		"--no-sandbox",
		"--disable-gpu",
		"--hide-scrollbars",
		"--window-size=1360,900",
		"--virtual-time-budget=10000",
		"--dump-dom",
		"file://"+outPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("chromium repro failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `id="portal-repro"`) {
		t.Fatalf("repro marker missing from chromium DOM dump:\n%s", out)
	}
	re := regexp.MustCompile(`(?s)<pre id="portal-repro"[^>]*>(.*?)</pre>`)
	m := re.FindStringSubmatch(string(out))
	if len(m) < 2 {
		t.Fatalf("could not parse repro marker from DOM dump:\n%s", out)
	}
	var payload struct {
		Calls     int       `json:"calls"`
		Durations []float64 `json:"durations"`
	}
	if err := json.Unmarshal([]byte(m[1]), &payload); err != nil {
		t.Fatalf("parse repro payload: %v\nraw=%s", err, m[1])
	}
	t.Logf("portal repro payload: calls=%d durations=%v", payload.Calls, payload.Durations)
	if payload.Calls < 2 {
		t.Fatalf("expected at least initial render + subject switch render, got %#v", payload)
	}
	if len(payload.Durations) < 2 {
		t.Fatalf("expected at least two diffRuns timings, got %#v", payload)
	}
	if payload.Durations[len(payload.Durations)-1] > 20 {
		t.Fatalf("expected subject-switch render to stay cheap, got %#v", payload)
	}
}
