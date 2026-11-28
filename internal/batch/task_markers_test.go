package batch

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/prompt"
)

// The marker nonce is recorded and read inside its worktree only. A sidecar
// reached through a symlink leading out of the worktree is never written, and
// reading it finds no markers.
func TestTaskMarkers_StayInsideTheWorktree(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		dir := t.TempDir()
		(&AgentRun{sandbox: &fakeSandbox{workDir: dir}}).recordTaskNonce(testTaskNonce)
		if nonce, ok := readTaskNonce(dir); !ok || nonce != testTaskNonce {
			t.Fatalf("nonce = %q, %t", nonce, ok)
		}
	})
	for name, escape := range map[string]func(t *testing.T, dir, sibling string){
		"through the state directory": func(t *testing.T, dir, sibling string) {
			symlink(t, sibling, filepath.Join(dir, ".sandman"))
		},
		"through the sidecar itself": func(t *testing.T, dir, sibling string) {
			if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0o755); err != nil {
				t.Fatal(err)
			}
			symlink(t, filepath.Join(sibling, taskAnchorsFile), filepath.Join(dir, ".sandman", taskAnchorsFile))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, sibling := t.TempDir(), t.TempDir()
			siblingAnchors := filepath.Join(sibling, taskAnchorsFile)
			if err := os.WriteFile(siblingAnchors, []byte(testTaskNonce+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			escape(t, dir, sibling)

			if _, ok := readTaskNonce(dir); ok {
				t.Fatal("read a nonce from outside the worktree")
			}
			task := checkpointTask(30)
			result, err := CompactWorktreeTask(task, TaskCompactionRequest{WorkDir: dir, Threshold: testTaskCompactBytes, Now: time.Now()})
			if err != nil || result.Compacted || result.Content != task || result.Reason != unmarkedTaskNote {
				t.Fatalf("automatic: compacted = %t, reason %q, err %v", result.Compacted, result.Reason, err)
			}
			// Forced compaction reads the Task as one with no sidecar at all:
			// from the verified built-in layout.
			now := time.Now()
			forced, err := CompactWorktreeTask(task, TaskCompactionRequest{WorkDir: dir, Threshold: testTaskCompactBytes, Force: true, Now: now})
			baseline, baseErr := CompactWorktreeTask(task, TaskCompactionRequest{WorkDir: t.TempDir(), Threshold: testTaskCompactBytes, Force: true, Now: now})
			switch {
			case baseErr != nil:
				t.Fatal(baseErr)
			case err != nil && forced.Content != task:
				t.Fatalf("forced: failed with %v but changed the Task", err)
			case err == nil && (forced.Compacted != baseline.Compacted || forced.Content != baseline.Content):
				t.Fatalf("forced: compacted = %t, want %t as without markers", forced.Compacted, baseline.Compacted)
			}

			(&AgentRun{sandbox: &fakeSandbox{workDir: dir}}).recordTaskNonce("fresh")
			if got, _ := os.ReadFile(siblingAnchors); string(got) != testTaskNonce+"\n" {
				t.Fatalf("sibling markers = %q", got)
			}
			if entries, _ := os.ReadDir(sibling); len(entries) != 1 {
				t.Fatalf("wrote into the sibling: %v", entries)
			}
		})
	}
	t.Run("forced compaction still reads the built-in layout", func(t *testing.T) {
		dir, sibling := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0o755); err != nil {
			t.Fatal(err)
		}
		symlink(t, filepath.Join(sibling, taskAnchorsFile), filepath.Join(dir, ".sandman", taskAnchorsFile))
		task := unmarkedTask(30)
		result, err := CompactWorktreeTask(task, TaskCompactionRequest{WorkDir: dir, Threshold: testTaskCompactBytes, Force: true,
			Template: prompt.RenderConfig{}, Now: time.Now()})
		if err != nil || !result.Compacted {
			t.Fatalf("compacted = %t, reason %q, err %v", result.Compacted, result.Reason, err)
		}
	})
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// The compaction target is the floor of three quarters of any accepted budget.
func TestThreeQuarters_NeverOverflows(t *testing.T) {
	for n := range 64 {
		if got := threeQuarters(n); got != n*3/4 {
			t.Fatalf("threeQuarters(%d) = %d", n, got)
		}
	}
	for _, n := range []int{math.MaxInt / 2, math.MaxInt} {
		if got, want := threeQuarters(n), n-n/4-boolInt(n%4 != 0); got != want || got <= 0 {
			t.Fatalf("threeQuarters(%d) = %d, want %d", n, got, want)
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Anything at the sidecar's path other than a small regular file is read as no
// markers, at once: opening a named pipe does not wait for a writer.
func TestReadTaskNonce_OnlyFromASmallRegularFile(t *testing.T) {
	for name, place := range map[string]func(t *testing.T, path string){
		"named pipe": func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"oversized file": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("a", maxTaskNonceBytes+1)), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"empty file": func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, taskAnchorsName())
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			place(t, path)

			if nonce, ok := readWithin(t, dir); ok {
				t.Fatalf("read nonce %q", nonce)
			}
		})
	}
}

// readWithin reads a worktree's nonce, failing the test if that blocks.
func readWithin(t *testing.T, dir string) (string, bool) {
	t.Helper()
	type read struct {
		nonce string
		ok    bool
	}
	done := make(chan read, 1)
	go func() {
		nonce, ok := readTaskNonce(dir)
		done <- read{nonce, ok}
	}()
	select {
	case r := <-done:
		return r.nonce, r.ok
	case <-time.After(5 * time.Second):
		t.Fatal("reading the marker sidecar blocked")
		return "", false
	}
}

// A launch whose sidecar is a named pipe launches promptly: an oversized
// automatic continuation goes out unchanged with no archive, and a forced one
// is still read from the verified built-in layout.
func TestImplementationLaunch_TreatsANonRegularSidecarAsNoMarkers(t *testing.T) {
	toPipe := func(run *AgentRun) {
		path := filepath.Join(run.sandbox.WorkDir(), taskAnchorsName())
		if err := os.Remove(path); err != nil {
			panic(err)
		}
		if err := syscall.Mkfifo(path, 0o644); err != nil {
			panic(err)
		}
	}
	task := checkpointTask(30)
	archives := func(launch compactionLaunch) []string {
		found, _ := filepath.Glob(filepath.Join(launch.workDir, ".sandman", "task-history", "*.md"))
		return found
	}
	t.Run("valid sidecar", func(t *testing.T) {
		launch := launchWithCompaction(t, task, testTaskCompactBytes)
		if !strings.Contains(launch.stdin, "## Sandman Task History") || len(archives(launch)) != 1 {
			t.Fatal("a marked Task was not compacted")
		}
	})
	t.Run("automatic", func(t *testing.T) {
		launch := launchInWorktree(t, t.TempDir(), prompt.RenderConfig{TaskPrompt: task}, testTaskCompactBytes, nil, toPipe)
		if strings.Contains(launch.stdin, "## Sandman Task History") || !strings.Contains(launch.stdin, "## Checkpoint 1\n") || len(archives(launch)) != 0 {
			t.Fatal("a Task without readable markers was compacted automatically")
		}
	})
	t.Run("forced", func(t *testing.T) {
		launch := launchInWorktree(t, t.TempDir(), prompt.RenderConfig{TaskPrompt: task}, testTaskCompactBytes, nil, toPipe, func(run *AgentRun) {
			run.forceTaskCompaction = true
		})
		if !strings.Contains(launch.stdin, "## Sandman Task History") || len(archives(launch)) != 1 {
			t.Fatal("a forced compaction did not read the built-in layout")
		}
	})
}
