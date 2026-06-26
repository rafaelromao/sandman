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
        refreshCalls: window.__portalRefreshCalls || 0,
        changeCalls: window.__portalChangeCalls || 0
      });
      document.body.appendChild(pre);
    }, 2000);
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
		RefreshCalls  int    `json:"refreshCalls"`
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
      var firstTop = null;
      var secondTop = null;
      var firstRect = null;
      var secondRect = null;
      var firstText = null;
      var secondText = null;
      if (meta && meta.firstChild && meta.firstChild.nodeType === Node.TEXT_NODE) {
        var text = meta.innerText || meta.textContent || '';
        var split = text.indexOf('\n');
        if (split >= 0) {
          var range = document.createRange();
          range.setStart(meta.firstChild, 0);
          range.setEnd(meta.firstChild, split);
          var rects = range.getClientRects();
          if (rects.length) {
            firstTop = rects[0].top;
            firstRect = { left: rects[0].left, top: rects[0].top, width: rects[0].width, height: rects[0].height };
          }
          firstText = text.slice(0, split);
          range.setStart(meta.firstChild, split + 1);
          range.setEnd(meta.firstChild, meta.firstChild.textContent.length);
          rects = range.getClientRects();
          if (rects.length) {
            secondTop = rects[0].top;
            secondRect = { left: rects[0].left, top: rects[0].top, width: rects[0].width, height: rects[0].height };
          }
          secondText = text.slice(split + 1);
        }
      }
      var pre = document.createElement('pre');
      pre.id = 'portal-meta-order';
      pre.textContent = JSON.stringify({
        rowKey: row && row.getAttribute('data-run-key'),
        metaText: meta && meta.innerText,
        lineCount: meta ? meta.innerText.split('\n').length : 0,
        firstTop: firstTop,
        secondTop: secondTop,
        firstRect: firstRect,
        secondRect: secondRect,
        firstText: firstText,
        secondText: secondText
      });
      document.body.appendChild(pre);
    }, 250);
  `)
	dom, screenshotPath := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-meta-order")
	var result struct {
		RowKey     string      `json:"rowKey"`
		MetaText   string      `json:"metaText"`
		LineCount  int         `json:"lineCount"`
		FirstTop   *float64    `json:"firstTop"`
		SecondTop  *float64    `json:"secondTop"`
		FirstRect  *portalRect `json:"firstRect"`
		SecondRect *portalRect `json:"secondRect"`
		FirstText  string      `json:"firstText"`
		SecondText string      `json:"secondText"`
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
	if result.FirstTop == nil || result.SecondTop == nil || !(*result.FirstTop < *result.SecondTop) {
		t.Fatalf("expected batch line to render above run line, got %#v", result)
	}
	if result.FirstRect == nil || result.SecondRect == nil {
		t.Fatalf("expected screenshot rects for both lines, got %#v", result)
	}
	img, err := loadPortalScreenshot(screenshotPath)
	if err != nil {
		t.Fatalf("decode portal screenshot: %v", err)
	}
	bg := img.At(1, 1)
	if !rectContainsInk(img, *result.FirstRect, bg) {
		t.Fatalf("expected screenshot ink for batch line, got %#v", result)
	}
	if !rectContainsInk(img, *result.SecondRect, bg) {
		t.Fatalf("expected screenshot ink for run line, got %#v", result)
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
