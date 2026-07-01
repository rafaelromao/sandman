package cmd

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

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
`
