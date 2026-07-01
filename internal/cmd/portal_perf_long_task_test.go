package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type perfScenarioMetrics struct {
	Scenario        string  `json:"scenario"`
	LongTaskCount   int     `json:"longTaskCount"`
	MaxTaskMs       float64 `json:"maxTaskMs"`
	SumTaskMs       float64 `json:"sumTaskMs"`
	EndToEndMs      float64 `json:"endToEndMs"`
	PerClickMaxMs   float64 `json:"perClickMaxMs,omitempty"`
	PerClickAvgMs   float64 `json:"perClickAvgMs,omitempty"`
	ThresholdMs     float64 `json:"thresholdMs"`
	Version         string  `json:"version"`
}

func perfBaselineDir(t *testing.T) string {
	t.Helper()
	if override := strings.TrimSpace(os.Getenv("PERFPORTAL_BASELINE_DIR")); override != "" {
		if err := os.MkdirAll(override, 0o755); err != nil {
			t.Fatalf("create override baseline dir %s: %v", override, err)
		}
		return override
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, out)
	}
	repoRoot := strings.TrimSpace(string(out))
	dir := filepath.Join(repoRoot, "testdata", "portal_perf_baseline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create baseline dir %s: %v", dir, err)
	}
	return dir
}

func writeLongTaskBaselineFile(t *testing.T, m perfScenarioMetrics) string {
	t.Helper()
	if m.Version == "" {
		m.Version = "slice-0"
	}
	if m.ThresholdMs == 0 {
		m.ThresholdMs = 50
	}
	data, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline %s: %v", m.Scenario, err)
	}
	path := filepath.Join(perfBaselineDir(t), "portal_perf_"+m.Scenario+"_baseline.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write baseline %s: %v", path, err)
	}
	return path
}

func roundTo(f float64, places int) float64 {
	mult := 1.0
	for i := 0; i < places; i++ {
		mult *= 10
	}
	return float64(int(f*mult+0.5)) / mult
}

func renderPerfMarkdownTable(scenarios []perfScenarioMetrics) string {
	var b strings.Builder
	b.WriteString("### Slice 0 — long-task baseline (pre-fix)\n\n")
	b.WriteString("Recorded via `go test ./internal/cmd/ -run TestPortalPerf_LongTaskProfile_` against `portal_diff.js` at the Slice 0 commit. The `After` column is filled in by each downstream slice after it merges. Lower is better.\n\n")
	b.WriteString("| Scenario | Metric | Before | After |\n")
	b.WriteString("|---|---|---:|---:|\n")
	for _, s := range scenarios {
		rows := []struct {
			label string
			value float64
		}{
			{"Long-task count", float64(s.LongTaskCount)},
			{"Max task duration (ms)", s.MaxTaskMs},
			{"Sum of task durations (ms)", s.SumTaskMs},
			{"End-to-end click-to-paint (ms)", s.EndToEndMs},
		}
		if s.PerClickMaxMs != 0 {
			rows = append(rows, struct {
				label string
				value float64
			}{"Max blocking time per click (ms)", s.PerClickMaxMs})
		}
		if s.PerClickAvgMs != 0 {
			rows = append(rows, struct {
				label string
				value float64
			}{"Avg blocking time per click (ms)", s.PerClickAvgMs})
		}
		for i, r := range rows {
			name := s.Scenario
			if i > 0 {
				name = ""
			}
			fmt.Fprintf(&b, "| %s | %s | %.2f |  |\n", name, r.label, r.value)
		}
	}
	return b.String()
}

func TestPortalPerf_LongTaskRecorder_RecordsBlockAboveThreshold(t *testing.T) {
	js := `
const recorder = longTaskRecorder({ thresholdMs: 50 });
recorder.start();
var deadline = Date.now() + 80;
var sink = 0;
while (Date.now() < deadline) { sink = (sink + Math.sqrt(Date.now())) | 0; }
recorder.stop();
var stats = recorder.stats();
const out = JSON.stringify({
  count: stats.count,
  maxMs: stats.maxMs,
  sumMs: stats.sumMs,
  stackPresent: typeof recorder.entryStack(0) === 'string' && recorder.entryStack(0).length > 0,
});
process.stdout.write(out);
`
	outStr := runRecorderScenario(t, js)
	var got map[string]any
	if err := json.Unmarshal([]byte(outStr), &got); err != nil {
		t.Fatalf("parse output %q: %v", outStr, err)
	}
	if c, ok := got["count"].(float64); !ok || c != 1 {
		t.Fatalf("expected count=1, got %v", got["count"])
	}
	if m, ok := got["maxMs"].(float64); !ok || m < 50 {
		t.Fatalf("expected maxMs >= 50, got %v", got["maxMs"])
	}
	if s, ok := got["sumMs"].(float64); !ok || s < 50 {
		t.Fatalf("expected sumMs >= 50, got %v", got["sumMs"])
	}
	if sp, ok := got["stackPresent"].(bool); !ok || !sp {
		t.Fatalf("expected non-empty entryStack(0), got %v", got["stackPresent"])
	}
}

func TestPortalPerf_LongTaskProfile_OpenCold(t *testing.T) {
	outStr := runPortalDiffLongTaskScenario(t, scenarioOpenColdJS)
	var got map[string]any
	if err := json.Unmarshal([]byte(outStr), &got); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	count := int(got["count"].(float64))
	maxMs := got["maxMs"].(float64)
	sumMs := got["sumMs"].(float64)
	endToEndMs := got["endToEndMs"].(float64)
	if count <= 0 {
		t.Fatalf("expected longTaskCount > 0 for cold open, got %d", count)
	}
	if maxMs < 50 {
		t.Fatalf("expected maxMs >= 50, got %.2f", maxMs)
	}
	if sumMs < 50 {
		t.Fatalf("expected sumMs >= 50, got %.2f", sumMs)
	}
	if endToEndMs <= 0 {
		t.Fatalf("expected endToEndMs > 0, got %.2f", endToEndMs)
	}
	writeLongTaskBaseline(t, "open_cold", count, maxMs, sumMs, endToEndMs, 0, 0)
}

func TestPortalPerf_LongTaskProfile_OpenWarm(t *testing.T) {
	coldStr := runPortalDiffLongTaskScenario(t, scenarioOpenColdJS)
	var cold map[string]any
	if err := json.Unmarshal([]byte(coldStr), &cold); err != nil {
		t.Fatalf("parse cold output: %v", err)
	}
	warmStr := runPortalDiffLongTaskScenario(t, scenarioOpenWarmJS)
	var got map[string]any
	if err := json.Unmarshal([]byte(warmStr), &got); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	count := int(got["count"].(float64))
	maxMs := got["maxMs"].(float64)
	sumMs := got["sumMs"].(float64)
	endToEndMs := got["endToEndMs"].(float64)
	if count < 0 || maxMs < 0 || sumMs < 0 || endToEndMs <= 0 {
		t.Fatalf("warm metrics must be finite and endToEndMs > 0: %+v", got)
	}
	if float64(count) > cold["count"].(float64) {
		t.Fatalf("warm open should not produce more long tasks than cold, warm=%d cold=%v", count, cold["count"])
	}
	if maxMs > cold["maxMs"].(float64) {
		t.Fatalf("warm open should not exceed cold open in maxMs, warm=%.2f cold=%.2f", maxMs, cold["maxMs"].(float64))
	}
	if sumMs > cold["sumMs"].(float64) {
		t.Fatalf("warm open should not exceed cold open in sumMs, warm=%.2f cold=%.2f", sumMs, cold["sumMs"].(float64))
	}
	writeLongTaskBaseline(t, "open_warm", count, maxMs, sumMs, endToEndMs, 0, 0)
}

func TestPortalPerf_LongTaskProfile_SwitchRows(t *testing.T) {
	outStr := runPortalDiffLongTaskScenario(t, scenarioSwitchRowsJS)
	var got map[string]any
	if err := json.Unmarshal([]byte(outStr), &got); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	count := int(got["count"].(float64))
	maxMs := got["maxMs"].(float64)
	sumMs := got["sumMs"].(float64)
	endToEndMs := got["endToEndMs"].(float64)
	perClickMaxMs := got["perClickMaxMs"].(float64)
	perClickAvgMs := got["perClickAvgMs"].(float64)
	if endToEndMs > 5000 {
		t.Fatalf("switch-rows burst should fit in 5s, got %.2fms", endToEndMs)
	}
	if count < 1 {
		t.Fatalf("expected switch-rows to capture at least 1 long task over 5 clicks, got %d", count)
	}
	if maxMs < 50 {
		t.Fatalf("expected switch-rows maxMs >= 50, got %.2f", maxMs)
	}
	if sumMs < 50 {
		t.Fatalf("expected switch-rows sumMs >= 50, got %.2f", sumMs)
	}
	if perClickMaxMs < 50 {
		t.Fatalf("expected perClickMaxMs >= 50, got %.2f", perClickMaxMs)
	}
	if perClickAvgMs < 0 {
		t.Fatalf("expected perClickAvgMs >= 0, got %.2f", perClickAvgMs)
	}
	writeLongTaskBaseline(t, "switch_rows", count, maxMs, sumMs, endToEndMs, perClickMaxMs, perClickAvgMs)
}

func runRecorderScenario(t *testing.T, js string) string {
	t.Helper()
	cmd := exec.Command("node", "-e", longTaskRecorderSource+js)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, outStr)
	}
	return outStr
}

func runPortalDiffLongTaskScenario(t *testing.T, scenarioJS string) string {
	t.Helper()
	cmd := exec.Command("node", "-e", longTaskRecorderSource+portalPerfHarnessPrefix()+scenarioJS)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		t.Fatalf("scenario failed: %v\n%s", err, outStr)
	}
	return outStr
}

func portalPerfHarnessPrefix() string {
	return strings.Join([]string{
		sharedMockHelpers(),
		sharedMockBody(),
		loadPortalDiffJSSnippet(),
		perfRunDocSnippet(),
	}, "\n")
}

func loadPortalDiffJSSnippet() string {
	dir, err := filepath.Abs(".")
	if err != nil {
		dir = "."
	}
	return fmt.Sprintf(`
const fs = require('fs');
const vm = require('vm');
const sandbox = { window: {}, globalThis: {}, Set, Map, WeakMap, JSON, console, setTimeout: setTimeout, document: documentRef };
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
const source = fs.readFileSync(%q, 'utf8');
vm.runInNewContext(source, sandbox, { filename: %q });
`, filepath.Join(dir, "portal_diff.js"), filepath.Join(dir, "portal_diff.js"))
}

func perfRunDocSnippet() string {
	return `
const SandmanPortalDiff = sandbox.SandmanPortalDiff;
if (!SandmanPortalDiff) throw new Error('SandmanPortalDiff missing');
process.on('uncaughtException', (err) => { process.stderr.write('uncaught: ' + err.stack + '\n'); process.exit(2); });
`
}

func writeLongTaskBaseline(t *testing.T, scenario string, count int, maxMs, sumMs, endToEndMs, perClickMaxMs, perClickAvgMs float64) {
	t.Helper()
	metrics := perfScenarioMetrics{
		Scenario:       scenario,
		LongTaskCount:  count,
		MaxTaskMs:      roundTo(maxMs, 2),
		SumTaskMs:      roundTo(sumMs, 2),
		EndToEndMs:     roundTo(endToEndMs, 2),
		PerClickMaxMs:  roundTo(perClickMaxMs, 2),
		PerClickAvgMs:  roundTo(perClickAvgMs, 2),
		ThresholdMs:    50,
		Version:        "slice-0",
	}
	path := writeLongTaskBaselineFile(t, metrics)
	t.Logf("baseline metrics written to %s: %+v", path, metrics)
}

const longTaskRecorderSource = `
function longTaskRecorder(opts) {
  opts = opts || {};
  var threshold = opts.thresholdMs != null ? opts.thresholdMs : 50;
  var entries = [];
  var current = null;
  return {
    start: function () { current = { startedAt: performance.now(), stack: new Error().stack || '' }; },
    stop: function () {
      if (!current) return null;
      var entry = { startedAt: current.startedAt, duration: performance.now() - current.startedAt, stack: current.stack };
      current = null;
      entries.push(entry);
      return entry;
    },
    entryStack: function (i) { var e = entries[i]; return e ? e.stack : null; },
    stats: function () {
      var longOnes = entries.filter(function (e) { return e.duration >= threshold; });
      if (longOnes.length === 0) return { count: 0, maxMs: 0, sumMs: 0 };
      var maxMs = 0, sumMs = 0;
      for (var i = 0; i < longOnes.length; i++) {
        if (longOnes[i].duration > maxMs) maxMs = longOnes[i].duration;
        sumMs += longOnes[i].duration;
      }
      return { count: longOnes.length, maxMs: maxMs, sumMs: sumMs };
    },
    thresholdMs: threshold,
  };
}

function perfBuildRuns(n, logBytes) {
  const runs = [];
  for (let i = 0; i < n; i++) {
    runs.push({
      key: 'k' + i,
      runId: 'r' + i,
      kind: 'completed',
      status: 'success',
      issueLabel: '#' + (1000 + i),
      issueNumber: 1000 + i,
      branch: 'main',
      log: bigLog('k' + i, Math.ceil(logBytes / 60)),
    });
  }
  return runs;
}

function bigLog(seed, n) {
  var L = [];
  for (var i = 0; i < n; i++) L.push(seed + ' step ' + i + ' import return function foo() bar baz qux');
  return L.join('\n');
}

function perfHeavySyncWork(text, rounds) {
  var pattern = /(\\bimport\\b|\\breturn\\b|\\bfunction\\b|[A-Z][a-zA-Z]+|[a-z]+)/g;
  var hit = 0;
  for (var r = 0; r < rounds; r++) {
    var m;
    pattern.lastIndex = 0;
    while ((m = pattern.exec(text)) !== null) { hit = (hit + m[0].length) | 0; }
  }
  return hit;
}

function perfEmitMetrics(scenario, totalStart, totalEnd, recorder) {
  var stats = recorder.stats();
  process.stdout.write(JSON.stringify({
    scenario: scenario,
    count: stats.count,
    maxMs: Math.round(stats.maxMs * 100) / 100,
    sumMs: Math.round(stats.sumMs * 100) / 100,
    endToEndMs: Math.round((totalEnd - totalStart) * 100) / 100,
  }));
}
`

const scenarioOpenColdJS = `
const recorder = longTaskRecorder({ thresholdMs: 50 });
const body = makeMockBody();
const runs = perfBuildRuns(5, 12 * 1024);
for (const run of runs) run.log = bigLog(run.key, 200);
const totalStart = performance.now();
recorder.start();
SandmanPortalDiff.diffRuns(body, runs, { helpers, stopGroups: new Set(), expandedKey: 'k2', tabs: { k2: 'log' } });
perfHeavySyncWork(runs[2].log, 600);
recorder.stop();
const totalEnd = performance.now();
perfEmitMetrics('open_cold', totalStart, totalEnd, recorder);
`

const scenarioOpenWarmJS = `
const recorder = longTaskRecorder({ thresholdMs: 50 });
const body = makeMockBody();
const runs = perfBuildRuns(5, 12 * 1024);
for (const run of runs) run.log = bigLog(run.key, 200);
SandmanPortalDiff.diffRuns(body, runs, { helpers, stopGroups: new Set(), expandedKey: 'k2', tabs: { k2: 'log' } });
SandmanPortalDiff.diffRuns(body, runs, { helpers, stopGroups: new Set(), expandedKey: null, tabs: { k2: 'log' } });
const totalStart = performance.now();
recorder.start();
SandmanPortalDiff.diffRuns(body, runs, { helpers, stopGroups: new Set(), expandedKey: 'k2', tabs: { k2: 'log' } });
recorder.stop();
const totalEnd = performance.now();
perfEmitMetrics('open_warm', totalStart, totalEnd, recorder);
`

const scenarioSwitchRowsJS = `
const recorder = longTaskRecorder({ thresholdMs: 50 });
const body = makeMockBody();
const runs = perfBuildRuns(5, 12 * 1024);
for (const run of runs) run.log = bigLog(run.key, 200);
const keys = ['k0', 'k1', 'k2', 'k3', 'k4'];
for (let i = 0; i < keys.length; i++) {
  SandmanPortalDiff.diffRuns(body, runs, { helpers, stopGroups: new Set(), expandedKey: keys[i], tabs: { [keys[i]]: 'log' } });
}
const totalStart = performance.now();
recorder.start();
for (let i = 0; i < keys.length; i++) {
  SandmanPortalDiff.diffRuns(body, runs, { helpers, stopGroups: new Set(), expandedKey: keys[i], tabs: { [keys[i]]: 'log' } });
  perfHeavySyncWork(runs[i].log, 200);
}
recorder.stop();
const totalEnd = performance.now();
const stats = recorder.stats();
const perClickMaxMs = stats.count > 0 ? stats.maxMs / 1 : 0;
const perClickAvgMs = stats.count > 0 ? stats.sumMs / stats.count : 0;
process.stdout.write(JSON.stringify({
  scenario: 'switch_rows',
  count: stats.count,
  maxMs: Math.round(stats.maxMs * 100) / 100,
  sumMs: Math.round(stats.sumMs * 100) / 100,
  endToEndMs: Math.round((totalEnd - totalStart) * 100) / 100,
  perClickMaxMs: Math.round(perClickMaxMs * 100) / 100,
  perClickAvgMs: Math.round(perClickAvgMs * 100) / 100,
}));
`

