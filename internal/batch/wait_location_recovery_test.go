package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
)

// A restarted scheduler that names an indexed Batch by its frozen directory
// transfers and renews waiting ownership there under the Batch's public ID.
// Another Batch whose public ID equals the frozen directory's basename must
// never receive the identity, the wait record or the event attribution.
func TestRunBatchRehydratesWaitAtFrozenRunDirLocation(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	initGitRepo(t, root)
	cfg := &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}
	layout := paths.NewLayout(cfg, root)
	now := time.Now().UTC()
	const (
		issue    = 42
		runID    = "260929120000-abcd-42"
		oldBatch = "260929120000-abcd-42"
		branch   = "42-wait"
		publicID = "261001090000-beef-42"
		frozen   = "frozen-x"
	)
	frozenDir := filepath.Join(layout.BatchesDir, frozen)
	shadowDir := filepath.Join(layout.BatchesDir, "frozen-y")
	for _, dir := range []string{frozenDir, shadowDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{
		{ID: publicID, Path: frozenDir, Status: batchindex.StatusActive},
		// Negative control: this Batch's public ID is the RunDir basename.
		{ID: frozen, Path: shadowDir, Status: batchindex.StatusActive},
	}}).Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	log := &spyEventLog{events: []events.Event{
		{Type: "run.started", RunID: runID, Issue: issue, Timestamp: now.Add(-10 * time.Minute), Payload: map[string]any{
			"batch_id": oldBatch, "branch": branch, "base_branch": "main",
		}},
		{Type: "run.await", RunID: runID, Issue: issue, Timestamp: now.Add(-9 * time.Minute), Payload: map[string]any{
			"await_reason": "pending", "batch_id": oldBatch, "branch": branch,
			"ci_wait": map[string]any{"deadline_unix_seconds": now.Add(30 * time.Minute).Unix()},
		}},
	}}
	historical := len(log.snapshot())
	if err := daemon.RenewRunWait(layout, layout.LocateBatch(oldBatch, ""), daemon.RunWait{
		Protocol: "run-wait/v1", RunID: runID, BatchID: oldBatch, Issue: issue,
		Branch: branch, BaseBranch: "main", OperationID: "ci:17:head", OperationDeadline: now.Add(30 * time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(layout.WorktreeDir, branch, ".sandman", "task.md")
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taskPath, []byte("# Task\n\nResume the implementation.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := Request{RunDir: filepath.Join(".sandman", "batches", frozen)}
	if err := ApplyReadyContinuations(&request, FindReadyContinuations(log.snapshot(), layout), layout, 1800); err != nil {
		t.Fatal(err)
	}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{issue: {Number: issue, State: "open", Title: "Waiting issue"}},
		prs:    map[string]*github.PR{branch: {Number: 17, State: "merged", Merged: true, Body: "Closes #42", HeadRefName: branch}},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, log,
		WithErrorLog(io.Discard), WithRunnableFactory(&controlledRunnableFactory{runnables: map[int]Runnable{}}), WithRunSessionOpts(runSessionOptions{
			baseBranchSync: func(string, string) error { return nil },
			currentHead:    func(string) (string, error) { return "current-sha", nil },
		}))
	result, err := o.RunBatch(context.Background(), request)
	if err != nil || result == nil || len(result.Runs) != 1 || result.Runs[0].Status != "success" {
		t.Fatalf("RunBatch = %+v, %v", result, err)
	}
	if recovered := request.RecoveryWaits[issue]; recovered.BatchID != publicID {
		t.Fatalf("transferred wait identity = %q, want public ID %q", recovered.BatchID, publicID)
	}
	persisted, err := daemon.ReadRunWaitAt(layout, paths.BatchLocation{ID: publicID, Dir: frozenDir}, runID)
	if err != nil || persisted.BatchID != publicID || !persisted.LeaseExpiresAt.After(now) {
		t.Fatalf("renewed owner at frozen location = %+v, %v", persisted, err)
	}
	for _, foreign := range []string{shadowDir, layout.BatchDir(publicID)} {
		if _, err := os.Stat(filepath.Join(foreign, "runs", runID)); !os.IsNotExist(err) {
			t.Fatalf("waiting ownership leaked into %s: %v", foreign, err)
		}
	}
	for _, event := range log.snapshot()[historical:] {
		if id, ok := event.Payload["batch_id"]; ok && id != publicID {
			t.Fatalf("%s attributed to Batch %v, want %q", event.Type, id, publicID)
		}
	}
	if state := events.ProjectRunStates(log.snapshot())[0]; !state.IsTerminal() || state.BatchID() != publicID {
		t.Fatalf("recovered Run projection = status %q batch %q", state.Status(), state.BatchID())
	}
}

// Run manifests and Run folders follow the row's frozen Batch directory while
// recording the Batch's public identity.
func TestRunSessionWritesRunArtifactsAtFrozenBatchDir(t *testing.T) {
	layout := paths.NewLayout(nil, t.TempDir())
	frozenDir := filepath.Join(layout.BatchesDir, "frozen-x")
	const (
		publicID = "261001090000-beef-42"
		runID    = "261001090000-beef-42"
	)
	session := newRunSession(&runExecutor{deps: runDeps{layout: layout, errorLog: io.Discard}}, RowSpec{IssueNumber: 42, RunID: runID, BatchID: publicID, BatchDir: frozenDir, Branches: map[int]string{42: "42-fix"}})
	if got, want := session.runFolderFor(runID), filepath.Join(frozenDir, "runs", runID); got != want {
		t.Fatalf("run folder = %q, want %q", got, want)
	}
	if got, want := session.runLogPathFor(runID), filepath.Join(frozenDir, "runs", runID, "run.log"); got != want {
		t.Fatalf("run log = %q, want %q", got, want)
	}
	batchDir, manifest := session.initialRunManifest("42-fix", &fakeSandbox{workDir: t.TempDir()})
	if batchDir != frozenDir || manifest.BatchID != publicID {
		t.Fatalf("initial manifest at %q for %q, want %q for %q", batchDir, manifest.BatchID, frozenDir, publicID)
	}
	if err := daemon.WriteRunManifest(batchDir, manifest.RunID, manifest); err != nil {
		t.Fatal(err)
	}
	session.updateRunManifestStatus(manifest.RunID, batchindex.RunManifestStatusSuccess)
	if saved, err := daemon.ReadRunManifest(frozenDir, manifest.RunID); err != nil || saved.Status != batchindex.RunManifestStatusSuccess {
		t.Fatalf("terminal manifest at frozen dir = %+v, %v", saved, err)
	}
	if _, err := os.Stat(layout.BatchDir(publicID)); !os.IsNotExist(err) {
		t.Fatalf("run artifacts leaked into the public ID's canonical dir: %v", err)
	}
}
