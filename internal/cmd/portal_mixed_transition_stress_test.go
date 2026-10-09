package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type portalStressSource struct {
	mu          sync.Mutex
	runID       string
	path        string
	records     []portalLogRecord
	connections int
	requests    int
	headers     []string
	queries     []string
	initialID   string
}

func newPortalStressSource(t *testing.T, runID string) *portalStressSource {
	t.Helper()
	path := filepath.Join(t.TempDir(), runID+".log")
	var content strings.Builder
	for i := 0; i < 8000; i++ {
		fmt.Fprintf(&content, "[%s] stress-record-%05d repeated-source-content-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", runID, i)
	}
	for _, text := range []string{"10:09:32 current command", "10:16:22 current output", "same displayed text", "same displayed text", ""} {
		fmt.Fprintf(&content, "[%s] %s\n", runID, text)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
		t.Fatalf("write stress saved log: %v", err)
	}
	reader, err := newPortalLogSource(path, runID)
	if err != nil {
		t.Fatalf("open stress saved log: %v", err)
	}
	batch, err := reader.snapshot()
	_ = reader.Close()
	if err != nil {
		t.Fatalf("snapshot stress saved log: %v", err)
	}
	if !batch.Bounded || batch.End <= portalLogSnapshotLimit {
		t.Fatalf("stress fixture snapshot = bounded %v, end %d; want a bounded log above %d bytes", batch.Bounded, batch.End, portalLogSnapshotLimit)
	}
	return &portalStressSource{runID: runID, path: path, records: append([]portalLogRecord(nil), batch.Records...)}
}

func (s *portalStressSource) firstSnapshot() (portalLogBatch, error) {
	reader, err := newPortalLogSource(s.path, s.runID)
	if err != nil {
		return portalLogBatch{}, err
	}
	defer reader.Close()
	return reader.snapshot()
}

func (s *portalStressSource) appendFixtureRecord() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reader, err := newPortalLogSource(s.path, s.runID)
	if err != nil {
		return err
	}
	defer reader.Close()
	info, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("%s-source-%02d", s.runID, s.requests)
	raw := []byte("[" + s.runID + "] " + text + "\n")
	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	s.records = append(s.records, portalLogRecord{
		RunID: s.runID, Generation: reader.gen, Start: info.Size(), End: info.Size() + int64(len(raw)), Text: text,
	})
	return nil
}

func (s *portalStressSource) recordRequest(header, query string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	s.connections++
	s.headers = append(s.headers, header)
	s.queries = append(s.queries, query)
	return s.requests
}

func (s *portalStressSource) expected() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return portalStressRecordsText(s.records)
}

func portalStressRecordsText(records []portalLogRecord) string {
	var result strings.Builder
	for _, record := range records {
		result.WriteString(record.Text)
		result.WriteByte('\n')
	}
	return result.String()
}

func servePortalStressStream(w http.ResponseWriter, r *http.Request, source *portalStressSource) error {
	request := source.recordRequest(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("cursor"))
	if request == 1 {
		batch, err := source.firstSnapshot()
		if err != nil {
			return err
		}
		source.mu.Lock()
		source.records = append([]portalLogRecord(nil), batch.Records...)
		source.initialID = encodePortalLogCursor(batch.Cursor)
		source.mu.Unlock()
		if err := writePortalLogEvent(w, "snapshot", batch, encodePortalLogCursor(batch.Cursor)); err != nil {
			return err
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Deliberately close before end. The next request must resume through
		// the production saved-log stream decision.
		return nil
	}
	if request == 2 {
		if err := source.appendFixtureRecord(); err != nil {
			return err
		}
	}
	encoded := r.Header.Get("Last-Event-ID")
	if encoded == "" {
		encoded = r.URL.Query().Get("cursor")
	}
	initial, err := decodePortalLogCursor(encoded)
	if err != nil {
		return fmt.Errorf("decode cursor request=%d header=%q query=%q: %w", request, r.Header.Get("Last-Event-ID"), r.URL.Query().Get("cursor"), err)
	}
	reader, err := newPortalLogSource(source.path, source.runID)
	if err != nil {
		return err
	}
	return streamPortalSavedLog(r.Context(), w, reader, &initial, func() bool { return true })
}

func TestPortalStressStream_ResumesThroughProductionSource(t *testing.T) {
	source := newPortalStressSource(t, "261009160000-source-2772")
	firstRequest := httptest.NewRequest(http.MethodGet, "/api/runs/stream?cursor=invalid-original-cursor", nil)
	firstResponse := httptest.NewRecorder()
	if err := servePortalStressStream(firstResponse, firstRequest, source); err != nil {
		t.Fatalf("first stress stream: %v", err)
	}
	source.mu.Lock()
	initialID := source.initialID
	source.mu.Unlock()
	secondRequest := httptest.NewRequest(http.MethodGet, "/api/runs/stream?cursor=invalid-original-cursor", nil)
	secondRequest.Header.Set("Last-Event-ID", initialID)
	secondResponse := httptest.NewRecorder()
	if err := servePortalStressStream(secondResponse, secondRequest, source); err != nil {
		t.Fatalf("resumed stress stream: %v", err)
	}
	if strings.Contains(secondResponse.Body.String(), "event: snapshot") {
		t.Fatal("valid Last-Event-ID caused an unnecessary replacement snapshot")
	}
	if !strings.Contains(secondResponse.Body.String(), "event: append") || !strings.Contains(secondResponse.Body.String(), "event: end") {
		t.Fatalf("resumed stress stream = %q, want append and end", secondResponse.Body.String())
	}
}

// TestPortalMixedTransitionStress drives the production Portal page through
// tab, subject, row, and native reconnect transitions while a real HTTP SSE
// source appends position-identified records. The browser wrapper records the
// structured source oracle, and the final DOM projections are compared with
// that oracle for both grouped subjects and the second row.
func TestPortalMixedTransitionStress(t *testing.T) {
	const implRun = "261009160000-impl-2772"
	const reviewRun = "261009160000-review-2772"
	const secondRun = "261009160000-second-2772"
	sources := map[string]*portalStressSource{
		implRun:   newPortalStressSource(t, implRun),
		reviewRun: newPortalStressSource(t, reviewRun),
		secondRun: newPortalStressSource(t, secondRun),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runID := r.URL.Query().Get("runKey")
		source := sources[runID]
		if source == nil {
			http.Error(w, "unknown run", http.StatusNotFound)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "keep-alive")
		_, _ = fmt.Fprint(w, "retry: 50\n\n")
		if err := servePortalStressStream(w, r, source); err != nil {
			t.Errorf("serve production stress stream: %v", err)
		}
	}))
	defer server.Close()

	runs := []map[string]any{
		{"key": implRun, "runId": implRun, "kind": "active", "status": "running", "issueLabel": "#2772", "issueNumber": 2772, "batchKey": implRun, "logPath": "stress.log"},
		{"key": reviewRun, "runId": reviewRun, "kind": "active", "status": "reviewing", "issueLabel": "PR42", "issueNumber": 2772, "prNumber": 42, "review": true, "batchKey": reviewRun, "logPath": "stress.log"},
		{"key": secondRun, "runId": secondRun, "kind": "active", "status": "running", "issueLabel": "#2773", "issueNumber": 2773, "batchKey": secondRun, "logPath": "stress.log"},
	}
	runsJSON, err := json.Marshal(runs)
	if err != nil {
		t.Fatalf("marshal stress runs: %v", err)
	}
	stateJSON := `{"expandedRunKey":"` + implRun + `","tabs":{"` + implRun + `":"log"},"commandFormCollapsed":false,"showArchived":false,"activeBatches":false,"sortBy":"started","sortDir":"desc"}`
	page := buildPortalReproPage(t, stateJSON, runsJSON, `
    window.__stressOracle = {};
    window.__stressActionCounts = {events: 0, details: 0, subjects: 0, rows: 0, reconnects: 0};
    function stressRecord(runID, type, data) {
      var payload = JSON.parse(data || '{}');
      var records = window.__stressOracle[runID] || [];
      if (type === 'snapshot') records = payload.records || [];
      if (type === 'append') {
        (payload.records || []).forEach(function (record) {
          var identity = String(record.generation) + ':' + String(record.start) + ':' + String(record.end);
          if (!records.some(function (existing) {
            return String(existing.generation) + ':' + String(existing.start) + ':' + String(existing.end) === identity;
          })) records.push(record);
        });
      }
      window.__stressOracle[runID] = records;
    }
    var __stressNativeEventSource = window.EventSource;
    window.EventSource = function (url, options) {
      var source = new __stressNativeEventSource(url, options);
      var runID = new URL(url, window.location.href).searchParams.get('runKey');
      ['snapshot', 'append'].forEach(function (type) {
        source.addEventListener(type, function (event) { stressRecord(runID, type, event.data); });
      });
      window.__stressActionCounts.reconnects += 1;
      return source;
    };
    function stressClickTab(tab) {
      var button = document.querySelector('button[data-action="set-tab"][data-tab="' + tab + '"]');
      if (!button) throw new Error('missing ' + tab + ' tab');
      button.click();
      if (tab === 'events') window.__stressActionCounts.events += 1;
      if (tab === 'details') window.__stressActionCounts.details += 1;
    }
    function stressSelectSubject(value) {
      var select = document.querySelector('select[data-action="set-subject"]');
      if (!select) throw new Error('missing subject selector');
      select.value = value;
      select.dispatchEvent(new Event('change', { bubbles: true }));
      window.__stressActionCounts.subjects += 1;
    }
    function stressClickRow(runID) {
      var row = document.querySelector('tr[data-run-key="' + runID + '"]');
      if (!row) throw new Error('missing row ' + runID);
      row.click();
      window.__stressActionCounts.rows += 1;
    }
    function stressRunActions() {
      var actions = [
        function () { stressClickTab('events'); },
        function () { stressClickTab('log'); },
        function () { stressClickTab('details'); },
        function () { stressClickTab('log'); },
        function () { stressSelectSubject('`+reviewRun+`'); },
        function () { stressSelectSubject('`+implRun+`'); },
        function () { stressClickRow('`+secondRun+`'); },
        function () { stressClickRow('`+implRun+`'); },
        function () { stressClickTab('events'); },
        function () { stressClickTab('log'); }
      ];
      var index = 0;
      function next() {
        if (index === 50) return stressCollect(0);
        actions[index % actions.length]();
        index += 1;
        setTimeout(next, 30);
      }
      next();
    }
    function stressOpenSubject(value, done) {
      var select = document.querySelector('select[data-action="set-subject"]');
      if (select && select.value !== value) stressSelectSubject(value);
      setTimeout(done, 250);
    }
    function stressEnsureRun(runID) {
      if (state.expandedRunKey === runID) return;
      var row = document.querySelector('tr[data-run-key="' + runID + '"]');
      if (!row) throw new Error('missing row while ensuring ' + runID);
      row.click();
    }
    var stressRendered = {};
    var stressModel = {};
    function stressCollect(stage) {
      var values = ['`+implRun+`', '`+reviewRun+`', '`+secondRun+`'];
      if (stage < values.length) {
        stressEnsureRun(stage === 2 ? '`+secondRun+`' : '`+implRun+`');
        stressOpenSubject(values[stage], function () {
          var pre = document.querySelector('pre[data-scroll-key]');
          if (!pre) throw new Error('missing rendered stress pane for ' + values[stage]);
          stressRendered[values[stage]] = pre.textContent || '';
          stressModel[values[stage]] = portalLogModel.text(values[stage]);
          stressCollect(stage + 1);
        });
        return;
      }
      var marker = document.createElement('pre');
      marker.id = 'portal-mixed-transition-stress';
      marker.textContent = JSON.stringify({oracle: window.__stressOracle, rendered: stressRendered, model: stressModel, actions: window.__stressActionCounts});
      document.body.appendChild(marker);
    }
    setTimeout(stressRunActions, 120);
  `)
	page = strings.Replace(page, `const streamPath = "/api/runs/stream";`, `const streamPath = "`+server.URL+`/api/runs/stream?cursor=invalid-original-cursor";`, 1)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-mixed-transition-stress")
	var result struct {
		Oracle   map[string][]portalLogRecord `json:"oracle"`
		Rendered map[string]string            `json:"rendered"`
		Model    map[string]string            `json:"model"`
		Actions  struct {
			Events     int `json:"events"`
			Details    int `json:"details"`
			Subjects   int `json:"subjects"`
			Rows       int `json:"rows"`
			Reconnects int `json:"reconnects"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("parse mixed-transition payload: %v\nraw=%s", err, payload)
	}
	if result.Actions.Events == 0 || result.Actions.Details == 0 || result.Actions.Subjects == 0 || result.Actions.Rows == 0 || result.Actions.Reconnects < 2 {
		t.Fatalf("mixed transition coverage incomplete: %+v", result.Actions)
	}
	for runID, source := range sources {
		want := source.expected()
		if got := portalStressRecordsText(result.Oracle[runID]); got != want {
			source.mu.Lock()
			requests := source.requests
			headers := append([]string(nil), source.headers...)
			queries := append([]string(nil), source.queries...)
			source.mu.Unlock()
			t.Fatalf("browser source oracle for %s = %q, want HTTP source range %q (requests=%d headers=%q queries=%q)", runID, got, want, requests, headers, queries)
		}
		if got := result.Rendered[runID]; got != want {
			t.Fatalf("DOM log for %s = %q, want source range %q", runID, got, want)
		}
		if got := result.Model[runID]; got != want {
			t.Fatalf("accepted model for %s = %q, want source range %q", runID, got, want)
		}
		source.mu.Lock()
		requests := source.requests
		headers := append([]string(nil), source.headers...)
		queries := append([]string(nil), source.queries...)
		source.mu.Unlock()
		if requests < 2 {
			t.Fatalf("HTTP requests for %s = %d, want initial request plus native reconnect", runID, requests)
		}
		source.mu.Lock()
		initialID := source.initialID
		source.mu.Unlock()
		if headers[1] != initialID {
			t.Fatalf("Last-Event-ID for %s = %q, want accepted cursor %q", runID, headers[1], initialID)
		}
		if queries[1] != "invalid-original-cursor" {
			t.Fatalf("reconnect URL cursor for %s = %q, want the deliberately stale original cursor", runID, queries[1])
		}
	}
}
