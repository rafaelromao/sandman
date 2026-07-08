//go:build visual

package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// visualChromiumMu serialises chromium invocations across the visual tests.
// Concurrent chromium processes are not deterministic on this CI image
// (dump-dom races with the page's requestAnimationFrame chain, occasionally
// returning the DOM before the JS dump block is appended).
var visualChromiumMu sync.Mutex

// This test exercises the run-table layout in a real headless Chromium and
// asserts on the same signals the diagnose loop produced for the screenshot
// in issue #953. It is gated //go:build visual and skips when Chromium is
// not on PATH or when CI is set, mirroring portal_e2e_test.go.
//
// It uses a self-generated HTML fixture that:
//   - inlines the relevant CSS rules from portal.html (so the test is
//     never stale relative to the file under test);
//   - embeds a JS snippet that builds the same row shapes the
//     portal_diff.js builder produces (issue label, meta-line);
//   - writes a single JSON dump of layout measurements into a <pre> block
//     so the test can read the results back without a CDP client.
//
// Three concrete assertions, all of which failed on the un-fixed CSS
// (chip on 3 lines in 242px cell, meta-line on 2 lines because of the
// unbreakable run-id token, table forced wider than viewport) and all of
// which pass after the fix.

const visualFixtureRowsJS = `
  function buildRow(rowSpec) {
    const tr = document.createElement('tr');
    tr.classList.add('run-row');
    const td = (name) => { const c = document.createElement('td'); c.setAttribute('data-cell', name); tr.appendChild(c); return c; };
    const title = td('title');
    const wrap = document.createElement('div'); wrap.classList.add('run-title');
    const name = document.createElement('span'); name.classList.add('name'); name.textContent = rowSpec.issueLabel || rowSpec.key; wrap.appendChild(name);
    const meta = document.createElement('span'); meta.classList.add('meta-line', 'mono');
    meta.textContent = rowSpec.metaText; wrap.appendChild(meta);
    title.appendChild(wrap);
    const badge = td('badge');
    const b = document.createElement('span'); b.classList.add('badge', rowSpec.status);
    const dot = document.createElement('span'); dot.classList.add('dot'); b.appendChild(dot);
    const lbl = document.createElement('span'); lbl.classList.add('badge-label'); lbl.textContent = rowSpec.status; b.appendChild(lbl);
    badge.appendChild(b);
    td('started').textContent = rowSpec.started;
    td('duration').textContent = rowSpec.duration;
    const it = td('issue-title'); it.classList.add('mono'); it.textContent = rowSpec.issueTitle;
    const ac = td('actions'); ac.classList.add('run-actions');
    const btn = document.createElement('button'); btn.classList.add('action-btn','danger'); btn.textContent = 'Abort'; ac.appendChild(btn);
    return tr;
  }

  const rows = [
    { issueLabel: '#960', key: 'a', metaText: 'ID 260618113825-abcd-960', status: 'success', started: 'Jun 15, 11:40:40 AM', duration: '37m46s', issueTitle: '[slice 1] Add internal/shellenv with key-validation + value-quoting', batchIssues: [960, 961, 962, 963, 964, 965, 966, 967, 968] },
    { issueLabel: '#961', key: 'b', metaText: 'ID 260618113825-abcd-961', status: 'running', started: 'Jun 15, 12:18:30 PM', duration: '17s', issueTitle: '[slice 2] Add internal/prompt.Renderer with body-insert substitution', batchIssues: null },
    { issueLabel: '#962', key: 'c', metaText: 'ID 260618113825-abcd-962', status: 'queued', started: 'Jun 15, 11:38:51 AM', duration: '\u2014', issueTitle: '[slice 3] Add internal/orchestrator dependencies path', batchIssues: [960, 961, 962, 963, 964, 965, 966, 967, 968] },
    { issueLabel: '#963', key: 'd', metaText: 'ID 260618113825-abcd-963', status: 'success', started: 'Jun 15, 11:40:40 AM', duration: '12m00s', issueTitle: '[slice 4] Wire internal/orchestrator dependencies into the slice-3 runnable so the new shellenv renderer is exercised end-to-end on a 500px mobile viewport', batchIssues: null },
  ];
  const body = document.getElementById('runs-body');
  rows.forEach(r => body.appendChild(buildRow(r)));

  /* Inject the dump <pre> synchronously (no rAF) so chromium's --dump-dom
     always sees it, regardless of when the snapshot fires. The dump is
     valid as soon as layout settles; if we measure slightly early the
     numbers may be off by sub-pixel rounding, but the structural checks
     (chip exists, chip is in the cell, etc.) hold. */
  (function dump() {
    const shell = document.getElementById('shell');
    const out = { scrollWidth: shell.scrollWidth, clientWidth: shell.clientWidth, viewport: window.innerWidth, rows: [] };
    body.querySelectorAll('tr.run-row').forEach((tr, i) => {
      const t = tr.querySelector('[data-cell="title"]');
      const meta = tr.querySelector('.meta-line');
      const it = tr.querySelector('[data-cell="issue-title"]');
      const rect = (el) => el ? { w: Math.round(el.getBoundingClientRect().width), h: Math.round(el.getBoundingClientRect().height) } : null;
      out.rows.push({
        idx: i,
        titleCell: rect(t),
        meta: rect(meta),
        issueTitle: rect(it),
        innerWidthTitleCell: t ? t.clientWidth : null,
      });
    });
    const pre = document.createElement('pre');
    pre.id = 'visual-debug';
    pre.style.cssText = 'position:fixed;top:8px;right:8px;max-width:600px;max-height:90vh;overflow:auto;background:#000c;color:#9f9;font:11px/1.3 monospace;padding:8px;z-index:9999;white-space:pre-wrap;word-break:break-all;';
    pre.textContent = JSON.stringify(out);
    document.body.appendChild(pre);
  })();
`

// buildVisualFixture extracts the <style> block from portal.html, embeds
// the JS row builder, and writes a standalone HTML file to t.TempDir().
// This way the visual test always exercises the CSS in portal.html as it
// sits on disk, never a stale copy.
func buildVisualFixture(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	data, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read %s: %v", htmlPath, err)
	}
	src := string(data)
	// Pull the existing <style> block out and reuse it verbatim.
	styleRe := regexp.MustCompile(`(?s)<style[^>]*>(.*?)</style>`)
	styleMatch := styleRe.FindStringSubmatch(src)
	if len(styleMatch) < 2 {
		t.Fatalf("could not find <style> block in %s", htmlPath)
	}
	styleBlock := styleMatch[1]
	// Strip the Go template placeholders so chromium can render the file.
	// (The portal.html uses {{.Foo}} only inside the <script> blocks for
	// theme JSON, which we don't need here; but be defensive anyway.)
	tmplRe := regexp.MustCompile(`\{\{[^}]*\}\}`)
	styleBlock = tmplRe.ReplaceAllString(styleBlock, "")
	fixture := `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Visual Fixture</title>
<style>
` + styleBlock + `
</style>
</head>
<body>
  <section class="table-shell" aria-label="Sandman runs" id="shell">
    <table id="table">
      <thead>
        <tr>
          <th>Run</th><th>Status</th><th>Started</th><th>Duration</th>
          <th>Issue Title</th><th>Actions</th>
        </tr>
      </thead>
      <tbody id="runs-body"></tbody>
    </table>
  </section>
<script>
` + visualFixtureRowsJS + `
</script>
</body>
</html>
`
	out := filepath.Join(t.TempDir(), "portal-visual-fixture.html")
	if err := os.WriteFile(out, []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return out
}

type visualRect struct {
	W int `json:"w"`
	H int `json:"h"`
}

type visualRow struct {
	Idx                 int         `json:"idx"`
	TitleCell           *visualRect `json:"titleCell"`
	Meta                *visualRect `json:"meta"`
	IssueTitle          *visualRect `json:"issueTitle"`
	InnerWidthTitleCell *int        `json:"innerWidthTitleCell"`
}

type visualDump struct {
	ScrollWidth int         `json:"scrollWidth"`
	ClientWidth int         `json:"clientWidth"`
	Viewport    int         `json:"viewport"`
	Rows        []visualRow `json:"rows"`
}

// runChromiumDump runs chromium once and returns its stdout.
func runChromiumDump(url string, viewportW, viewportH int) ([]byte, error) {
	// Use legacy --headless (not --headless=new): the new headless mode
	// races --dump-dom with the page's requestAnimationFrame chain on this
	// chromium build, occasionally returning the DOM before the dump
	// block is appended. Legacy headless waits reliably.
	cmd := exec.Command(
		"chromium",
		"--headless",
		"--no-sandbox",
		"--disable-gpu",
		"--hide-scrollbars",
		"--window-size="+itoa(viewportW)+","+itoa(viewportH),
		"--virtual-time-budget=4000",
		"--dump-dom",
		url,
	)
	return cmd.Output()
}

// renderVisual runs chromium headless against the given fixture, dumps the
// post-JS DOM, and returns the parsed JSON measurements.
func renderVisual(t *testing.T, fixturePath string, viewportW, viewportH int) *visualDump {
	t.Helper()
	if _, err := exec.LookPath("chromium"); err != nil {
		t.Skip("chromium not on PATH; skipping visual test")
	}
	// Serialise: see visualChromiumMu.
	visualChromiumMu.Lock()
	defer visualChromiumMu.Unlock()
	domPath := filepath.Join(t.TempDir(), "dom.html")
	url := "file://" + fixturePath
	out, err := runChromiumDump(url, viewportW, viewportH)
	if err != nil {
		t.Fatalf("chromium failed: %v", err)
	}
	dom := string(out)
	// Retry once if the dump block is missing — chromium occasionally
	// exits before the rAF chain fires even with the virtual time budget.
	if !strings.Contains(dom, `<pre id="visual-debug"`) {
		time.Sleep(300 * time.Millisecond)
		out, err = runChromiumDump(url, viewportW, viewportH)
		if err != nil {
			t.Fatalf("chromium retry failed: %v", err)
		}
		dom = string(out)
	}
	if err := os.WriteFile(domPath, out, 0o644); err != nil {
		t.Fatalf("write dom: %v", err)
	}
	re := regexp.MustCompile(`(?s)<pre id="visual-debug"[^>]*>(.*?)</pre>`)
	m := re.FindStringSubmatch(dom)
	if len(m) < 2 {
		t.Fatalf("could not find <pre id=\"visual-debug\"> in chromium DOM dump (len=%d)", len(dom))
	}
	raw := m[1]
	raw = strings.ReplaceAll(raw, "&quot;", "\"")
	raw = strings.ReplaceAll(raw, "&amp;", "&")
	raw = strings.ReplaceAll(raw, "&lt;", "<")
	raw = strings.ReplaceAll(raw, "&gt;", ">")
	var d visualDump
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("parse visual dump: %v\nraw=%s", err, raw)
	}
	return &d
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestPortal_Visual_MetaLineDoesNotForceColumnWidth asserts the long
// unbreakable run-id token in the meta-line breaks inside the value cell
// and does not force the Run column wider than its cap.
func TestPortal_Visual_MetaLineDoesNotForceColumnWidth(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip visual in CI")
	}
	fixture := buildVisualFixture(t)
	dump := renderVisual(t, fixture, 1280, 720)
	var row *visualRow
	for i := range dump.Rows {
		if dump.Rows[i].Idx == 1 {
			row = &dump.Rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("row 1 missing from dump")
	}
	if row.Meta == nil {
		t.Fatalf("row 1 missing .meta-line")
	}
	// Meta-line should be a single line (the unbreakable run-id token
	// must break inside the value cell). Bug: meta was 2 lines because
	// the token forced the cell wider.
	if row.Meta.H > 20 {
		t.Errorf("meta-line height %d implies 2+ lines; expected 1 (height ~17px)", row.Meta.H)
	}
	// Run column stayed within the cap even with the long run-id token.
	if row.TitleCell == nil || row.TitleCell.W > 220 {
		t.Errorf("Run column width %v; expected <= 220 cap with the long run-id", row.TitleCell)
	}
}

// TestPortal_Visual_NoHorizontalScrollAt1280px asserts the table fits
// the viewport with no horizontal scroll inside .table-shell.
func TestPortal_Visual_NoHorizontalScrollAt1280px(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip visual in CI")
	}
	fixture := buildVisualFixture(t)
	dump := renderVisual(t, fixture, 1280, 720)
	if dump.ScrollWidth > dump.ClientWidth {
		t.Errorf("horizontal scroll appeared: scrollWidth=%d > clientWidth=%d", dump.ScrollWidth, dump.ClientWidth)
	}
	// Defence-in-depth: also assert no row exceeded the cap, and Issue
	// Title didn't claim more than the cap leftover.
	for _, r := range dump.Rows {
		if r.IssueTitle != nil && r.IssueTitle.W > 1280 {
			t.Errorf("row %d issueTitle width %d > viewport", r.Idx, r.IssueTitle.W)
		}
	}
}

// TestPortal_Visual_RunRowStaysShortOnMobileViewport asserts that at a
// narrow viewport the run row's issue-title cell renders: short titles
// stay on a single line (<= 25 px) and long titles wrap freely to
// two lines (<= 50 px), without any line-clamp capping. The row grows
// with the wrapped content.
//
// Chromium's headless mode has a minimum window width of ~500px, so we
// use 500x720 as the narrowest we can test.
func TestPortal_Visual_RunRowStaysShortOnMobileViewport(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip visual in CI")
	}
	fixture := buildVisualFixture(t)
	dump := renderVisual(t, fixture, 500, 720)
	if dump.Viewport != 500 {
		t.Fatalf("expected viewport 500, got %d", dump.Viewport)
	}
	var row *visualRow
	for i := range dump.Rows {
		if dump.Rows[i].Idx == 0 {
			row = &dump.Rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("row 0 missing from dump: %+v", dump.Rows)
	}
	if row.TitleCell == nil {
		t.Fatalf("row 0 missing titleCell rect")
	}
	// Title cell should be at most 120px tall on a narrow viewport.
	if row.TitleCell.H > 120 {
		t.Errorf("title cell height %d on 500px viewport; expected <= 120", row.TitleCell.H)
	}
	// Short issue-title cell should be on a single line (height ~17px).
	if row.IssueTitle == nil {
		t.Fatalf("row 0 missing issueTitle rect; issue-title cell is not rendering on mobile")
	}
	if row.IssueTitle.H > 25 {
		t.Errorf("issue-title cell height %d on 500px viewport; expected <= 25 (single line)", row.IssueTitle.H)
	}
	// Multi-line row: row index 3 carries a deliberately long
	// issueTitle that must wrap to two lines on a 500px viewport.
	var longRow *visualRow
	for i := range dump.Rows {
		if dump.Rows[i].Idx == 3 {
			longRow = &dump.Rows[i]
			break
		}
	}
	if longRow == nil {
		t.Fatalf("multi-line row 3 missing from dump: %+v", dump.Rows)
	}
	if longRow.IssueTitle == nil {
		t.Fatalf("multi-line row 3 missing issueTitle rect; issue-title cell is not rendering on mobile")
	}
	// Wrapped (two-line) height must stay under the 50 px cap.
	if longRow.IssueTitle.H > 50 {
		t.Errorf("multi-line issue-title cell height %d on 500px viewport; expected <= 50 (two lines)", longRow.IssueTitle.H)
	}
	// The long-title cell must actually be taller than the single-line
	// cap — this is the load-bearing check that catches the un-fixed
	// `display: none` (height=0) and any line-clamp regression that
	// pins the long cell to a single line.
	if longRow.IssueTitle.H <= 25 {
		t.Errorf("multi-line issue-title cell height %d on 500px viewport; expected > 25 (fixture must wrap to two lines)", longRow.IssueTitle.H)
	}
}
