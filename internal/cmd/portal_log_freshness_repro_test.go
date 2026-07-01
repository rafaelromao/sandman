package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPortalRefresh_ActiveLogPaneUpdatesWhenOutputAdvances(t *testing.T) {
	batchID := "abcd-260618113825"
	runID := batchID + "-issue-42"
	initialRun := map[string]any{
		"key":          runID,
		"runId":        runID,
		"kind":         "active",
		"status":       "running",
		"issueLabel":   "#42",
		"issueNumber":  42,
		"batchKey":     batchID,
		"socketPath":   "/tmp/" + runID + ".sock",
		"log":          "initial line 1\ninitial line 2",
		"lastOutputAt": "2025-06-26T12:00:00Z",
	}
	refreshedRun := map[string]any{
		"key":          runID,
		"runId":        runID,
		"kind":         "active",
		"status":       "running",
		"issueLabel":   "#42",
		"issueNumber":  42,
		"batchKey":     batchID,
		"socketPath":   "/tmp/" + runID + ".sock",
		"log":          "initial line 1\ninitial line 2\nfresh line 3",
		"lastOutputAt": "2025-06-26T12:01:00Z",
	}
	runsJSON, err := json.Marshal([]map[string]any{initialRun})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	refreshedRunsJSON, err := json.Marshal([]map[string]any{refreshedRun})
	if err != nil {
		t.Fatalf("marshal refreshed runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`
	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalFetchPayloads = [
      { runs: `+string(runsJSON)+` },
      { runs: `+string(refreshedRunsJSON)+` }
    ];
    window.fetch = async function () {
      window.__portalFetchCalls += 1;
      var next = window.__portalFetchPayloads.length ? window.__portalFetchPayloads.shift() : { runs: `+string(refreshedRunsJSON)+` };
      return {
        ok: true,
        status: 200,
        json: async function () { return next; },
        text: async function () { return ''; },
      };
    };
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var pre = detail && detail.querySelector('.detail-content pre[data-scroll-key]');
      window.__portalActiveBefore = {
        refreshCalls: window.__portalRefreshCalls || 0,
        detailText: pre ? pre.textContent : '',
        renderedLog: pre ? pre.getAttribute('data-rendered-log') : ''
      };
    }, 40);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var pre = detail && detail.querySelector('.detail-content pre[data-scroll-key]');
      window.__portalActiveMid = {
        fetchCalls: window.__portalFetchCalls || 0,
        detailText: pre ? pre.textContent : '',
        renderedLog: pre ? pre.getAttribute('data-rendered-log') : ''
      };
    }, 400);
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var pre = detail && detail.querySelector('.detail-content pre[data-scroll-key]');
      var marker = document.createElement('pre');
      marker.id = 'portal-active-freshness';
      marker.textContent = JSON.stringify({
        hasDetail: !!pre,
        beforeRefresh: window.__portalActiveBefore || null,
        midPoll: window.__portalActiveMid || null,
        fetchCalls: window.__portalFetchCalls || 0,
        detailText: pre ? pre.textContent : '',
        renderedLog: pre ? pre.getAttribute('data-rendered-log') : ''
      });
      document.body.appendChild(marker);
    }, 2500);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-active-freshness")
	var result struct {
		HasDetail     bool `json:"hasDetail"`
		BeforeRefresh struct {
			RefreshCalls int    `json:"refreshCalls"`
			DetailText   string `json:"detailText"`
			RenderedLog  string `json:"renderedLog"`
		} `json:"beforeRefresh"`
		MidPoll struct {
			FetchCalls  int    `json:"fetchCalls"`
			DetailText  string `json:"detailText"`
			RenderedLog string `json:"renderedLog"`
		} `json:"midPoll"`
		FetchCalls  int    `json:"fetchCalls"`
		DetailText  string `json:"detailText"`
		RenderedLog string `json:"renderedLog"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse active freshness payload: %v\nraw=%s", err, payload)
	}
	if !result.HasDetail {
		t.Fatalf("expected an expanded detail row, got %#v", result)
	}
	if result.BeforeRefresh.RefreshCalls != 0 {
		t.Fatalf("expected the pre-refresh snapshot to run before poll refresh, got %#v", result)
	}
	if strings.Contains(result.BeforeRefresh.DetailText, "fresh line 3") {
		t.Fatalf("expected pre-refresh text to remain stale, got %#v", result)
	}
	if strings.Contains(result.BeforeRefresh.RenderedLog, "fresh line 3") {
		t.Fatalf("expected pre-refresh rendered-log cache to remain stale, got %#v", result)
	}
	if result.FetchCalls < 2 {
		t.Fatalf("expected at least 2 refresh fetches, got %#v", result)
	}
	if result.MidPoll.FetchCalls < 2 || !strings.Contains(result.MidPoll.DetailText, "fresh line 3") {
		t.Fatalf("expected the second poll to refresh the active pane by mid-point, got %#v", result)
	}
	if !strings.Contains(result.DetailText, "fresh line 3") {
		t.Fatalf("expected refreshed log text to be visible, got %#v", result)
	}
	if !strings.Contains(result.RenderedLog, "fresh line 3") {
		t.Fatalf("expected refreshed log cache to include the new line, got %#v", result)
	}
}

func TestPortalRefresh_StreamedLogPaneIsNotOverwrittenByPollRefreshes(t *testing.T) {
	batchID := "abcd-260618113825"
	runID := batchID + "-issue-42"
	initialRun := map[string]any{
		"key":          runID,
		"runId":        runID,
		"kind":         "active",
		"status":       "running",
		"issueLabel":   "#42",
		"issueNumber":  42,
		"batchKey":     batchID,
		"socketPath":   "/tmp/" + runID + ".sock",
		"log":          "initial line 1",
		"lastOutputAt": "2025-06-26T12:00:00Z",
	}
	staleRun := map[string]any{
		"key":          runID,
		"runId":        runID,
		"kind":         "active",
		"status":       "running",
		"issueLabel":   "#42",
		"issueNumber":  42,
		"batchKey":     batchID,
		"socketPath":   "/tmp/" + runID + ".sock",
		"log":          "initial line 1",
		"lastOutputAt": "2025-06-26T12:01:00Z",
	}
	runsJSON, err := json.Marshal([]map[string]any{initialRun})
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	staleRunsJSON, err := json.Marshal([]map[string]any{staleRun})
	if err != nil {
		t.Fatalf("marshal stale runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + runID + `","tabs":{"` + runID + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`
	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__portalFetchPayloads = [
      { runs: `+string(runsJSON)+` },
      { runs: `+string(staleRunsJSON)+` }
    ];
    window.__portalStreams = [];
    window.EventSource = function (url) {
      this.url = url;
      this._onmessage = null;
      this._emitted = false;
      Object.defineProperty(this, 'onmessage', {
        configurable: true,
        get: function () { return this._onmessage; },
        set: function (fn) {
          this._onmessage = fn;
          if (this._emitted || typeof fn !== 'function') return;
          this._emitted = true;
          var self = this;
          setTimeout(function () {
            if (self._onmessage) self._onmessage({ data: 'streamed line 2' });
            if (!window.__portalStreamBefore) {
              window.__portalStreamBefore = {
                refreshCalls: window.__portalFetchCalls || 0,
                hasStream: window.__portalStreams.length > 0
              };
            }
          }, 0);
        }
      });
      this.onerror = null;
      this.closed = false;
      this.close = function () { this.closed = true; };
      window.__portalStreams.push(this);
    };
    window.fetch = async function () {
      window.__portalFetchCalls += 1;
      var next = window.__portalFetchPayloads.length ? window.__portalFetchPayloads.shift() : { runs: `+string(staleRunsJSON)+` };
      return {
        ok: true,
        status: 200,
        json: async function () { return next; },
        text: async function () { return ''; },
      };
    };
    setTimeout(function () {
      var detail = document.querySelector('tr.detail-row[data-detail-for="`+runID+`"]');
      var pre = detail && detail.querySelector('.detail-content pre[data-scroll-key]');
      var marker = document.createElement('pre');
      marker.id = 'portal-stream-freshness';
      marker.textContent = JSON.stringify({
        beforePoll: window.__portalStreamBefore || null,
        hasStream: window.__portalStreams.length > 0,
        fetchCalls: window.__portalFetchCalls || 0,
        detailText: pre ? pre.textContent : '',
        renderedLog: pre ? pre.getAttribute('data-rendered-log') : ''
      });
      document.body.appendChild(marker);
    }, 2500);
  `)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-stream-freshness")
	var result struct {
		BeforePoll struct {
			RefreshCalls int  `json:"refreshCalls"`
			HasStream    bool `json:"hasStream"`
		} `json:"beforePoll"`
		HasStream   bool   `json:"hasStream"`
		FetchCalls  int    `json:"fetchCalls"`
		DetailText  string `json:"detailText"`
		RenderedLog string `json:"renderedLog"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse stream freshness payload: %v\nraw=%s", err, payload)
	}
	if !result.HasStream {
		t.Fatalf("expected a live stream to be established, got %#v", result)
	}
	if result.BeforePoll.RefreshCalls < 1 {
		t.Fatalf("expected the pre-poll stream snapshot to capture at least one refresh, got %#v", result)
	}
	if !result.BeforePoll.HasStream {
		t.Fatalf("expected a live stream to be present before the stale poll, got %#v", result)
	}
	if result.FetchCalls < 2 {
		t.Fatalf("expected at least 2 refresh fetches, got %#v", result)
	}
	if !strings.Contains(result.DetailText, "streamed line 2") {
		t.Fatalf("expected streamed line to remain visible after refresh, got %#v", result)
	}
	if !strings.Contains(result.RenderedLog, "streamed line 2") {
		t.Fatalf("expected streamed line to remain in rendered log cache, got %#v", result)
	}
}
