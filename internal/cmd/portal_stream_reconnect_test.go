package cmd

import (
	"encoding/json"
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
