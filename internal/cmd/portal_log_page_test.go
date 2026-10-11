package cmd

import (
	"encoding/json"
	"testing"
)

func TestPortalLogFrontend_LateParentDetailCannotClearSelectedReviewLoadingState(t *testing.T) {
	runsJSON := []byte(`[{"key":"parent","runId":"parent","kind":"active","status":"running","issueNumber":42,"issueLabel":"#42","log":"parent source"},{"key":"review","runId":"review","kind":"active","status":"reviewing","issueNumber":42,"issueLabel":"PR42","prNumber":42,"review":true,"log":"review source"}]`)
	page := buildPortalReproPage(t, `{"expandedRunKey":"parent","tabs":{"parent":"log","review":"log"}}`, runsJSON, `
window.fetch = async function (url) {
  if (String(url).indexOf('summary=1') >= 0) return { ok: true, json: async () => ({ runs: `+string(runsJSON)+` }) };
  return new Promise(resolve => {
    const runId = new URL(url, 'http://localhost').searchParams.get('runKey');
    window['__resolve_' + runId] = () => resolve({ ok: true, json: async () => ({ run: { key: runId, runId, events: [], log: 'late metadata text must not own the log' } }) });
  });
};
function startSwitch() {
  const select = document.querySelector('select[data-action="set-subject"]');
  if (!select || !window.__resolve_parent) { setTimeout(startSwitch, 10); return; }
  select.value = 'review';
  select.dispatchEvent(new Event('change', { bubbles: true }));
  inspectReview();
}

async function inspectReview() {
  const state = window.sandmanPortalLogs.get('review');
  const panel = document.querySelector('.detail-panel');
  if (!window.__resolve_review || !state || state.text !== 'review source\n' || !panel.classList.contains('is-loading')) { setTimeout(inspectReview, 10); return; }
  window.__resolve_parent();
  // Complete the old fetch's promise chain before the next animation frame.
  for (let i = 0; i < 10; i++) await Promise.resolve();
  const marker = document.createElement('pre');
  marker.id = 'portal-late-detail';
  marker.textContent = JSON.stringify({ busy: panel.classList.contains('is-loading'), acceptedText: window.sandmanPortalLogs.get('review').text });
  document.body.appendChild(marker);
  window.__resolve_review();
}
setTimeout(startSwitch, 20);
`)
	dom, _ := runPortalChromium(t, page)
	var result struct {
		Busy         bool   `json:"busy"`
		AcceptedText string `json:"acceptedText"`
	}
	if err := json.Unmarshal([]byte(extractPortalMarker(t, dom, "portal-late-detail")), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Busy || result.AcceptedText != "review source\n" {
		t.Fatalf("late parent callback changed selected review view/model: %+v", result)
	}
}

func TestPortalLogFrontend_SourceModelWinsBothDetailFetchCompletionOrders(t *testing.T) {
	const runID = "source-order"
	runs := []map[string]any{{"key": runID, "runId": runID, "kind": "active", "status": "running", "issueNumber": 2772, "issueLabel": "#2772", "log": "stale summary text"}}
	runsJSON, err := json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"detail-before-live-append", "live-append-before-detail"} {
		t.Run(order, func(t *testing.T) {
			stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`
			page := buildPortalReproPage(t, stateJSON, runsJSON, `
window.__detailOrder = '`+order+`';
window.__detailCompleted = false;
window.__resolveRunDetail = null;
window.__portalNextFrame = function (runId) {
  return { type: 'snapshot', runId, generation: 'saved-source', start: 0, end: 7,
    records: [{ start: 0, end: 7, text: 'source' }], cursor: 'source-cursor-7' };
};
window.fetch = async function (url) {
  const value = String(url);
  if (value.indexOf('summary=1') >= 0) return { ok: true, status: 200, headers: { get: () => '"stable"' }, json: async () => ({ runs: `+string(runsJSON)+` }) };
  const stale = { ok: true, status: 200, json: async () => { window.__detailCompleted = true; return { run: { key: '`+runID+`', runId: '`+runID+`', events: [], log: 'stale detail text must not replace Saved Run Log' } }; } };
  if (window.__detailOrder === 'detail-before-live-append') return stale;
  return new Promise(resolve => { window.__resolveRunDetail = () => resolve(stale); });
};
async function finishOrderTest() {
  const wait = async (predicate, description) => {
    const deadline = Date.now() + 3000;
    while (Date.now() < deadline) {
      if (predicate()) return;
      await new Promise(resolve => setTimeout(resolve, 10));
    }
    throw new Error('timed out waiting for ' + description);
  };
  await wait(() => window.sandmanPortalLogs.get('`+runID+`')?.cursor, 'source snapshot');
  const state = window.sandmanPortalLogs.get('`+runID+`');
  const stream = window.__portalStreams.find(source => source.runId === '`+runID+`');
  if (window.__detailOrder === 'live-append-before-detail') {
    await wait(() => !!window.__resolveRunDetail, 'deferred detail request');
    stream.onmessage({ data: JSON.stringify({ type: 'append', runId: '`+runID+`', generation: state.generation,
      start: 7, end: 12, records: [{ start: 7, end: 12, text: 'live' }], cursor: 'source-cursor-12' }) });
    await wait(() => window.sandmanPortalLogs.get('`+runID+`').end === 12, 'live append acceptance');
    window.__resolveRunDetail();
  } else {
    await wait(() => window.__detailCompleted, 'detail completion before live append');
    stream.onmessage({ data: JSON.stringify({ type: 'append', runId: '`+runID+`', generation: state.generation,
      start: 7, end: 12, records: [{ start: 7, end: 12, text: 'live' }], cursor: 'source-cursor-12' }) });
  }
  await wait(() => {
    const pre = document.querySelector('pre[data-scroll-key="`+runID+`"]');
    return pre && !pre.getAttribute('data-rendering-log') && pre.textContent === window.sandmanPortalLogs.get('`+runID+`').text;
  }, 'source projection after detail completion');
  const pre = document.querySelector('pre[data-scroll-key="`+runID+`"]');
  const result = { order: window.__detailOrder, model: window.sandmanPortalLogs.get('`+runID+`').text, pane: pre.textContent,
    cursor: window.sandmanPortalLogs.get('`+runID+`').cursor, stale: pre.textContent.indexOf('stale') >= 0 };
  const marker = document.createElement('pre');
  marker.id = 'portal-detail-source-order';
  marker.textContent = JSON.stringify(result);
  document.body.appendChild(marker);
}
setTimeout(() => finishOrderTest().catch(error => {
  const marker = document.createElement('pre');
  marker.id = 'portal-detail-source-order';
  marker.textContent = JSON.stringify({ error: String(error && error.stack || error) });
  document.body.appendChild(marker);
}), 20);
`)
			dom, _ := runPortalChromium(t, page)
			var result struct {
				Order  string `json:"order"`
				Model  string `json:"model"`
				Pane   string `json:"pane"`
				Cursor string `json:"cursor"`
				Stale  bool   `json:"stale"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal([]byte(extractPortalMarker(t, dom, "portal-detail-source-order")), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error != "" || result.Order != order || result.Model != "source\nlive\n" || result.Pane != result.Model || result.Cursor != "source-cursor-12" || result.Stale {
				t.Fatalf("detail completion %s changed authoritative log state: %+v", order, result)
			}
		})
	}
}
