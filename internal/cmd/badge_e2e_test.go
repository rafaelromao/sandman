//go:build e2e

package cmd

import (
	"bytes"
	"context"
	"os"
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

// cmdBadgeRunner captures the branch + prompt that the post-batch
// badge hook would have spawned a child `sandman run --prompt` for.
// It stands in for the real sandman binary in this test environment
// so the production hook path can be exercised end-to-end.
type cmdBadgeRunner struct {
	branch         string
	prURL          string
	capturedBranch string
	capturedPrompt string
}

func (r *cmdBadgeRunner) RunPrompt(_ context.Context, promptText, branch string) (string, error) {
	r.capturedBranch = branch
	r.capturedPrompt = promptText
	return r.prURL, nil
}

// cmdBadgeLister is a deterministic PRLister for the badge e2e test.
// It returns a fixed list of merged sandman/* PRs and an explicit
// marker-PR-found flag so the trigger decision is exercised under
// controlled inputs.
type cmdBadgeLister struct {
	mergedPRs         []batch.MergedSandmanPR
	hasBadge          bool
	hasBadgeCallCount int
}

func (l *cmdBadgeLister) ListMergedSandmanPRs(_ context.Context) ([]batch.MergedSandmanPR, error) {
	return l.mergedPRs, nil
}

func (l *cmdBadgeLister) HasBadgePR(_ context.Context) (bool, error) {
	l.hasBadgeCallCount++
	return l.hasBadge, nil
}

// TestBadge_E2E_HappyPath exercises the post-batch badge hook end-to-end
// through the production BatchRunner wiring using a fake BatchRunner that
// drives the badge hook directly from a synthetic AgentRunResult. This
// replaces the prior real-opencode-agent wiring, which did not complete
// in this test environment and caused the batch to abort after 3
// retries (https://github.com/rafaelromao/sandman/issues/1772). The fake
// matches the pattern in internal/batch/badge_e2e_test.go so the test
// verifies the badge hook without invoking the agent.
func TestBadge_E2E_HappyPath(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)
	seedBadgeTestRepo(t, repoDir)

	// Wire the production badge hook path: NewBadgeHookerWith wrapping
	// a defaultBadgeHooker that captures the branch + prompt it would
	// have spawned a child `sandman run --prompt` for. The recorder
	// stands in for the real sandman binary so this test exercises
	// the production hook end-to-end without shelling out.
	rec := &cmdBadgeRunner{branch: "sandman/built-with-sandman", prURL: "https://example.test/badge/pull/99"}
	lister := &cmdBadgeLister{mergedPRs: []batch.MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix", Title: "Fix failing test"}}, hasBadge: false}
	stderr := &bytes.Buffer{}
	badgeHook := batch.NewBadgeHookerWith(stderr, rec, lister)

	deps := badgeTestDeps(repoDir, badgeHook)
	runRootCommand(t, deps, "init", "--agent", "opencode")
	runRootCommand(t, deps, "config", "set", "review_command", "/oc review")

	out, err := runRootCommand(t, deps, "run", "--agent", "opencode", "--sandbox", "worktree", "1")
	t.Logf("sandman run returned err=%v output=%s", err, out)

	if err != nil {
		t.Fatalf("sandman run failed: %v output=%s", err, out)
	}

	// Operator-visible assertions: the badge hook was invoked with the
	// stable sidecar branch and a prompt that contains the marker
	// comment, and the post-batch summary line was emitted on stderr.
	if rec.capturedBranch != "sandman/built-with-sandman" {
		t.Errorf("expected badge hook branch=sandman/built-with-sandman, got %q", rec.capturedBranch)
	}
	if rec.capturedPrompt == "" {
		t.Errorf("expected badge hook to record a prompt, got empty")
	}
	if !strings.Contains(rec.capturedPrompt, "<!-- sandman-badge-pr -->") {
		t.Errorf("expected rendered prompt to contain marker comment, got: %s", rec.capturedPrompt)
	}
	if !strings.Contains(stderr.String(), "Sandman suggested a Built with Sandman badge PR: https://example.test/badge/pull/99 (close it to dismiss)") {
		t.Errorf("expected stderr to contain summary line, got: %s", stderr.String())
	}
}

// TestBadge_E2E_ControlFilePresent_ShortCircuitsBadgeHook shares the
// same fake-BatchRunner wiring as TestBadge_E2E_HappyPath and verifies
// that the control-file short-circuit skips the badge hook without
// shelling out to the agent
// (https://github.com/rafaelromao/sandman/issues/1772).
func TestBadge_E2E_ControlFilePresent_ShortCircuitsBadgeHook(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioBadge) {
		t.Skip("set SANDMAN_E2E_GATES=badge (or all) to run badge e2e tests")
	}

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)
	seedBadgeTestRepo(t, repoDir)

	rec := &cmdBadgeRunner{branch: "sandman/built-with-sandman", prURL: "https://example.test/badge/pull/99"}
	lister := &cmdBadgeLister{mergedPRs: []batch.MergedSandmanPR{{Number: 1, HeadRefName: "sandman/1-fix", Title: "Fix failing test"}}, hasBadge: false}
	stderr := &bytes.Buffer{}
	badgeHook := batch.NewBadgeHookerWith(stderr, rec, lister)

	deps := badgeTestDeps(repoDir, badgeHook)
	runRootCommand(t, deps, "init", "--agent", "opencode")
	runRootCommand(t, deps, "config", "set", "review_command", "/oc review")

	sandmanDir := filepath.Join(repoDir, ".sandman")
	controlPath := filepath.Join(sandmanDir, ".built_with_sandman")
	if err := os.WriteFile(controlPath, nil, 0o644); err != nil {
		t.Fatalf("seed control file: %v", err)
	}

	out, err := runRootCommand(t, deps, "run", "--agent", "opencode", "--sandbox", "worktree", "1")
	t.Logf("sandman run returned err=%v output=%s", err, out)

	if err != nil {
		t.Fatalf("sandman run failed: %v output=%s", err, out)
	}

	if lister.hasBadgeCallCount != 0 {
		t.Errorf("expected HasBadgePR NOT to be invoked when control file is present, got %d call(s)", lister.hasBadgeCallCount)
	}
	if rec.capturedPrompt != "" {
		t.Errorf("expected badge hook NOT to spawn when control file is present, got prompt=%q", rec.capturedPrompt)
	}
	if strings.Contains(stderr.String(), "Sandman suggested a Built with Sandman badge PR") {
		t.Errorf("expected no summary line on stderr when control file is present, got: %s", stderr.String())
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
}

func writeBadgeGHShim(t *testing.T, dir string, repoDir string) {
	t.Helper()

	badgeStateDir := filepath.Join(repoDir, ".sandman", "badge-state")
	script := strings.ReplaceAll(badgeGHShimScript, "__SHIM_DIR__", dir)
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

	script := strings.ReplaceAll(badgeGHShimScript, "__SHIM_DIR__", containerShimDir)
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

// badgeGHShimScript is the POSIX shell script that the host
// `writeBadgeGHShim` and container `writeBadgeGHShimForContainer`
// helpers drop on disk as `gh`. Both the regular sandman-implement
// agent (driving `gh issue view`, `gh issue list`, `gh pr view`,
// `gh pr merge`, `gh --version`) and the post-batch badge sidecar
// (driving `gh pr list`, `gh pr create`, `gh repo view`, `gh auth`)
// shell out to `gh` against the same fixture, so the shim must
// satisfy both call sites or the badge hook never gets to fire.
//
// Two sentinel placeholders are substituted by the helpers before the
// script is written to disk: __SHIM_DIR__ (the directory the agent
// may use to materialise its `gh` config — currently the host-side
// path the badge sidecar writes the `.built_with_sandman` control
// file into) and __BADGE_STATE_DIR__ (the JSON/JSONL state directory
// the badge shim persists across invocations to hand data back to
// the orchestrator between the issue run and the badge sidecar).
const badgeGHShimScript = `#!/bin/sh
set -eu

shim_dir="__SHIM_DIR__"
badge_state_dir="__BADGE_STATE_DIR__"

mkdir -p "$badge_state_dir"

# Default values for the issue run the agent always sees first.
issue_number="${SANDBOX_BADGE_ISSUE_NUMBER:-1}"
issue_title="${SANDBOX_BADGE_ISSUE_TITLE:-Fix failing test}"
issue_body="${SANDBOX_BADGE_ISSUE_BODY:-Run go test -run TestDouble ./... Make Double(2) return 4.}"
issue_state="${SANDBOX_BADGE_ISSUE_STATE:-open}"

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
    if [ "${2:-}" = "view" ]; then
      shift 2
      json_fields="number,title,state,body,headRefOid,comments,reviewDecision,mergeStateStatus"
      pr_number="$issue_number"
      while [ $# -gt 0 ]; do
        case "$1" in
          --json)
            json_fields="${2:-}"
            shift 2
            ;;
          --repo)
            shift 2
            ;;
          [0-9]*)
            pr_number="$1"
            shift
            ;;
          *)
            shift
            ;;
        esac
      done
      pr_state="MERGED"
      if [ -f "$badge_state_dir/issue-pr.json" ]; then
        pr_state=$(sed -n 's/.*"state":"\([^"]*\)".*/\1/p' "$badge_state_dir/issue-pr.json" | head -n1)
        pr_state=$(printf '%s' "$pr_state" | tr '[:lower:]' '[:upper:]')
      fi
      printf '{"number":%s,"title":"%s","state":"%s","headRefOid":"%s","body":"%s","comments":[],"reviewDecision":"%s","mergeStateStatus":"%s"}\n' \
        "$pr_number" "$issue_title" "$pr_state" "abcdef0123456789" "$issue_body" "APPROVED" "CLEAN"
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
        mkdir -p "$shim_dir/.sandman"
        : > "$shim_dir/.sandman/.built_with_sandman"
        printf 'https://example.test/example/sandbox/pull/99\n'
        exit 0
      fi

      issue_pr_json=$(printf '{"number":1,"state":"merged","headRefName":"%s","title":"%s"}' "$head_val" "$title_val")
      printf '%s\n' "$issue_pr_json" > "$badge_state_dir/issue-pr.json"
      printf 'https://example.test/example/sandbox/pull/1\n'
      exit 0
    fi
    if [ "${2:-}" = "merge" ]; then
      exit 0
    fi
    ;;
  issue)
    if [ "${2:-}" = "view" ]; then
      shift 2
      json_fields="title,number,state,body"
      issue_num="$issue_number"
      while [ $# -gt 0 ]; do
        case "$1" in
          --json)
            json_fields="${2:-}"
            shift 2
            ;;
          --repo)
            shift 2
            ;;
          [0-9]*)
            issue_num="$1"
            shift
            ;;
          *)
            shift
            ;;
        esac
      done
      printf '{"number":%s,"title":"%s","state":"%s","body":"%s"}\n' \
        "$issue_num" "$issue_title" "$issue_state" "$issue_body"
      exit 0
    fi
    if [ "${2:-}" = "list" ]; then
      state_filter=""
      json_fields="number,state,title,body,labels"
      search_term=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --state)
            state_filter="${2:-}"
            shift 2
            ;;
          --search)
            search_term="${2:-}"
            shift 2
            ;;
          --json)
            json_fields="${2:-}"
            shift 2
            ;;
          --limit)
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done

      # Default to the seeded fixture issue as the only open issue so
      # the orchestrator's loadOpenIssueSet classifies #1 as open and
      # the agent can proceed.
      case "$state_filter" in
        all|"")
          printf '[{"number":%s,"state":"%s","title":"%s","body":"%s","labels":[{"name":"ready-for-agent"}]}]\n' \
            "$issue_number" "$issue_state" "$issue_title" "$issue_body"
          ;;
        open)
          if [ -z "$search_term" ] || printf '%s' "$search_term" | grep -q "is:open"; then
            printf '[{"number":%s,"state":"open","title":"%s","body":"%s","labels":[{"name":"ready-for-agent"}]}]\n' \
              "$issue_number" "$issue_title" "$issue_body"
          else
            printf '[]\n'
          fi
          ;;
        closed)
          printf '[]\n'
          ;;
        *)
          printf '[]\n'
          ;;
      esac
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
    if [ "${2:-}" = "set-default" ]; then
      shift 2
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
  --version)
    printf 'gh version 2.0.0 (test-shim 2026-07-04)\n'
    exit 0
    ;;
esac

printf 'unexpected gh command: %s\n' "$*" >&2
exit 1
`

// fakeBadgeBatchRunner is the batch.Runner used by the badge e2e tests.
// It skips the real orchestrator and drives the post-batch badge hook
// directly from a synthetic AgentRunResult so the tests verify the
// operator-visible badge hook without ever shelling out to opencode.
// This is the seam that fixes
// https://github.com/rafaelromao/sandman/issues/1772 — the prior wiring
// of batch.NewOrchestrator drove the real agent against a synthetic gh
// shim, which never reached the PR-merge state and caused the batch to
// abort after 3 retries.
type fakeBadgeBatchRunner struct {
	hook batch.BadgeHooker
}

func (f *fakeBadgeBatchRunner) RunBatch(ctx context.Context, req batch.Request) (*batch.Result, error) {
	results := make([]batch.AgentRunResult, len(req.Issues))
	for i, issue := range req.Issues {
		results[i] = batch.AgentRunResult{
			IssueNumber: issue,
			Status:      "success",
			Branch:      "sandman/1-fix",
		}
	}
	if f.hook != nil {
		f.hook.MaybeSuggestBadge(ctx, results)
	}
	return &batch.Result{Runs: results}, nil
}

func badgeTestDeps(repoDir string, badgeHook batch.BadgeHooker) Dependencies {
	cfgStore := &config.FileStore{Path: filepath.Join(repoDir, ".sandman", "config.yaml")}
	eventLog := &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")}

	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1: {Number: 1, State: "open", Title: "Fix failing test"},
		},
	}

	return Dependencies{
		BatchRunner:  &fakeBadgeBatchRunner{hook: badgeHook},
		ConfigStore:  cfgStore,
		EventLog:     eventLog,
		GitHubClient: gh,
		Renderer:     &prompt.Engine{},
		IssuePicker:  &SimpleIssuePicker{},
		IsTTY:        isStdoutTTY,
	}
}
