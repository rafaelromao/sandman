package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPortalRowReopen_PreservesQueuedStreamTailBeforeReplay reproduces a
// close/reopen race: a rAF-coalesced line arrives just before the details row
// is detached. The cached pane must retain that line, and a later SSE replay
// must append only genuinely new entries after it.
func TestPortalRowReopen_PreservesQueuedStreamTailBeforeReplay(t *testing.T) {
	const runID = "260901113403-b864-444"
	const snapshotLine = "09:00 snapshot"
	const bufferedLine = "09:01 buffered"
	const replayedLine = "09:02 replayed"

	run := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#444",
		"issueNumber": 444,
		"batchKey":    "260901113403-b864",
		"socketPath":  "/tmp/" + runID + ".sock",
		"log":         snapshotLine + "\n",
	}
	runsJSON, err := json.Marshal([]map[string]any{run})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalRafQueue = [];
    window.requestAnimationFrame = function (cb) {
      window.__portalRafQueue.push(cb);
      return window.__portalRafQueue.length;
    };
    window.__portalRunRaf = function (index) {
      var cb = window.__portalRafQueue.splice(index, 1)[0];
      if (typeof cb === 'function') cb(performance.now());
    };
    window.__portalRunAllRafs = function () {
      while (window.__portalRafQueue.length) window.__portalRunRaf(0);
    };
    window.__portalStreams = [];
    window.EventSource = function (url) {
      this.url = url;
      this.readyState = 1;
      this.closed = false;
      this.onmessage = null;
      this.onerror = null;
      this.close = function () {
        this.closed = true;
        this.readyState = 2;
      };
      window.__portalStreams.push(this);
    };
    setTimeout(function () {
      window.__portalRunAllRafs();
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      var firstStream = window.__portalStreams[0];
      if (!row || !firstStream || typeof firstStream.onmessage !== 'function') {
        throw new Error('initial active row and stream were not mounted');
      }

      // Queue a live tail, then deliberately run the collapse render before
      // its rAF flush. This is the detach-before-flush ordering that used to
      // lose the tail from the cached pane.
      firstStream.onmessage({ data: '`+bufferedLine+`' });
      row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      window.__portalRunRaf(window.__portalRafQueue.length - 1);
      window.__portalRunAllRafs();

      row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      window.__portalRunAllRafs();
      var replayStream = window.__portalStreams[1];
      if (!replayStream || typeof replayStream.onmessage !== 'function') {
        throw new Error('reopened active row did not create a replacement stream');
      }
      // The broadcaster replays the persisted prefix and buffered live tail
      // before delivering new output. Each replayed cached line must dedupe.
      replayStream.onmessage({ data: '`+snapshotLine+`' });
      replayStream.onmessage({ data: '`+bufferedLine+`' });
      replayStream.onmessage({ data: '`+replayedLine+`' });
      setTimeout(function () {
        window.__portalRunAllRafs();
        var pre = document.querySelector('pre[data-scroll-key="`+runID+`"]');
        var marker = document.createElement('pre');
        marker.id = 'portal-reopen-log-order';
        marker.textContent = JSON.stringify({
          renderedLog: pre ? pre.getAttribute('data-rendered-log') || '' : '',
          streamCount: window.__portalStreams.length,
        });
        document.body.appendChild(marker);
      }, 20);
    }, 80);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-reopen-log-order")
	var result struct {
		RenderedLog string `json:"renderedLog"`
		StreamCount int    `json:"streamCount"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse reopen ordering payload: %v\nraw=%s", err, payload)
	}
	want := snapshotLine + "\n" + bufferedLine + "\n" + replayedLine + "\n"
	if result.RenderedLog != want {
		t.Fatalf("reopened log order = %q, want %q (streams=%d)", result.RenderedLog, want, result.StreamCount)
	}
}

// TestPortalRowReopen_DiscardsStaleReplayBeforeCachedSuffix covers the real
// production boundary: the portal cache holds the newest 64 KiB while a new
// SSE connection replays the broadcaster's older, larger history. Replays
// before the cached suffix must not be appended after it.
func TestPortalRowReopen_DiscardsStaleReplayBeforeCachedSuffix(t *testing.T) {
	const runID = "260901131553-08ee-444"
	const cachedStart = "13:18:46 $ gh run view current --log"
	const cachedEnd = "13:19:58 -> Read current fixture"
	const staleFirst = "13:17:16 -> Read earlier fixture"
	const staleSecond = "13:17:22 -> Read another earlier fixture"
	const liveTail = "13:20:17 $ cargo test -p threeterm-mcp"

	run := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#444",
		"issueNumber": 444,
		"batchKey":    "260901131553-08ee-444+24",
		"socketPath":  "/tmp/" + runID + ".sock",
		"log":         cachedStart + "\n" + cachedEnd + "\n",
	}
	runsJSON, err := json.Marshal([]map[string]any{run})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalRafQueue = [];
    window.requestAnimationFrame = function (cb) {
      window.__portalRafQueue.push(cb);
      return window.__portalRafQueue.length;
    };
    window.__portalRunRaf = function (index) {
      var cb = window.__portalRafQueue.splice(index, 1)[0];
      if (typeof cb === 'function') cb(performance.now());
    };
    window.__portalRunAllRafs = function () {
      while (window.__portalRafQueue.length) window.__portalRunRaf(0);
    };
    window.__portalStreams = [];
    window.EventSource = function (url) {
      this.url = url;
      this.readyState = 1;
      this.closed = false;
      this.onmessage = null;
      this.onerror = null;
      this.close = function () {
        this.closed = true;
        this.readyState = 2;
      };
      window.__portalStreams.push(this);
    };
    setTimeout(function () {
      window.__portalRunAllRafs();
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      if (!row || window.__portalStreams.length !== 1) {
        throw new Error('initial active row and stream were not mounted');
      }

      row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      window.__portalRunAllRafs();
      row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      window.__portalRunAllRafs();

      var replayStream = window.__portalStreams[1];
      if (!replayStream || typeof replayStream.onmessage !== 'function') {
        throw new Error('reopened active row did not create a replacement stream');
      }
      // The reconnect begins before the pane's 64 KiB cached suffix, reaches
      // that suffix, then carries genuinely new live output.
      replayStream.onmessage({ data: '`+staleFirst+`' });
      replayStream.onmessage({ data: '`+staleSecond+`' });
      replayStream.onmessage({ data: '`+cachedStart+`' });
      replayStream.onmessage({ data: '`+cachedEnd+`' });
      replayStream.onmessage({ data: '`+liveTail+`' });
      setTimeout(function () {
        window.__portalRunAllRafs();
        var pre = document.querySelector('pre[data-scroll-key="`+runID+`"]');
        var marker = document.createElement('pre');
        marker.id = 'portal-reopen-stale-replay';
        marker.textContent = JSON.stringify({
          renderedLog: pre ? pre.getAttribute('data-rendered-log') || '' : '',
          streamCount: window.__portalStreams.length,
        });
        document.body.appendChild(marker);
      }, 20);
    }, 80);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-reopen-stale-replay")
	var result struct {
		RenderedLog string `json:"renderedLog"`
		StreamCount int    `json:"streamCount"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse stale replay payload: %v\nraw=%s", err, payload)
	}
	for _, stale := range []string{"13:17:16", "13:17:22"} {
		if strings.Contains(result.RenderedLog, stale) {
			t.Fatalf("reopened log retained stale replay %q: %q (streams=%d)", stale, result.RenderedLog, result.StreamCount)
		}
	}
	if strings.Count(result.RenderedLog, cachedStart) != 1 || strings.Count(result.RenderedLog, "13:19:58") != 1 || strings.Count(result.RenderedLog, liveTail) != 1 {
		t.Fatalf("reopened log must retain one cached suffix and one live tail: %q (streams=%d)", result.RenderedLog, result.StreamCount)
	}
	if strings.Index(result.RenderedLog, cachedStart) > strings.Index(result.RenderedLog, liveTail) {
		t.Fatalf("live tail preceded cached suffix: %q (streams=%d)", result.RenderedLog, result.StreamCount)
	}
}

// TestPortalRowReopen_DoesNotAcceptAnEarlierRepeatedLineAsReplayCheckpoint
// covers a replay that contains a duplicate of a cached line before reaching
// the cached suffix. The duplicate must not release the replay gate early,
// or following historical lines are appended after the newer cached log.
func TestPortalRowReopen_DoesNotAcceptAnEarlierRepeatedLineAsReplayCheckpoint(t *testing.T) {
	const runID = "260924114000-08ee-555"
	const repeatedLine = "12:30:00 $ cargo test -p host"
	const cachedTail = "12:41:52 PR-Review: CI pending"
	const staleLine = "12:27:13 * Grep old source snippet"
	const liveTail = "12:42:01 Read current source"

	run := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#555",
		"issueNumber": 555,
		"batchKey":    "260924114000-08ee-555+7",
		"socketPath":  "/tmp/" + runID + ".sock",
		"log":         repeatedLine + "\n" + cachedTail + "\n",
	}
	runsJSON, err := json.Marshal([]map[string]any{run})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalRafQueue = [];
    window.requestAnimationFrame = function (cb) {
      window.__portalRafQueue.push(cb);
      return window.__portalRafQueue.length;
    };
    window.__portalRunAllRafs = function () {
      while (window.__portalRafQueue.length) {
        var cb = window.__portalRafQueue.shift();
        if (typeof cb === 'function') cb(performance.now());
      }
    };
    window.__portalStreams = [];
    window.EventSource = function (url) {
      this.url = url;
      this.readyState = 1;
      this.closed = false;
      this.onmessage = null;
      this.onerror = null;
      this.close = function () {
        this.closed = true;
        this.readyState = 2;
      };
      window.__portalStreams.push(this);
    };
    setTimeout(function () {
      window.__portalRunAllRafs();
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      if (!row || window.__portalStreams.length !== 1) {
        throw new Error('initial active row and stream were not mounted');
      }

      row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      window.__portalRunAllRafs();
      row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      window.__portalRunAllRafs();

      var replayStream = window.__portalStreams[1];
      if (!replayStream || typeof replayStream.onmessage !== 'function') {
        throw new Error('reopened active row did not create a replacement stream');
      }
      // This earlier occurrence is text-identical to a cached line, but the
      // replay has not reached the cached suffix yet. The later occurrence
      // starts the real overlap.
      replayStream.onmessage({ data: '`+repeatedLine+`' });
      replayStream.onmessage({ data: '`+staleLine+`' });
      replayStream.onmessage({ data: '`+repeatedLine+`' });
      replayStream.onmessage({ data: '`+cachedTail+`' });
      replayStream.onmessage({ data: '`+liveTail+`' });
      setTimeout(function () {
        window.__portalRunAllRafs();
        var pre = document.querySelector('pre[data-scroll-key="`+runID+`"]');
        var marker = document.createElement('pre');
        marker.id = 'portal-reopen-repeated-checkpoint';
        marker.textContent = JSON.stringify({
          renderedLog: pre ? pre.getAttribute('data-rendered-log') || '' : '',
          streamCount: window.__portalStreams.length,
        });
        document.body.appendChild(marker);
      }, 20);
    }, 80);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-reopen-repeated-checkpoint")
	var result struct {
		RenderedLog string `json:"renderedLog"`
		StreamCount int    `json:"streamCount"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse repeated-checkpoint payload: %v\nraw=%s", err, payload)
	}
	want := repeatedLine + "\n" + cachedTail + "\n" + liveTail + "\n"
	if result.RenderedLog != want {
		t.Fatalf("reopened log = %q, want %q (streams=%d)", result.RenderedLog, want, result.StreamCount)
	}
}

func TestPortalRowReopen_AcceptsLiveOutputWhenCachedSuffixIsMissingFromReplay(t *testing.T) {
	const runID = "260926104500-08ee-556"
	const cachedHead = "10:00:00 $ previous output"
	const cachedTail = "10:01:00 latest cached output"
	const replayLine = "10:02:00 * replay starts after cached suffix"
	const liveLine = "10:03:00 live output after replay"

	run := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#556",
		"issueNumber": 556,
		"batchKey":    "260926104500-08ee-556+1",
		"socketPath":  "/tmp/" + runID + ".sock",
		"log":         cachedHead + "\n" + cachedTail + "\n",
	}
	runsJSON, err := json.Marshal([]map[string]any{run})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalRafQueue = [];
    window.requestAnimationFrame = function (cb) { window.__portalRafQueue.push(cb); return window.__portalRafQueue.length; };
    window.__portalRunAllRafs = function () {
      while (window.__portalRafQueue.length) {
        var cb = window.__portalRafQueue.shift();
        if (typeof cb === 'function') cb(performance.now());
      }
    };
    window.__portalStreams = [];
    window.EventSource = function (url) {
      this.url = url;
      this.readyState = 1;
      this.listeners = {};
      this.close = function () { this.readyState = 2; };
      this.addEventListener = function (type, fn) { this.listeners[type] = fn; };
      this.dispatchEvent = function (event) {
        if (this.listeners[event.type]) this.listeners[event.type](event);
      };
      window.__portalStreams.push(this);
    };
    setTimeout(function () {
      window.__portalRunAllRafs();
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      if (!row || window.__portalStreams.length !== 1) throw new Error('initial active row and stream were not mounted');

      row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      window.__portalRunAllRafs();
      row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      window.__portalRunAllRafs();

      var replayStream = window.__portalStreams[1];
      if (!replayStream) throw new Error('reopened active row did not create a replacement stream');
      replayStream.onmessage({ data: '`+replayLine+`' });
      replayStream.dispatchEvent({ type: 'replay-complete' });
      replayStream.onmessage({ data: '`+liveLine+`' });
      setTimeout(function () {
        window.__portalRunAllRafs();
        var pre = document.querySelector('pre[data-scroll-key="`+runID+`"]');
        var marker = document.createElement('pre');
        marker.id = 'portal-reopen-missing-checkpoint';
        marker.textContent = JSON.stringify({ renderedLog: pre ? pre.getAttribute('data-rendered-log') || '' : '' });
        document.body.appendChild(marker);
      }, 20);
    }, 80);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-reopen-missing-checkpoint")
	var result struct {
		RenderedLog string `json:"renderedLog"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse missing-checkpoint payload: %v\nraw=%s", err, payload)
	}
	want := cachedHead + "\n" + cachedTail + "\n" + liveLine + "\n"
	if result.RenderedLog != want {
		t.Fatalf("reopened log = %q, want cached log followed by live output %q", result.RenderedLog, want)
	}
}

// TestPortalTabRoundTrip_DoesNotAppendHistoricalReplayAfterNewerSnapshot is
// the production-page reproduction for the original report. The tab buttons
// tear down and recreate the stream while the cached log pane remains newer
// than the broadcaster replay. Text-only deduplication cannot distinguish the
// historical records from new output, so this must fail on the baseline.
func TestPortalTabRoundTrip_DoesNotAppendHistoricalReplayAfterNewerSnapshot(t *testing.T) {
	const runID = "261007101600-08ee-2772"
	const currentCommand = "10:09:32 current command"
	const currentOutput = "10:16:22 current output"
	const oldCommand = "09:25:07 old command"
	const oldOutput = "09:27:15 old output"
	const newOutput = "10:16:37 new live output"
	const detailsOutput = "10:16:38 details live output"

	run := map[string]any{
		"key":         runID,
		"runId":       runID,
		"kind":        "active",
		"status":      "running",
		"issueLabel":  "#2772",
		"issueNumber": 2772,
		"batchKey":    runID,
		"socketPath":  "/tmp/" + runID + ".sock",
		"log":         currentCommand + "\n" + currentOutput + "\n",
	}
	runsJSON, err := json.Marshal([]map[string]any{run})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`

	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalRafQueue = [];
    window.requestAnimationFrame = function (cb) { window.__portalRafQueue.push(cb); return window.__portalRafQueue.length; };
    window.__portalRunAllRafs = function () {
      while (window.__portalRafQueue.length) {
        var cb = window.__portalRafQueue.shift();
        if (typeof cb === 'function') cb(performance.now());
      }
    };
    window.__portalStreams = [];
    window.EventSource = function (url) {
      this.url = url;
      this.readyState = 1;
      this.closed = false;
      this.listeners = {};
      this.addEventListener = function (type, fn) { this.listeners[type] = fn; };
      this.dispatchEvent = function (event) {
        if (this.listeners[event.type]) this.listeners[event.type](event);
      };
      this.close = function () { this.closed = true; this.readyState = 2; };
      window.__portalStreams.push(this);
    };
    setTimeout(function () {
      window.__portalRunAllRafs();
      var row = document.querySelector('tr[data-run-key="`+runID+`"]');
      var eventsTab = row && document.querySelector('button[data-action="set-tab"][data-tab="events"]');
      if (!row || !eventsTab || window.__portalStreams.length !== 1) throw new Error('initial log view was not mounted');
      eventsTab.click();
      window.__portalRunAllRafs();
      setTimeout(function () {
        var logTab = document.querySelector('button[data-action="set-tab"][data-tab="log"]');
        if (!logTab) throw new Error('log tab was not restored');
        logTab.click();
        window.__portalRunAllRafs();
         var replay = window.__portalStreams[1];
         if (!replay || typeof replay.onmessage !== 'function') throw new Error('tab return did not create a stream');
         replay.dispatchEvent({ type: 'snapshot', data: JSON.stringify({
           runId: '`+runID+`', generation: 'saved-generation-1', start: 0, end: 2,
           cursor: { runId: '`+runID+`', generation: 'saved-generation-1', offset: 2 },
           records: [
             { runId: '`+runID+`', generation: 'saved-generation-1', start: 0, end: 1, text: '`+currentCommand+`' },
             { runId: '`+runID+`', generation: 'saved-generation-1', start: 1, end: 2, text: '`+currentOutput+`' },
           ],
         }) });
         // Legacy replay text must not mutate a stream after the structured
         // Saved Run Log snapshot has established the source contract.
         replay.onmessage({ data: '`+oldCommand+`' });
         replay.onmessage({ data: '`+oldOutput+`' });
         replay.dispatchEvent({ type: 'append', data: JSON.stringify({
           runId: '`+runID+`', generation: 'saved-generation-1', start: 2, end: 3,
           cursor: { runId: '`+runID+`', generation: 'saved-generation-1', offset: 3 },
           records: [{ runId: '`+runID+`', generation: 'saved-generation-1', start: 2, end: 3, text: '`+newOutput+`' }],
         }) });
         setTimeout(function () {
           var detailsTab = document.querySelector('button[data-action="set-tab"][data-tab="details"]');
           if (!detailsTab) throw new Error('details tab was not mounted');
           detailsTab.click();
           window.__portalRunAllRafs();
           setTimeout(function () {
             var secondLogTab = document.querySelector('button[data-action="set-tab"][data-tab="log"]');
             if (!secondLogTab) throw new Error('log tab was not restored after details');
             secondLogTab.click();
             window.__portalRunAllRafs();
             var replayAfterDetails = window.__portalStreams[2];
             if (!replayAfterDetails) throw new Error('details return did not create a stream');
             replayAfterDetails.dispatchEvent({ type: 'snapshot', data: JSON.stringify({
               runId: '`+runID+`', generation: 'saved-generation-1', start: 0, end: 2,
               cursor: { runId: '`+runID+`', generation: 'saved-generation-1', offset: 2 },
               records: [
                 { runId: '`+runID+`', generation: 'saved-generation-1', start: 0, end: 1, text: '`+currentCommand+`' },
                 { runId: '`+runID+`', generation: 'saved-generation-1', start: 1, end: 2, text: '`+currentOutput+`' },
               ],
             }) });
             replayAfterDetails.onmessage({ data: '`+oldCommand+`' });
             replayAfterDetails.onmessage({ data: '`+oldOutput+`' });
             replayAfterDetails.dispatchEvent({ type: 'append', data: JSON.stringify({
               runId: '`+runID+`', generation: 'saved-generation-1', start: 3, end: 4,
               cursor: { runId: '`+runID+`', generation: 'saved-generation-1', offset: 4 },
               records: [{ runId: '`+runID+`', generation: 'saved-generation-1', start: 3, end: 4, text: '`+detailsOutput+`' }],
             }) });
             setTimeout(function () {
               window.__portalRunAllRafs();
               var pre = document.querySelector('pre[data-scroll-key="`+runID+`"]');
               var marker = document.createElement('pre');
               marker.id = 'portal-tab-round-trip-order';
               marker.textContent = JSON.stringify({ renderedLog: pre ? pre.getAttribute('data-rendered-log') || '' : '' });
               document.body.appendChild(marker);
             }, 20);
           }, 40);
         }, 20);
      }, 40);
    }, 80);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-tab-round-trip-order")
	var result struct {
		RenderedLog string `json:"renderedLog"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse tab round-trip payload: %v\nraw=%s", err, payload)
	}
	want := currentCommand + "\n" + currentOutput + "\n" + newOutput + "\n" + detailsOutput + "\n"
	if result.RenderedLog != want {
		t.Fatalf("tab round-trip log = %q, want %q", result.RenderedLog, want)
	}
}
