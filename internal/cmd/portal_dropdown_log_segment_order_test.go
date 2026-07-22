package cmd

import (
	"encoding/json"
	"testing"
)

// TestPortalDropdownSwitch_DrainsOutgoingCoalescerBeforeCache pins the
// behaviour added for the "log segments concatenated out of order" report:
// when the subject picker is used to switch subjects, the change handler
// must drain the coalescer buffer for the outgoing subject before the
// subject-switch render moves its pane into the per-subject cache. The
// previous implementation only called stopRunStream, which left any
// un-flushed lines buffered in streamCoalescer. A subsequent rAF would then
// find the pane detached from runsBody and silently drop the lines,
// presenting the cached pane as missing its leading streamed segments when
// the user returned to the original subject. The fix calls
// streamCoalescer.flushPending on the outgoing key immediately after the
// SSE source is closed, so the cached pane always carries the full stream
// tail that was visible at the moment of the switch.
func TestPortalDropdownSwitch_DrainsOutgoingCoalescerBeforeCache(t *testing.T) {
	const runID = "260618113825-abcd-1"
	const reviewID = "PR42"

	runLog := "parent log line 1\nparent log line 2\nparent log line 3"
	reviewLog := "review log line 1\nreview log line 2"

	parent := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#1",
		"issueNumber": 1,
		"batchKey":    runID,
		"socketPath":  "/tmp/" + runID + ".sock",
		"log":         runLog,
	}
	child := map[string]any{
		"key":         reviewID,
		"runId":       reviewID,
		"kind":        "active",
		"status":      "reviewing",
		"review":      true,
		"issueLabel":  reviewID,
		"issueNumber": 1,
		"prNumber":    42,
		"socketPath":  "/tmp/" + reviewID + ".sock",
		"log":         reviewLog,
	}
	runsJSON, err := json.Marshal([]map[string]any{parent, child})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log","` + reviewID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	streamLines := []string{
		"streamed line A",
		"streamed line B",
		"streamed line C",
		"streamed line D",
		"streamed line E",
		"streamed line F",
	}
	streamLinesJSON, err := json.Marshal(streamLines)
	if err != nil {
		t.Fatalf("marshal stream lines: %v", err)
	}

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalStreamedLines = `+string(streamLinesJSON)+`;
    window.__portalRafQueue = [];
    window.__portalRafRuns = 0;
    window.__portalOnMessage = null;
    // Override requestAnimationFrame so the test can drive flushes
    // deterministically. Each rAF is appended to a queue; the test runs
    // them explicitly via __portalRafTick() before/after each assertion.
    var __origRaf = window.requestAnimationFrame;
    window.requestAnimationFrame = function (cb) {
      window.__portalRafQueue.push(cb);
      return window.__portalRafQueue.length;
    };
    window.__portalRafTick = function (steps) {
      var n = steps || 1;
      for (var i = 0; i < n; i++) {
        var cb = window.__portalRafQueue.shift();
        if (typeof cb !== 'function') break;
        window.__portalRafRuns += 1;
        try { cb(performance.now()); } catch (err) { window.__portalLastError = String(err); }
      }
    };
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
          window.__portalOnMessage = fn;
          window.__portalStreamOpens += 1;
        }
      });
      this.close = function () { self.closed = true; self.readyState = 2; };
    };
    window.__portalFeed = function () {
      var fn = window.__portalOnMessage;
      if (typeof fn !== 'function') throw new Error('no onmessage hooked');
      var lines = window.__portalStreamedLines || [];
      for (var i = 0; i < lines.length; i++) fn({ data: lines[i] });
    };
    setTimeout(function () {
      // Drain any rAFs queued by initial render so we start clean.
      window.__portalRafTick(8);
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      if (!detail) throw new Error('missing parent detail row');
      var parentPre = detail.querySelector('pre[data-scroll-key="`+runID+`"]');
      if (!parentPre) throw new Error('missing parent log pre');
      var beforeAttr = parentPre.getAttribute('data-rendered-log') || '';
      // Feed all six lines through the production onmessage hook without
      // ticking rAFs afterwards — the coalescer's rAF will sit pending
      // in __portalRafQueue until the dropdown switch (or until we tick).
      window.__portalFeed();
      var select = detail.querySelector('select[data-action="set-subject"]');
      if (!select) throw new Error('missing subject selector');
      select.value = '`+reviewID+`';
      select.dispatchEvent(new Event('change', { bubbles: true }));
      // After the change handler returns, before any queued rAF has run,
      // the outgoing pane must already carry the buffered lines. The
      // change handler is the only place that can flush them before the
      // subject-switch render moves the pane into the per-subject cache.
      var preAttrAfterSwitch = parentPre.getAttribute('data-rendered-log') || '';
      var result = document.createElement('pre');
      result.id = 'portal-dropdown-marker';
      result.textContent = JSON.stringify({
        beforeAttr: beforeAttr,
        preAttrAfterSwitch: preAttrAfterSwitch,
        preHasAllLines: (preAttrAfterSwitch.indexOf('streamed line A') >= 0)
          && (preAttrAfterSwitch.indexOf('streamed line B') >= 0)
          && (preAttrAfterSwitch.indexOf('streamed line C') >= 0)
          && (preAttrAfterSwitch.indexOf('streamed line D') >= 0)
          && (preAttrAfterSwitch.indexOf('streamed line E') >= 0)
          && (preAttrAfterSwitch.indexOf('streamed line F') >= 0),
        streamOpens: window.__portalStreamOpens,
        rafRuns: window.__portalRafRuns,
      });
      document.body.appendChild(result);
    }, 80);
  `)
	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-dropdown-marker")
	var result struct {
		BeforeAttr         string `json:"beforeAttr"`
		PreAttrAfterSwitch string `json:"preAttrAfterSwitch"`
		PreHasAllLines     bool   `json:"preHasAllLines"`
		StreamOpens        int    `json:"streamOpens"`
		RafRuns            int    `json:"rafRuns"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse dropdown payload: %v\nraw=%s", err, payload)
	}
	if !result.PreHasAllLines {
		t.Fatalf("expected outgoing parent pane to carry the queued streamed lines A..F immediately after the dropdown change handler returns. Without flushPending on the outgoing key, the coalescer's pending rAF fires only after the subject-switch render has detached the pane to the per-subject cache, so the buffered lines are silently dropped. Got attr=%q (streamOpens=%d, rafRuns=%d).", result.PreAttrAfterSwitch, result.StreamOpens, result.RafRuns)
	}
}
