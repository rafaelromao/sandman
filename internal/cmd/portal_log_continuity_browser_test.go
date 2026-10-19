package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortalLogContinuityBrowser_TabRoundTrips drives the issue #2772 actual
// tab paths and a forced native EventSource reconnect through the production
// saved-log endpoint and embedded page.
// Unlike buildPortalReproPage/runPortalChromium, it substitutes no browser APIs
// and does not load a file:// page or extract/redefine production functions.
func TestPortalLogContinuityBrowser_TabRoundTrips(t *testing.T) {
	chromium := requirePortalContinuityTool(t, "chromium")
	node := requirePortalContinuityTool(t, "node")
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate continuity browser driver")
	}
	driverPath := filepath.Join(filepath.Dir(thisFile), "portal_log_continuity_browser_driver_test.js")
	driver, err := os.ReadFile(driverPath)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", driverPath).CombinedOutput(); err != nil {
		t.Fatalf("browser driver syntax: %v\n%s", err, out)
	}
	for _, tab := range []string{"events", "details"} {
		t.Run(tab, func(t *testing.T) {
			fixture := newPortalLogContinuityFixture(t, tab, driver)
			// The handler argument is the transport seam for the subsequent
			// saved-log/structured-frame implementation. Keep the page, browser
			// actions, append-only artifact and complete-sequence oracle intact.
			registry := newPortalLogRegistry()
			t.Cleanup(registry.Close)
			server := httptest.NewServer(fixture.handler(fixture.savedLogStream(registry)))
			defer server.Close()
			result := runPortalContinuityBrowser(t, chromium, server.URL, fixture.results)
			if result.Error != "" {
				t.Fatalf("actual-page browser reproduction: %s; stream status=%+v", result.Error, fixture.status())
			}
			status := fixture.status()
			if status.Connections != 60 || status.Closed < 59 || status.ForcedCloses != 3 || status.Error != "" {
				t.Fatalf("did not exercise native reconnect plus repeated tab replacements: %+v", status)
			}
			if len(status.ResumeCursors) != 60 || status.ResumeCursors[0] != "" || status.ResumeCursors[1] != "" {
				t.Fatalf("expected fresh URL cursors only for initial/native streams: %+v", status.ResumeCursors)
			}
			if len(status.LastEventIDs) != 60 || status.LastEventIDs[1] != result.InitialCursor {
				t.Fatalf("native EventSource reconnect did not send its last received source cursor: %+v", status.LastEventIDs)
			}
			for i := 2; i < len(status.ResumeCursors); i++ {
				if status.ResumeCursors[i] == "" {
					t.Fatalf("tab stream %d did not resume from accepted position: %+v", i+1, status.ResumeCursors)
				}
			}
			if result.StressTransitions != 50 || status.SummaryNotModified == 0 {
				t.Fatalf("actual page did not settle 50 mixed transitions under unchanged 304 polls: transitions=%d 304s=%d", result.StressTransitions, status.SummaryNotModified)
			}
			if result.MaxWarmTransitionMs >= 1000 || result.LiveUpdateMs >= 1000 {
				t.Fatalf("Portal exceeded sub-second warm/live responsiveness: warm-max=%.1fms live=%.1fms samples=%v", result.MaxWarmTransitionMs, result.LiveUpdateMs, result.WarmTransitionMs)
			}
			if result.ModelCacheBytes > 8*1024*1024 || result.ModelRuns > 16 || result.DOMLogNodes > 1000 {
				t.Fatalf("Portal continuity retention is not bounded: model=%d bytes/%d runs, DOM nodes=%d", result.ModelCacheBytes, result.ModelRuns, result.DOMLogNodes)
			}
			initial := fixture.readRange(t, fixture.snapshotStart, fixture.initialEnd)
			if result.InitialLog != initial {
				t.Fatalf("initial production Log pane differs from the saved 256 KiB retained range: %s", portalContinuityDifference(result.InitialLog, initial))
			}
			if result.InitialStart != fixture.snapshotStart || result.InitialEnd != fixture.initialEnd || result.InitialGeneration == "" {
				t.Fatalf("initial accepted range does not identify the saved source: %+v", result)
			}
			info, err := os.Stat(fixture.run.LogPath)
			if err != nil {
				t.Fatal(err)
			}
			sourceFile, err := os.Open(fixture.run.LogPath)
			if err != nil {
				t.Fatal(err)
			}
			finalStart, err := portalLogRetainedStart(sourceFile, info.Size())
			_ = sourceFile.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := fixture.readRange(t, finalStart, info.Size())
			t.Logf("saved run.log=%d bytes; accepted source range=[%d,%d); streams=%d; 304 polls=%d; warm max=%.1fms; live=%.1fms; model=%d bytes/%d runs; DOM nodes=%d",
				info.Size(), finalStart, info.Size(), status.Connections, status.SummaryNotModified,
				result.MaxWarmTransitionMs, result.LiveUpdateMs, result.ModelCacheBytes, result.ModelRuns, result.DOMLogNodes)
			// Compare every visible byte, including padding, newlines and all
			// records. Short labels below are diagnostics, never the oracle.
			if result.VisibleLog != want {
				t.Fatalf("Log -> %s -> Log violates saved-source continuity: %s\nvisible records: %s\nsource suffix + appended records: %s",
					tab, portalContinuityDifference(result.VisibleLog, want), portalContinuityLabels(result.VisibleLog), portalContinuityLabels(want))
			}
			if result.FinalStart != finalStart || result.FinalEnd != info.Size() || result.FinalGeneration != result.InitialGeneration {
				t.Fatalf("final DOM range/generation differs from source: final=[%d,%d) generation=%q, source=[%d,%d) generation=%q",
					result.FinalStart, result.FinalEnd, result.FinalGeneration, finalStart, info.Size(), result.InitialGeneration)
			}
		})
	}
}

func TestPortalLogContinuityBrowser_DurationSortUsesActiveGroupedDuration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	activeSince := now.Add(-10 * time.Minute)
	waitingSince := now.Add(-2 * time.Hour)
	waitingParent := map[string]any{
		"key": "waiting-parent", "runId": "waiting-parent", "kind": "active", "status": "reviewing",
		"waiting": true, "reviewLive": true, "reviewCount": 1, "issueNumber": 42, "issueLabel": "#42",
		"startedAt": now.Add(-2 * time.Hour).Format(time.RFC3339), "duration": "10m",
		"activeDurationSeconds": 600, "executionSince": waitingSince.Format(time.RFC3339),
	}
	activeParent := map[string]any{
		"key": "active-parent", "runId": "active-parent", "kind": "active", "status": "running",
		"issueNumber": 43, "issueLabel": "#43", "startedAt": now.Add(-45 * time.Minute).Format(time.RFC3339),
		"duration": "30m", "activeDurationSeconds": 1800, "executionSince": activeSince.Format(time.RFC3339),
	}
	groupedReview := map[string]any{
		"key": "review-child", "runId": "review-child", "kind": "active", "status": "reviewing", "review": true,
		"issueNumber": 42, "issueLabel": "PR42", "startedAt": now.Add(-90 * time.Minute).Format(time.RFC3339),
		"duration": "90m", "activeDurationSeconds": 0, "executionSince": now.Add(-90 * time.Minute).Format(time.RFC3339),
	}
	runsJSON, err := json.Marshal([]map[string]any{waitingParent, activeParent, groupedReview})
	if err != nil {
		t.Fatal(err)
	}
	stateJSON := `{"expandedRunKey":null,"tabs":{},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"duration","sortDir":"desc"}`
	page := buildPortalReproPage(t, stateJSON, runsJSON, "")
	page = strings.Replace(page, "</body>", `<script>
  let captureAttempts = 0;
  function captureDurationOrder() {
    const rows = Array.from(document.querySelectorAll('#runs-body > tr[data-run-key]'))
      .filter(row => !row.classList.contains('detail-row'));
    captureAttempts += 1;
    if (rows.length < 2 && captureAttempts < 100) { setTimeout(captureDurationOrder, 10); return; }
    const parent = rows.find(row => row.getAttribute('data-run-key') === 'waiting-parent');
    const marker = document.createElement('pre');
    marker.id = 'portal-active-duration-sort';
    marker.textContent = JSON.stringify({
      rowCount: rows.length,
      error: document.getElementById('error-banner')?.textContent || '',
      order: rows.map(row => row.getAttribute('data-run-key')),
      waitingDuration: parent && parent.querySelector('[data-cell="duration"] .duration-value')?.textContent,
      reviewChildVisible: rows.some(row => row.getAttribute('data-run-key') === 'review-child'),
    });
    document.body.appendChild(marker);
  }
  setTimeout(captureDurationOrder, 50);
</script></body>`, 1)
	dom, _ := runPortalChromium(t, page)
	var result struct {
		Order              []string `json:"order"`
		RowCount           int      `json:"rowCount"`
		WaitingDuration    string   `json:"waitingDuration"`
		ReviewChildVisible bool     `json:"reviewChildVisible"`
		Error              string   `json:"error"`
	}
	if err := json.Unmarshal([]byte(extractPortalMarker(t, dom, "portal-active-duration-sort")), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error != "" || result.RowCount != 2 || len(result.Order) != 2 || result.Order[0] != "active-parent" || result.Order[1] != "waiting-parent" {
		t.Fatalf("duration sort order = %#v (rows=%d error=%q), want 40m active before 10m waiting", result.Order, result.RowCount, result.Error)
	}
	if result.WaitingDuration != "10m" || result.ReviewChildVisible {
		t.Fatalf("grouped waiting row = duration %q, review child visible %v; want parent duration 10m and grouped child hidden", result.WaitingDuration, result.ReviewChildVisible)
	}
}

// TestPortalLogContinuityBrowser_OrphanReviewKeepsAbortAndClock is the
// actual-page regression for issue #2799. An orphan review-only issue group
// used to be rebuilt from a field whitelist that dropped the source row's
// batch and execution-clock metadata, so its Abort control, stale line and
// live duration disappeared after grouping and duration sort lost its clock.
func TestPortalLogContinuityBrowser_OrphanReviewKeepsAbortAndClock(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	orphanReview := map[string]any{
		"key": "review-2799", "runId": "review-2799", "kind": "active", "status": "reviewing", "review": true,
		"issueNumber": 2799, "issueLabel": "PR2810", "prNumber": 2810, "batchKey": "batch-2799",
		"startedAt": now.Add(-40 * time.Minute).Format(time.RFC3339), "duration": "30m",
		"activeDurationSeconds": 1800, "executionSince": now.Add(-10 * time.Minute).Format(time.RFC3339),
		"lastOutputAt": now.Add(-5 * time.Minute).Format(time.RFC3339),
	}
	shorterRun := map[string]any{
		"key": "impl-43", "runId": "impl-43", "kind": "active", "status": "running",
		"issueNumber": 43, "issueLabel": "#43", "batchKey": "batch-43",
		"startedAt": now.Add(-2 * time.Minute).Format(time.RFC3339), "duration": "1m",
		"activeDurationSeconds": 60, "executionSince": now.Add(-time.Minute).Format(time.RFC3339),
		"lastOutputAt": now.Format(time.RFC3339),
	}
	runsJSON, err := json.Marshal([]map[string]any{shorterRun, orphanReview})
	if err != nil {
		t.Fatal(err)
	}
	stateJSON := `{"expandedRunKey":null,"tabs":{},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"duration","sortDir":"desc"}`
	page := buildPortalReproPage(t, stateJSON, runsJSON, "")
	const abortDisabled = "const abortSupported = false;"
	if !strings.Contains(page, abortDisabled) {
		t.Fatal("Portal repro page is missing its abort-support switch")
	}
	page = strings.Replace(page, abortDisabled, "const abortSupported = true;", 1)
	page = strings.Replace(page, "</body>", `<script>
  let captureAttempts = 0;
  function captureOrphanReview() {
    const rows = Array.from(document.querySelectorAll('#runs-body > tr[data-run-key]'))
      .filter(row => !row.classList.contains('detail-row'));
    captureAttempts += 1;
    if (rows.length < 2 && captureAttempts < 100) { setTimeout(captureOrphanReview, 10); return; }
    const orphan = rows.find(row => row.getAttribute('data-run-key') === 'review-2799');
    const marker = document.createElement('pre');
    marker.id = 'portal-orphan-review';
    marker.textContent = JSON.stringify({
      error: document.getElementById('error-banner')?.textContent || '',
      order: rows.map(row => row.getAttribute('data-run-key')),
      label: orphan?.querySelector('[data-cell="title"] .name')?.textContent || '',
      abortVisible: !!orphan?.querySelector('button[data-action="abort-run"]'),
      duration: orphan?.querySelector('[data-cell="duration"] .duration-value')?.textContent || '',
      staleText: orphan?.querySelector('[data-cell="duration"] .stale-line')?.textContent || '',
    });
    document.body.appendChild(marker);
  }
  setTimeout(captureOrphanReview, 50);
</script></body>`, 1)
	dom, _ := runPortalChromium(t, page)
	var result struct {
		Error        string   `json:"error"`
		Order        []string `json:"order"`
		Label        string   `json:"label"`
		AbortVisible bool     `json:"abortVisible"`
		Duration     string   `json:"duration"`
		StaleText    string   `json:"staleText"`
	}
	if err := json.Unmarshal([]byte(extractPortalMarker(t, dom, "portal-orphan-review")), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error != "" || len(result.Order) != 2 || result.Order[0] != "review-2799" || result.Order[1] != "impl-43" {
		t.Fatalf("duration sort order = %#v (error=%q), want the 40m orphan review before the 2m run", result.Order, result.Error)
	}
	if result.Label != "PR 2810 (#2799)" {
		t.Fatalf("orphan review label = %q, want %q", result.Label, "PR 2810 (#2799)")
	}
	if !result.AbortVisible {
		t.Fatalf("orphan review row lost its Abort control after grouping: %+v", result)
	}
	if !strings.HasPrefix(strings.TrimSpace(result.Duration), "40m") {
		t.Fatalf("orphan review duration = %q, want the live source clock (~40m)", result.Duration)
	}
	if !strings.Contains(result.StaleText, "stale") {
		t.Fatalf("orphan review stale line = %q, want the source staleness signal", result.StaleText)
	}
}

type portalWaitingDurationBrowserResult struct {
	InitialStatus         string `json:"initialStatus"`
	InitialDuration       string `json:"initialDuration"`
	WaitingStatus         string `json:"waitingStatus"`
	Waiting               bool   `json:"waiting"`
	ReviewLive            bool   `json:"reviewLive"`
	WaitingDuration       string `json:"waitingDuration"`
	After304Duration      string `json:"after304Duration"`
	PostAwaitStatus       int    `json:"postAwaitStatus"`
	Waiting304Count       int    `json:"waiting304Count"`
	ResumedStatus         string `json:"resumedStatus"`
	ResumedWaiting        bool   `json:"resumedWaiting"`
	ResumedExecutionSince string `json:"resumedExecutionSince"`
	ResumedDuration       string `json:"resumedDuration"`
	Error                 string `json:"error,omitempty"`
}

func TestPortalLogContinuityBrowser_WaitingDuration(t *testing.T) {
	chromium := requirePortalContinuityTool(t, "chromium")
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const runID = "261012120000-b2805-2805"
	const reviewID = "261012120000-b2805-2805-PR2805"
	startedAt := time.Now().UTC().Truncate(time.Second)
	awaitAt := startedAt.Add(5 * time.Minute)
	reviewStartedAt := awaitAt.Add(time.Minute)
	resumeAt := awaitAt.Add(2 * time.Hour)
	eventLog := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}
	writePortalLog(t, eventLog.Path, []events.Event{{
		Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 2805,
		Payload: map[string]any{"branch": "2805-duration", "batch_id": "batch-2805"},
	}})

	handler := &portalHandler{repoRoot: repoRoot, runsIndex: getPortalRunsIndex(repoRoot)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/instances", handler.handleInstances)
	mux.HandleFunc("/api/runs", handler.handleRuns)
	mux.HandleFunc("/api/runs/stream", handler.handleRunStream)
	mux.HandleFunc("/api/runs/abort", handler.handleRunAbort)
	mux.HandleFunc("/api/runs/archive", handler.handleRunArchive)
	mux.HandleFunc("/api/logs", handler.handleLogs)
	mux.HandleFunc("/", handler.handlePage)
	handler.handler = mux
	index := handler.runsIndex
	var clockMu sync.Mutex
	portalNow := awaitAt
	index.view.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return portalNow
	}
	setPortalNow := func(now time.Time) {
		clockMu.Lock()
		portalNow = now
		clockMu.Unlock()
	}

	pageResponse := httptest.NewRecorder()
	handler.ServeHTTP(pageResponse, httptest.NewRequest(http.MethodGet, "/", nil))
	page := pageResponse.Body.String()
	mainScript := "<script>\n    const apiPath = \"\\/api\\/runs\";"
	if !strings.Contains(page, mainScript) {
		t.Fatal("embedded Portal page is missing its production application script marker")
	}
	prelude := fmt.Sprintf(`<script>
  window.__portalDurationNow = %d;
  Date.now = function () { return window.__portalDurationNow; };
  window.__portalSummaryStatuses = [];
  window.__portalSummaryRuns = [];
  window.__portalSummaryETag = '';
  window.__portalDurationFetch = window.fetch.bind(window);
  window.fetch = async function (input, init) {
    const response = await window.__portalDurationFetch(input, init);
    if (String(input).indexOf('summary=1') >= 0) {
      window.__portalSummaryStatuses.push(response.status);
      if (response.status !== 304) {
        const payload = await response.clone().json();
        window.__portalSummaryRuns = Array.isArray(payload.runs) ? payload.runs : [];
      }
      const etag = response.headers.get('ETag');
      if (etag) window.__portalSummaryETag = etag;
    }
    return response;
  };
  const portalSetInterval = window.setInterval.bind(window);
  window.setInterval = function (callback, delay, ...args) {
    return portalSetInterval(callback, callback && callback.name === 'refresh' ? 100 : delay, ...args);
  };
</script>
`+mainScript, startedAt.Add(5*time.Minute).UnixMilli())
	page = strings.Replace(page, mainScript, prelude, 1)
	driver := fmt.Sprintf(`
<script>
(async function () {
  const runID = %q;
  const wait = async function (predicate, label) {
    const deadline = performance.now() + 20000;
    while (performance.now() < deadline) {
      if (predicate()) return;
      await new Promise(resolve => setTimeout(resolve, 20));
    }
    throw new Error('timed out waiting for ' + label);
  };
  const row = () => document.querySelector('#runs-body > tr[data-run-key="' + runID + '"]');
  const label = () => row()?.querySelector('.badge-label')?.textContent || '';
  const duration = () => row()?.querySelector('[data-cell="duration"] .duration-value')?.textContent || '';
  const projected = () => window.__portalSummaryRuns.find(run => run && run.runId === runID && !run.review);
  const result = {};
  try {
    await wait(() => row() && projected(), 'initial running row');
    result.initialStatus = label();
    result.initialDuration = duration();

    const etagBeforeAwait = window.__portalSummaryETag;
    const awaitResponse = await window.__portalDurationFetch('/__test/await', { method: 'POST' });
    if (!awaitResponse.ok) throw new Error('await control failed: HTTP ' + awaitResponse.status);
    const awaitState = await awaitResponse.json();
    const summaryResponse = await window.__portalDurationFetch('/api/runs?summary=1', {
      headers: etagBeforeAwait ? { 'If-None-Match': etagBeforeAwait } : {},
    });
    result.postAwaitStatus = summaryResponse.status;
    if (summaryResponse.status !== 200) throw new Error('await transition returned stale summary status ' + summaryResponse.status);
    const summaryPayload = await summaryResponse.json();
    window.__portalSummaryRuns = Array.isArray(summaryPayload.runs) ? summaryPayload.runs : [];
    await wait(() => projected()?.waiting === true && projected()?.status === 'reviewing' && projected()?.reviewLive === true
      && label() === 'reviewing', 'fresh waiting projection with linked live review');
    result.waitingStatus = label();
    result.waiting = projected().waiting;
    result.reviewLive = projected().reviewLive;
    result.waitingDuration = duration();

    const before304 = window.__portalSummaryStatuses.filter(status => status === 304).length;
    window.__portalDurationNow = Date.parse(awaitState.browserNow) + 4 * 60 * 60 * 1000;
    await wait(() => window.__portalSummaryStatuses.filter(status => status === 304).length > before304,
      'unchanged summary 304 while waiting');
    result.waiting304Count = window.__portalSummaryStatuses.filter(status => status === 304).length - before304;
    result.after304Duration = duration();

    const resumeResponse = await window.__portalDurationFetch('/__test/resume', { method: 'POST' });
    if (!resumeResponse.ok) throw new Error('resume control failed: HTTP ' + resumeResponse.status);
    const resumeState = await resumeResponse.json();
    window.__portalDurationNow = Date.parse(resumeState.browserNow);
    await wait(() => projected()?.waiting !== true && projected()?.status === 'reviewing' && projected()?.executionSince,
      'fresh resumed projection with live review aggregate');
    await wait(() => /^7m(?:0s)?$/.test(duration()), 'resumed duration to advance from its active baseline');
    result.resumedStatus = label();
    result.resumedWaiting = projected().waiting === true;
    result.resumedExecutionSince = projected().executionSince || '';
    result.resumedDuration = duration();
  } catch (error) {
    result.error = String(error && error.stack || error);
  }
  await window.__portalDurationFetch('/__test/result', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(result),
  });
})();
</script>
`, runID)
	page = strings.Replace(page, "</body>", driver+"</body>", 1)
	if !strings.Contains(page, "__portalDurationFetch") {
		t.Fatal("waiting-duration browser driver was not inserted into the production page")
	}

	results := make(chan portalWaitingDurationBrowserResult, 1)
	serverHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__test/await":
			primed, err := index.SummarySnapshot(r.Context(), "")
			if err != nil {
				http.Error(w, "prime running summary cache: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if len(primed.Runs) != 1 || primed.Runs[0].RunID != runID || primed.Runs[0].Status != "running" {
				http.Error(w, fmt.Sprintf("summary cache was not primed with the running row: %#v", primed.Runs), http.StatusConflict)
				return
			}
			if err := eventLog.Log(events.Event{Type: "run.await", Timestamp: awaitAt, RunID: runID, Issue: 2805}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := eventLog.Log(events.Event{Type: "run.started", Timestamp: reviewStartedAt, RunID: reviewID, Issue: 2805, Payload: map[string]any{
				"review": true, "pr_number": 2805, "issue_number": 2805, "batch_id": "batch-review-2805",
			}}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			setPortalNow(awaitAt.Add(3 * time.Hour))
			waiting, err := index.SummarySnapshot(r.Context(), primed.ETag)
			if err != nil {
				http.Error(w, "refresh waiting summary: "+err.Error(), http.StatusInternalServerError)
				return
			}
			var waitingParent *portalRun
			for i := range waiting.Runs {
				if waiting.Runs[i].RunID == runID {
					waitingParent = &waiting.Runs[i]
					break
				}
			}
			if waiting.NotModified || waiting.ETag == primed.ETag || waitingParent == nil || !waitingParent.Waiting || !waitingParent.ReviewLive || waitingParent.Status != "reviewing" {
				http.Error(w, fmt.Sprintf("await event did not produce a fresh waiting summary: %#v", waiting), http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"browserNow": awaitAt.Add(3 * time.Hour).Format(time.RFC3339Nano)})
		case "/__test/resume":
			if err := eventLog.Log(events.Event{Type: "run.continued", Timestamp: resumeAt, RunID: runID, Issue: 2805, Payload: map[string]any{
				"branch": "2805-duration", "batch_id": "batch-2805-resumed", "previous_run_id": runID,
			}}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			setPortalNow(resumeAt.Add(2 * time.Minute))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"browserNow": resumeAt.Add(2 * time.Minute).Format(time.RFC3339Nano)})
		case "/__test/result":
			var result portalWaitingDurationBrowserResult
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&result); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			results <- result
			w.WriteHeader(http.StatusNoContent)
		case "/":
			w.Header().Set("Content-Type", pageResponse.Header().Get("Content-Type"))
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, page)
		default:
			handler.ServeHTTP(w, r)
		}
	})
	server := httptest.NewServer(serverHandler)
	defer server.Close()
	result := runPortalWaitingDurationBrowser(t, chromium, server.URL, results)
	if result.Error != "" {
		t.Fatalf("actual Portal waiting-duration flow: %s", result.Error)
	}
	if result.InitialStatus != "running" || !portalDurationMatches(result.InitialDuration, 5*time.Minute) {
		t.Fatalf("initial row status/duration = %q/%q, want running/5m", result.InitialStatus, result.InitialDuration)
	}
	if result.WaitingStatus != "reviewing" || !result.Waiting || !result.ReviewLive || !portalDurationMatches(result.WaitingDuration, 5*time.Minute) {
		t.Fatalf("waiting live-review projection = %+v, want reviewing + waiting + live review at frozen 5m", result)
	}
	if result.PostAwaitStatus != http.StatusOK || result.Waiting304Count == 0 || !portalDurationsEqual(result.After304Duration, result.WaitingDuration) {
		t.Fatalf("304 poll changed waiting duration: %+v", result)
	}
	if result.ResumedStatus != "reviewing" || result.ResumedWaiting || result.ResumedExecutionSince == "" || !portalDurationMatches(result.ResumedDuration, 7*time.Minute) {
		t.Fatalf("same-RunID resumed live-review projection = %+v, want active 7m from its saved baseline", result)
	}
}

type portalWaitingAbortBrowserResult struct {
	InitialStatus        string `json:"initialStatus"`
	InitialWaiting       bool   `json:"initialWaiting"`
	InitialFinished      bool   `json:"initialFinished"`
	InitialAbortVisible  bool   `json:"initialAbortVisible"`
	AbortResponseStatus  int    `json:"abortResponseStatus"`
	TerminalStatus       string `json:"terminalStatus"`
	TerminalKind         string `json:"terminalKind"`
	TerminalFinished     bool   `json:"terminalFinished"`
	TerminalAbortVisible bool   `json:"terminalAbortVisible"`
	Error                string `json:"error,omitempty"`
}

func TestPortalLogContinuityBrowser_WaitingAbort(t *testing.T) {
	chromium := requirePortalContinuityTool(t, "chromium")
	if !portalAbortSupported() {
		t.Skip("skip waiting abort browser test on unsupported platform")
	}
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const runID = "261013120000-b2797-2797"
	const issueNumber = 2797
	startedAt := time.Now().UTC().Truncate(time.Second)
	awaitAt := startedAt.Add(time.Minute)
	eventLog := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}
	writePortalLog(t, eventLog.Path, []events.Event{
		{
			Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: issueNumber,
			Payload: map[string]any{"branch": "2797-waiting-abort", "batch_id": "batch-2797"},
		},
		{Type: "run.await", Timestamp: awaitAt, RunID: runID, Issue: issueNumber},
	})

	handler := &portalHandler{repoRoot: repoRoot, runsIndex: getPortalRunsIndex(repoRoot)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/instances", handler.handleInstances)
	mux.HandleFunc("/api/runs", handler.handleRuns)
	mux.HandleFunc("/api/runs/stream", handler.handleRunStream)
	mux.HandleFunc("/api/runs/abort", handler.handleRunAbort)
	mux.HandleFunc("/api/runs/archive", handler.handleRunArchive)
	mux.HandleFunc("/api/logs", handler.handleLogs)
	mux.HandleFunc("/", handler.handlePage)
	handler.handler = mux

	previousAborter := portalRunAborter
	t.Cleanup(func() { portalRunAborter = previousAborter })
	var abortMu sync.Mutex
	var abortRunKey string
	var abortIssue int
	portalRunAborter = func(_ context.Context, _ string, runKey string, issue int) error {
		abortMu.Lock()
		abortRunKey = runKey
		abortIssue = issue
		abortMu.Unlock()
		return eventLog.Log(events.Event{
			Type: "run.aborted", Timestamp: time.Now().UTC(), RunID: runID, Issue: issue,
			Payload: map[string]any{"status": "aborted", "branch": "2797-waiting-abort", "batch_id": "batch-2797"},
		})
	}

	pageResponse := httptest.NewRecorder()
	handler.ServeHTTP(pageResponse, httptest.NewRequest(http.MethodGet, "/", nil))
	page := pageResponse.Body.String()
	mainScript := "<script>\n    const apiPath = \"\\/api\\/runs\";"
	if !strings.Contains(page, mainScript) {
		t.Fatal("embedded Portal page is missing its production application script marker")
	}
	prelude := `<script>
  window.confirm = function () { return true; };
  window.__portalAbortSummaryRuns = [];
  window.__portalAbortResponseStatus = 0;
  const portalAbortFetch = window.fetch.bind(window);
  window.fetch = async function (input, init) {
    const response = await portalAbortFetch(input, init);
    const path = String(input);
    if (path.indexOf('/api/runs/abort') >= 0) window.__portalAbortResponseStatus = response.status;
    if (path.indexOf('summary=1') >= 0 && response.status === 200) {
      const payload = await response.clone().json();
      window.__portalAbortSummaryRuns = Array.isArray(payload.runs) ? payload.runs : [];
    }
    return response;
  };
</script>
` + mainScript
	page = strings.Replace(page, mainScript, prelude, 1)
	driver := fmt.Sprintf(`<script>
(async function () {
  const runID = %q;
  const wait = async function (predicate, label) {
    const deadline = performance.now() + 20000;
    while (performance.now() < deadline) {
      if (predicate()) return;
      await new Promise(resolve => setTimeout(resolve, 20));
    }
    throw new Error('timed out waiting for ' + label);
  };
  const row = () => document.querySelector('#runs-body > tr[data-run-key="' + runID + '"]');
  const projected = () => window.__portalAbortSummaryRuns.find(run => run && run.runId === runID);
  const result = {};
  try {
    await wait(() => row() && projected(), 'initial waiting row');
    const initial = projected();
    result.initialStatus = row()?.querySelector('.badge-label')?.textContent || '';
    result.initialWaiting = initial.waiting === true;
    result.initialFinished = initial.finishedAt != null;
    result.initialAbortVisible = !!row()?.querySelector('button[data-action="abort-run"]');
    const button = row()?.querySelector('button[data-action="abort-run"]');
    if (!button) throw new Error('waiting row has no Abort button');
    button.click();
    await wait(() => {
      const terminal = projected();
      return terminal?.status === 'aborted' && terminal?.kind === 'completed'
        && terminal?.finishedAt != null && !row()?.querySelector('button[data-action="abort-run"]');
    }, 'terminal aborted row');
    const terminal = projected();
    result.abortResponseStatus = window.__portalAbortResponseStatus;
    result.terminalStatus = terminal.status;
    result.terminalKind = terminal.kind;
    result.terminalFinished = terminal.finishedAt != null;
    result.terminalAbortVisible = !!row()?.querySelector('button[data-action="abort-run"]');
  } catch (error) {
    result.error = String(error && error.stack || error);
  }
  await fetch('/__test/result', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(result),
  });
})();
</script>
`, runID)
	page = strings.Replace(page, "</body>", driver+"</body>", 1)

	results := make(chan portalWaitingAbortBrowserResult, 1)
	serverHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__test/result" {
			var result portalWaitingAbortBrowserResult
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&result); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			results <- result
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", pageResponse.Header().Get("Content-Type"))
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, page)
			return
		}
		handler.ServeHTTP(w, r)
	})
	server := httptest.NewServer(serverHandler)
	defer server.Close()
	result := runPortalWaitingAbortBrowser(t, chromium, server.URL, results)
	if result.Error != "" {
		t.Fatalf("actual Portal waiting-abort flow: %s", result.Error)
	}
	if result.InitialStatus != "waiting" || !result.InitialWaiting || result.InitialFinished || !result.InitialAbortVisible {
		t.Fatalf("initial waiting projection = %+v, want waiting lifecycle with Abort", result)
	}
	if result.AbortResponseStatus != http.StatusOK || result.TerminalStatus != "aborted" || result.TerminalKind != "completed" || !result.TerminalFinished || result.TerminalAbortVisible {
		t.Fatalf("terminal waiting-abort projection = %+v", result)
	}
	abortMu.Lock()
	gotRunKey, gotIssue := abortRunKey, abortIssue
	abortMu.Unlock()
	if gotRunKey != runID || gotIssue != issueNumber {
		t.Fatalf("abort request = runKey %q issue %d, want %q issue %d", gotRunKey, gotIssue, runID, issueNumber)
	}
}

func runPortalWaitingAbortBrowser(t *testing.T, chromium, url string, results <-chan portalWaitingAbortBrowserResult) portalWaitingAbortBrowserResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chromium,
		"--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--no-first-run", "--no-default-browser-check", "--disable-background-timer-throttling",
		"--disable-renderer-backgrounding", "--window-size=1360,900", url)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(exited)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()
	select {
	case result := <-results:
		return result
	case <-exited:
		t.Fatalf("Chromium exited before waiting-abort assertions: %v\n%s", exitErr, output.String())
		return portalWaitingAbortBrowserResult{}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("actual-page waiting-abort browser timed out: %v\n%s", ctx.Err(), output.String())
		return portalWaitingAbortBrowserResult{}
	}
}

func portalDurationMatches(value string, want time.Duration) bool {
	return strings.TrimSpace(value) == want.String() || strings.TrimSpace(value) == strings.TrimSuffix(want.String(), "0s")
}

func portalDurationsEqual(left, right string) bool {
	leftDuration, leftErr := time.ParseDuration(strings.TrimSpace(left))
	rightDuration, rightErr := time.ParseDuration(strings.TrimSpace(right))
	return leftErr == nil && rightErr == nil && leftDuration == rightDuration
}

func runPortalWaitingDurationBrowser(t *testing.T, chromium, url string, results <-chan portalWaitingDurationBrowserResult) portalWaitingDurationBrowserResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chromium,
		"--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--no-first-run", "--no-default-browser-check", "--disable-background-timer-throttling",
		"--disable-renderer-backgrounding", "--window-size=1360,900",
		"--user-data-dir="+t.TempDir(), url)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(exited)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()
	select {
	case result := <-results:
		return result
	case <-exited:
		t.Fatalf("Chromium exited before waiting-duration assertions: %v\n%s", exitErr, output.String())
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("actual-page waiting-duration browser timed out: %v\n%s", ctx.Err(), output.String())
	}
	return portalWaitingDurationBrowserResult{}
}

func requirePortalContinuityTool(t *testing.T, name string) string {
	t.Helper()
	if name == "chromium" {
		if configured := strings.TrimSpace(os.Getenv("SANDMAN_PORTAL_CHROMIUM")); configured != "" {
			path, err := exec.LookPath(configured)
			if err != nil {
				t.Fatalf("SANDMAN_PORTAL_CHROMIUM is not executable: %v", err)
			}
			return path
		}
	}
	path, err := exec.LookPath(name)
	if err == nil {
		return path
	}
	if os.Getenv("SANDMAN_REQUIRE_PORTAL_BROWSER") == "1" {
		t.Fatalf("SANDMAN_REQUIRE_PORTAL_BROWSER=1 requires %s on PATH: %v", name, err)
	}
	t.Skipf("%s not on PATH; set SANDMAN_REQUIRE_PORTAL_BROWSER=1 to require continuity browser coverage", name)
	return ""
}

type portalContinuityBrowserResult struct {
	InitialLog          string    `json:"initialLog"`
	InitialStart        int64     `json:"initialStart"`
	InitialEnd          int64     `json:"initialEnd"`
	InitialGeneration   string    `json:"initialGeneration"`
	InitialCursor       string    `json:"initialCursor"`
	VisibleLog          string    `json:"visibleLog"`
	FinalStart          int64     `json:"finalStart"`
	FinalEnd            int64     `json:"finalEnd"`
	FinalGeneration     string    `json:"finalGeneration"`
	FinalCursor         string    `json:"finalCursor"`
	ReloadGeneration    string    `json:"reloadGeneration"`
	StressTransitions   int       `json:"stressTransitions"`
	MaxWarmTransitionMs float64   `json:"maxWarmTransitionMs"`
	WarmTransitionMs    []float64 `json:"warmTransitionMs"`
	LiveUpdateMs        float64   `json:"liveUpdateMs"`
	ModelCacheBytes     int       `json:"modelCacheBytes"`
	ModelRuns           int       `json:"modelRuns"`
	DOMLogNodes         int       `json:"domLogNodes"`
	ReloadVerified      bool      `json:"reloadVerified"`
	RestartLoaded       bool      `json:"restartLoaded"`
	Ended               bool      `json:"ended"`
	Error               string    `json:"error"`
}

type portalContinuityStreamStatus struct {
	Connections        int      `json:"connections"`
	Closed             int      `json:"closed"`
	ResumeCursors      []string `json:"resumeCursors"`
	LastEventIDs       []string `json:"lastEventIds"`
	SummaryNotModified int      `json:"summaryNotModified"`
	ForcedCloses       int      `json:"forcedCloses"`
	Error              string   `json:"error,omitempty"`
}

type portalLogContinuityFixture struct {
	repoRoot             string
	run                  portalRun
	reviewRun            *portalRun
	tab                  string
	driver               []byte
	initialEnd           int64
	snapshotStart        int64
	liveText             string
	writer               *os.File
	reviewWriter         *os.File
	eventLog             *events.JSONLLogger
	finishOnComplete     bool
	reloadOnce           bool
	resumeOnly           bool
	forceNativeReconnect bool
	results              chan portalContinuityBrowserResult
	mu                   sync.Mutex
	streamStatus         portalContinuityStreamStatus
	closeNext            int
}

func newPortalLogContinuityFixture(t *testing.T, tab string, driver []byte) *portalLogContinuityFixture {
	t.Helper()
	const runID = "261007090000-b277-2772"
	const recordBytes = 8 * 1024
	f := &portalLogContinuityFixture{
		repoRoot: t.TempDir(), tab: tab, driver: driver,
		results:              make(chan portalContinuityBrowserResult, 1),
		liveText:             "10:16:37 new live output\n10:16:38 second live output\n",
		forceNativeReconnect: true,
	}
	runDir := filepath.Join(f.repoRoot, ".sandman", "batches", "261007090000-b277", "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.run = portalRun{
		Key: runID, RunID: runID, Kind: "active", Status: "running", IssueNumber: 2772, IssueLabel: "#2772",
		BatchKey: "261007090000-b277", StartedAt: time.Now(), SourceExists: true,
		LogPath: filepath.Join(runDir, "run.log"), SocketPath: filepath.Join(runDir, "run.sock"),
		Events: []portalEvent{{Type: "run.started", Timestamp: time.Now()}},
	}
	var source strings.Builder
	for i := 0; i < 40; i++ {
		var label string
		switch {
		case i < 8:
			label = fmt.Sprintf("08:00:%02d outside replay %02d", i, i)
		case i == 8:
			label = "09:25:07 old command"
		case i == 9:
			label = "09:27:15 old output"
		case i < 32:
			label = fmt.Sprintf("09:28:%02d old output %02d", i, i)
		case i == 32:
			label = "10:09:32 current command"
		case i < 39:
			label = fmt.Sprintf("10:10:%02d current progress %02d", i, i)
		default:
			label = "10:16:22 current output"
		}
		// Fixed-size complete records make both production history limits
		// record-aligned. Generated padding crosses the renderer's 16 KiB
		// threshold without a large literal or thousands of SSE messages.
		prefix := label + " | "
		source.WriteString(prefix + strings.Repeat(".", recordBytes-len(prefix)-1) + "\n")
	}
	writer, err := os.OpenFile(f.run.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.writer = writer
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := io.WriteString(writer, source.String()); err != nil {
		t.Fatal(err)
	}
	f.initialEnd = int64(source.Len())
	if f.initialEnd <= portalLogRetainedBytes {
		t.Fatal("saved-log fixture must exceed the production 256 KiB retained-range limit")
	}
	f.snapshotStart = f.initialEnd - portalLogRetainedBytes
	f.run.Log = f.readRange(t, f.snapshotStart, f.initialEnd)
	f.eventLog = &events.JSONLLogger{Path: filepath.Join(f.repoRoot, ".sandman", "events.jsonl")}
	if err := f.eventLog.Log(events.Event{Type: "run.started", Timestamp: time.Now(), RunID: runID, Issue: 2772, Payload: map[string]any{"batch_id": f.run.BatchKey, "branch": "2772-portal-log"}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *portalLogContinuityFixture) readRange(t *testing.T, start, end int64) string {
	t.Helper()
	data, err := f.rawRange(start, end)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *portalLogContinuityFixture) rawRange(start, end int64) ([]byte, error) {
	return f.rawRangeFor(f.run.RunID, start, end)
}

func (f *portalLogContinuityFixture) rawRangeFor(runID string, start, end int64) ([]byte, error) {
	path := f.run.LogPath
	if f.reviewRun != nil && runID == f.reviewRun.RunID {
		path = f.reviewRun.LogPath
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.NewSectionReader(file, start, end-start))
	if err == nil && int64(len(data)) != end-start {
		err = fmt.Errorf("saved log range [%d,%d) returned %d bytes", start, end, len(data))
	}
	return data, err
}

func (f *portalLogContinuityFixture) status() portalContinuityStreamStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamStatus
}

func (f *portalLogContinuityFixture) handler(stream http.Handler) http.Handler {
	mux := http.NewServeMux()
	// Render through the production template/page-data builder so later
	// embedded frontend modules are automatically exercised by this test.
	page := &portalHandler{repoRoot: f.repoRoot}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		page.handlePage(recorder, r)
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		// Only add a test driver; production scripts and handlers are intact.
		_, _ = io.WriteString(w, strings.Replace(recorder.Body.String(), "</body>", `<script src="/__test/driver.js"></script></body>`, 1))
	})
	mux.HandleFunc("/api/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Query().Get("summary") == "1" {
			w.Header().Set("ETag", `"continuity-fixture"`)
			if r.Header.Get("If-None-Match") == `"continuity-fixture"` {
				f.mu.Lock()
				f.streamStatus.SummaryNotModified++
				f.mu.Unlock()
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		if r.URL.Query().Get("runKey") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"run": f.runByKey(r.URL.Query().Get("runKey"))})
		} else {
			// Preload the bounded suffix in metadata, as in the baseline report.
			runs := []portalRun{f.run}
			if f.reviewRun != nil {
				runs = append(runs, *f.reviewRun)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs})
		}
	})
	mux.Handle("/api/runs/stream", f.trackStream(stream))
	f.addTestRoutes(mux)
	return mux
}

func (f *portalLogContinuityFixture) addTestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/__test/driver.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(f.driver)
	})
	mux.HandleFunc("/__test/config", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		finishOnComplete, reloadOnce, resumeOnly := f.finishOnComplete, f.reloadOnce, f.resumeOnly
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runKey": f.run.Key, "tab": f.tab, "lastLiveLine": "10:16:38 second live output",
			"finishOnComplete": finishOnComplete, "reloadOnce": reloadOnce, "resumeOnly": resumeOnly,
			"skipNativeReconnect": !f.forceNativeReconnect,
			"reviewKey": func() string {
				if f.reviewRun != nil {
					return f.reviewRun.Key
				}
				return ""
			}(),
		})
	})
	mux.HandleFunc("/__test/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.status())
	})
	mux.HandleFunc("/__test/close-next", func(w http.ResponseWriter, r *http.Request) {
		count, err := strconv.Atoi(r.URL.Query().Get("count"))
		if err != nil || count < 1 {
			http.Error(w, "invalid close count", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.closeNext = count
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/source", func(w http.ResponseWriter, r *http.Request) {
		start, startErr := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
		end, endErr := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
		if startErr != nil || endErr != nil || start < 0 || end < start {
			http.Error(w, "invalid source range", http.StatusBadRequest)
			return
		}
		runID := r.URL.Query().Get("runId")
		if runID == "" {
			runID = f.run.RunID
		}
		text, err := f.rawRangeFor(runID, start, end)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(text)
	})
	mux.HandleFunc("/__test/append", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		writer := f.writer
		if f.reviewRun != nil && r.URL.Query().Get("runKey") == f.reviewRun.Key {
			writer = f.reviewWriter
		}
		_, err := io.WriteString(writer, f.liveText)
		if err != nil {
			f.streamStatus.Error = err.Error()
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/finish", func(w http.ResponseWriter, r *http.Request) {
		if err := f.eventLog.Log(events.Event{Type: "run.finished", Timestamp: time.Now(), RunID: f.run.RunID, Issue: f.run.IssueNumber, Payload: map[string]any{"status": "success"}}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/result", func(w http.ResponseWriter, r *http.Request) {
		var result portalContinuityBrowserResult
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&result); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.results <- result
		w.WriteHeader(http.StatusNoContent)
	})
}

// binaryHandler proxies the real built CLI server through a small test sidecar
// that supplies deterministic controls and a Saved Run Log byte oracle. The
// Portal page, run APIs and SSE endpoint all remain served by the executable.
func (f *portalLogContinuityFixture) binaryHandler(upstream string) http.Handler {
	target, _ := url.Parse(upstream)
	proxy := httputil.NewSingleHostReverseProxy(target)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream+"/", nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for key, values := range response.Header {
			if strings.EqualFold(key, "Content-Length") {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		page := strings.Replace(string(body), "</body>", `<script src="/__test/driver.js"></script></body>`, 1)
		_, _ = io.WriteString(w, page)
	})
	mux.Handle("/api/runs/stream", f.trackStream(proxy))
	mux.Handle("/api/", f.trackNotModified(proxy))
	f.addTestRoutes(mux)
	return mux
}

func (f *portalLogContinuityFixture) trackNotModified(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&portalContinuityStatusWriter{ResponseWriter: w, fixture: f}, r)
	})
}

type portalContinuityStatusWriter struct {
	http.ResponseWriter
	fixture *portalLogContinuityFixture
	wrote   bool
}

func (w *portalContinuityStatusWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	if status == http.StatusNotModified {
		w.fixture.mu.Lock()
		w.fixture.streamStatus.SummaryNotModified++
		w.fixture.mu.Unlock()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *portalContinuityStatusWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *portalContinuityStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (f *portalLogContinuityFixture) trackStream(stream http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.streamStatus.Connections++
		f.streamStatus.ResumeCursors = append(f.streamStatus.ResumeCursors, r.URL.Query().Get("cursor"))
		closeStream := f.closeNext > 0
		if closeStream {
			f.closeNext--
			f.streamStatus.ForcedCloses++
		}
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.streamStatus.Closed++
			f.mu.Unlock()
		}()
		f.mu.Lock()
		dropSnapshot := f.forceNativeReconnect && f.streamStatus.Connections == 1
		f.streamStatus.LastEventIDs = append(f.streamStatus.LastEventIDs, r.Header.Get("Last-Event-ID"))
		f.mu.Unlock()
		if closeStream {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if dropSnapshot {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			stream.ServeHTTP(&portalContinuityDropWriter{ResponseWriter: w, cancel: cancel}, r.WithContext(ctx))
			return
		}
		stream.ServeHTTP(w, r)
	})
}

// portalContinuityDropWriter forces a real HTTP disconnect immediately after
// the first snapshot has been flushed. Chromium's native EventSource must
// reconnect with the received SSE id rather than a test-injected callback.
type portalContinuityDropWriter struct {
	http.ResponseWriter
	cancel  context.CancelFunc
	dropped bool
}

func (w *portalContinuityDropWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if !w.dropped && bytes.Contains(p, []byte(`"type":"snapshot"`)) {
		w.dropped = true
		_ = http.NewResponseController(w.ResponseWriter).Flush()
		w.cancel()
	}
	return n, err
}

func (w *portalContinuityDropWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *portalContinuityDropWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (f *portalLogContinuityFixture) savedLogStream(registry *portalLogRegistry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		run := f.runByKey(r.URL.Query().Get("runKey"))
		if run.RunID == "" {
			http.Error(w, "unknown fixture run", http.StatusNotFound)
			return
		}
		servePortalLogStream(w, r, run, portalLogStreamOptions{
			Scope:    f.repoRoot,
			Registry: registry,
			Terminal: func(context.Context) (bool, error) { return false, nil },
		})
	})
}

func (f *portalLogContinuityFixture) runByKey(key string) portalRun {
	if f.reviewRun != nil && (key == f.reviewRun.Key || key == f.reviewRun.RunID) {
		return *f.reviewRun
	}
	if key == f.run.Key || key == f.run.RunID {
		return f.run
	}
	return portalRun{}
}

func runPortalContinuityBrowser(t *testing.T, chromium, url string, results <-chan portalContinuityBrowserResult) portalContinuityBrowserResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chromium,
		"--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--no-first-run", "--no-default-browser-check", "--disable-background-timer-throttling",
		"--disable-renderer-backgrounding", "--window-size=1360,900",
		"--user-data-dir="+t.TempDir(), url)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(exited)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()
	select {
	case result := <-results:
		return result
	case <-exited:
		t.Fatalf("chromium exited before browser assertions: %v\n%s", exitErr, output.String())
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-exited
		t.Fatalf("actual-page continuity browser timed out: %v\n%s", ctx.Err(), output.String())
	}
	return portalContinuityBrowserResult{}
}

func portalContinuityDifference(got, want string) string {
	common := 0
	for common < len(got) && common < len(want) && got[common] == want[common] {
		common++
	}
	return fmt.Sprintf("first differing byte=%d, visible bytes=%d, source-range bytes=%d", common, len(got), len(want))
}

func portalContinuityLabels(text string) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, line := range lines {
		lines[i], _, _ = strings.Cut(line, " | ")
	}
	return fmt.Sprintf("%q", lines)
}
