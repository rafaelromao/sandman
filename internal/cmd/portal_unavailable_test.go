package cmd

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/runid"
)

// TestPortalRunsView_UnavailableFlagFromBatchIndex covers slice #1 of #1312:
// a row whose batchindex entry is marked StatusUnavailable appears in
// /api/runs JSON with `unavailable: true` and `archived: false`.
func TestPortalRunsView_UnavailableFlagFromBatchIndex(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	layout := paths.NewLayout(nil, repoRoot)

	// Use realistic runid output: batch ID and per-row RunID differ,
	// exercising the sourceDirID-based lookup.
	ts := "20250101T100000Z"
	shortid := "abcd"
	batchID := runid.NewBatchID(runid.KindIssue, 2, "42", ts, shortid)
	rowRunID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)

	startedAt := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	writePortalLog(t, filepath.Join(layout.EventsLogPath), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: rowRunID, Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: rowRunID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "sandman/42-fix"}},
	})

	// Create a dead batch directory with a runs/ subdirectory so that
	// BatchKey gets populated from the row-to-batch reverse lookup.
	batchDir := filepath.Join(layout.BatchesDir, batchID)
	if err := os.MkdirAll(filepath.Join(batchDir, "runs", rowRunID), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "manifest.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Seed the batch index with a StatusUnavailable entry keyed by the
	// batch ID. The row's RunID differs (no +N suffix), so the lookup
	// must go through BatchKey → sourceDirID to find the match.
	batchIdx := &batchindex.Index{
		Version: batchindex.IndexVersion,
		Entries: []batchindex.Entry{
			{
				ID:        batchID,
				Path:      batchDir,
				Kind:      batchindex.KindIssue,
				Status:    batchindex.StatusUnavailable,
				CreatedAt: startedAt,
				Issues:    []int{42},
			},
		},
	}
	if err := batchIdx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	if len(payload.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(payload.Runs))
	}
	got := payload.Runs[0]
	if !got.Unavailable {
		t.Fatalf("expected Unavailable=true for batchindex StatusUnavailable entry, got %+v", got)
	}
	if got.Archived {
		t.Fatalf("expected Archived=false for unavailable entry, got %+v", got)
	}

	// Round-trip the run through JSON to confirm the wire contract carries
	// the field too. The /api/runs handler is what the portal actually
	// reads; this guards against a struct change that hides the field.
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(wire, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["unavailable"] != true {
		t.Fatalf("expected wire JSON to carry unavailable=true, got %v", raw["unavailable"])
	}
	if _, ok := raw["archived"]; !ok {
		t.Fatalf("expected wire JSON to carry archived key (always-present contract), got %v", raw)
	}
	if raw["archived"] != false {
		t.Fatalf("expected wire JSON to carry archived=false, got %v", raw["archived"])
	}
}
