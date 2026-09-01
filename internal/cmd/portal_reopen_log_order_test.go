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
      // The replay includes the persisted prefix and then a new line. The
      // prefix must dedupe, while the cached live tail remains between them.
      replayStream.onmessage({ data: '`+snapshotLine+`' });
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
