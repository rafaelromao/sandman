//go:build e2e

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ralphTestOwner = "rafaelromao"
	ralphTestRepo  = "sandman"
)

func writeRalphGHShim(t *testing.T, dir string, candidatesJSON, issuesExtraJSON string) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
set -eu

shim_dir="__SHIM_DIR__"
owner="%s"
repo="%s"

case "$1" in
  issue)
    if [ "${2:-}" = "list" ]; then
      cat <<'JSON'
%s
JSON
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
      number="${3:-}"
      cat "$shim_dir/issue-view-$number.json" 2>/dev/null || {
        printf '{"error":"unknown issue %%s"}' "$number" >&2
        exit 1
      }
      exit 0
    fi
    ;;
  repo)
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"name":"%s","owner":{"login":"%s"}}
JSON
      exit 0
    fi
    ;;
  api)
    path=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -H)
          shift 2
          ;;
        repos/*)
          path="$1"
          shift
          ;;
        *)
          shift
          ;;
      esac
    done
    case "$path" in
      repos/%s/%s/issues/[0-9]*)
        number=$(printf '%%s' "$path" | sed 's|.*/issues/||')
        cat "$shim_dir/issue-$number.json" 2>/dev/null || {
          printf '{"error":"unknown api issue %%s"}' "$number" >&2
          exit 1
        }
        exit 0
        ;;
      repos/%s/%s/issues/[0-9]*/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %%s\n' "$path" >&2
    exit 1
    ;;
  auth)
    if [ "${2:-}" = "status" ]; then
      cat <<'JSON'
github.com
  ✓ Logged in to github.com as test-user (keyring)
  ✓ Git operations for github.com configured to use https protocol.
  ✓ Token: ghp_xxxxxxxxxxxxxxxxxxxx
JSON
      exit 0
    fi
    if [ "${2:-}" = "setup-git" ]; then
      exit 0
    fi
    ;;
esac

printf 'unexpected gh command: %%s\n' "$*" >&2
exit 1
`, ralphTestOwner, ralphTestRepo, candidatesJSON, ralphTestRepo, ralphTestOwner, ralphTestOwner, ralphTestRepo, ralphTestOwner, ralphTestRepo)

	script = strings.ReplaceAll(script, "__SHIM_DIR__", dir)
	ghPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	extraFiles := strings.Split(strings.TrimSpace(issuesExtraJSON), "\n---\n")
	for _, entry := range extraFiles {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\n", 2)
		if len(parts) != 2 {
			continue
		}
		filename := strings.TrimSpace(parts[0])
		content := strings.TrimSpace(parts[1])
		if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0644); err != nil {
			t.Fatalf("write gh shim data file %s: %v", filename, err)
		}
	}
}

func writeRalphFakeAgent(t *testing.T, dir string, selectionIssuesJSON string) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
set -eu

# Selection phase: when selection-prompt.md exists, write selected-issues.json
if [ -f ".sandman/selection-prompt.md" ]; then
  mkdir -p .sandman
  cat > .sandman/selected-issues.json <<'ISSUES'
%s
ISSUES
  exit 0
fi

# Run phase: just succeed
exit 0
`, selectionIssuesJSON)
	for _, name := range []string{"opencode", "pi"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(script), 0755); err != nil {
			t.Fatalf("write fake agent %s: %v", name, err)
		}
	}
}

func setupRalphE2ETest(t *testing.T, binPath, repoDir, remoteDir string) {
	t.Helper()
	runGit(t, repoDir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	isolatedHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(isolatedHome, ".ssh"), 0755); err != nil {
		t.Fatalf("create isolated ssh dir: %v", err)
	}
	gitConfig := fmt.Sprintf("[user]\n\tname = Test\n[url %q]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n", "file://"+remoteDir)
	if err := os.WriteFile(filepath.Join(isolatedHome, ".gitconfig"), []byte(gitConfig), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	t.Setenv("HOME", isolatedHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolatedHome, ".config"))

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "go", "--tool-version", "latest", "--default-agent", "opencode")
	if err != nil {
		t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
	}

	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md", ".sandman/priority-selection-prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}
}

var singleCandidateIssue = `[{"number":1,"title":"Fix failing test","body":"Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]}]`

var singleIssueData = `issue-1.json
{"number":1,"state":"open","title":"Fix failing test","body":"Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}],"blocked_by":null,"issue_dependencies":{},"issue_dependencies_summary":{}}
---
issue-view-1.json
{"number":1,"title":"Fix failing test","body":"Make Double(2) return 4."}`

func TestRun_RalphFlow_SelectsIssueViaAgentAndRunsBatchInWorktree(t *testing.T) {
	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	remoteDir := initRunIntegrationRepoWithRemote(t, repoDir)
	setupRalphE2ETest(t, binPath, repoDir, remoteDir)

	ghShimDir := t.TempDir()
	writeRalphGHShim(t, ghShimDir, singleCandidateIssue, singleIssueData)
	prependPath(t, ghShimDir)

	agentDir := t.TempDir()
	writeRalphFakeAgent(t, agentDir, "[1]")
	prependPath(t, agentDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "run", "--ralph=1", "--sandbox", "worktree")
	if err != nil {
		t.Fatalf("sandman run failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#1  success") {
		t.Fatalf("expected issue #1 success in summary, got:\n%s", out)
	}

	worktreePath := filepath.Join(repoDir, ".sandman", "worktrees", "sandman", "1-fix-failing-test")
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected worktree at %s, got: %v", worktreePath, err)
	}
}

var multiCandidateIssues = `[{"number":1,"title":"Fix failing test","body":"Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]},{"number":5,"title":"Add login page","body":"Users need a login form.","labels":[{"name":"ready-for-agent"}]}]`

var multiIssueData = `issue-1.json
{"number":1,"state":"open","title":"Fix failing test","body":"Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}],"blocked_by":null,"issue_dependencies":{},"issue_dependencies_summary":{}}
---
issue-view-1.json
{"number":1,"title":"Fix failing test","body":"Make Double(2) return 4."}
---
issue-5.json
{"number":5,"state":"open","title":"Add login page","body":"Users need a login form.","labels":[{"name":"ready-for-agent"}],"blocked_by":null,"issue_dependencies":{},"issue_dependencies_summary":{}}
---
issue-view-5.json
{"number":5,"title":"Add login page","body":"Users need a login form."}`

func TestRun_RalphSelectionFlow_AgentSelectsSubsetOfCandidates(t *testing.T) {
	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	remoteDir := initRunIntegrationRepoWithRemote(t, repoDir)
	setupRalphE2ETest(t, binPath, repoDir, remoteDir)

	ghShimDir := t.TempDir()
	writeRalphGHShim(t, ghShimDir, multiCandidateIssues, multiIssueData)
	prependPath(t, ghShimDir)

	agentDir := t.TempDir()
	writeRalphFakeAgent(t, agentDir, "[5]")
	prependPath(t, agentDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "run", "--ralph=2", "--sandbox", "worktree")
	if err != nil {
		t.Fatalf("sandman run failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#5  success") {
		t.Fatalf("expected issue #5 success in summary, got:\n%s", out)
	}
	// Agent selected [5] from candidates [1,5]; even though --ralph=2 allows
	// up to 2, the agent chose only 1. Verify #1 was NOT run.
	if strings.Contains(out, "#1  success") || strings.Contains(out, "#1  failure") {
		t.Fatalf("expected issue #1 not to be in summary, got:\n%s", out)
	}

	// Verify correct worktree was created (not issue 1's)
	worktreePath := filepath.Join(repoDir, ".sandman", "worktrees", "sandman", "5-add-login-page")
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected worktree at %s, got: %v", worktreePath, err)
	}
	unexpectedWorktree := filepath.Join(repoDir, ".sandman", "worktrees", "sandman", "1-fix-failing-test")
	if _, err := os.Stat(unexpectedWorktree); !os.IsNotExist(err) {
		t.Fatalf("expected no worktree for unselected issue 1")
	}
}

func TestRun_RalphSelectionFlow_AgentSelectsMultipleIssues(t *testing.T) {
	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	remoteDir := initRunIntegrationRepoWithRemote(t, repoDir)
	setupRalphE2ETest(t, binPath, repoDir, remoteDir)

	ghShimDir := t.TempDir()
	writeRalphGHShim(t, ghShimDir, multiCandidateIssues, multiIssueData)
	prependPath(t, ghShimDir)

	agentDir := t.TempDir()
	writeRalphFakeAgent(t, agentDir, "[1, 5]")
	prependPath(t, agentDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "run", "--ralph=2", "--sandbox", "worktree")
	if err != nil {
		t.Fatalf("sandman run failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 2 succeeded, 0 failed") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#1  success") {
		t.Fatalf("expected issue #1 success in summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#5  success") {
		t.Fatalf("expected issue #5 success in summary, got:\n%s", out)
	}

	for _, path := range []string{
		".sandman/worktrees/sandman/1-fix-failing-test",
		".sandman/worktrees/sandman/5-add-login-page",
	} {
		if _, err := os.Stat(filepath.Join(repoDir, path)); err != nil {
			t.Fatalf("expected worktree at %s, got: %v", path, err)
		}
	}
}
