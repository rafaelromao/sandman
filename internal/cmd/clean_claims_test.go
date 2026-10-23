package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

func TestClean_PrunesTerminalRunClaims(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		removed bool
	}{
		{args: []string{"--stale"}, removed: true},
		{args: []string{"--all"}, removed: true},
		{args: []string{"--all", "--dry-run"}, removed: false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			deps := newRunDepsAuto(t, &fakeBatchRunner{})
			dir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
			started := time.Now().Add(-time.Hour)
			deps.EventLog = &fakeEventLog{events: []events.Event{
				{Type: "run.started", RunID: "done", Issue: 41, Timestamp: started},
				{Type: "run.aborted", RunID: "done", Issue: 41, Timestamp: started.Add(time.Minute)},
			}}
			deps.GitRunner = &fakeGitRunner{}
			sandmanDir := filepath.Join(dir, ".sandman")
			for _, id := range []string{"done", "unknown"} {
				claim, err := daemon.ClaimRun(sandmanDir, id)
				if err != nil {
					t.Fatal(err)
				}
				_ = claim.Close()
			}

			var buf bytes.Buffer
			cmd := NewCleanCmd(deps)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("clean %v: %v\n%s", tc.args, err, buf.String())
			}

			claims := filepath.Join(sandmanDir, "state", "run-claims")
			if _, err := os.Stat(filepath.Join(claims, "done.lock")); (err != nil) != tc.removed {
				t.Fatalf("terminal Run claim removed=%v, want %v (err=%v)", err != nil, tc.removed, err)
			}
			if _, err := os.Stat(filepath.Join(claims, "unknown.lock")); err != nil {
				t.Fatalf("claim of a Run without events was removed: %v", err)
			}
			if tc.removed && !strings.Contains(buf.String(), "Removed 1 terminal run claim file(s).") {
				t.Fatalf("clean output does not report pruned claims:\n%s", buf.String())
			}
		})
	}
}
