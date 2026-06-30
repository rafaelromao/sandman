//go:build e2e

package cmd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/review"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const pathlenTempPrefix = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

func TestRun_PathlenWorktreeAndReviewSockets(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioPathlen) {
		t.Skip("set SANDMAN_E2E_GATES=pathlen (or all) to run pathlen e2e")
	}

	t.Run("worktree run", func(t *testing.T) {
		repoDir := newPathlenRepo(t)
		t.Chdir(repoDir)
		initRunIntegrationRepoWithRemote(t, repoDir)

		gh := &fakeGitHubClient{
			issues: map[int]*github.Issue{
				42: {Number: 42, Title: "Pathlen regression", Body: "Exercise long Unix socket paths."},
			},
			prs: map[string]*github.PR{
				"sandman/42-pathlen-regression": {
					Number:      42,
					State:       "closed",
					Merged:      true,
					HeadRefName: "sandman/42-pathlen-regression",
				},
			},
		}

		deps := newRunIntegrationDepsWithSandboxAndGit(config.Agent{Command: "sleep 1; exit 0"}, "worktree", config.GitConfig{BaseBranch: "main"}, gh)
		out, err := runRootCommand(t, deps, "run", "--sandbox", "worktree", "42")
		if err != nil {
			t.Fatalf("run failed: %v\noutput:\n%s", err, out)
		}

		batchesDir := filepath.Join(repoDir, ".sandman", "batches")
		batchDir := firstChildDir(t, batchesDir)
		batchSockPath := filepath.Join(batchDir, "batch.sock")
		if got := len(batchSockPath); got <= 108 {
			t.Fatalf("batch.sock path length = %d, want > 108: %s", got, batchSockPath)
		}
		if _, err := os.Stat(batchSockPath); !os.IsNotExist(err) {
			t.Fatalf("expected no batch.sock file at %s, got err=%v", batchSockPath, err)
		}

		if got := len(filepath.Join(batchDir, "runs", "x", "run.sock")); got <= 108 {
			t.Fatalf("run.sock path length lower bound = %d, want > 108", got)
		}
		assertNoNamedFiles(t, batchesDir, "batch.sock", "run.sock")
	})

	t.Run("review daemon", func(t *testing.T) {
		repoDir := newPathlenRepo(t)
		t.Chdir(repoDir)
		initRunIntegrationRepoWithRemote(t, repoDir)

		sandmanDir := filepath.Join(repoDir, ".sandman")
		d := &review.Daemon{BaseDir: sandmanDir}
		if err := d.StartSocket(); err != nil {
			t.Fatalf("StartSocket: %v", err)
		}
		t.Cleanup(func() {
			if err := d.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
		})

		reviewSockPath := ReviewSocketPath(sandmanDir)
		if got := len(reviewSockPath); got <= 108 {
			t.Fatalf("review.sock path length = %d, want > 108: %s", got, reviewSockPath)
		}
		if _, err := os.Stat(reviewSockPath); !os.IsNotExist(err) {
			t.Fatalf("expected no review.sock file at %s, got err=%v", reviewSockPath, err)
		}

		conn, err := net.Dial("unix", reviewAbstractSocketName())
		if err != nil {
			t.Fatalf("dial abstract review socket: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close review socket connection: %v", err)
		}
	})
}

func newPathlenRepo(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", pathlenTempPrefix)
	if err != nil {
		t.Fatalf("create long-path repo: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func firstChildDir(t *testing.T, dir string) string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(dir, entry.Name())
		}
	}
	t.Fatalf("no child directory found under %s", dir)
	return ""
}

func assertNoNamedFiles(t *testing.T, root string, names ...string) {
	t.Helper()

	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[name] = struct{}{}
	}

	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if _, ok := blocked[d.Name()]; ok {
			return fmt.Errorf("found unexpected %s at %s", d.Name(), path)
		}
		return nil
	}); err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
}

func reviewAbstractSocketName() string {
	return "@sandman-" + fmt.Sprintf("%x", reviewSocketHashString("reviews"))
}

func reviewSocketHashString(s string) uint64 {
	h := uint64(0)
	for i, c := range s {
		h = h*31 + uint64(c) + uint64(i)
	}
	return h
}
