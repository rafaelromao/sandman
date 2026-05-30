//go:build e2e

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRalphGHShim(t *testing.T, dir string) {
	t.Helper()

	script := `#!/bin/sh
set -eu

shim_dir="__SHIM_DIR__"

case "$1" in
  issue)
    if [ "${2:-}" = "list" ]; then
      # gh issue list --search <query> --json number,title,body,labels --limit 100
      cat <<'JSON'
[{"number":1,"title":"Fix failing test","body":"Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]}]
JSON
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"number":1,"title":"Fix failing test","body":"Make Double(2) return 4."}
JSON
      exit 0
    fi
    ;;
  repo)
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"name":"sandman","owner":{"login":"rafaelromao"}}
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
      repos/rafaelromao/sandman/issues/1)
        cat <<'JSON'
{"number":1,"state":"open","title":"Fix failing test","body":"Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}],"blocked_by":null,"issue_dependencies":{},"issue_dependencies_summary":{}}
JSON
        exit 0
        ;;
      repos/rafaelromao/sandman/issues/1/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %s\n' "$path" >&2
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

printf 'unexpected gh command: %s\n' "$*" >&2
exit 1
`
	script = strings.ReplaceAll(script, "__SHIM_DIR__", dir)
	ghPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
}

func writeRalphFakeAgent(t *testing.T, dir string) {
	t.Helper()

	script := `#!/bin/sh
set -eu

# Selection phase: when selection-prompt.md exists, write selected-issues.json
if [ -f ".sandman/selection-prompt.md" ]; then
  mkdir -p .sandman
  printf '[1]\n' > .sandman/selected-issues.json
  exit 0
fi

# Run phase: just succeed
exit 0
`
	for _, name := range []string{"opencode", "pi"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(script), 0755); err != nil {
			t.Fatalf("write fake agent %s: %v", name, err)
		}
	}
}

func TestRun_RalphFlow_SelectsIssueViaAgentAndRunsBatchInWorktree(t *testing.T) {
	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	remoteDir := initRunIntegrationRepoWithRemote(t, repoDir)
	runGit(t, repoDir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	ghShimDir := t.TempDir()
	writeRalphGHShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	agentDir := t.TempDir()
	writeRalphFakeAgent(t, agentDir)
	prependPath(t, agentDir)

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

	scaffoldFiles := []string{
		".sandman/config.yaml",
		".sandman/Dockerfile",
		".sandman/prompt.md",
		".sandman/priority-selection-prompt.md",
	}
	for _, rel := range scaffoldFiles {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	out, err = runSandmanBinary(t, binPath, repoDir, "run", "--ralph=1", "--sandbox", "worktree")
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
