//go:build e2e

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestBadge_E2E_HappyPath(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	allowed, err := parseBadgeE2EProviders()
	if err != nil {
		t.Fatal(err)
	}
	if allowed != nil && !allowed["badge"] {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	remoteDir := filepath.Join(repoDir, "remote")
	if err := os.MkdirAll(remoteDir, 0755); err != nil {
		t.Fatalf("create remote dir: %v", err)
	}
	bareInit := exec.Command("git", "init", "--bare")
	bareInit.Dir = remoteDir
	if out, err := bareInit.CombinedOutput(); err != nil {
		t.Fatalf("init bare remote: %v: %s", err, out)
	}
	runGit(t, repoDir, "remote", "add", "origin", remoteDir)
	runGit(t, repoDir, "push", "-u", "origin", "main")

	seedBadgeTestRepo(t, repoDir)
	runGit(t, repoDir, "remote", "set-url", "origin", "git@github.com:rafaelromao/sandman.git")

	ghShimDir := t.TempDir()
	writeBadgeGHShim(t, ghShimDir, repoDir)
	prependPath(t, ghShimDir)

	deps := badgeTestDeps(repoDir)
	runRootCommand(t, deps, "init", "--agent", "opencode")
	runRootCommand(t, deps, "config", "set", "review_command", "/oc review")

	badgeGHShimDir := filepath.Join(repoDir, ".sandman", "bin")
	writeBadgeGHShimForContainer(t, badgeGHShimDir, repoDir)

	out, err := runRootCommand(t, deps, "run", "--agent", "opencode", "--sandbox", "worktree", "1")
	t.Logf("sandman run returned err=%v output=%s", err, out)

	if err != nil {
		t.Fatalf("sandman run failed: %v output=%s", err, out)
	}

	badgeStateDir := filepath.Join(repoDir, ".sandman", "badge-state")
	badgePRFile := filepath.Join(badgeStateDir, "badge-pr.json")
	if _, err := os.Stat(badgePRFile); err != nil {
		t.Fatalf("expected badge PR to be created at %s: %v", badgePRFile, err)
	}

	badgePRData, err := os.ReadFile(badgePRFile)
	if err != nil {
		t.Fatalf("read badge PR file: %v", err)
	}
	badgePRContent := string(badgePRData)

	if !strings.Contains(badgePRContent, `"headRefName":"sandman/built-with-sandman"`) {
		t.Errorf("expected badge PR head to be sandman/built-with-sandman, got: %s", badgePRContent)
	}
	if !strings.Contains(badgePRContent, `"title":"chore: add Built with Sandman badge"`) {
		t.Errorf("expected badge PR title to be 'chore: add Built with Sandman badge', got: %s", badgePRContent)
	}

	readmePath := filepath.Join(repoDir, "README.md")
	readmeData, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readmeContent := string(readmeData)

	if !strings.Contains(readmeContent, "[![Built with Sandman]") {
		t.Errorf("expected README to contain badge markdown, got: %s", readmeContent)
	}
}

func seedBadgeTestRepo(t *testing.T, dir string) {
	t.Helper()

	files := map[string]string{
		"go.mod": `module example.com/badge

go 1.24
`,
		"double.go": `package badge

func Double(n int) int {
	return 0
}
`,
		"double_test.go": `package badge

import "testing"

func TestDouble(t *testing.T) {
	if got := Double(2); got != 4 {
		t.Fatalf("Double(2) = %d, want 4", got)
	}
}
`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "feat: seed failing test")
	runGit(t, dir, "push", "origin", "main")
}

func writeBadgeGHShim(t *testing.T, dir string, repoDir string) {
	t.Helper()

	badgeStateDir := filepath.Join(repoDir, ".sandman", "badge-state")
	script := `#!/bin/sh
set -eu

shim_dir="__SHIM_DIR__"
badge_state_dir="__BADGE_STATE_DIR__"

mkdir -p "$badge_state_dir"

case "$1" in
  pr)
    if [ "${2:-}" = "list" ]; then
      state_filter=""
      json_fields="number,headRefName,title"
      while [ $# -gt 0 ]; do
        case "$1" in
          --state)
            state_filter="${2:-}"
            shift 2
            ;;
          --json)
            json_fields="${2:-}"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done

      if [ "$state_filter" = "merged" ]; then
        issue_pr_file="$badge_state_dir/issue-pr.json"
        if [ -f "$issue_pr_file" ]; then
          cat "$issue_pr_file"
        else
          echo '[]'
        fi
        exit 0
      fi

      if [ "$state_filter" = "all" ]; then
        badge_pr_file="$badge_state_dir/badge-pr.json"
        if [ -f "$badge_pr_file" ]; then
          cat "$badge_pr_file"
        else
          echo '[]'
        fi
        exit 0
      fi

      echo '[]'
      exit 0
    fi
    if [ "${2:-}" = "create" ]; then
      shift 2

      head_val=""
      base_val=""
      title_val=""
      body_val=""

      while [ $# -gt 0 ]; do
        case "$1" in
          --head)
            head_val="${2:-}"
            shift 2
            ;;
          --base)
            base_val="${2:-}"
            shift 2
            ;;
          --title)
            title_val="${2:-}"
            shift 2
            ;;
          --body-file)
            body_val="$(cat "${2:-}")"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done

      if echo "$head_val" | grep -q "sandman/built-with-sandman"; then
        printf '%s' "$body_val" > "$badge_state_dir/badge-pr-body.txt"
        badge_pr_json=$(printf '{"number":99,"state":"open","headRefName":"%s","title":"%s"}' "$head_val" "$title_val")
        printf '%s\n' "$badge_pr_json" > "$badge_state_dir/badge-pr.json"
        printf 'https://example.test/example/sandbox/pull/99\n'
        exit 0
      fi

      issue_pr_json=$(printf '{"number":1,"state":"merged","headRefName":"%s","title":"%s"}' "$head_val" "$title_val")
      printf '%s\n' "$issue_pr_json" > "$badge_state_dir/issue-pr.json"
      printf 'https://example.test/example/sandbox/pull/1\n'
      exit 0
    fi
    ;;
  repo)
    if [ "${2:-}" = "view" ]; then
      json_field=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --json)
            json_field="${2:-}"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done
      case "$json_field" in
        name*)
          printf '{"name":"badge-test","owner":{"login":"example"}}\n'
          ;;
        description*)
          printf '{"description":"A test repo for badge PR"}\n'
          ;;
        defaultBranchRef*)
          printf '{"defaultBranchRef":{"name":"main"}}\n'
          ;;
        *)
          printf '{"name":"badge-test","owner":{"login":"example"}}\n'
          ;;
      esac
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
        --repo)
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
      repos/example/badge-test/issues/1)
        cat <<'JSON'
{"number":1,"title":"Fix failing test","body":"Run go test -run TestDouble ./... Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/badge-test/issues/1/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %s\n' "$path" >&2
    exit 1
    ;;
  auth)
    if [ "${2:-}" = "token" ]; then
      printf 'ghp_xxxxxxxxxxxxxxxxxxxx\n'
      exit 0
    fi
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
	script = strings.ReplaceAll(script, "__BADGE_STATE_DIR__", badgeStateDir)

	ghPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
}

func writeBadgeGHShimForContainer(t *testing.T, hostDir string, repoDir string) {
	t.Helper()

	containerShimDir := "/workspace/.sandman/bin"
	badgeStateDir := "/workspace/.sandman/badge-state"

	script := `#!/bin/sh
set -eu

shim_dir="__SHIM_DIR__"
badge_state_dir="__BADGE_STATE_DIR__"

mkdir -p "$badge_state_dir"

case "$1" in
  pr)
    if [ "${2:-}" = "list" ]; then
      state_filter=""
      json_fields="number,headRefName,title"
      while [ $# -gt 0 ]; do
        case "$1" in
          --state)
            state_filter="${2:-}"
            shift 2
            ;;
          --json)
            json_fields="${2:-}"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done

      if [ "$state_filter" = "merged" ]; then
        issue_pr_file="$badge_state_dir/issue-pr.json"
        if [ -f "$issue_pr_file" ]; then
          cat "$issue_pr_file"
        else
          echo '[]'
        fi
        exit 0
      fi

      if [ "$state_filter" = "all" ]; then
        badge_pr_file="$badge_state_dir/badge-pr.json"
        if [ -f "$badge_pr_file" ]; then
          cat "$badge_pr_file"
        else
          echo '[]'
        fi
        exit 0
      fi

      echo '[]'
      exit 0
    fi
    if [ "${2:-}" = "create" ]; then
      shift 2

      head_val=""
      base_val=""
      title_val=""
      body_val=""

      while [ $# -gt 0 ]; do
        case "$1" in
          --head)
            head_val="${2:-}"
            shift 2
            ;;
          --base)
            base_val="${2:-}"
            shift 2
            ;;
          --title)
            title_val="${2:-}"
            shift 2
            ;;
          --body-file)
            body_val="$(cat "${2:-}")"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done

      if echo "$head_val" | grep -q "sandman/built-with-sandman"; then
        printf '%s' "$body_val" > "$badge_state_dir/badge-pr-body.txt"
        badge_pr_json=$(printf '{"number":99,"state":"open","headRefName":"%s","title":"%s"}' "$head_val" "$title_val")
        printf '%s\n' "$badge_pr_json" > "$badge_state_dir/badge-pr.json"
        printf 'https://example.test/example/sandbox/pull/99\n'
        exit 0
      fi

      issue_pr_json=$(printf '{"number":1,"state":"merged","headRefName":"%s","title":"%s"}' "$head_val" "$title_val")
      printf '%s\n' "$issue_pr_json" > "$badge_state_dir/issue-pr.json"
      printf 'https://example.test/example/sandbox/pull/1\n'
      exit 0
    fi
    ;;
  repo)
    if [ "${2:-}" = "view" ]; then
      json_field=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --json)
            json_field="${2:-}"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done
      case "$json_field" in
        name*)
          printf '{"name":"badge-test","owner":{"login":"example"}}\n'
          ;;
        description*)
          printf '{"description":"A test repo for badge PR"}\n'
          ;;
        defaultBranchRef*)
          printf '{"defaultBranchRef":{"name":"main"}}\n'
          ;;
        *)
          printf '{"name":"badge-test","owner":{"login":"example"}}\n'
          ;;
      esac
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
        --repo)
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
      repos/example/badge-test/issues/1)
        cat <<'JSON'
{"number":1,"title":"Fix failing test","body":"Run go test -run TestDouble ./... Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/badge-test/issues/1/events)
        printf '[]\n'
        exit 0
        ;;
    esac
    printf 'unexpected gh api path: %s\n' "$path" >&2
    exit 1
    ;;
  auth)
    if [ "${2:-}" = "token" ]; then
      printf 'ghp_xxxxxxxxxxxxxxxxxxxx\n'
      exit 0
    fi
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

	script = strings.ReplaceAll(script, "__SHIM_DIR__", containerShimDir)
	script = strings.ReplaceAll(script, "__BADGE_STATE_DIR__", badgeStateDir)

	if err := os.MkdirAll(hostDir, 0755); err != nil {
		t.Fatalf("create gh shim dir: %v", err)
	}
	ghPath := filepath.Join(hostDir, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	badgeStateContainerPath := "/workspace/.sandman/badge-state"
	dockerfileContent := string(dockerfile)
	if !strings.Contains(dockerfileContent, "COPY .sandman/bin/gh") {
		dockerfileContent += "\nCOPY .sandman/bin/gh /usr/local/bin/gh\nRUN chmod +x /usr/local/bin/gh\n"
	}
	if !strings.Contains(dockerfileContent, badgeStateContainerPath) {
		dockerfileContent += "\nRUN mkdir -p " + badgeStateContainerPath + "\n"
	}
	if err := os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644); err != nil {
		t.Fatalf("append gh shim to Dockerfile: %v", err)
	}
}

func badgeTestDeps(repoDir string) Dependencies {
	cfgStore := &config.FileStore{Path: filepath.Join(repoDir, ".sandman", "config.yaml")}
	eventLog := &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")}

	return Dependencies{
		BatchRunner:  batch.NewOrchestrator(&github.CLIClient{}, &prompt.Engine{}, cfgStore, eventLog),
		ConfigStore:  cfgStore,
		EventLog:     eventLog,
		GitHubClient: &github.CLIClient{},
		Renderer:     &prompt.Engine{},
		IssuePicker:  &SimpleIssuePicker{},
		IsTTY:        isStdoutTTY,
	}
}

func parseBadgeE2EProviders() (map[string]bool, error) {
	raw := os.Getenv("SANDMAN_E2E_GATES")
	if raw == "" {
		return nil, nil
	}
	return testenv.ParseList(raw, []string{testenv.E2EScenarioBadge, testenv.E2EScenarioBatch, testenv.E2EScenarioContinueMulti, testenv.E2EScenarioOpencodeSubagent}, "scenario")
}
