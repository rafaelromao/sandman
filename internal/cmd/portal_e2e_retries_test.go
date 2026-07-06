//go:build e2e

package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortal_E2E_RetriesContract_RetryRunCarriesCountsAndEvents is the
// end-to-end test that locks in the public contract introduced by slices
// 1-3. It writes a real .sandman/events.jsonl containing a run.started,
// two run.retry events, and a run.finished with retries_total: 3,
// retries_done: 2, starts a real portal HTTP server pointed at the temp
// repository, issues a real GET /api/runs, and asserts the JSON response
// carries the new fields on the matching row plus exactly two run.retry
// entries in the row's events array. The test exercises the same code
// path the live portal uses (newPortalHandler + http server + JSON
// encoder); no orchestrator, event log reader, or portal struct is
// mocked.
func TestPortal_E2E_RetriesContract_RetryRunCarriesCountsAndEvents(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "abcd-260618113825-retry"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(10 * time.Minute)
	retryPayload := map[string]any{
		"attempt":         2,
		"max_attempts":    3,
		"previous_status": "failure",
		"last_log_lines":  []string{"line one", "line two", "line three"},
		"branch":          "sandman/42-fix",
	}

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: startedAt.Add(2 * time.Minute), RunID: runID, Issue: 42, Payload: retryPayload},
		{Type: "run.retry", Timestamp: startedAt.Add(5 * time.Minute), RunID: runID, Issue: 42, Payload: retryPayload},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{
			"status":        "success",
			"branch":        "sandman/42-fix",
			"retries_total": 3,
			"retries_done":  2,
		}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)

	body := readPortalRunsRawBody(t, server.URL)
	if strings.Contains(string(body), "retriesTotal") == false {
		t.Fatalf("expected retriesTotal in response body, got: %s", body)
	}
	if strings.Contains(string(body), "retriesDone") == false {
		t.Fatalf("expected retriesDone in response body, got: %s", body)
	}

	var payload struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode runs: %v", err)
	}

	var row map[string]any
	for _, run := range payload.Runs {
		if id, _ := run["runId"].(string); id == runID {
			row = run
			break
		}
	}
	if row == nil {
		t.Fatalf("expected a row for runId %q, got %#v", runID, payload.Runs)
	}

	if got, _ := row["retriesTotal"].(float64); got != 3 {
		t.Fatalf("expected retriesTotal=3 on matching row, got %v (raw: %s)", row["retriesTotal"], body)
	}
	if got, _ := row["retriesDone"].(float64); got != 2 {
		t.Fatalf("expected retriesDone=2 on matching row, got %v (raw: %s)", row["retriesDone"], body)
	}

	rawEvents, ok := row["events"].([]any)
	if !ok {
		t.Fatalf("expected events array on matching row, got %T (raw: %s)", row["events"], body)
	}

	var retryEvents []map[string]any
	for _, raw := range rawEvents {
		event, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("expected event object, got %T (raw: %s)", raw, body)
		}
		if event["type"] == "run.retry" {
			retryEvents = append(retryEvents, event)
		}
	}
	if len(retryEvents) != 2 {
		t.Fatalf("expected exactly 2 run.retry events on matching row, got %d: %#v (raw: %s)", len(retryEvents), retryEvents, body)
	}

	for i, event := range retryEvents {
		payload, ok := event["payload"].(map[string]any)
		if !ok {
			t.Fatalf("event %d: expected payload object, got %T (raw: %s)", i, event["payload"], body)
		}
		if got, _ := payload["attempt"].(float64); got != 2 {
			t.Fatalf("event %d: expected attempt=2, got %v (raw: %s)", i, payload["attempt"], body)
		}
		if got, _ := payload["max_attempts"].(float64); got != 3 {
			t.Fatalf("event %d: expected max_attempts=3, got %v (raw: %s)", i, payload["max_attempts"], body)
		}
		if got, _ := payload["previous_status"].(string); got != "failure" {
			t.Fatalf("event %d: expected previous_status=failure, got %v (raw: %s)", i, payload["previous_status"], body)
		}
		if got, _ := payload["branch"].(string); got != "sandman/42-fix" {
			t.Fatalf("event %d: expected branch=sandman/42-fix, got %v (raw: %s)", i, payload["branch"], body)
		}
		lines, ok := payload["last_log_lines"].([]any)
		if !ok {
			t.Fatalf("event %d: expected last_log_lines array, got %T (raw: %s)", i, payload["last_log_lines"], body)
		}
		if len(lines) != 3 {
			t.Fatalf("event %d: expected 3 last_log_lines, got %d (raw: %s)", i, len(lines), body)
		}
	}
}

// TestPortal_E2E_RetriesContract_CleanRunOmitsFields pins the omitempty
// contract for the new portalRun fields. A clean run (no retries) must
// produce a /api/runs response whose body does not contain the strings
// retriesTotal or retriesDone anywhere, so the existing payload for
// clean runs is byte-equivalent to the pre-change baseline.
func TestPortal_E2E_RetriesContract_CleanRunOmitsFields(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "abcd-260618113825-clean"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{
			"status": "success",
			"branch": "sandman/42-fix",
		}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)

	body := readPortalRunsRawBody(t, server.URL)

	if strings.Contains(string(body), "retriesTotal") {
		t.Fatalf("expected retriesTotal to be omitted from clean run response, got: %s", body)
	}
	if strings.Contains(string(body), "retriesDone") {
		t.Fatalf("expected retriesDone to be omitted from clean run response, got: %s", body)
	}

	var payload struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode runs: %v", err)
	}

	var row map[string]any
	for _, run := range payload.Runs {
		if id, _ := run["runId"].(string); id == runID {
			row = run
			break
		}
	}
	if row == nil {
		t.Fatalf("expected a row for runId %q, got %#v", runID, payload.Runs)
	}
	if _, present := row["retriesTotal"]; present {
		t.Fatalf("expected retriesTotal to be absent on clean run row, got %#v", row)
	}
	if _, present := row["retriesDone"]; present {
		t.Fatalf("expected retriesDone to be absent on clean run row, got %#v", row)
	}
}

// TestPortal_E2E_RetriesContract_LegacyFinishedRunDefaultsToZero pins
// the backward-compat contract for event log entries written before the
// run-idle-timeout enrichment. A run.finished payload that is missing
// the retries_total and retries_done keys entirely must still produce
// a /api/runs row whose RetriesTotal and RetriesDone are 0, so legacy
// event logs are readable through the new contract. Because the
// portalRun fields use omitempty, a zero value is omitted from the JSON
// body (matching the clean-run contract), and the legacy run is
// indistinguishable from a clean run on the wire — the difference is
// only visible in the decoded Go struct.
func TestPortal_E2E_RetriesContract_LegacyFinishedRunDefaultsToZero(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "abcd-260618113825-legacy"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{
			"status": "success",
			"branch": "sandman/42-fix",
		}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)

	rows := readPortalRuns(t, server.URL)

	var row *portalRun
	for i := range rows {
		if rows[i].RunID == runID {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("expected a row for runId %q, got %#v", runID, rows)
	}
	if row.RetriesTotal != 0 {
		t.Fatalf("expected RetriesTotal=0 for legacy finished run, got %d", row.RetriesTotal)
	}
	if row.RetriesDone != 0 {
		t.Fatalf("expected RetriesDone=0 for legacy finished run, got %d", row.RetriesDone)
	}
}

// readPortalRunsRawBody issues a real GET /api/runs against the given
// portal server and returns the raw response body. The byte-exact
// contract assertions (no retriesTotal / retriesDone substrings) need
// the raw body, not a decoded struct, because the omitempty test would
// be defeated by Go silently dropping zero-valued fields during
// decoding.
func readPortalRunsRawBody(t *testing.T, baseURL string) []byte {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/runs")
	if err != nil {
		t.Fatalf("GET /api/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return body
}

// TestPortal_E2E_RowCarriesLiveAttemptAndRetryReason is the end-to-end
// regression test for slice 1 of #1499. It writes events.jsonl with
// run.started + run.retry (no run.finished yet), starts a real portal
// HTTP server pointed at the temp repository, hits /api/runs?runKey=<id>
// and /api/runs, and asserts the per-run JSON response carries
// attempts: 1 and lastRetryReason: "agent-stalled" (the literal value
// the test writer chose; slice 3 of #1499 will populate the orchestrator
// reason vocabulary). The test is decoupled from the orchestrator's
// future writer changes by writing the event itself. It also locks the
// behavior on the second endpoint (/api/runs) so the row's
// attempts/lastRetryReason survive the summary JSON encoding.
func TestPortal_E2E_RowCarriesLiveAttemptAndRetryReason(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "abcd-260618113825-active-retry"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	retryAt := startedAt.Add(2 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.retry", Timestamp: retryAt, RunID: runID, Issue: 42, Payload: map[string]any{
			"attempt":         2,
			"max_attempts":    3,
			"previous_status": "failure",
			"reason":          "agent-stalled",
			"branch":          "sandman/42-fix",
		}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)

	t.Run("runKey endpoint exposes attempts and lastRetryReason", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/api/runs?runKey=" + runID)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var payload struct {
			Run portalRun `json:"run"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("decode runKey response: %v", err)
		}
		if payload.Run.RunID != runID {
			t.Fatalf("RunID = %q, want %q", payload.Run.RunID, runID)
		}
		if payload.Run.Attempts != 1 {
			t.Fatalf("Attempts = %d, want 1 (retry count: one retry has actually occurred; payload attempt=2 maps to retry count 1)", payload.Run.Attempts)
		}
		if payload.Run.LastRetryReason != "agent-stalled" {
			t.Fatalf("LastRetryReason = %q, want %q", payload.Run.LastRetryReason, "agent-stalled")
		}
	})

	t.Run("summary endpoint exposes attempts and lastRetryReason", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/api/runs?summary=1")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		var payload struct {
			Runs []map[string]any `json:"runs"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode runs: %v", err)
		}
		var row map[string]any
		for _, run := range payload.Runs {
			if id, _ := run["runId"].(string); id == runID {
				row = run
				break
			}
		}
		if row == nil {
			t.Fatalf("expected a row for runId %q, got %#v", runID, payload.Runs)
		}
		if got, _ := row["attempts"].(float64); got != 1 {
			t.Fatalf("attempts = %v, want 1 (retry count: one retry has actually occurred; payload attempt=2 maps to retry count 1; raw: %s)", row["attempts"], body)
		}
		if got, _ := row["lastRetryReason"].(string); got != "agent-stalled" {
			t.Fatalf("lastRetryReason = %q, want %q (raw: %s)", got, "agent-stalled", body)
		}
	})
}

// TestPortal_E2E_CleanRunOmitsAttemptsAndLastRetryReason locks the
// omitempty contract for the slice-1 portalRun fields. A clean run
// (no retries) must produce a /api/runs response whose body does not
// contain the strings attempts or lastRetryReason anywhere, so the
// existing payload for clean runs is byte-equivalent to the pre-change
// baseline on the wire for the new keys.
func TestPortal_E2E_CleanRunOmitsAttemptsAndLastRetryReason(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const runID = "abcd-260618113825-clean-1499"
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{
			"status": "success",
			"branch": "sandman/42-fix",
		}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)

	body := readPortalRunsRawBody(t, server.URL)

	if strings.Contains(string(body), `"attempts"`) {
		t.Fatalf("expected attempts to be omitted from clean run response, got: %s", body)
	}
	if strings.Contains(string(body), `"lastRetryReason"`) {
		t.Fatalf("expected lastRetryReason to be omitted from clean run response, got: %s", body)
	}
}

// TestPortal_E2E_ParentSuccWithLiveChild is the HTTP end-to-end regression
// test for the cross-batch portal contract (issue #1825): a parent impl row
// with a live review child surfaces through /api/runs without the parent
// row's own status being rewritten, and without backend-stamped review
// metadata crossing batch boundaries. This variant exercises the
// JSON-over-HTTP serialization path.
func TestPortal_E2E_ParentSuccWithLiveChild(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "PR42-live")
	sockPath := filepath.Join(batchDir, "batch.sock")
	createUnixRunSocket(t, sockPath)
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{1}, CreatedAt: startedAt, BatchId: "PR42-live"}); err != nil {
		t.Fatal(err)
	}
	addBatchToIndex(t, repoRoot, "PR42-live", batchDir, []int{1})

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "abcd-260618113825-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: startedAt.Add(1 * time.Minute), RunID: "abcd-260618113825-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix", "status": "success"}},
		{Type: "run.started", Timestamp: startedAt.Add(30 * time.Second), RunID: "PR42-live", Issue: 1, Payload: map[string]any{"review": true, "pr_number": 42, "branch": "sandman/review-PR42"}},
	})

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)

	rows := readPortalRuns(t, server.URL)

	var parentRow *portalRun
	var reviewChild *portalRun
	for i := range rows {
		if rows[i].IssueNumber == 1 && !rows[i].Review {
			parentRow = &rows[i]
		}
		if rows[i].IssueNumber == 1 && rows[i].Review {
			reviewChild = &rows[i]
		}
	}
	if parentRow == nil {
		t.Fatalf("expected parent row for issue #1, got %#v", rows)
	}
	if parentRow.Status != "success" {
		t.Fatalf("expected parent Status='success' (terminal run.finished value preserved after aggregateReviewChildren removal), got %q", parentRow.Status)
	}
	if reviewChild == nil {
		t.Fatalf("expected review child row for issue #1, got %#v", rows)
	}
	if !reviewChild.Review {
		t.Fatalf("expected review child Review=true, got %v", reviewChild.Review)
	}
	if reviewChild.PRNumber != 42 {
		t.Fatalf("expected review child PRNumber=42, got %d", reviewChild.PRNumber)
	}
}
