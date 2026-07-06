package cmd

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// portalHtmlPathForTest resolves the absolute path to portal.html next to the
// test file, for embedding into node harness scripts.
func portalHtmlPathForTest(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	return filepath.Join(filepath.Dir(currentFile), "portal.html")
}

// TestPortalLoadRunDetail_ForceFetchBypassesHasLogGuard pins the fix for the
// stale-log gap on tab switch. When an active run already has a log, the
// normal poll path must still early-return (no refetch), but an explicit
// forceFetch (issued on tab switch to Log) must bypass that guard so the
// pane is rebuilt with the current tail before the live stream re-attaches.
func TestPortalLoadRunDetail_ForceFetchBypassesHasLogGuard(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal freshness test")
	}
	portalHtmlPath := portalHtmlPathForTest(t)
	const runID = "abcd-260618113825-42"

	script := `
const fs = require('fs');
const vm = require('vm');
const portalHtmlSrc = fs.readFileSync(` + "`" + portalHtmlPath + "`" + `, 'utf8');
const runID = ` + "`" + runID + "`" + `;

function extractFunction(src, name) {
  const re = new RegExp('(?:^|\\n)[ \\t]*(?:async[ \\t]+)?function[ \\t]+' + name + '\\s*\\([^)]*\\)\\s*\\{', 'm');
  const startMatch = re.exec(src);
  if (!startMatch) throw new Error('could not extract ' + name);
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

const loadRunDetailSrc = extractFunction(portalHtmlSrc, 'loadRunDetail');
const run = { key: runID, runId: runID, backendKey: runID, kind: 'active', status: 'running', log: 'existing stale log' };

let fetchCalls = 0;
const sandbox = {
  run: run,
  state: { runs: [run], tabs: {} },
  loadingDetailKeys: new Set(),
  findRunByIdentity: function () { return run; },
  isWaitStateRun: function () { return false; },
  subjectTabKey: function (k) { return k; },
  setDetailLoading: function () { return true; },
  scheduleRender: function () {},
  normalizeRunIdentity: function (x) { return x; },
  apiPath: '/api/runs',
  fetch: async function () {
    fetchCalls += 1;
    return {
      ok: true,
      status: 200,
      json: async function () { return { run: { key: runID, runId: runID, log: 'fresh tail' } }; },
      text: async function () { return ''; },
    };
  },
};
sandbox.state.tabs[runID] = 'log';
sandbox.JSON = JSON;
sandbox.Object = Object;

vm.runInNewContext(loadRunDetailSrc + '\nthis.loadRunDetail = loadRunDetail;', sandbox, { filename: 'portal.html#loadRunDetail' });
if (typeof sandbox.loadRunDetail !== 'function') throw new Error('loadRunDetail did not eval into the sandbox');

(async () => {
  await sandbox.loadRunDetail(runID);
  const callsAfterNoForce = fetchCalls;
  await sandbox.loadRunDetail(runID, { forceFetch: true });
  const callsAfterForce = fetchCalls;
  // After forceFetch, the merge must apply the freshly-fetched detail.log to
  // state.runs[idx]. Otherwise the stale cached beginning survives and the
  // SSE stream's tail-replay creates the very gap forceFetch was meant to close.
  const logAfterForce = sandbox.state.runs[0].log;
  process.stdout.write(JSON.stringify({ callsAfterNoForce: callsAfterNoForce, callsAfterForce: callsAfterForce, logAfterForce: logAfterForce }));
})();
`

	out, err := exec.Command("node", "-e", script).Output()
	if err != nil {
		t.Fatalf("freshness harness failed: %v\n%s", err, out)
	}
	var got struct {
		CallsAfterNoForce int    `json:"callsAfterNoForce"`
		CallsAfterForce   int    `json:"callsAfterForce"`
		LogAfterForce     string `json:"logAfterForce"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse harness output %q: %v", strings.TrimSpace(string(out)), err)
	}
	if got.CallsAfterNoForce != 0 {
		t.Fatalf("expected no detail fetch without forceFetch (hasLog guard), got %d calls", got.CallsAfterNoForce)
	}
	if got.CallsAfterForce != 1 {
		t.Fatalf("expected one detail fetch with forceFetch (bypassing hasLog guard), got %d calls", got.CallsAfterForce)
	}
	if got.LogAfterForce != "fresh tail" {
		t.Fatalf("expected forceFetch to apply detail.log to state.runs[idx], got %q (stale cached log survived the merge)", got.LogAfterForce)
	}
}

// TestPortalReconcileRunStreams_GatesStreamWhileDetailLoading pins the second
// half of the gap fix: the live stream must not attach while a fresh detail
// snapshot is being fetched, otherwise the daemon's tail replay would be
// appended onto a stale cached pane and leave a gap in the middle. Once the
// detail load completes (loadingDetailKeys cleared), the stream may attach.
func TestPortalReconcileRunStreams_GatesStreamWhileDetailLoading(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal stream gate test")
	}
	portalHtmlPath := portalHtmlPathForTest(t)
	const runID = "abcd-260618113825-42"

	script := `
const fs = require('fs');
const vm = require('vm');
const portalHtmlSrc = fs.readFileSync(` + "`" + portalHtmlPath + "`" + `, 'utf8');
const runID = ` + "`" + runID + "`" + `;

function extractFunction(src, name) {
  const re = new RegExp('(?:^|\\n)[ \\t]*(?:async[ \\t]+)?function[ \\t]+' + name + '\\s*\\([^)]*\\)\\s*\\{', 'm');
  const startMatch = re.exec(src);
  if (!startMatch) throw new Error('could not extract ' + name);
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

const fns = ['startRunStream', 'stopRunStream', 'stopAllRunStreams', 'reconcileRunStreams']
  .map(function (n) { return extractFunction(portalHtmlSrc, n); })
  .join('\n');

const run = { key: runID, runId: runID, kind: 'active', socketPath: '/tmp/run.sock' };
const events = [];
const sandbox = {
  run: run,
  state: { expandedRunKey: runID, tabs: {} },
  streamingKeys: new Set(),
  streamSources: {},
  loadingDetailKeys: new Set(),
  streamPath: '/api/runs/stream',
  EventSource: function (url) { events.push(['new', url]); this.close = function () { events.push(['close']); }; },
  findRunByIdentity: function () { return run; },
  isWaitStateRun: function () { return false; },
  subjectRunIdentity: function (r) { return r && r.runId; },
  streamPreFor: function () { return null; },
  streamCoalescer: { seedKnownLines: function () {}, clearBuffer: function () {}, scheduleLine: function () {} },
};
sandbox.state.tabs[runID] = 'log';
sandbox.JSON = JSON;
sandbox.Object = Object;
sandbox.Set = Set;

vm.runInNewContext(fns + '\nthis.reconcileRunStreams = reconcileRunStreams;', sandbox, { filename: 'portal.html#streams' });
if (typeof sandbox.reconcileRunStreams !== 'function') throw new Error('reconcileRunStreams did not eval into the sandbox');

// Case A: no detail load in flight -> stream attaches.
sandbox.reconcileRunStreams();
const startedWhenIdle = events.some(function (e) { return e[0] === 'new'; });

// Case B: detail load in flight -> stream gated, no attach.
sandbox.streamSources = {};
sandbox.streamingKeys = new Set();
events.length = 0;
sandbox.loadingDetailKeys.add(runID);
sandbox.reconcileRunStreams();
const startedWhenLoading = events.some(function (e) { return e[0] === 'new'; });

process.stdout.write(JSON.stringify({ startedWhenIdle: startedWhenIdle, startedWhenLoading: startedWhenLoading }));
`

	out, err := exec.Command("node", "-e", script).Output()
	if err != nil {
		t.Fatalf("stream gate harness failed: %v\n%s", err, out)
	}
	var got struct {
		StartedWhenIdle    bool `json:"startedWhenIdle"`
		StartedWhenLoading bool `json:"startedWhenLoading"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse harness output %q: %v", strings.TrimSpace(string(out)), err)
	}
	if !got.StartedWhenIdle {
		t.Fatalf("expected stream to attach when no detail load is in flight, got %v", got)
	}
	if got.StartedWhenLoading {
		t.Fatalf("expected stream to be gated while a detail load is in flight, got %v", got)
	}
}
