package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextRolloverContainerSandboxRestoresHostPathsBeforeReuse(t *testing.T) {
	repoPath := t.TempDir()
	workDir := filepath.Join(repoPath, "worktree")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	gitFile := filepath.Join(workDir, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: /workspace/.git/worktrees/context\n"), 0o644); err != nil {
		t.Fatalf("write container git pointer: %v", err)
	}

	worktree := &fakeWorktreeForContainer{workDir: workDir}
	sb := NewContainerSandbox(worktree, &fakeContainer{id: "context"}, "docker", repoPath)
	if err := sb.RestoreHostPaths(); err != nil {
		t.Fatalf("restore host paths: %v", err)
	}
	restored, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatalf("read restored git pointer: %v", err)
	}
	if strings.Contains(string(restored), "/workspace") || !strings.Contains(string(restored), repoPath) {
		t.Fatalf("restored git pointer = %q, want host path", restored)
	}

	if err := sb.rewriteGitPaths(); err != nil {
		t.Fatalf("reapply container paths: %v", err)
	}
	rewritten, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatalf("read rewritten git pointer: %v", err)
	}
	if !strings.Contains(string(rewritten), "/workspace") {
		t.Fatalf("rewritten git pointer = %q, want container path", rewritten)
	}
}
