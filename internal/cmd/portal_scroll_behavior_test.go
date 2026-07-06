package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPortal_ToggleRunUsesInstantNearestScrollIntoView covers the primary
// behavior in issue #1560: when toggleRun opens a previously-closed row,
// the rAF render callback must invoke the row's scrollIntoView with
// behavior: 'instant' and block: 'nearest' — not behavior: 'smooth', which
// queues a microtask animation that compounds with the next poll's diff.
//
// It runs the live toggleRun function extracted from portal.html through
// a vm sandbox, so the assertion surfaces whatever the production code
// actually calls. If portal.html ever regresses to behavior: 'smooth'
// (or any non-instant / non-nearest options), this test goes red.
func TestPortal_ToggleRunUsesInstantNearestScrollIntoView(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal scroll behavior test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	helperDir := filepath.Dir(currentFile)
	portalHtmlPath := filepath.Join(helperDir, "portal.html")

	script := `
const fs = require('fs');
const vm = require('vm');
const portalHtmlSrc = fs.readFileSync(` + "`" + portalHtmlPath + "`" + `, 'utf8');

function extractFunction(src, name) {
  const re = new RegExp('(?:^|\\n)[ \\t]*function[ \\t]+' + name + '\\s*\\([^)]*\\)\\s*\\{', 'm');
  const startMatch = re.exec(src);
  if (!startMatch) return null;
  const openIdx = startMatch.index + startMatch[0].length - 1;
  let depth = 1, i = openIdx + 1;
  while (i < src.length && depth > 0) {
    const ch = src[i];
    if (ch === '{') depth++;
    else if (ch === '}') depth--;
    i++;
  }
  return src.slice(startMatch.index, i);
}

const toggleRunSrc = extractFunction(portalHtmlSrc, 'toggleRun');
if (!toggleRunSrc) throw new Error('could not extract toggleRun from portal.html');

const scrollCalls = [];
const stubRow = {
  scrollIntoView: function (opts) {
    scrollCalls.push(opts);
  },
};
const sandbox = {
  console: console,
  state: {
    expandedRunKey: null,
    runs: [{ key: 'r1', runId: 'r1', kind: 'completed', status: 'success', issueLabel: '#1', issueNumber: 1 }],
    tabs: {},
    activeBatches: false,
    showArchived: false,
    selectedStatus: '',
  },
  findRunByIdentity: function (key) {
    return sandbox.state.runs.find((r) => r && (r.key === key || r.runId === key)) || null;
  },
  isWaitStateRun: function () { return false; },
  subjectRunIdentity: function (run) { return run ? (run.runId || run.key || '') : ''; },
  summarizeReviewGroup: function (group) { return { source: (group && group[0]) || null }; },
  persistPortalViewState: function () {},
  scheduleRender: function (cb) { if (typeof cb === 'function') cb(); },
  loadRunDetail: function () {},
  findRunRowByIdentity: function (key) {
    if (key === sandbox.state.expandedRunKey) return stubRow;
    return null;
  },
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
sandbox.JSON = JSON;
sandbox.Set = Set;
sandbox.Map = Map;

const scheduleRenderStub = 'function scheduleRender(callback) { if (typeof callback === "function") { callback(); } }';
vm.runInNewContext(toggleRunSrc + '\n' + scheduleRenderStub + '\nthis.toggleRun = toggleRun; this.scheduleRender = scheduleRender;', sandbox, { filename: 'portal.html#behavior' });

if (typeof sandbox.toggleRun !== 'function') throw new Error('toggleRun did not eval into the sandbox');
sandbox.toggleRun('r1');

const captured = {
  expandedAfter: sandbox.state.expandedRunKey,
  scrollCount: scrollCalls.length,
  lastOptions: scrollCalls.length > 0 ? scrollCalls[scrollCalls.length - 1] : null,
  behavior: scrollCalls.length > 0 ? scrollCalls[scrollCalls.length - 1].behavior : null,
  block: scrollCalls.length > 0 ? scrollCalls[scrollCalls.length - 1].block : null,
  raw: JSON.stringify(scrollCalls),
};
process.stdout.write(JSON.stringify(captured));
`

	cmd := exec.Command("node", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("portal scroll behavior harness failed: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse harness output %q: %v", strings.TrimSpace(string(out)), err)
	}
	if expanded, _ := got["expandedAfter"].(string); expanded != "r1" {
		t.Fatalf("toggleRun did not open row r1: expandedAfter=%v", got["expandedAfter"])
	}
	if count, _ := got["scrollCount"].(float64); count != 1 {
		t.Fatalf("expected exactly 1 scrollIntoView call on the row, got %v", got["scrollCount"])
	}
	if b, _ := got["behavior"].(string); b != "instant" {
		t.Fatalf("expected behavior == \"instant\", got %v (last options: %s)", b, string(out))
	}
	if b, _ := got["block"].(string); b != "nearest" {
		t.Fatalf("expected block == \"nearest\", got %v (last options: %s)", b, string(out))
	}
}

// TestPortal_NoSmoothScrollIntoViewInPortalHtml is the audit guard for
// acceptance criterion #2: no scrollIntoView call in portal.html may use
// behavior: 'smooth'. The grep is intentionally broad — it also catches
// 'smooth' adjacent to any scrollIntoView token — so a regression to
// smooth-scroll would fail this test.
func TestPortal_NoSmoothScrollIntoViewInPortalHtml(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	portalHtmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	data, err := os.ReadFile(portalHtmlPath)
	if err != nil {
		t.Fatalf("read portal.html: %v", err)
	}
	assertNoSmoothScroll(t, "portal.html", string(data))
}

// TestPortal_NoSmoothScrollIntoViewInPortalDiffJS is the audit guard for
// acceptance criterion #2 applied to portal_diff.js. Even though current
// code has no scrollIntoView call here, locking the audit as a test
// prevents future regressions.
func TestPortal_NoSmoothScrollIntoViewInPortalDiffJS(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	diffPath := filepath.Join(filepath.Dir(currentFile), "portal_diff.js")
	data, err := os.ReadFile(diffPath)
	if err != nil {
		t.Fatalf("read portal_diff.js: %v", err)
	}
	assertNoSmoothScroll(t, "portal_diff.js", string(data))
}

func assertNoSmoothScroll(t *testing.T, file, src string) {
	t.Helper()
	for _, needle := range []string{"scrollIntoView", "scrollTo(", "scrollTo (", "scrollBy(", "scrollBy ("} {
		idx := 0
		for {
			at := strings.Index(src[idx:], needle)
			if at < 0 {
				break
			}
			lineStart := strings.LastIndex(src[:idx+at], "\n") + 1
			lineEnd := strings.Index(src[idx+at:], "\n")
			if lineEnd < 0 {
				lineEnd = len(src)
			} else {
				lineEnd += idx + at
			}
			line := src[lineStart:lineEnd]
			if strings.Contains(line, "'smooth'") || strings.Contains(line, "\"smooth\"") {
				t.Fatalf("%s contains a smooth-scrolling %s call:\n  %s", file, needle, strings.TrimSpace(line))
			}
			idx += at + len(needle)
		}
	}
}

// TestPortal_SwitchRows_InstantScrollDoesNotRegress is a regression
// guard after the smooth→instant+nearest fix: the Slice 0 rapid-row-
// switch scenario must not regress versus the pinned slice-0 baseline.
// It asserts against the pinned copy at
// portal_perf_switch_rows_baseline_slice0.json, not the live baseline
// that TestPortalPerf_LongTaskProfile_SwitchRows rewrites on every run.
// The Node-vm perf harness has no compositor, so smooth→instant does
// not surface as a long-task delta here — see the buildPerfBaselineComment
// helper at internal/cmd/portal_perf_long_task_test.go for the AC #3/#5
// scope narrowing accepted for #1559. Observing the AC #5 long-task
// drop literally requires a real-browser capture, which CI cannot do.
func TestPortal_SwitchRows_InstantScrollDoesNotRegress(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal scroll behavior test")
	}

	baselineDir := perfBaselineDir(t)
	if !pinnedSwitchRowsBaselineExists(t, baselineDir) {
		t.Skipf("pinned slice-0 baseline missing at %s (testdata/ is gitignored; populate locally before running)", baselineDir)
	}

	pinnedBaseline, err := loadPinnedSwitchRowsBaseline(t, baselineDir)
	if err != nil {
		t.Fatalf("load pinned slice-0 baseline: %v", err)
	}

	outStr := runPortalDiffLongTaskScenario(t, scenarioSwitchRowsJS)
	var got map[string]any
	if err := json.Unmarshal([]byte(outStr), &got); err != nil {
		t.Fatalf("parse harness output %q: %v", outStr, err)
	}
	count, ok := got["count"].(float64)
	if !ok {
		t.Fatalf("harness did not return count: %+v", got)
	}
	endToEnd, ok := got["endToEndMs"].(float64)
	if !ok {
		t.Fatalf("harness did not return endToEndMs: %+v", got)
	}

	baselineCount := pinnedBaseline.LongTaskCount
	const regressionSlackCount = 1
	const regressionFactor = 2.0
	const floorSlackMs = 50.0
	t.Logf("switch_rows post-fix: count=%v endToEndMs=%v vs pinned baseline: count=%d endToEndMs=%.2f",
		count, endToEnd, baselineCount, pinnedBaseline.EndToEndMs)
	if int(count) > baselineCount+regressionSlackCount {
		t.Fatalf("switch_rows longTaskCount regressed: got %v > baseline %d + slack %d", count, baselineCount, regressionSlackCount)
	}
	ceiling := pinnedBaseline.EndToEndMs*regressionFactor + floorSlackMs
	if endToEnd > ceiling {
		t.Fatalf("switch_rows endToEndMs regressed: got %.2f > baseline %.2f * %.2f + %.2f (ceiling %.2f)", endToEnd, pinnedBaseline.EndToEndMs, regressionFactor, floorSlackMs, ceiling)
	}
}

type pinnedSwitchRowsBaseline struct {
	LongTaskCount int     `json:"longTaskCount"`
	MaxTaskMs     float64 `json:"maxTaskMs"`
	SumTaskMs     float64 `json:"sumTaskMs"`
	EndToEndMs    float64 `json:"endToEndMs"`
}

func loadPinnedSwitchRowsBaseline(t *testing.T, baselineDir string) (*pinnedSwitchRowsBaseline, error) {
	t.Helper()
	pinnedPath := filepath.Join(baselineDir, "portal_perf_switch_rows_baseline_slice0.json")
	data, err := os.ReadFile(pinnedPath)
	if err != nil {
		return nil, err
	}
	var b pinnedSwitchRowsBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// pinnedSwitchRowsBaselineExists reports whether the slice-0 baseline file is
// available on disk. The file lives under testdata/, which is gitignored —
// test authors populate it locally before running the regression test.
// Callers should skip rather than fail when the file is absent.
func pinnedSwitchRowsBaselineExists(t *testing.T, baselineDir string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(baselineDir, "portal_perf_switch_rows_baseline_slice0.json"))
	return err == nil
}
