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

func loadPortalReproAssets(t *testing.T) (string, string, string, string) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	dir := filepath.Dir(currentFile)
	html, err := os.ReadFile(filepath.Join(dir, "portal.html"))
	if err != nil {
		t.Fatalf("read portal html: %v", err)
	}
	stateJS, err := os.ReadFile(filepath.Join(dir, "portal_state.js"))
	if err != nil {
		t.Fatalf("read portal state: %v", err)
	}
	scrollJS, err := os.ReadFile(filepath.Join(dir, "portal_scroll.js"))
	if err != nil {
		t.Fatalf("read portal scroll: %v", err)
	}
	diffJS, err := os.ReadFile(filepath.Join(dir, "portal_diff.js"))
	if err != nil {
		t.Fatalf("read portal diff: %v", err)
	}
	return string(html), string(stateJS), string(scrollJS), string(diffJS)
}

func buildPortalReproPage(t *testing.T, stateJSON string, runsJSON []byte, body string) string {
	t.Helper()
	html, stateJS, scrollJS, diffJS := loadPortalReproAssets(t)
	page := html
	page = strings.ReplaceAll(page, "{{.SupportedThemesJSON}}", `['sandman']`)
	page = strings.ReplaceAll(page, "{{.PortalStateJS}}", stateJS)
	page = strings.ReplaceAll(page, "{{.PortalScrollJS}}", scrollJS)
	page = strings.ReplaceAll(page, "{{.PortalDiffJS}}", diffJS)
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
    window.__portalFetchCalls = 0;
    window.__portalRefreshCalls = 0;
    window.__portalRefresh = null;
    window.setInterval = function (cb) {
      if (cb && cb.name === 'refresh') {
        window.__portalRefresh = function () {
          window.__portalRefreshCalls += 1;
          return cb();
        };
      }
      return 1;
    };
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
      window.__portalFetchCalls += 1;
      return {
        ok: true,
        status: 200,
        json: async function () { return { runs: %s }; },
        text: async function () { return ''; },
      };
    };
%s
    const apiPath = "/api/runs";`, strconv.Quote(stateJSON), string(runsJSON), body)
	page = strings.Replace(page, "<script>\n    const apiPath = \"/api/runs\";", injection, 1)
	return page
}

func runPortalChromium(t *testing.T, page string) string {
	t.Helper()
	if _, err := exec.LookPath("chromium"); err != nil {
		t.Skip("chromium not on PATH; skipping portal repro")
	}
	outPath := filepath.Join(t.TempDir(), "portal-repro.html")
	if err := os.WriteFile(outPath, []byte(page), 0o644); err != nil {
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
	return string(out)
}

func extractPortalMarker(t *testing.T, dom, id string) string {
	t.Helper()
	if !strings.Contains(dom, `id="`+id+`"`) {
		t.Fatalf("repro marker %s missing from chromium DOM dump:\n%s", id, dom)
	}
	re := regexp.MustCompile(`(?s)<pre id="` + regexp.QuoteMeta(id) + `"[^>]*>(.*?)</pre>`)
	m := re.FindStringSubmatch(dom)
	if len(m) < 2 {
		t.Fatalf("could not parse repro marker %s from DOM dump:\n%s", id, dom)
	}
	return m[1]
}

func TestPortalReviewSubjectSwitch_PreservesSelectedSubjectAcrossRefresh(t *testing.T) {
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

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    setTimeout(function () {
      var select = document.querySelector('select[data-action="set-subject"]');
      if (!select) throw new Error('missing subject selector');
      select.value = 'PR42';
      select.dispatchEvent(new Event('change', { bubbles: true }));
    }, 50);
    setTimeout(function () {
      if (typeof window.__portalRefresh === 'function') {
        window.__portalRefresh();
      }
    }, 100);
    setTimeout(function () {
      var row = document.querySelector('tr[data-run-key="issue-1"]');
      var detail = document.querySelector('tr.detail-row[data-detail-for="issue-1"]');
      var select = document.querySelector('select[data-action="set-subject"]');
      var title = row && row.querySelector('[data-cell="title"] .name');
      var meta = row && row.querySelector('[data-cell="title"] .meta-line');
      var content = detail && detail.querySelector('.detail-content');
      var pre = document.createElement('pre');
      pre.id = 'portal-repro';
      pre.textContent = JSON.stringify({
        selected: select && select.value,
        rowKey: row && row.getAttribute('data-run-key'),
        detailFor: detail && detail.getAttribute('data-detail-for'),
        rowName: title && title.textContent,
        metaText: meta && meta.textContent,
        subjectFingerprint: content && content.getAttribute('data-rendered-subject-fingerprint'),
        fetchCalls: window.__portalFetchCalls || 0,
        refreshCalls: window.__portalRefreshCalls || 0,
        changeCalls: window.__portalChangeCalls || 0
      });
      document.body.appendChild(pre);
    }, 2000);
  `)
	dom := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-repro")
	var result struct {
		Selected           string `json:"selected"`
		RowKey             string `json:"rowKey"`
		DetailFor          string `json:"detailFor"`
		RowName            string `json:"rowName"`
		MetaText           string `json:"metaText"`
		SubjectFingerprint string `json:"subjectFingerprint"`
		FetchCalls         int    `json:"fetchCalls"`
		RefreshCalls       int    `json:"refreshCalls"`
		ChangeCalls        int    `json:"changeCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse repro payload: %v\nraw=%s", err, payload)
	}
	if result.Selected != "PR42" {
		t.Fatalf("expected selected subject PR42 after refresh, got %#v", result)
	}
	if result.RowKey != "issue-1" || result.DetailFor != "issue-1" {
		t.Fatalf("expected the visible parent row to stay locked to issue-1, got %#v", result)
	}
	if !strings.Contains(result.SubjectFingerprint, "PR42") {
		t.Fatalf("expected detail fingerprint to keep the selected subject, got %#v", result)
	}
	if result.FetchCalls < 2 || result.RefreshCalls < 1 || result.ChangeCalls < 1 {
		t.Fatalf("expected change + refresh path to run, got %#v", result)
	}
}

func TestPortalBatchMetadata_RendersBatchAboveRun(t *testing.T) {
	batchID := "abcd-260618113825"
	runID := batchID + "-issue-42"
	run := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "completed",
		"status":      "success",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    batchID,
		"startedAt":   "2025-06-26T12:00:00Z",
	}
	runsJSON, err := json.Marshal([]map[string]any{run})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":null,"tabs":{},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`
	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    setTimeout(function () {
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      var meta = row && row.querySelector('[data-cell="title"] .meta-line');
      var pre = document.createElement('pre');
      pre.id = 'portal-meta-order';
      pre.textContent = JSON.stringify({
        rowKey: row && row.getAttribute('data-run-key'),
        metaText: meta && meta.textContent,
        lineCount: meta ? meta.textContent.split('\n').length : 0
      });
      document.body.appendChild(pre);
    }, 250);
  `)
	dom := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-meta-order")
	var result struct {
		RowKey    string `json:"rowKey"`
		MetaText  string `json:"metaText"`
		LineCount int    `json:"lineCount"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse meta payload: %v\nraw=%s", err, payload)
	}
	if result.RowKey != runID {
		t.Fatalf("expected batch row %s, got %#v", runID, result)
	}
	lines := strings.Split(strings.TrimSpace(result.MetaText), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected batch metadata to span two lines, got %#v", result)
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[0]), "Batch: "+batchID) {
		t.Fatalf("expected batch identifier on first line, got %#v", result)
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[1]), "Run: "+runID) {
		t.Fatalf("expected run identifier on second line, got %#v", result)
	}
}
