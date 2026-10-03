package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
)

// Exercise the production start path before inspecting, attaching, archiving,
// and recovering. The second row's last durable state models a daemon crash.
func TestLocations_ProductionMultiIssueFlow(t *testing.T) {
	env := newRunSessionTestEnv(t)
	gh := &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Terminal sibling"},
		43: {Number: 43, Title: "Live sibling"},
	}}
	marker := env.markerPath
	agent := fmt.Sprintf("sh -c 'case \"$PWD\" in */42-*) echo terminal-log; exit 1;; *) echo live-log; touch %q; while :; do sleep 1; done;; esac'", marker)
	store := &fakeStore{config: &config.Config{
		DefaultAgent: "custom", Agent: "custom", ReviewCommand: "/sandman review",
		WorktreeDir: ".sandman/worktrees", Sandbox: "worktree",
		Git:            config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"custom": {Command: agent}},
	}}
	log := &events.JSONLLogger{Path: env.eventsPath}
	runner := batch.NewOrchestrator(gh, &prompt.Engine{}, store, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		cmd := NewRunCmd(Dependencies{BatchRunner: runner, ConfigStore: store, EventLog: log,
			GitHubClient: gh, Renderer: &prompt.Engine{}, IsTTY: func() bool { return false }, RepoRoot: env.repoDir})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"42", "43", "--parallel", "2", "--retries", "0"})
		done <- cmd.ExecuteContext(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("run did not stop")
		}
	})
	waitForPathTB(t, marker, 15*time.Second)
	layout := paths.NewLayout(nil, env.repoDir)
	var idx *batchindex.Index
	var terminalID, liveID string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		idx, _ = batchindex.Load(layout.BatchesIndexPath)
		for _, e := range readJSONLEvents(t, env.eventsPath) {
			if e.Issue == 42 && e.Type == "run.finished" {
				terminalID = e.RunID
			}
			if e.Issue == 43 && e.Type == "run.started" {
				liveID = e.RunID
			}
		}
		if terminalID != "" && liveID != "" && idx != nil && len(idx.Batches) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if terminalID == "" || liveID == "" || idx == nil || len(idx.Batches) != 1 {
		t.Fatal("production rows did not start/finish")
	}
	b := idx.Batches[0]
	// Persist an independently named public identity, as supported by the
	// index schema; the production-created physical artifacts stay untouched.
	b.ID = "public-" + b.ID
	idx.Batches[0] = b
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		t.Fatal(err)
	}
	// Remove the independent review daemon so attach has exactly one target.
	if err := os.Remove(layout.ReviewSocketPath()); err != nil {
		t.Fatal(err)
	}
	sock, err := findDaemonSocket(env.repoDir)
	if err != nil || sock != daemon.BatchSocketPath(b.Path) {
		t.Fatalf("attach = %q, %v", sock, err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	view := &portalRunsView{}
	rows, err := view.compute(env.repoDir, log)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{terminalID, liveID} {
		found := false
		for _, row := range rows {
			if row.RunID == id {
				found = true
				if row.RunDir != daemon.RunFolder(b.Path, id) {
					t.Errorf("Portal directory = %q", row.RunDir)
				}
			}
		}
		if !found {
			t.Fatalf("Portal missing %q", id)
		}
	}
	archive := NewArchiveCmd(Dependencies{RepoRoot: env.repoDir})
	archive.SetOut(&bytes.Buffer{})
	archive.SetArgs([]string{"run", terminalID})
	if err := archive.Execute(); err != nil {
		t.Fatal(err)
	}
	idx, err = batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	rec := idx.RunRecordFor(b.ID, terminalID)
	if rec == nil || rec.Status != batchindex.RunRecordStatusArchived {
		t.Fatal("archive record missing")
	}
	data, err := os.ReadFile(filepath.Join(env.repoDir, rec.ArchivePath, "run.log"))
	if err != nil || !strings.Contains(string(data), "terminal-log") {
		t.Fatalf("archived log = %q, %v", data, err)
	}
	if _, err := os.Stat(daemon.RunFolder(b.Path, liveID)); err != nil {
		t.Fatal("archive moved live sibling", err)
	}
	rows, err = (&portalRunsView{}).compute(env.repoDir, log)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.RunID == terminalID && (!row.Archived || row.RunDir != filepath.Join(env.repoDir, rec.ArchivePath) || !strings.Contains(row.Log, "terminal-log")) {
			t.Fatalf("archived Portal row = %+v", row)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("run did not stop")
	}
	// Leave only the state durable immediately before a crash of row 43.
	list := readJSONLEvents(t, env.eventsPath)
	crashEvents := list[:0]
	for _, e := range list {
		if e.RunID == liveID && (e.Type == "run.aborted" || e.Type == "run.finished" || e.Type == "run.cancelled") {
			continue
		}
		crashEvents = append(crashEvents, e)
	}
	if err := daemon.UpdateRunManifestStatus(b.Path, liveID, batchindex.RunManifestStatusActive); err != nil {
		t.Fatal(err)
	}
	recoveryLog := &events.JSONLLogger{Path: filepath.Join(env.sandmanDir, "recovery-test.jsonl")}
	n, _, err := daemon.RecoverStaleRuns(layout.SandmanDir, crashEvents, recoveryLog)
	if err != nil || n != 1 {
		t.Fatalf("recovery = %d, %v", n, err)
	}
	m, err := daemon.ReadRunManifest(b.Path, liveID)
	if err != nil || m.Status != batchindex.RunManifestStatusAborted {
		t.Fatalf("recovered physical manifest = %+v, %v", m, err)
	}
	recovered, _ := recoveryLog.Read()
	n, _, err = daemon.RecoverStaleRuns(layout.SandmanDir, append(crashEvents, recovered...), recoveryLog)
	if err != nil || n != 0 {
		t.Fatalf("repeat recovery = %d, %v", n, err)
	}
}
