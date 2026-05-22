package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
)

type blockingBatchRunner struct {
	started chan struct{}
	release chan struct{}
	result  *batch.Result
	err     error
}

func (b *blockingBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	b.started <- struct{}{}
	<-b.release
	return b.result, b.err
}

func TestRun_WritesLiveRunMetadataAndRemovesDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	runner := &blockingBatchRunner{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		result:  &batch.Result{},
	}
	deps := newRunDeps(runner)

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		cmd := NewRunCmd(deps)
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"42", "43"})
		done <- cmd.Execute()
	}()

	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to start")
	}

	runsDir := filepath.Join(dir, ".sandman", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 live run, got %d", len(entries))
	}

	runPath := filepath.Join(runsDir, entries[0].Name(), "run.json")
	data, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var runMeta map[string]any
	if err := json.Unmarshal(data, &runMeta); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if runMeta["run_id"] == "" {
		t.Fatal("run_id missing from run.json")
	}
	if runMeta["pid"] == nil {
		t.Fatal("pid missing from run.json")
	}
	if runMeta["started_at"] == "" {
		t.Fatal("started_at missing from run.json")
	}

	issues, ok := runMeta["issues"].([]any)
	if !ok {
		t.Fatalf("issues missing or wrong type: %#v", runMeta["issues"])
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %#v", issues)
	}

	close(runner.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to finish")
	}

	if _, err := os.Stat(filepath.Join(runsDir, entries[0].Name())); !os.IsNotExist(err) {
		t.Fatalf("run dir should be removed after daemon exits, got err=%v", err)
	}
}

func TestRun_AllowsConcurrentLiveRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	runner1 := &blockingBatchRunner{
		started: make(chan struct{}, 1),
		release: release,
		result:  &batch.Result{},
	}
	runner2 := &blockingBatchRunner{
		started: make(chan struct{}, 1),
		release: release,
		result:  &batch.Result{},
	}

	done := make(chan error, 2)
	startRun := func(issue string, deps Dependencies) {
		go func() {
			var buf bytes.Buffer
			cmd := NewRunCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{issue})
			done <- cmd.Execute()
		}()
	}

	startRun("42", newRunDeps(runner1))
	select {
	case <-runner1.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first run to start")
	}

	startRun("43", newRunDeps(runner2))
	select {
	case <-runner2.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for second run to start")
	}

	entries, err := os.ReadDir(filepath.Join(dir, ".sandman", "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 live runs, got %d", len(entries))
	}

	close(release)

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for run to finish")
		}
	}
}
