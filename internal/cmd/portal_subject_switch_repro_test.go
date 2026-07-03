package cmd

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
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
    window.setInterval = function (cb) {
      if (cb && cb.name === 'refresh') {
        setTimeout(function () {
          window.__portalRefreshCalls += 1;
          return cb();
        }, 100);
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

func runPortalChromium(t *testing.T, page string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("chromium"); err != nil {
		t.Skip("chromium not on PATH; skipping portal repro")
	}
	outPath := filepath.Join(t.TempDir(), "portal-repro.html")
	screenshotPath := filepath.Join(t.TempDir(), "portal-repro.png")
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
		"--screenshot="+screenshotPath,
		"file://"+outPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("chromium repro failed: %v\n%s", err, out)
	}
	if info, err := os.Stat(screenshotPath); err != nil {
		t.Fatalf("chromium screenshot missing: %v", err)
	} else if info.Size() == 0 {
		t.Fatal("chromium screenshot was empty")
	}
	return string(out), screenshotPath
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

type portalRect struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

func TestPortalReviewSubjectSwitch_PreservesSelectedSubjectAcrossRefresh(t *testing.T) {
	const logLineCount = 6000
	parentLogLines := make([]string, logLineCount)
	childLogLines := make([]string, logLineCount)
	for i := range parentLogLines {
		parentLogLines[i] = "parent log line " + strconv.Itoa(i+1) + " xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
		childLogLines[i] = "child log line " + strconv.Itoa(i+1) + " yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	}
	parentLog := strings.Join(parentLogLines, "\n")
	childLog := strings.Join(childLogLines, "\n")

	parent := map[string]any{
		"key":         "issue-1",
		"runId":       "issue-1",
		"kind":        "active",
		"status":      "reviewing",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"batchKey":    "issue-1",
		"reviewCount": 1,
		"log":         parentLog,
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
		"log":         childLog,
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
      var poll = document.querySelector('#poll-interval');
      if (poll) {
        poll.value = '500';
        poll.dispatchEvent(new Event('change', { bubbles: true }));
      }
    }, 50);
    setTimeout(function () {
      var row = document.querySelector('tr[data-run-key="issue-1"]');
      var detail = document.querySelector('tr.detail-row[data-detail-for="issue-1"]');
      var select = document.querySelector('select[data-action="set-subject"]');
      var title = row && row.querySelector('[data-cell="title"] .name');
      var meta = row && row.querySelector('[data-cell="title"] .meta-line');
      var selectedLabel = select && select.selectedIndex >= 0 && select.options[select.selectedIndex] ? select.options[select.selectedIndex].textContent : null;
      var pre = document.createElement('pre');
      pre.id = 'portal-repro';
      pre.textContent = JSON.stringify({
        selected: select && select.value,
        selectedLabel: selectedLabel,
        rowKey: row && row.getAttribute('data-run-key'),
        detailFor: detail && detail.getAttribute('data-detail-for'),
        rowName: title && title.textContent,
        metaText: meta && meta.innerText,
        detailText: detail && detail.innerText,
        fetchCalls: window.__portalFetchCalls || 0,
        changeCalls: window.__portalChangeCalls || 0
      });
      document.body.appendChild(pre);
    }, 2500);
  `)
	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-repro")
	var result struct {
		Selected      string `json:"selected"`
		SelectedLabel string `json:"selectedLabel"`
		RowKey        string `json:"rowKey"`
		DetailFor     string `json:"detailFor"`
		RowName       string `json:"rowName"`
		MetaText      string `json:"metaText"`
		DetailText    string `json:"detailText"`
		FetchCalls    int    `json:"fetchCalls"`
		ChangeCalls   int    `json:"changeCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse repro payload: %v\nraw=%s", err, payload)
	}
	if result.Selected != "PR42" {
		t.Fatalf("expected selected subject PR42 after refresh, got %#v", result)
	}
	if result.SelectedLabel != "PR42" {
		t.Fatalf("expected visible subject label PR42 after refresh, got %#v", result)
	}
	if result.RowKey != "issue-1" || result.DetailFor != "issue-1" {
		t.Fatalf("expected the visible parent row to stay locked to issue-1, got %#v", result)
	}
	if result.RowName != "#1" {
		t.Fatalf("expected visible row title to stay on the parent issue, got %#v", result)
	}
	if !strings.HasPrefix(strings.TrimSpace(result.MetaText), "Batch: issue-1") {
		t.Fatalf("expected visible batch metadata on the parent row, got %#v", result)
	}
	if !strings.Contains(result.DetailText, "child log line 1") || strings.Contains(result.DetailText, "parent log line 1") {
		t.Fatalf("expected visible detail panel to stay on the child log after refresh, got %#v", result)
	}
	if result.FetchCalls < 2 || result.ChangeCalls < 1 {
		t.Fatalf("expected change + refresh path to run, got %#v", result)
	}
}

func TestPortalReviewSubjectSwitch_GroupBeforeFilterKeepsParentIssueVisible(t *testing.T) {
	parent := map[string]any{
		"key":         "issue-1",
		"runId":       "issue-1",
		"kind":        "active",
		"status":      "success",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"reviewCount": 1,
		"log":         "parent log line 1",
	}
	child := map[string]any{
		"key":         "PR42",
		"runId":       "PR42",
		"kind":        "active",
		"status":      "reviewing",
		"issueLabel":  "PR42",
		"issueNumber": 1,
		"prNumber":    42,
		"review":      true,
		"log":         "child log line 1",
	}
	runsJSON, err := json.Marshal([]map[string]any{parent, child})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"PR42","tabs":{"PR42":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"selectedStatus":"reviewing","sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    setTimeout(function () {
      var select = document.querySelector('select[data-action="set-subject"]');
      var row = document.querySelector('tr[data-run-key]');
      var title = row && row.querySelector('[data-cell="title"] .name');
      var marker = document.createElement('pre');
      marker.id = 'portal-parent-visible';
      marker.textContent = JSON.stringify({
        rowKey: row && row.getAttribute('data-run-key'),
        rowName: title && title.textContent,
        selected: select && select.value,
        optionValues: select ? Array.from(select.options).map(function (opt) { return opt.value; }) : [],
      });
      document.body.appendChild(marker);
    }, 200);
  `)
	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-parent-visible")
	var result struct {
		RowKey       string   `json:"rowKey"`
		RowName      string   `json:"rowName"`
		Selected     string   `json:"selected"`
		OptionValues []string `json:"optionValues"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse parent-visible payload: %v\nraw=%s", err, payload)
	}
	if result.RowKey != "issue-1" {
		t.Fatalf("expected grouped row to stay on the parent issue, got %#v", result)
	}
	if result.RowName != "#1" {
		t.Fatalf("expected parent issue label to stay visible, got %#v", result)
	}
	if result.Selected != "PR42" {
		t.Fatalf("expected subject selector to stay on the review child, got %#v", result)
	}
	if len(result.OptionValues) != 2 || result.OptionValues[0] != "issue-1" || result.OptionValues[1] != "PR42" {
		t.Fatalf("expected distinct parent and review options, got %#v", result.OptionValues)
	}
}

func TestPortalLogPane_SubjectSwitchReusesCachedPane(t *testing.T) {
	const parentLog = "parent log line 1\nparent log line 2\nparent log line 3"
	const childLog = "child log line 1\nchild log line 2\nchild log line 3"

	parent := map[string]any{
		"key":         "issue-1",
		"runId":       "issue-1",
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"log":         parentLog,
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
		"log":         childLog,
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
      var detail = document.querySelector('tr.detail-row[data-detail-for="issue-1"] .detail-content');
      var parentPre1 = detail && detail.querySelector('pre[data-scroll-key]');
      if (!parentPre1) throw new Error('expected parent log pane initially');
      var parentFirstChild = parentPre1.firstChild;

      select.value = 'PR42';
      select.dispatchEvent(new Event('change', { bubbles: true }));

      setTimeout(function () {
        var childPre = detail && detail.querySelector('pre[data-scroll-key]');
        if (!childPre || childPre === parentPre1) throw new Error('expected fresh child pane on first subject switch');
        if (childPre.textContent.indexOf('child log line 1') === -1) throw new Error('expected child log after subject switch, got ' + (childPre && childPre.textContent));

        select.value = 'issue-1';
        select.dispatchEvent(new Event('change', { bubbles: true }));

        setTimeout(function () {
          var parentPre2 = detail && detail.querySelector('pre[data-scroll-key]');
          if (!parentPre2 || parentPre2 !== parentPre1) throw new Error('expected parent pane node to be reused on subject round-trip');
          if (parentPre2.firstChild !== parentFirstChild) throw new Error('expected parent pane children to be reused on subject round-trip');
          if (parentPre2.textContent.indexOf('parent log line 1') === -1) throw new Error('expected parent log after returning to parent subject');

          var pre = document.createElement('pre');
          pre.id = 'portal-log-cache';
          pre.textContent = JSON.stringify({
            parentNodeReused: parentPre2 === parentPre1,
            parentChildrenReused: parentPre2.firstChild === parentFirstChild,
            childNodeDistinct: childPre !== parentPre1,
            childText: childPre && childPre.textContent,
            parentText: parentPre2 && parentPre2.textContent,
          });
          document.body.appendChild(pre);
        }, 250);
      }, 250);
    }, 50);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-log-cache")
	var result struct {
		ParentNodeReused     bool   `json:"parentNodeReused"`
		ParentChildrenReused bool   `json:"parentChildrenReused"`
		ChildNodeDistinct    bool   `json:"childNodeDistinct"`
		ChildText            string `json:"childText"`
		ParentText           string `json:"parentText"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse log-cache payload: %v\nraw=%s", err, payload)
	}
	if !result.ParentNodeReused {
		t.Fatalf("expected parent pane to be reused on subject round-trip, got %#v", result)
	}
	if !result.ParentChildrenReused {
		t.Fatalf("expected parent pane children to be reused on subject round-trip, got %#v", result)
	}
	if !result.ChildNodeDistinct {
		t.Fatalf("expected child pane to be distinct from parent pane on first subject switch, got %#v", result)
	}
	if !strings.Contains(result.ChildText, "child log line 1") {
		t.Fatalf("expected child log text, got %#v", result)
	}
	if !strings.Contains(result.ParentText, "parent log line 1") {
		t.Fatalf("expected parent log text, got %#v", result)
	}
}

func TestPortalSummaryPoll_UsesIfNoneMatchAndKeepsRowsOn304(t *testing.T) {
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
	injection := `<script>
    window.__portalFetchCalls = 0;
    window.__portalIfNoneMatch = [];
    window.__portalRenderCalls = 0;
    window.requestAnimationFrame = function (cb) {
      window.__portalRenderCalls += 1;
      return setTimeout(function () { cb(performance.now()); }, 0);
    };
    window.cancelAnimationFrame = function (id) { clearTimeout(id); };
    window.setInterval = function (cb) {
      if (cb && cb.name === 'refresh') {
        setTimeout(function () { return cb(); }, 120);
      }
      return 1;
    };
    window.fetch = async function (input, init) {
      var headers = init && init.headers ? init.headers : {};
      var ifNoneMatch = '';
      if (headers && typeof headers.get === 'function') {
        ifNoneMatch = headers.get('If-None-Match') || '';
      } else if (headers && typeof headers === 'object') {
        ifNoneMatch = headers['If-None-Match'] || headers['if-none-match'] || '';
      }
      window.__portalIfNoneMatch.push(ifNoneMatch);
      window.__portalFetchCalls += 1;
      if (window.__portalFetchCalls === 1) {
        return {
          ok: true,
          status: 200,
          headers: { get: function (name) { return name === 'ETag' ? '"etag-1"' : ''; } },
          json: async function () {
            return { runs: [{ key: 'r1', runId: 'r1', kind: 'completed', status: 'success', issueLabel: '#1', issueNumber: 1, archived: false, unavailable: false, sourceExists: true }] };
          },
          text: async function () { return ''; },
        };
      }
      return {
        ok: false,
        status: 304,
        headers: { get: function (name) { return name === 'ETag' ? '"etag-1"' : ''; } },
        json: async function () { return { runs: [] }; },
        text: async function () { return ''; },
      };
    };
    setTimeout(function () {
      var row = document.querySelector('tr[data-run-key="r1"]');
      var marker = document.createElement('pre');
      marker.id = 'portal-summary-etag';
      marker.textContent = JSON.stringify({
        fetchCalls: window.__portalFetchCalls || 0,
        ifNoneMatch: window.__portalIfNoneMatch || [],
        renderCalls: window.__portalRenderCalls || 0,
        rowStillShown: !!row,
      });
      document.body.appendChild(marker);
    }, 2600);
    const apiPath = "/api/runs";`
	page = strings.Replace(page, "<script>\n    const apiPath = \"/api/runs\";", injection, 1)
	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-summary-etag")
	var result struct {
		FetchCalls    int      `json:"fetchCalls"`
		IfNoneMatch   []string `json:"ifNoneMatch"`
		RenderCalls   int      `json:"renderCalls"`
		RowStillShown bool     `json:"rowStillShown"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse summary-etag payload: %v\nraw=%s", err, payload)
	}
	if result.FetchCalls != 2 {
		t.Fatalf("expected 2 summary fetches, got %#v", result)
	}
	if len(result.IfNoneMatch) != 2 || result.IfNoneMatch[0] != "" || result.IfNoneMatch[1] != `"etag-1"` {
		t.Fatalf("expected second poll to send stored ETag, got %#v", result)
	}
	if result.RenderCalls != 1 {
		t.Fatalf("expected only initial render on 304 refresh, got %#v", result)
	}
	if !result.RowStillShown {
		t.Fatalf("expected row to stay visible after 304 refresh, got %#v", result)
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
      var metaRect = meta ? { left: meta.getBoundingClientRect().left, top: meta.getBoundingClientRect().top, width: meta.getBoundingClientRect().width, height: meta.getBoundingClientRect().height } : null;
      var pre = document.createElement('pre');
      pre.id = 'portal-meta-order';
      pre.textContent = JSON.stringify({
        rowKey: row && row.getAttribute('data-run-key'),
        metaText: meta && meta.innerText,
        metaRect: metaRect,
        lineCount: meta ? meta.innerText.split('\n').length : 0
      });
      document.body.appendChild(pre);
    }, 250);
  `)
	dom, screenshotPath := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-meta-order")
	var result struct {
		RowKey    string      `json:"rowKey"`
		MetaText  string      `json:"metaText"`
		MetaRect  *portalRect `json:"metaRect"`
		LineCount int         `json:"lineCount"`
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
	if result.MetaRect == nil {
		t.Fatalf("expected meta rect for screenshot scan, got %#v", result)
	}
	img, err := loadPortalScreenshot(screenshotPath)
	if err != nil {
		t.Fatalf("decode portal screenshot: %v", err)
	}
	bg := img.At(1, 1)
	bands := inkBands(img, *result.MetaRect, bg)
	if len(bands) < 2 || bands[0] >= bands[1] {
		t.Fatalf("expected screenshot bands for batch and run lines in order, got %#v bands=%v", result, bands)
	}
}

func TestPortalRefresh_LocksRowIdentityAcrossMixedBatchPayloads(t *testing.T) {
	batchID := "abcd-260618113825"
	runID := batchID + "-issue-42"
	initialRun := map[string]any{
		"key":         batchID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    batchID,
		"log":         "initial mixed log line 1\ninitial mixed log line 2",
	}
	refreshedRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    batchID,
		"log":         "refreshed mixed log line 1\nrefreshed mixed log line 2",
	}
	runsJSON, err := json.Marshal([]map[string]any{initialRun})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	refreshedRunsJSON, err := json.Marshal([]map[string]any{refreshedRun})
	if err != nil {
		t.Fatalf("marshal refreshed runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`
	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalFetchPayloads = [
      { runs: `+string(runsJSON)+` },
      { runs: `+string(refreshedRunsJSON)+` }
    ];
    window.__portalFetchCalls = 0;
    window.fetch = async function () {
      window.__portalFetchCalls += 1;
      var next = window.__portalFetchPayloads.length ? window.__portalFetchPayloads.shift() : { runs: [] };
      return {
        ok: true,
        status: 200,
        json: async function () { return next; },
        text: async function () { return ''; },
      };
    };
    (function pollForInitialRender(deadline) {
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var pre = detail && detail.querySelector('.detail-content pre[data-scroll-key]');
      var textReady = !!(pre && pre.innerText && pre.innerText.indexOf('initial mixed log line 1') !== -1);
      if (row && detail && pre && textReady) {
        window.__portalInitialRow = row;
        window.__portalInitialDetail = detail;
        window.__portalInitialDetailText = pre.innerText;
        return;
      }
      if (performance.now() >= deadline) {
        window.__portalInitialRow = row;
        window.__portalInitialDetail = detail;
        window.__portalInitialDetailText = pre ? pre.innerText : '';
        return;
      }
      setTimeout(function () { pollForInitialRender(deadline); }, 10);
    })(performance.now() + 1500);
    setTimeout(function () {
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var title = row && row.querySelector('[data-cell="title"] .name');
      var meta = row && row.querySelector('[data-cell="title"] .meta-line');
      var detailPre = detail && detail.querySelector('.detail-content pre[data-scroll-key]');
      var pre = document.createElement('pre');
      pre.id = 'portal-identity-refresh';
      pre.textContent = JSON.stringify({
        initialSameRow: window.__portalInitialRow === row,
        initialSameDetail: window.__portalInitialDetail === detail,
        initialDetailText: window.__portalInitialDetailText,
        rowKey: row && row.getAttribute('data-run-key'),
        detailFor: detail && detail.getAttribute('data-detail-for'),
        titleText: title && title.textContent,
        metaText: meta && meta.innerText,
        detailText: detailPre && detailPre.innerText,
        fetchCalls: window.__portalFetchCalls || 0,
      });
      document.body.appendChild(pre);
    }, 2200);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-identity-refresh")
	var result struct {
		InitialSameRow    bool   `json:"initialSameRow"`
		InitialSameDetail bool   `json:"initialSameDetail"`
		InitialDetailText string `json:"initialDetailText"`
		RowKey            string `json:"rowKey"`
		DetailFor         string `json:"detailFor"`
		TitleText         string `json:"titleText"`
		MetaText          string `json:"metaText"`
		DetailText        string `json:"detailText"`
		FetchCalls        int    `json:"fetchCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse identity payload: %v\nraw=%s", err, payload)
	}
	if !result.InitialSameRow {
		t.Fatalf("expected same rendered row node to survive refresh, got %#v", result)
	}
	if !result.InitialSameDetail {
		t.Fatalf("expected same detail node to survive refresh, got %#v", result)
	}
	if !strings.Contains(result.InitialDetailText, "initial mixed log line 1") {
		t.Fatalf("expected initial detail text to reflect first payload, got %#v", result)
	}
	if result.RowKey != runID {
		t.Fatalf("expected row identity locked to run %s, got %#v", runID, result)
	}
	if result.DetailFor != runID {
		t.Fatalf("expected detail linkage locked to run %s, got %#v", runID, result)
	}
	if result.TitleText != "#42" {
		t.Fatalf("expected visible title to stay on issue label, got %#v", result)
	}
	if !strings.Contains(result.MetaText, "Batch: "+batchID) || !strings.Contains(result.MetaText, "Run: "+runID) {
		t.Fatalf("expected batch/run metadata on refreshed row, got %#v", result)
	}
	if !strings.Contains(result.DetailText, "refreshed mixed log line 1") || strings.Contains(result.DetailText, "initial mixed log line 1") {
		t.Fatalf("expected refreshed detail text only, got %#v", result)
	}
	if result.FetchCalls < 2 {
		t.Fatalf("expected initial render plus refresh fetches, got %#v", result)
	}
}

func TestPortalRefresh_DiscardsQueuedExpandedStateBeforeDetailFetch(t *testing.T) {
	queued := map[string]any{
		"key":         "queued-1",
		"runId":       "queued-1",
		"kind":        "active",
		"status":      "queued",
		"issueLabel":  "#7",
		"issueNumber": 7,
		"batchKey":    "batch-7",
	}
	runsJSON, err := json.Marshal([]map[string]any{queued})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"queued-1","tabs":{"queued-1":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="queued-1"]');
      var stored = null;
      try {
        stored = JSON.parse(sessionStorage.getItem('sandman.portal.view-state.v1') || 'null');
      } catch (err) {}
      var pre = document.createElement('pre');
      pre.id = 'portal-queued-normalize';
      pre.textContent = JSON.stringify({
        detailExists: !!detail,
        expandedRunKey: stored && stored.expandedRunKey,
        fetchCalls: window.__portalFetchCalls || 0,
      });
      document.body.appendChild(pre);
    }, 2200);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-queued-normalize")
	var result struct {
		DetailExists   bool   `json:"detailExists"`
		ExpandedRunKey string `json:"expandedRunKey"`
		FetchCalls     int    `json:"fetchCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse queued-normalize payload: %v\nraw=%s", err, payload)
	}
	if result.DetailExists {
		t.Fatalf("expected queued state not to open detail, got %#v", result)
	}
	if result.ExpandedRunKey != "" {
		t.Fatalf("expected queued expanded state to be cleared, got %#v", result)
	}
	if result.FetchCalls > 2 {
		t.Fatalf("expected queued state not to trigger a detail fetch, got %#v", result)
	}
}

func TestPortalRefresh_IgnoresEmptyExpandedStateBeforeDetailFetch(t *testing.T) {
	run := map[string]any{
		"key":         "run-1",
		"runId":       "run-1",
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"batchKey":    "batch-1",
	}
	runsJSON, err := json.Marshal([]map[string]any{run})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"","tabs":{},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="run-1"]');
      var stored = null;
      try {
        stored = JSON.parse(sessionStorage.getItem('sandman.portal.view-state.v1') || 'null');
      } catch (err) {}
      var pre = document.createElement('pre');
      pre.id = 'portal-empty-identity';
      pre.textContent = JSON.stringify({
        detailExists: !!detail,
        expandedRunKey: stored && stored.expandedRunKey,
        fetchCalls: window.__portalFetchCalls || 0,
      });
      document.body.appendChild(pre);
    }, 2200);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-empty-identity")
	var result struct {
		DetailExists   bool   `json:"detailExists"`
		ExpandedRunKey string `json:"expandedRunKey"`
		FetchCalls     int    `json:"fetchCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse empty-identity payload: %v\nraw=%s", err, payload)
	}
	if result.DetailExists {
		t.Fatalf("expected empty expanded state not to open detail, got %#v", result)
	}
	if result.ExpandedRunKey != "" {
		t.Fatalf("expected empty expanded state to stay empty, got %#v", result)
	}
	if result.FetchCalls > 2 {
		t.Fatalf("expected empty expanded state not to trigger detail fetch, got %#v", result)
	}
}

func TestPortalRefresh_ActiveEventsTabHydratesDetail(t *testing.T) {
	runID := "abcd-260618113825-issue-42"
	summaryRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "live log line 1\nlive log line 2",
	}
	detailRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "live log line 1\nlive log line 2",
		"events": []map[string]any{
			{"type": "run.started", "timestamp": "2026-06-30T14:02:18Z", "payload": map[string]any{"branch": "sandman/review-42"}},
			{"type": "run.retry", "timestamp": "2026-06-30T14:03:18Z", "payload": map[string]any{"attempt": 2, "reason": "agent-stalled"}},
		},
	}
	runsJSON, err := json.Marshal([]map[string]any{summaryRun})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	detailRunJSON, err := json.Marshal(detailRun)
	if err != nil {
		t.Fatalf("marshal detail run: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.setInterval = function () { return 1; };
    window.__portalFetchCalls = 0;
    window.__portalDetailFetchCalls = 0;
    window.fetch = async function (input) {
      window.__portalFetchCalls += 1;
      var url = String(input || '');
      if (url.indexOf('?runKey=') >= 0) window.__portalDetailFetchCalls += 1;
      var payload = { runs: `+string(runsJSON)+` };
      if (url.indexOf('?runKey=`+runID+`') >= 0) {
        payload = { run: `+string(detailRunJSON)+` };
      }
      return {
        ok: true,
        status: 200,
        json: async function () { return payload; },
        text: async function () { return ''; },
      };
    };
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var button = detail && detail.querySelector('button[data-action="set-tab"][data-tab="events"]');
      if (!button) throw new Error('missing Events tab button');
      button.click();
    }, 120);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var pre = detail && detail.querySelector('pre[data-rendered-json]');
      var marker = document.createElement('pre');
      marker.id = 'portal-active-events-hydrates';
      marker.textContent = JSON.stringify({
        detailText: pre && pre.innerText,
        fetchCalls: window.__portalFetchCalls || 0,
        detailFetchCalls: window.__portalDetailFetchCalls || 0,
      });
      document.body.appendChild(marker);
    }, 700);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-active-events-hydrates")
	var result struct {
		DetailText       string `json:"detailText"`
		FetchCalls       int    `json:"fetchCalls"`
		DetailFetchCalls int    `json:"detailFetchCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse active events hydrate payload: %v\nraw=%s", err, payload)
	}
	if !strings.Contains(result.DetailText, "run.started") || !strings.Contains(result.DetailText, "agent-stalled") {
		t.Fatalf("expected active run events to hydrate, got %#v", result)
	}
	if strings.TrimSpace(result.DetailText) == "[]" {
		t.Fatalf("expected non-empty events JSON, got %#v", result)
	}
	if result.DetailFetchCalls < 1 {
		t.Fatalf("expected active-run detail fetch on Events tab click, got %#v", result)
	}
}

func TestPortalRefresh_ExpandedRunShowsLoadingCursorWhileDetailFetchPending(t *testing.T) {
	runID := "abcd-260618113825-issue-42"
	summaryRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "",
	}
	detailRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "live log line 1\nlive log line 2",
	}
	runsJSON, err := json.Marshal([]map[string]any{summaryRun})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	detailRunJSON, err := json.Marshal(detailRun)
	if err != nil {
		t.Fatalf("marshal detail run: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.setInterval = function () { return 1; };
    window.__portalFetchCalls = 0;
    window.__portalDetailFetchCalls = 0;
    window.__portalDetailResolve = null;
    window.fetch = async function (input) {
      window.__portalFetchCalls += 1;
      var url = String(input || '');
      if (url.indexOf('?summary=1') >= 0) {
        return {
          ok: true,
          status: 200,
          json: async function () { return { runs: `+string(runsJSON)+` }; },
          text: async function () { return ''; },
        };
      }
      if (url.indexOf('?runKey=`+runID+`') >= 0) {
        window.__portalDetailFetchCalls += 1;
        return {
          ok: true,
          status: 200,
          json: async function () {
            return await new Promise(function (resolve) {
              window.__portalDetailResolve = resolve;
            });
          },
          text: async function () { return ''; },
        };
      }
      throw new Error('unexpected fetch ' + url);
    };
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var panel = detail && detail.querySelector('.detail-panel');
      var marker = document.createElement('pre');
      marker.id = 'portal-detail-loading-before';
      marker.textContent = JSON.stringify({
        busy: !!(panel && panel.classList.contains('is-loading')),
        ariaBusy: panel && panel.getAttribute('aria-busy'),
        fetchCalls: window.__portalFetchCalls || 0,
        detailFetchCalls: window.__portalDetailFetchCalls || 0,
      });
      document.body.appendChild(marker);
      if (typeof window.__portalDetailResolve === 'function') {
        window.__portalDetailResolve({ run: `+string(detailRunJSON)+` });
      }
    }, 150);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var panel = detail && detail.querySelector('.detail-panel');
      var marker = document.createElement('pre');
      marker.id = 'portal-detail-loading-after';
      marker.textContent = JSON.stringify({
        busy: !!(panel && panel.classList.contains('is-loading')),
        ariaBusy: panel && panel.getAttribute('aria-busy'),
        fetchCalls: window.__portalFetchCalls || 0,
        detailFetchCalls: window.__portalDetailFetchCalls || 0,
      });
      document.body.appendChild(marker);
    }, 700);
  `)

	dom, _ := runPortalChromium(t, page)
	before := extractPortalMarker(t, dom, "portal-detail-loading-before")
	after := extractPortalMarker(t, dom, "portal-detail-loading-after")
	var beforePayload struct {
		Busy             bool   `json:"busy"`
		AriaBusy         string `json:"ariaBusy"`
		FetchCalls       int    `json:"fetchCalls"`
		DetailFetchCalls int    `json:"detailFetchCalls"`
	}
	if err := json.Unmarshal([]byte(before), &beforePayload); err != nil {
		t.Fatalf("parse loading-before payload: %v\nraw=%s", err, before)
	}
	if !beforePayload.Busy || beforePayload.AriaBusy != "true" {
		t.Fatalf("expected loading cursor before fetch settles, got %#v", beforePayload)
	}
	if beforePayload.DetailFetchCalls < 1 {
		t.Fatalf("expected detail fetch to start before settle, got %#v", beforePayload)
	}
	var afterPayload struct {
		Busy             bool   `json:"busy"`
		AriaBusy         string `json:"ariaBusy"`
		FetchCalls       int    `json:"fetchCalls"`
		DetailFetchCalls int    `json:"detailFetchCalls"`
	}
	if err := json.Unmarshal([]byte(after), &afterPayload); err != nil {
		t.Fatalf("parse loading-after payload: %v\nraw=%s", err, after)
	}
	if afterPayload.Busy || afterPayload.AriaBusy != "" {
		t.Fatalf("expected loading cursor to clear after fetch settles, got %#v", afterPayload)
	}
	if afterPayload.DetailFetchCalls < 1 {
		t.Fatalf("expected detail fetch to complete, got %#v", afterPayload)
	}
}

func TestPortalRefresh_ExpandedDetailsTabShowsLoadingCursorWhileDetailFetchPending(t *testing.T) {
	runID := "abcd-260618113825-issue-42"
	summaryRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "",
	}
	detailRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "live log line 1\nlive log line 2",
	}
	runsJSON, err := json.Marshal([]map[string]any{summaryRun})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	detailRunJSON, err := json.Marshal(detailRun)
	if err != nil {
		t.Fatalf("marshal detail run: %v", err)
	}
	stateJSON := `{"expandedRunKey":null,"tabs":{"` + runID + `":"details"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.setInterval = function () { return 1; };
    window.__portalFetchCalls = 0;
    window.__portalDetailFetchCalls = 0;
    window.__portalDetailResolve = null;
    window.fetch = async function (input) {
      window.__portalFetchCalls += 1;
      var url = String(input || '');
      if (url.indexOf('?summary=1') >= 0) {
        return {
          ok: true,
          status: 200,
          json: async function () { return { runs: `+string(runsJSON)+` }; },
          text: async function () { return ''; },
        };
      }
      if (url.indexOf('?runKey=`+runID+`') >= 0) {
        window.__portalDetailFetchCalls += 1;
        return {
          ok: true,
          status: 200,
          json: async function () {
            return await new Promise(function (resolve) {
              window.__portalDetailResolve = resolve;
            });
          },
          text: async function () { return ''; },
        };
      }
      throw new Error('unexpected fetch ' + url);
    };
    setTimeout(function () {
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      if (!row) throw new Error('missing run row');
      row.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    }, 50);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var panel = detail && detail.querySelector('.detail-panel');
      var marker = document.createElement('pre');
      marker.id = 'portal-expanded-details-loading-before';
      marker.textContent = JSON.stringify({
        busy: !!(panel && panel.classList.contains('is-loading')),
        ariaBusy: panel && panel.getAttribute('aria-busy'),
        fetchCalls: window.__portalFetchCalls || 0,
        detailFetchCalls: window.__portalDetailFetchCalls || 0,
      });
      document.body.appendChild(marker);
      if (typeof window.__portalDetailResolve === 'function') {
        window.__portalDetailResolve({ run: `+string(detailRunJSON)+` });
      }
    }, 150);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var panel = detail && detail.querySelector('.detail-panel');
      var marker = document.createElement('pre');
      marker.id = 'portal-expanded-details-loading-after';
      marker.textContent = JSON.stringify({
        busy: !!(panel && panel.classList.contains('is-loading')),
        ariaBusy: panel && panel.getAttribute('aria-busy'),
        fetchCalls: window.__portalFetchCalls || 0,
        detailFetchCalls: window.__portalDetailFetchCalls || 0,
      });
      document.body.appendChild(marker);
    }, 700);
  `)

	dom, _ := runPortalChromium(t, page)
	before := extractPortalMarker(t, dom, "portal-expanded-details-loading-before")
	after := extractPortalMarker(t, dom, "portal-expanded-details-loading-after")
	var beforePayload struct {
		Busy             bool   `json:"busy"`
		AriaBusy         string `json:"ariaBusy"`
		FetchCalls       int    `json:"fetchCalls"`
		DetailFetchCalls int    `json:"detailFetchCalls"`
	}
	if err := json.Unmarshal([]byte(before), &beforePayload); err != nil {
		t.Fatalf("parse expanded-details loading-before payload: %v\nraw=%s", err, before)
	}
	if !beforePayload.Busy || beforePayload.AriaBusy != "true" {
		t.Fatalf("expected loading cursor on expanded details-tab open, got %#v", beforePayload)
	}
	if beforePayload.DetailFetchCalls < 1 {
		t.Fatalf("expected detail fetch on expanded details-tab open, got %#v", beforePayload)
	}
	var afterPayload struct {
		Busy             bool   `json:"busy"`
		AriaBusy         string `json:"ariaBusy"`
		FetchCalls       int    `json:"fetchCalls"`
		DetailFetchCalls int    `json:"detailFetchCalls"`
	}
	if err := json.Unmarshal([]byte(after), &afterPayload); err != nil {
		t.Fatalf("parse expanded-details loading-after payload: %v\nraw=%s", err, after)
	}
	if afterPayload.Busy || afterPayload.AriaBusy != "" {
		t.Fatalf("expected loading cursor to clear after expanded details fetch settles, got %#v", afterPayload)
	}
	if afterPayload.DetailFetchCalls < 1 {
		t.Fatalf("expected detail fetch to complete, got %#v", afterPayload)
	}
}

func TestPortalRefresh_LogTabSwitchShowsLoadingCursorWhileDetailFetchPending(t *testing.T) {
	runID := "abcd-260618113825-issue-42"
	summaryRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "live log line 1\nlive log line 2",
	}
	detailRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "live log line 1\nlive log line 2\nlive log line 3",
	}
	runsJSON, err := json.Marshal([]map[string]any{summaryRun})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	detailRunJSON, err := json.Marshal(detailRun)
	if err != nil {
		t.Fatalf("marshal detail run: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"details"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.setInterval = function () { return 1; };
    window.__portalFetchCalls = 0;
    window.__portalDetailFetchCalls = 0;
    window.__portalDetailResolve = null;
    window.fetch = async function (input) {
      window.__portalFetchCalls += 1;
      var url = String(input || '');
      if (url.indexOf('?summary=1') >= 0) {
        return {
          ok: true,
          status: 200,
          json: async function () { return { runs: `+string(runsJSON)+` }; },
          text: async function () { return ''; },
        };
      }
      if (url.indexOf('?runKey=`+runID+`') >= 0) {
        window.__portalDetailFetchCalls += 1;
        return {
          ok: true,
          status: 200,
          json: async function () {
            return await new Promise(function (resolve) {
              window.__portalDetailResolve = resolve;
            });
          },
          text: async function () { return ''; },
        };
      }
      throw new Error('unexpected fetch ' + url);
    };
    setTimeout(function () {
      if (state && state.runs && state.runs.length) {
        state.runs[0].log = '';
      }
      if (typeof scheduleRender === 'function') scheduleRender();
    }, 150);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var button = detail && detail.querySelector('button[data-tab="log"]');
      if (!button) throw new Error('missing Log tab button');
      button.click();
    }, 250);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var panel = detail && detail.querySelector('.detail-panel');
      var marker = document.createElement('pre');
      marker.id = 'portal-log-tab-loading-before';
      marker.textContent = JSON.stringify({
        busy: !!(panel && panel.classList.contains('is-loading')),
        ariaBusy: panel && panel.getAttribute('aria-busy'),
        fetchCalls: window.__portalFetchCalls || 0,
        detailFetchCalls: window.__portalDetailFetchCalls || 0,
      });
      document.body.appendChild(marker);
      if (typeof window.__portalDetailResolve === 'function') {
        window.__portalDetailResolve({ run: `+string(detailRunJSON)+` });
      }
    }, 350);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var panel = detail && detail.querySelector('.detail-panel');
      var marker = document.createElement('pre');
      marker.id = 'portal-log-tab-loading-after';
      marker.textContent = JSON.stringify({
        busy: !!(panel && panel.classList.contains('is-loading')),
        ariaBusy: panel && panel.getAttribute('aria-busy'),
        fetchCalls: window.__portalFetchCalls || 0,
        detailFetchCalls: window.__portalDetailFetchCalls || 0,
      });
      document.body.appendChild(marker);
    }, 700);
  `)

	dom, _ := runPortalChromium(t, page)
	before := extractPortalMarker(t, dom, "portal-log-tab-loading-before")
	after := extractPortalMarker(t, dom, "portal-log-tab-loading-after")
	var beforePayload struct {
		Busy             bool   `json:"busy"`
		AriaBusy         string `json:"ariaBusy"`
		FetchCalls       int    `json:"fetchCalls"`
		DetailFetchCalls int    `json:"detailFetchCalls"`
	}
	if err := json.Unmarshal([]byte(before), &beforePayload); err != nil {
		t.Fatalf("parse log-tab loading-before payload: %v\nraw=%s", err, before)
	}
	if !beforePayload.Busy || beforePayload.AriaBusy != "true" {
		t.Fatalf("expected loading cursor during log-tab switch, got %#v", beforePayload)
	}
	if beforePayload.DetailFetchCalls < 1 {
		t.Fatalf("expected detail fetch on log-tab switch, got %#v", beforePayload)
	}
	var afterPayload struct {
		Busy             bool   `json:"busy"`
		AriaBusy         string `json:"ariaBusy"`
		FetchCalls       int    `json:"fetchCalls"`
		DetailFetchCalls int    `json:"detailFetchCalls"`
	}
	if err := json.Unmarshal([]byte(after), &afterPayload); err != nil {
		t.Fatalf("parse log-tab loading-after payload: %v\nraw=%s", err, after)
	}
	if afterPayload.Busy || afterPayload.AriaBusy != "" {
		t.Fatalf("expected loading cursor to clear after log-tab fetch settles, got %#v", afterPayload)
	}
	if afterPayload.DetailFetchCalls < 1 {
		t.Fatalf("expected detail fetch to complete, got %#v", afterPayload)
	}
}

func TestPortalRefresh_LogTabSwitchShowsLoadingCursorForMismatchedRunIdentity(t *testing.T) {
	runKey := "abcd-260618113825-row-42"
	runID := "abcd-260618113825-issue-42"
	summaryRun := map[string]any{
		"key":         runKey,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "live log line 1\nlive log line 2",
	}
	detailRun := map[string]any{
		"key":         runKey,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "abcd-260618113825",
		"log":         "live log line 1\nlive log line 2\nlive log line 3",
	}
	runsJSON, err := json.Marshal([]map[string]any{summaryRun})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	detailRunJSON, err := json.Marshal(detailRun)
	if err != nil {
		t.Fatalf("marshal detail run: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"details"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.setInterval = function () { return 1; };
    window.__portalFetchCalls = 0;
    window.__portalDetailFetchCalls = 0;
    window.__portalDetailResolve = null;
    window.fetch = async function (input) {
      window.__portalFetchCalls += 1;
      var url = String(input || '');
      if (url.indexOf('?summary=1') >= 0) {
        return {
          ok: true,
          status: 200,
          json: async function () { return { runs: `+string(runsJSON)+` }; },
          text: async function () { return ''; },
        };
      }
      if (url.indexOf('?runKey=') >= 0) {
        window.__portalDetailFetchCalls += 1;
        return {
          ok: true,
          status: 200,
          json: async function () {
            return await new Promise(function (resolve) {
              window.__portalDetailResolve = resolve;
            });
          },
          text: async function () { return ''; },
        };
      }
      throw new Error('unexpected fetch ' + url);
    };
    setTimeout(function () {
      window.requestAnimationFrame = function (cb) { return setTimeout(function () { cb(performance.now()); }, 250); };
      window.cancelAnimationFrame = function (id) { clearTimeout(id); };
    }, 100);
    setTimeout(function () {
      if (state && state.runs && state.runs.length) {
        state.runs[0].log = '';
      }
      if (typeof scheduleRender === 'function') scheduleRender();
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var button = detail && detail.querySelector('button[data-tab="log"]');
      if (!button) throw new Error('missing Log tab button');
      button.click();
    }, 150);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var panel = detail && detail.querySelector('.detail-panel');
      var marker = document.createElement('pre');
      marker.id = 'portal-log-tab-mismatch-loading-before';
      marker.textContent = JSON.stringify({
        busy: !!(panel && panel.classList.contains('is-loading')),
        ariaBusy: panel && panel.getAttribute('aria-busy'),
        fetchCalls: window.__portalFetchCalls || 0,
        detailFetchCalls: window.__portalDetailFetchCalls || 0,
      });
      document.body.appendChild(marker);
      if (typeof window.__portalDetailResolve === 'function') {
        window.__portalDetailResolve({ run: `+string(detailRunJSON)+` });
      }
    }, 250);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var panel = detail && detail.querySelector('.detail-panel');
      var marker = document.createElement('pre');
      marker.id = 'portal-log-tab-mismatch-loading-after';
      marker.textContent = JSON.stringify({
        busy: !!(panel && panel.classList.contains('is-loading')),
        ariaBusy: panel && panel.getAttribute('aria-busy'),
        fetchCalls: window.__portalFetchCalls || 0,
        detailFetchCalls: window.__portalDetailFetchCalls || 0,
      });
      document.body.appendChild(marker);
    }, 700);
  `)

	dom, _ := runPortalChromium(t, page)
	before := extractPortalMarker(t, dom, "portal-log-tab-mismatch-loading-before")
	after := extractPortalMarker(t, dom, "portal-log-tab-mismatch-loading-after")
	var beforePayload struct {
		Busy             bool   `json:"busy"`
		AriaBusy         string `json:"ariaBusy"`
		FetchCalls       int    `json:"fetchCalls"`
		DetailFetchCalls int    `json:"detailFetchCalls"`
	}
	if err := json.Unmarshal([]byte(before), &beforePayload); err != nil {
		t.Fatalf("parse mismatch loading-before payload: %v\nraw=%s", err, before)
	}
	if !beforePayload.Busy || beforePayload.AriaBusy != "true" {
		t.Fatalf("expected loading cursor during mismatched log-tab switch, got %#v", beforePayload)
	}
	if beforePayload.DetailFetchCalls < 1 {
		t.Fatalf("expected detail fetch on mismatched log-tab switch, got %#v", beforePayload)
	}
	var afterPayload struct {
		Busy             bool   `json:"busy"`
		AriaBusy         string `json:"ariaBusy"`
		FetchCalls       int    `json:"fetchCalls"`
		DetailFetchCalls int    `json:"detailFetchCalls"`
	}
	if err := json.Unmarshal([]byte(after), &afterPayload); err != nil {
		t.Fatalf("parse mismatch loading-after payload: %v\nraw=%s", err, after)
	}
	if afterPayload.Busy || afterPayload.AriaBusy != "" {
		t.Fatalf("expected loading cursor to clear after mismatched log-tab fetch settles, got %#v", afterPayload)
	}
	if afterPayload.DetailFetchCalls < 1 {
		t.Fatalf("expected detail fetch to complete, got %#v", afterPayload)
	}
}

func TestPortalRowClick_IgnoresForcedToggleAttrsOnQueuedRun(t *testing.T) {
	queued := map[string]any{
		"key":         "queued-2",
		"runId":       "queued-2",
		"kind":        "active",
		"status":      "queued",
		"issueLabel":  "#8",
		"issueNumber": 8,
		"batchKey":    "batch-8",
	}
	runsJSON, err := json.Marshal([]map[string]any{queued})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":null,"tabs":{},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    setTimeout(function () {
      var row = document.querySelector('tr[data-run-key="queued-2"]');
      if (!row) throw new Error('missing queued row');
      row.setAttribute('data-action', 'toggle-run');
      row.setAttribute('role', 'button');
      row.setAttribute('tabindex', '0');
      row.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    }, 50);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="queued-2"]');
      var stored = null;
      try {
        stored = JSON.parse(sessionStorage.getItem('sandman.portal.view-state.v1') || 'null');
      } catch (err) {}
      var pre = document.createElement('pre');
      pre.id = 'portal-queued-click';
      pre.textContent = JSON.stringify({
        detailExists: !!detail,
        expandedRunKey: stored && stored.expandedRunKey,
        fetchCalls: window.__portalFetchCalls || 0,
      });
      document.body.appendChild(pre);
    }, 2200);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-queued-click")
	var result struct {
		DetailExists   bool   `json:"detailExists"`
		ExpandedRunKey string `json:"expandedRunKey"`
		FetchCalls     int    `json:"fetchCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse queued-click payload: %v\nraw=%s", err, payload)
	}
	if result.DetailExists {
		t.Fatalf("expected forced toggle attrs to be ignored on queued row, got %#v", result)
	}
	if result.ExpandedRunKey != "" {
		t.Fatalf("expected queued row click to leave expanded state unchanged, got %#v", result)
	}
	if result.FetchCalls > 2 {
		t.Fatalf("expected queued click not to trigger detail fetch, got %#v", result)
	}
}

func TestPortalReviewSubjectSwitch_ReusesCachedParentPaneAcrossRoundTrip(t *testing.T) {
	parent := map[string]any{
		"key":         "issue-1",
		"runId":       "issue-1",
		"kind":        "active",
		"status":      "reviewing",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"reviewCount": 1,
		"log":         "parent log line 1\nparent log line 2\nparent log line 3",
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
		"log":         "review log line 1\nreview log line 2",
	}
	runsJSON, err := json.Marshal([]map[string]any{parent, child})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"issue-1","tabs":{"issue-1":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__hlCalls = 0;
    if (typeof window.requestIdleCallback === 'function') {
      window.requestIdleCallback = function () { return 0; };
    }
    var __origHL = SandmanPortalDiff.highlightTerminalLog;
    SandmanPortalDiff.highlightTerminalLog = function () {
      window.__hlCalls += 1;
      return __origHL.apply(this, arguments);
    };
    setTimeout(function () {
      var pre = document.querySelector('pre[data-scroll-key]');
      if (!pre) throw new Error('missing initial parent log pre');
      window.__initialPre = pre;
      window.__initialFirstChild = pre.firstChild;
      window.__beforeChildCalls = window.__hlCalls;
      var select = document.querySelector('select[data-action="set-subject"]');
      if (!select) throw new Error('missing subject selector');
      select.value = 'PR42';
      select.dispatchEvent(new Event('change', { bubbles: true }));
    }, 50);
    setTimeout(function () {
      var pre = document.querySelector('pre[data-scroll-key]');
      if (!pre) throw new Error('missing child log pre after first switch');
      window.__childSamePane = pre === window.__initialPre;
      window.__childCalls = window.__hlCalls - window.__beforeChildCalls;
      window.__beforeReturnCalls = window.__hlCalls;
      var select = document.querySelector('select[data-action="set-subject"]');
      if (!select) throw new Error('missing subject selector before return');
      select.value = 'issue-1';
      select.dispatchEvent(new Event('change', { bubbles: true }));
    }, 180);
    setTimeout(function () {
      var pre = document.querySelector('pre[data-scroll-key]');
      var marker = document.createElement('pre');
      marker.id = 'portal-subject-cache';
      marker.textContent = JSON.stringify({
        childSamePane: window.__childSamePane,
        childHighlightCalls: window.__childCalls,
        returnSamePane: pre === window.__initialPre,
        returnSameFirstChild: pre && window.__initialFirstChild ? pre.firstChild === window.__initialFirstChild : false,
        returnHighlightCalls: window.__hlCalls - window.__beforeReturnCalls,
        text: pre && pre.textContent
      });
      document.body.appendChild(marker);
    }, 360);
  `)
	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-subject-cache")
	var result struct {
		ChildSamePane       bool   `json:"childSamePane"`
		ChildHighlightCalls int    `json:"childHighlightCalls"`
		ReturnSamePane      bool   `json:"returnSamePane"`
		ReturnSameFirst     bool   `json:"returnSameFirstChild"`
		ReturnHighlight     int    `json:"returnHighlightCalls"`
		Text                string `json:"text"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse subject cache payload: %v\nraw=%s", err, payload)
	}
	if !strings.Contains(result.Text, "parent log line 1") {
		t.Fatalf("expected parent log after round-trip, got %#v", result)
	}
	if result.ChildSamePane {
		t.Fatalf("expected first subject switch to mount a fresh child pane, got %#v", result)
	}
	if result.ChildHighlightCalls < 1 {
		t.Fatalf("expected first subject switch to highlight the child pane, got %#v", result)
	}
	if !result.ReturnSamePane || !result.ReturnSameFirst {
		t.Fatalf("expected cached parent pane to be reused on return, got %#v", result)
	}
	if result.ReturnHighlight != 0 {
		t.Fatalf("expected return-to-parent to avoid re-highlighting, got %#v", result)
	}
}

func TestPortalSubjectSwitch_IgnoresEmptySelectionChange(t *testing.T) {
	parent := map[string]any{
		"key":         "issue-1",
		"runId":       "issue-1",
		"kind":        "active",
		"status":      "reviewing",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"reviewCount": 1,
		"log":         "parent log line 1\nparent log line 2",
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
		"log":         "review log line 1\nreview log line 2",
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
      select.value = '';
      select.dispatchEvent(new Event('change', { bubbles: true }));
    }, 50);
    setTimeout(function () {
      var select = document.querySelector('select[data-action="set-subject"]');
      var detail = document.querySelector('pre[data-scroll-key]');
      var stored = null;
      try {
        stored = JSON.parse(sessionStorage.getItem('sandman.portal.view-state.v1') || 'null');
      } catch (err) {}
      var marker = document.createElement('pre');
      marker.id = 'portal-empty-subject';
      marker.textContent = JSON.stringify({
        subjectValue: select && select.value,
        detailText: detail && detail.textContent,
        expandedRunKey: stored && stored.expandedRunKey,
        fetchCalls: window.__portalFetchCalls || 0,
      });
      document.body.appendChild(marker);
    }, 360);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-empty-subject")
	var result struct {
		SubjectValue   string `json:"subjectValue"`
		DetailText     string `json:"detailText"`
		ExpandedRunKey string `json:"expandedRunKey"`
		FetchCalls     int    `json:"fetchCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse empty-subject payload: %v\nraw=%s", err, payload)
	}
	if result.SubjectValue != "issue-1" {
		t.Fatalf("expected blank subject change to be ignored and parent to remain selected, got %#v", result)
	}
	if !strings.Contains(result.DetailText, "parent log line 1") {
		t.Fatalf("expected parent detail to remain visible after blank subject change, got %#v", result)
	}
	if result.ExpandedRunKey != "issue-1" {
		t.Fatalf("expected blank subject change to leave expanded state unchanged, got %#v", result)
	}
	if result.FetchCalls > 2 {
		t.Fatalf("expected blank subject change not to trigger extra fetch, got %#v", result)
	}
}

func TestPortalRefreshHydratesRestoredExpandedRunDetail(t *testing.T) {
	runID := "run-42-issue"
	summaryRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "completed",
		"status":      "success",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "run-42",
	}
	detailRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "completed",
		"status":      "success",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    "run-42",
		"log":         "hydrated log line 1\nhydrated log line 2",
		"events":      []map[string]any{{"type": "run.finished"}},
	}
	summaryRunsJSON, err := json.Marshal([]map[string]any{summaryRun})
	if err != nil {
		t.Fatalf("marshal summary runs: %v", err)
	}
	detailRunJSON, err := json.Marshal(detailRun)
	if err != nil {
		t.Fatalf("marshal detail run: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, summaryRunsJSON, `
    window.__portalFetchCalls = 0;
    window.fetch = async function (input) {
      window.__portalFetchCalls += 1;
      var url = String(input || '');
      var payload = url.indexOf('?runKey=') >= 0 ? { run: `+string(detailRunJSON)+` } : { runs: `+string(summaryRunsJSON)+` };
      return {
        ok: true,
        status: 200,
        json: async function () { return payload; },
        text: async function () { return ''; },
      };
    };
    setTimeout(function () {
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var detailPre = detail && detail.querySelector('.detail-content pre[data-scroll-key]');
      var marker = document.createElement('pre');
      marker.id = 'portal-refresh-hydrates';
      marker.textContent = JSON.stringify({
        rowKey: row && row.getAttribute('data-run-key'),
        detailFor: detail && detail.getAttribute('data-detail-for'),
        detailText: detailPre && detailPre.innerText,
        fetchCalls: window.__portalFetchCalls || 0,
      });
      document.body.appendChild(marker);
    }, 2200);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-refresh-hydrates")
	var result struct {
		RowKey     string `json:"rowKey"`
		DetailFor  string `json:"detailFor"`
		DetailText string `json:"detailText"`
		FetchCalls int    `json:"fetchCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse refresh hydrate payload: %v\nraw=%s", err, payload)
	}
	if result.RowKey != runID || result.DetailFor != runID {
		t.Fatalf("expected restored expanded row and detail to stay linked, got %#v", result)
	}
	if !strings.Contains(result.DetailText, "hydrated log line 1") {
		t.Fatalf("expected refreshed detail text to hydrate from runKey fetch, got %#v", result)
	}
	if result.FetchCalls < 2 {
		t.Fatalf("expected summary refresh plus detail fetch, got %#v", result)
	}
}

func TestPortalReviewSubjectSwitch_HydratesSelectedSubjectDetail(t *testing.T) {
	parent := map[string]any{
		"key":         "issue-1",
		"runId":       "issue-1",
		"kind":        "active",
		"status":      "reviewing",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"reviewCount": 1,
	}
	childSummary := map[string]any{
		"key":         "PR42",
		"runId":       "PR42",
		"kind":        "completed",
		"status":      "success",
		"issueLabel":  "PR42",
		"issueNumber": 1,
		"prNumber":    42,
		"review":      true,
	}
	childDetail := map[string]any{
		"key":         "PR42",
		"runId":       "PR42",
		"kind":        "completed",
		"status":      "success",
		"issueLabel":  "PR42",
		"issueNumber": 1,
		"prNumber":    42,
		"review":      true,
		"log":         "child hydrated log line 1\nchild hydrated log line 2",
		"events":      []map[string]any{{"type": "run.finished"}},
	}
	parentDetail := map[string]any{
		"key":         "issue-1",
		"runId":       "issue-1",
		"kind":        "active",
		"status":      "reviewing",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"reviewCount": 1,
		"log":         "parent hydrated log line 1\nparent hydrated log line 2",
		"events":      []map[string]any{{"type": "run.started"}},
	}
	runsJSON, err := json.Marshal([]map[string]any{parent, childSummary})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	parentDetailJSON, err := json.Marshal(parentDetail)
	if err != nil {
		t.Fatalf("marshal parent detail: %v", err)
	}
	childDetailJSON, err := json.Marshal(childDetail)
	if err != nil {
		t.Fatalf("marshal child detail: %v", err)
	}
	stateJSON := `{"expandedRunKey":"issue-1","tabs":{"issue-1":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalFetchCalls = 0;
    window.fetch = async function (input) {
      window.__portalFetchCalls += 1;
      var url = String(input || '');
      var next = { runs: `+string(runsJSON)+` };
      if (url.indexOf('?runKey=PR42') >= 0) {
        next = { run: `+string(childDetailJSON)+` };
      } else if (url.indexOf('?runKey=issue-1') >= 0) {
        next = { run: `+string(parentDetailJSON)+` };
      }
      return {
        ok: true,
        status: 200,
        json: async function () { return next; },
        text: async function () { return ''; },
      };
    };
	    setTimeout(function () {
	      var select = document.querySelector('select[data-action="set-subject"]');
	      if (!select) throw new Error('missing subject selector');
	      select.value = 'PR42';
	      select.dispatchEvent(new Event('change', { bubbles: true }));
	    }, 120);
	    setTimeout(function () {
	      var select = document.querySelector('select[data-action="set-subject"]');
	      var detail = document.querySelector('pre[data-scroll-key]');
	      var marker = document.createElement('pre');
	      marker.id = 'portal-subject-switch-hydrates';
	      marker.textContent = JSON.stringify({
	        subjectValue: select && select.value,
	        detailText: detail && detail.textContent,
	        fetchCalls: window.__portalFetchCalls || 0,
	      });
	      document.body.appendChild(marker);
	    }, 2200);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-subject-switch-hydrates")
	var result struct {
		SubjectValue string `json:"subjectValue"`
		DetailText   string `json:"detailText"`
		FetchCalls   int    `json:"fetchCalls"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse subject switch hydrate payload: %v\nraw=%s", err, payload)
	}
	if result.SubjectValue != "PR42" {
		t.Fatalf("expected switched subject to remain selected, got %#v", result)
	}
	if !strings.Contains(result.DetailText, "child hydrated log line 1") {
		t.Fatalf("expected switched subject detail to hydrate, got %#v", result)
	}
	if result.FetchCalls < 2 {
		t.Fatalf("expected initial refresh plus subject detail fetch, got %#v", result)
	}
}

func loadPortalScreenshot(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

func rectContainsInk(img image.Image, rect portalRect, background color.Color) bool {
	if rect.Width <= 0 || rect.Height <= 0 {
		return false
	}
	minX := clampInt(int(rect.Left), 0, img.Bounds().Dx()-1)
	maxX := clampInt(int(rect.Left+rect.Width), 0, img.Bounds().Dx()-1)
	minY := clampInt(int(rect.Top), 0, img.Bounds().Dy()-1)
	maxY := clampInt(int(rect.Top+rect.Height), 0, img.Bounds().Dy()-1)
	if minX > maxX || minY > maxY {
		return false
	}
	for y := minY; y <= maxY; y += max(1, (maxY-minY)/3) {
		for x := minX; x <= maxX; x += max(1, (maxX-minX)/5) {
			if !sameColor(img.At(x, y), background) {
				return true
			}
		}
	}
	return false
}

func inkBands(img image.Image, rect portalRect, background color.Color) []int {
	if rect.Width <= 0 || rect.Height <= 0 {
		return nil
	}
	minX := clampInt(int(rect.Left), 0, img.Bounds().Dx()-1)
	maxX := clampInt(int(rect.Left+rect.Width), 0, img.Bounds().Dx()-1)
	minY := clampInt(int(rect.Top), 0, img.Bounds().Dy()-1)
	maxY := clampInt(int(rect.Top+rect.Height), 0, img.Bounds().Dy()-1)
	if minX > maxX || minY > maxY {
		return nil
	}
	var bands []int
	inBand := false
	for y := minY; y <= maxY; y++ {
		hasInk := false
		for x := minX; x <= maxX; x += max(1, (maxX-minX)/12) {
			if !sameColor(img.At(x, y), background) {
				hasInk = true
				break
			}
		}
		if hasInk {
			if !inBand {
				bands = append(bands, y)
				inBand = true
			}
		} else {
			inBand = false
		}
	}
	return bands
}

func sameColor(a, b color.Color) bool {
	const tolerance = 0x2020
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return absDiff(ar, br) <= tolerance && absDiff(ag, bg) <= tolerance && absDiff(ab, bb) <= tolerance && absDiff(aa, ba) <= tolerance
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
