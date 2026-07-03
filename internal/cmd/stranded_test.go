package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestStrandedCmd_ListsAllStrandedWorktrees(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("git worktree add --force behavior differs on macOS; tracked by #1736")
	}
	repoDir := t.TempDir()
	initRunIntegrationRepo(t, repoDir)
	t.Chdir(repoDir)

	runGit(t, repoDir, "branch", "sandman/1-healthy")
	runGit(t, repoDir, "branch", "sandman/2-wrong")
	runGit(t, repoDir, "branch", "sandman/3-detached")

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, "sandman/1-healthy"), "sandman/1-healthy")
	runGit(t, repoDir, "worktree", "add", "--force", filepath.Join(worktreeBase, "sandman/2-wrong"), "sandman/1-healthy")
	runGit(t, repoDir, "worktree", "add", filepath.Join(worktreeBase, "sandman/3-detached"), "sandman/3-detached")
	runGit(t, filepath.Join(worktreeBase, "sandman/3-detached"), "checkout", "--detach", "HEAD")

	cmd := NewStrandedCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeBase}},
	})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stranded command failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sandman/2-wrong") {
		t.Errorf("expected output to mention sandman/2-wrong, got: %q", out)
	}
	if !strings.Contains(out, "sandman/3-detached") {
		t.Errorf("expected output to mention sandman/3-detached, got: %q", out)
	}
	if !strings.Contains(out, "Run: git -C") {
		t.Errorf("expected remediation command in output, got: %q", out)
	}
	if strings.Contains(out, "sandman/1-healthy is on") {
		t.Errorf("healthy worktree should not be flagged, got: %q", out)
	}
}

func TestStrandedCmd_JSONOutput(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("git worktree add --force behavior differs on macOS; tracked by #1736")
	}
	repoDir := t.TempDir()
	initRunIntegrationRepo(t, repoDir)
	t.Chdir(repoDir)

	runGit(t, repoDir, "branch", "sandman/9-bad")

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}

	wtPath := filepath.Join(worktreeBase, "sandman/9-bad")
	runGit(t, repoDir, "worktree", "add", wtPath, "sandman/9-bad")
	runGit(t, wtPath, "checkout", "--detach", "HEAD")

	cmd := NewStrandedCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeBase}},
	})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stranded --json failed: %v", err)
	}
	var results []sandbox.StrandedWorktreeInfo
	if err := json.Unmarshal(buf.Bytes(), &results); err != nil {
		t.Fatalf("decode JSON: %v: %s", err, buf.String())
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if results[0].ExpectedBranch != "refs/heads/sandman/9-bad" {
		t.Errorf("ExpectedBranch: got %q, want %q", results[0].ExpectedBranch, "refs/heads/sandman/9-bad")
	}
	if results[0].ActualBranch != "" {
		t.Errorf("ActualBranch: got %q, want empty (detached)", results[0].ActualBranch)
	}
}

func TestStrandedCmd_DefaultsWorktreeDirFromConfig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("git worktree add --force behavior differs on macOS; tracked by #1736")
	}
	repoDir := t.TempDir()
	initRunIntegrationRepo(t, repoDir)
	t.Chdir(repoDir)

	runGit(t, repoDir, "branch", "sandman/5-stuck")

	worktreeBase := filepath.Join(repoDir, ".sandman", "worktrees")
	if err := os.MkdirAll(worktreeBase, 0755); err != nil {
		t.Fatalf("mkdir worktreeBase: %v", err)
	}
	runGit(t, repoDir, "branch", "sandman/1-other")
	runGit(t, repoDir, "worktree", "add", "--force", filepath.Join(worktreeBase, "sandman/5-stuck"), "sandman/1-other")

	cmd := NewStrandedCmd(Dependencies{
		ConfigStore: &fakeStore{config: &config.Config{WorktreeDir: worktreeBase}},
	})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stranded command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "sandman/5-stuck") {
		t.Errorf("expected default worktree_dir to be honored, got: %q", buf.String())
	}
}
