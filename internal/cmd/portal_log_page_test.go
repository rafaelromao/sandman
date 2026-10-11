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
