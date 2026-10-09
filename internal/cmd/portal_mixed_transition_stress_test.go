package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type portalStressSource struct {
	mu          sync.Mutex
	runID       string
	generation  string
	offset      int64
	records     []portalLogRecord
	connections int
}

func newPortalStressSource(runID string, seed ...string) *portalStressSource {
	source := &portalStressSource{runID: runID, generation: "stress-" + runID}
	for _, text := range seed {
		source.appendLocked(text)
	}
	return source
}

func (s *portalStressSource) appendLocked(text string) portalLogRecord {
	record := portalLogRecord{
		RunID:      s.runID,
		Generation: s.generation,
		Start:      s.offset,
		End:        s.offset + int64(len(text)) + 1,
		Text:       text,
	}
	s.records = append(s.records, record)
	s.offset = record.End
	return record
}

func (s *portalStressSource) connect() (portalLogBatch, portalLogBatch, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connections++
	connection := s.connections
	snapshotRecords := append([]portalLogRecord(nil), s.records...)
	snapshot := portalLogBatch{
		RunID: s.runID, Generation: s.generation, Start: 0, End: s.offset,
		Records: snapshotRecords,
		Cursor:  portalLogCursor{RunID: s.runID, Generation: s.generation, Offset: s.offset},
	}
	record := s.appendLocked(fmt.Sprintf("%s-source-%02d", s.runID, connection))
	appendBatch := portalLogBatch{
		RunID: s.runID, Generation: s.generation, Start: record.Start, End: record.End,
		Records: []portalLogRecord{record},
		Cursor:  portalLogCursor{RunID: s.runID, Generation: s.generation, Offset: record.End},
	}
	return snapshot, appendBatch, connection
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
		implRun:   newPortalStressSource(implRun, "impl-seed-1", "same", ""),
		reviewRun: newPortalStressSource(reviewRun, "review-seed-1", "same", ""),
		secondRun: newPortalStressSource(secondRun, "second-seed-1", "same", ""),
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
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("stress SSE writer does not flush")
			return
		}
		snapshot, appendBatch, connection := source.connect()
		_, _ = fmt.Fprint(w, "retry: 50\n\n")
		if err := writePortalLogEvent(w, "snapshot", snapshot, encodePortalLogCursor(snapshot.Cursor)); err != nil {
			t.Errorf("write stress snapshot: %v", err)
			return
		}
		if err := writePortalLogEvent(w, "append", appendBatch, encodePortalLogCursor(appendBatch.Cursor)); err != nil {
			t.Errorf("write stress append: %v", err)
			return
		}
		flusher.Flush()
		// Closing every first connection forces the browser's native
		// EventSource reconnect; the replacement connection remains open until
		// the page changes subject or the test server shuts down.
		if connection%2 == 1 {
			return
		}
		<-r.Context().Done()
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
    function stressCollect(stage) {
      var values = ['`+implRun+`', '`+reviewRun+`', '`+secondRun+`'];
      if (stage < values.length) {
        stressEnsureRun(stage === 2 ? '`+secondRun+`' : '`+implRun+`');
        stressOpenSubject(values[stage], function () {
          var pre = document.querySelector('pre[data-scroll-key]');
          if (!pre) throw new Error('missing rendered stress pane for ' + values[stage]);
          stressRendered[values[stage]] = pre.getAttribute('data-rendered-log') || '';
          stressCollect(stage + 1);
        });
        return;
      }
      var marker = document.createElement('pre');
      marker.id = 'portal-mixed-transition-stress';
      marker.textContent = JSON.stringify({oracle: window.__stressOracle, rendered: stressRendered, actions: window.__stressActionCounts});
      document.body.appendChild(marker);
    }
    setTimeout(stressRunActions, 120);
  `)
	page = strings.Replace(page, `const streamPath = "/api/runs/stream";`, `const streamPath = "`+server.URL+`/api/runs/stream";`, 1)

	dom, _ := runPortalChromium(t, page)
	payload := extractPortalMarker(t, dom, "portal-mixed-transition-stress")
	var result struct {
		Oracle   map[string][]portalLogRecord `json:"oracle"`
		Rendered map[string]string            `json:"rendered"`
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
			t.Fatalf("browser source oracle for %s = %q, want HTTP source range %q", runID, got, want)
		}
		if got := result.Rendered[runID]; got != want {
			t.Fatalf("DOM log for %s = %q, want source range %q", runID, got, want)
		}
	}
}
