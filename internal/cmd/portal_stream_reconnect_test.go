package cmd

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPortalStream_PreservesReconnectOnTransientError is the regression
// guard for the live-update freeze: when the SSE bridge emits an error
// (transient network blip, intermediate proxy timeout, browser idle
// reaper), the EventSource transitions to readyState 0 (CONNECTING) and
// the browser schedules an automatic reconnect. The portal's onerror
// handler must NOT call stopRunStream in that case — doing so sets
// readyState 2 (CLOSED) on the source and permanently silences the
// live log until the user manually re-selects the row.
//
// The fix gates stopRunStream on `src.readyState === 2` (CLOSED). For
// transient errors the stream survives and the browser's native
// auto-reconnect takes over. The companion server-side change
// (portal_stream.go) emits periodic `: keepalive` SSE comments so the
// TCP connection stays warm and intermediate proxies do not close it
// during a quiet agent.
func TestPortalStream_PreservesReconnectOnTransientError(t *testing.T) {
	batchID := "260618113825-abcd"
	runID := batchID + "-42"
	initialRun := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#42",
		"issueNumber": 42,
		"batchKey":    batchID,
		"socketPath":  "/tmp/" + runID + ".sock",
		"log":         "initial line 1",
	}
	runsJSON, err := json.Marshal([]map[string]any{initialRun})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`
	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalStreams = [];
    window.__portalStreamEvents = [];
    window.EventSource = function (url) {
      this.url = url;
      this._onmessage = null;
      this._onerror = null;
      this.readyState = 1;
      this.closed = false;
      var self = this;
      Object.defineProperty(this, 'onmessage', {
        configurable: true,
        get: function () { return self._onmessage; },
        set: function (fn) {
          self._onmessage = fn;
          window.__portalStreamEvents.push(['set:onmessage']);
          if (typeof fn === 'function') {
            setTimeout(function () {
              if (self._onmessage && !self.closed) {
                self._onmessage({ data: 'LINE_BEFORE_ERROR' });
              }
            }, 30);
          }
        }
      });
      Object.defineProperty(this, 'onerror', {
        configurable: true,
        get: function () { return self._onerror; },
        set: function (fn) {
          self._onerror = fn;
          window.__portalStreamEvents.push(['set:onerror']);
          if (typeof fn !== 'function') return;
          setTimeout(function () {
            if (self._onerror && !self.closed) {
              self.readyState = 0;
              self._onerror(new Event('error'));
              // Browser auto-reconnect: when readyState stays at 0 and
              // the app has not closed the source, EventSource reopens
              // after a delay. If the app's onerror handler called
              // close() (via stopRunStream), readyState would jump to 2.
              setTimeout(function () {
                if (!self.closed && self.readyState === 0) {
                  self.readyState = 1;
                  window.__portalStreamEvents.push(['auto-reconnect']);
                  if (self._onmessage) self._onmessage({ data: 'LINE_AFTER_RECONNECT' });
                }
              }, 100);
            }
          }, 80);
        }
      });
      this.close = function () { self.closed = true; self.readyState = 2; };
      window.__portalStreams.push(this);
      window.__portalStreamEvents.push(['new:stream']);
    };
    setTimeout(function () {
      var marker = document.createElement('pre');
      marker.id = 'portal-reconnect-marker';
      marker.textContent = JSON.stringify({
        streamCount: window.__portalStreams.length,
        streamEvents: window.__portalStreamEvents,
        allClosed: window.__portalStreams.every(function (s) { return s.closed; })
      });
      document.body.appendChild(marker);
    }, 2500);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-reconnect-marker")
	var result struct {
		StreamCount  int     `json:"streamCount"`
		StreamEvents [][]any `json:"streamEvents"`
		AllClosed    bool    `json:"allClosed"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse reconnect payload: %v\nraw=%s", err, payload)
	}
	if result.StreamCount != 1 {
		t.Fatalf("expected exactly 1 EventSource created on initial expand, got %d", result.StreamCount)
	}
	hasError := false
	hasAutoReconnect := false
	for _, ev := range result.StreamEvents {
		if len(ev) == 0 {
			continue
		}
		switch ev[0] {
		case "set:onerror":
			hasError = true
		case "auto-reconnect":
			hasAutoReconnect = true
		}
	}
	if !hasError {
		t.Fatalf("expected onerror handler to fire (test setup is broken if it does not); events: %v", result.StreamEvents)
	}
	if !hasAutoReconnect {
		t.Fatalf("onerror handler called stopRunStream on a transient error (readyState 0), suppressing the browser's native EventSource auto-reconnect. Live log will freeze until the user re-selects the row. Events: %v", result.StreamEvents)
	}
	if result.AllClosed {
		t.Fatalf("all EventSources are closed after a transient error; auto-reconnect was suppressed")
	}
}

// TestPortalStream_ClosedSourceSchedulesReconcile pins recovery from a
// terminal EventSource error. A broadcaster can evict a portal client during
// a burst, which closes the bridge response and can leave EventSource in the
// CLOSED state. Removing that source without scheduling a render leaves no
// path to reconcileRunStreams and create its replacement when summary polling
// returns 304.
func TestPortalStream_ClosedSourceSchedulesReconcile(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping portal stream lifecycle test")
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	prefix := `
const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(` + "`" + htmlPath + "`" + `, 'utf8');
const startMatch = src.match(/function startRunStream\(run\) \{[\s\S]*?^\s{4}\}/m);
const stopMatch = src.match(/function stopRunStream\(runKey\) \{[\s\S]*?^\s{4}\}/m);
const reconcileMatch = src.match(/function reconcileRunStreams\(\) \{[\s\S]*?^\s{4}\}/m);
if (!startMatch || !stopMatch || !reconcileMatch) throw new Error('stream lifecycle functions not found');
let scheduleCalls = 0;
let sourceCount = 0;
const streamSources = {};
const streamingKeys = new Set();
const streamPath = '/api/runs/stream';
const state = {
  expandedRunKey: 'review-1',
  tabs: { 'review-1': 'log' },
  runs: [{ key: 'review-1', runId: 'review-1', kind: 'active', socketPath: '/tmp/review.sock' }]
};
const loadingDetailKeys = new Set();
const streamCoalescer = { seedKnownLines() {}, clearBuffer() {}, scheduleLine() {}, setBlocked() {}, flushPending() {} };
function streamPreFor() { return null; }
function findRunByIdentity(key) { return state.runs.find(function(run) { return run.key === key || run.runId === key; }) || null; }
function isWaitStateRun() { return false; }
function subjectRunIdentity(run) { return run ? (run.runId || run.key || '') : ''; }
function scheduleRender() { scheduleCalls++; reconcileRunStreams(); }
function EventSource() {
  const self = this;
  sourceCount++;
  this.readyState = 1;
  this.closed = false;
  this.close = function() { self.closed = true; self.readyState = 2; };
  Object.defineProperty(this, 'onerror', {
    configurable: true,
    set: function(fn) {
      self._onerror = fn;
      if (sourceCount !== 1) return;
      setTimeout(function() {
        self.readyState = 2;
        fn(new Error('bridge EOF'));
      }, 0);
    }
  });
}
vm.runInThisContext(startMatch[0] + '\n' + stopMatch[0] + '\n' + reconcileMatch[0], { filename: 'portal.html' });
startRunStream({ key: 'review-1', kind: 'active', socketPath: '/tmp/review.sock' });
setTimeout(function() {
  if (scheduleCalls === 0) throw new Error('closed source did not schedule a render to reconcile a replacement stream');
  if (sourceCount !== 2) throw new Error('expected replacement EventSource after CLOSED error, got ' + sourceCount);
  if (!streamSources['review-1']) throw new Error('replacement source was not retained');
  if (!streamingKeys.has('review-1')) throw new Error('replacement source was not marked as streaming');
  console.log('PASS');
}, 20);
`
	cmd := exec.Command("node", "-e", prefix)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("script did not emit PASS: %s", out)
	}
}
