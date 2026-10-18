package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/runid"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func writeRunSessionIdentity(t *testing.T, runDir, protocol, provider, sessionID string) {
	t.Helper()
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]string{"protocol": protocol, "provider": provider, "session_id": sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "session.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func portalAgentSessionRepo(t *testing.T) string {
	t.Helper()
	repoRoot := testenv.MkdirShort(t, "sm-agentsess-")
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return repoRoot
}

func TestPortal_AgentSessionID_ActiveClaudeRow(t *testing.T) {
	repoRoot := portalAgentSessionRepo(t)
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", "live-session")
	if err := os.MkdirAll(batchDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(batchDir, daemon.BatchManifest{Issues: []int{42}, CreatedAt: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), BatchId: "live-session"}); err != nil {
		t.Fatal(err)
	}
	createUnixRunSocket(t, filepath.Join(batchDir, "batch.sock"))
	addBatchToIndex(t, repoRoot, "live-session", batchDir, []int{42})
	runID := "abcd-260101120000-42"
	writeRunSessionIdentity(t, filepath.Join(batchDir, "runs", runID), "claude-session/v1", "claude", "6f1c2a4e-0b7d-4c1a-9a55-2f0e8d3b7c11")
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: runID, Issue: 42, Payload: map[string]any{"branch": "42-fix"}},
	})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(runs))
	}
	if got := runs[0].AgentSessionID; got != "6f1c2a4e-0b7d-4c1a-9a55-2f0e8d3b7c11" {
		t.Fatalf("AgentSessionID = %q, want the Claude session recorded in the Run folder", got)
	}
	data, err := json.Marshal(portalSummaryRuns(runs)[0])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["agentSessionId"] != "6f1c2a4e-0b7d-4c1a-9a55-2f0e8d3b7c11" {
		t.Fatalf("summary JSON agentSessionId = %v, want the session ID: %s", raw["agentSessionId"], data)
	}
}

func TestPortal_AgentSessionID_TerminalOpenCodeRow(t *testing.T) {
	repoRoot := portalAgentSessionRepo(t)
	batchID := "abcd-260101120000-99"
	runID := batchID
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	writeRunSessionIdentity(t, filepath.Join(batchDir, "runs", runID), "opencode-session/v1", "opencode", "ses_terminal")
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: runID, Issue: 99, Payload: map[string]any{"branch": "99-fix", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: runID, Issue: 99, Payload: map[string]any{"status": "success", "branch": "99-fix", "batch_id": batchID}},
	})
	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{99})

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(runs))
	}
	if got := runs[0].AgentSessionID; got != "ses_terminal" {
		t.Fatalf("AgentSessionID = %q, want the OpenCode session recorded in the Run folder", got)
	}
}

func TestPortal_AgentSessionID_OmittedWithoutIdentity(t *testing.T) {
	data, err := json.Marshal(portalRun{Key: "k", RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["agentSessionId"]; ok {
		t.Fatalf("agentSessionId present without an identity: %s", data)
	}
}

func TestPortal_AgentSessionID_ArchivedRow(t *testing.T) {
	repoRoot := portalAgentSessionRepo(t)
	layout := paths.NewLayout(nil, repoRoot)
	ts, shortid := "260101120000", "abcd"
	batchID := runid.NewBatchID(runid.KindIssue, 3, "42", ts, shortid)
	runID := runid.NewRunID(runid.KindIssue, "42", ts, shortid)
	startedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(5 * time.Minute)
	writePortalLog(t, layout.EventsLogPath, []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: runID, Issue: 42, Payload: map[string]any{"branch": "42-fix", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: finishedAt, RunID: runID, Issue: 42, Payload: map[string]any{"status": "success", "branch": "42-fix", "batch_id": batchID}},
	})
	archiveDir := filepath.Join(layout.ArchiveDir, batchID)
	writeRunSessionIdentity(t, filepath.Join(archiveDir, "runs", runID), "claude-session/v1", "claude", "6f1c2a4e-0b7d-4c1a-9a55-2f0e8d3b7c11")
	idx := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{{
		ID: batchID, Path: archiveDir, Kind: batchindex.KindIssue, Status: batchindex.StatusArchived,
		CreatedAt: startedAt, ArchivedAt: &finishedAt, Issues: []int{42},
	}}}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: layout.EventsLogPath})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(runs) != 1 || !runs[0].Archived {
		t.Fatalf("runs = %#v, want one archived row", runs)
	}
	if got := runs[0].AgentSessionID; got != "6f1c2a4e-0b7d-4c1a-9a55-2f0e8d3b7c11" {
		t.Fatalf("AgentSessionID = %q, want the session recorded in the archived Run folder", got)
	}
}

func TestPortal_AgentSessionID_FollowsAReplacedRecord(t *testing.T) {
	repoRoot := portalAgentSessionRepo(t)
	batchID := "abcd-260101120000-99"
	runID := batchID
	batchDir := filepath.Join(repoRoot, ".sandman", "batches", batchID)
	runDir := filepath.Join(batchDir, "runs", runID)
	writeRunSessionIdentity(t, runDir, "opencode-session/v1", "opencode", "ses_a")
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), RunID: runID, Issue: 99, Payload: map[string]any{"branch": "99-fix", "batch_id": batchID}},
		{Type: "run.finished", Timestamp: time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC), RunID: runID, Issue: 99, Payload: map[string]any{"status": "success", "branch": "99-fix", "batch_id": batchID}},
	})
	addBatchToIndex(t, repoRoot, batchID, batchDir, []int{99})
	view := &portalRunsView{}
	log := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}
	sessionPath := filepath.Join(runDir, "session.json")
	stamp := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(sessionPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if runs, err := view.compute(repoRoot, log); err != nil || runs[0].AgentSessionID != "ses_a" {
		t.Fatalf("first compute = %#v, %v; want ses_a", runs, err)
	}

	// A same-second atomic replace: only the size tells the records apart.
	writeRunSessionIdentity(t, runDir, "opencode-session/v1", "opencode", "ses_bb")
	if err := os.Chtimes(sessionPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	runs, err := view.compute(repoRoot, log)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].AgentSessionID != "ses_bb" {
		t.Fatalf("AgentSessionID = %q, want the replaced record's ses_bb", runs[0].AgentSessionID)
	}
}
