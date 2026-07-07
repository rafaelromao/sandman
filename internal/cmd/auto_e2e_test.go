//go:build e2e

package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
)

const (
	autoTestOwner = "rafaelromao"
	autoTestRepo  = "sandman"
)

func writeAutoGHShim(t *testing.T, dir string, candidatesJSON, issuesExtraJSON string) {
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
  pr)
    if [ "${2:-}" = "list" ]; then
      head=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --head)
            shift
            head="${1:-}"
            ;;
        esac
        shift
      done
      head_ref_oid="$(git rev-parse HEAD 2>/dev/null || printf 'abc123')"
      cat <<JSON
[{"number":1,"state":"merged","mergedAt":"2026-06-05T00:00:00Z","headRefName":"$head","headRefOid":"$head_ref_oid"}]
JSON
      exit 0
    fi
    if [ "${2:-}" = "create" ]; then
      printf 'https://github.com/%%s/%%s/pull/1\n' "$owner" "$repo"
      exit 0
    fi
    if [ "${2:-}" = "checks" ]; then
      printf 'all checks passed\n'
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
      printf 'https://github.com/%%s/%%s/pull/1\n' "$owner" "$repo"
      exit 0
    fi
    if [ "${2:-}" = "comment" ]; then
      printf 'commented\n'
      exit 0
    fi
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
`, autoTestOwner, autoTestRepo, candidatesJSON, autoTestRepo, autoTestOwner, autoTestOwner, autoTestRepo, autoTestOwner, autoTestRepo)

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

func writeAutoFakeAgent(t *testing.T, dir string, selectionIssuesJSON string) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
set -eu

# Selection phase: when selection-prompt.md exists, write selected-issues.json
if [ -f ".sandman/selection-prompt.md" ]; then
  mkdir -p .sandman/state
  cat > .sandman/state/selected-issues.json <<'ISSUES'
%s
ISSUES
  exit 0
fi

# Run phase: just succeed
exit 0
`, selectionIssuesJSON)
	for _, name := range []string{"opencode"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(script), 0755); err != nil {
			t.Fatalf("write fake agent %s: %v", name, err)
		}
	}
}

func setupAutoE2ETest(t *testing.T, binPath, repoDir, remoteDir string) {
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

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "go", "--tool-version", "latest", "--agent", "opencode")
	if err != nil {
		t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
	}

	if _, err := runSandmanBinary(t, binPath, repoDir, "config", "set", "review_command", "/oc review"); err != nil {
		t.Fatalf("sandman config set failed: %v", err)
	}

	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md", ".sandman/auto-selection-prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}
}

func sqliteStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func opencodeDBPath() string {
	home := os.Getenv("HOME")
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

func queryOpencodeSessionIDs(t *testing.T, repoDir string) []string {
	t.Helper()

	dbPath := opencodeDBPath()
	query := fmt.Sprintf(
		"select id from session where directory = %s or lower(title) like %s or lower(title) like %s order by id;",
		sqliteStringLiteral(repoDir),
		sqliteStringLiteral("afk auto mode issue selection%"),
		sqliteStringLiteral("auto mode issue selection%"),
	)
	out, err := exec.Command("sqlite3", "-noheader", dbPath, query).CombinedOutput()
	if err != nil {
		t.Fatalf("query opencode sessions: %v\n%s", err, out)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Fields(trimmed)
}

func cleanupOpencodeSessions(t *testing.T, repoDir string) {
	t.Helper()

	t.Cleanup(func() {
		if _, err := os.Stat(opencodeDBPath()); err != nil {
			return
		}

		ids := queryOpencodeSessionIDs(t, repoDir)
		if len(ids) == 0 {
			return
		}

		var sql strings.Builder
		sql.WriteString("PRAGMA foreign_keys=ON; BEGIN IMMEDIATE;")
		for _, id := range ids {
			quotedID := sqliteStringLiteral(id)
			sql.WriteString("DELETE FROM event WHERE aggregate_id = ")
			sql.WriteString(quotedID)
			sql.WriteString(";")
			sql.WriteString("DELETE FROM event_sequence WHERE aggregate_id = ")
			sql.WriteString(quotedID)
			sql.WriteString(";")
			sql.WriteString("DELETE FROM session WHERE id = ")
			sql.WriteString(quotedID)
			sql.WriteString(";")
		}
		sql.WriteString("COMMIT;")

		out, err := exec.Command("sqlite3", opencodeDBPath(), sql.String()).CombinedOutput()
		if err != nil {
			t.Fatalf("cleanup opencode sessions: %v\n%s", err, out)
		}
	})
}

func setupAutoSelectionRepo(t *testing.T, selectionJSON string) string {
	t.Helper()

	repoDir := t.TempDir()
	sandmanDir := filepath.Join(repoDir, ".sandman")
	if err := os.MkdirAll(sandmanDir, 0o755); err != nil {
		t.Fatalf("create sandman dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sandmanDir, "auto-selection-prompt.md"), []byte("custom prompt"), 0o644); err != nil {
		t.Fatalf("create auto-selection-prompt.md: %v", err)
	}
	cleanupOpencodeSessions(t, repoDir)

	agentDir := t.TempDir()
	writeAutoFakeAgent(t, agentDir, selectionJSON)
	t.Setenv("PATH", agentDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return repoDir
}

func autoSelectionConfig(autoMaxCount int) *config.Config {
	cfg := &config.Config{Agent: "opencode", ReviewCommand: "/oc review", AutoMaxCount: autoMaxCount}
	cfg.AgentProviders = map[string]config.Agent{
		"opencode": {Command: "opencode"},
	}
	return cfg
}

func jsonInts(nums ...int) string {
	if len(nums) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, n := range nums {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Itoa(n))
	}
	b.WriteByte(']')
	return b.String()
}

func runAutoSelectionCmd(t *testing.T, deps Dependencies, args ...string) (output string, err error) {
	t.Helper()

	var buf bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return buf.String(), err
}

var singleCandidateIssue = `[{"number":1,"title":"Fix failing test","body":"Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]}]`

var singleIssueData = `issue-1.json
{"number":1,"state":"open","title":"Fix failing test","body":"Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}],"blocked_by":null,"issue_dependencies":{},"issue_dependencies_summary":{}}
---
issue-view-1.json
{"number":1,"title":"Fix failing test","body":"Make Double(2) return 4."}`

func TestRun_AutoFlow_SelectsIssueViaAgentAndRunsBatchInWorktree(t *testing.T) {
	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	remoteDir := initRunIntegrationRepoWithRemote(t, repoDir)
	setupAutoE2ETest(t, binPath, repoDir, remoteDir)

	ghShimDir := t.TempDir()
	writeAutoGHShim(t, ghShimDir, singleCandidateIssue, singleIssueData)
	prependPath(t, ghShimDir)

	agentDir := t.TempDir()
	writeAutoFakeAgent(t, agentDir, "[1]")
	prependPath(t, agentDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "run", "--auto", "--count", "1", "--sandbox", "worktree")
	if err != nil {
		t.Fatalf("sandman run failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
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

func TestRun_AutoSelectionFlow_AgentSelectsSubsetOfCandidates(t *testing.T) {
	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	remoteDir := initRunIntegrationRepoWithRemote(t, repoDir)
	setupAutoE2ETest(t, binPath, repoDir, remoteDir)

	ghShimDir := t.TempDir()
	writeAutoGHShim(t, ghShimDir, multiCandidateIssues, multiIssueData)
	prependPath(t, ghShimDir)

	agentDir := t.TempDir()
	writeAutoFakeAgent(t, agentDir, "[5]")
	prependPath(t, agentDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "run", "--auto", "--count", "2", "--sandbox", "worktree")
	if err != nil {
		t.Fatalf("sandman run failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected success summary, got:\n%s", out)
	}
	if !strings.Contains(out, "#5  success") {
		t.Fatalf("expected issue #5 success in summary, got:\n%s", out)
	}
	// Agent selected [5] from candidates [1,5]; even though --count=2 allows
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

func TestRun_AutoSelectionFlow_AgentSelectsMultipleIssues(t *testing.T) {
	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	remoteDir := initRunIntegrationRepoWithRemote(t, repoDir)
	setupAutoE2ETest(t, binPath, repoDir, remoteDir)

	ghShimDir := t.TempDir()
	writeAutoGHShim(t, ghShimDir, multiCandidateIssues, multiIssueData)
	prependPath(t, ghShimDir)

	agentDir := t.TempDir()
	writeAutoFakeAgent(t, agentDir, "[1, 5]")
	prependPath(t, agentDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "run", "--auto", "--count", "2", "--sandbox", "worktree")
	if err != nil {
		t.Fatalf("sandman run failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 2 succeeded") {
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

func TestRun_AutoFlag_NoCount_UsesConfigDefault(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(1, 2, 3))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 3, Title: "Feature C"},
			{Number: 1, Title: "Feature A"},
			{Number: 2, Title: "Feature B"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(7)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{1, 2, 3}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	if gh.searchIssuesQuery != "label:ready-for-agent is:open" {
		t.Errorf("expected search query 'label:ready-for-agent is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_AutoFlag_CountFlagOverrides(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(1, 2))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 5, Title: "Feature E"},
			{Number: 2, Title: "Feature B"},
			{Number: 3, Title: "Feature C"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "--count", "2"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{1, 2}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_AutoFlag_DefaultCountIs50(t *testing.T) {
	candidates := make([]github.Issue, 0, 75)
	selected := make([]int, 0, 50)
	for i := 75; i >= 1; i-- {
		candidates = append(candidates, github.Issue{Number: i, Title: "Issue"})
		if i <= 50 {
			selected = append([]int{i}, selected...)
		}
	}
	repoDir := setupAutoSelectionRepo(t, jsonInts(selected...))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{searchIssuesResult: candidates}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(config.DefaultAutoMaxCount)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 50 {
		t.Fatalf("expected 50 issues (DefaultAutoMaxCount), got %d", len(spy.req.Issues))
	}
}

func TestRun_AutoFlag_ConfigZeroIsUnlimited(t *testing.T) {
	candidates := make([]github.Issue, 0, 75)
	selected := make([]int, 0, 75)
	for i := 75; i >= 1; i-- {
		candidates = append(candidates, github.Issue{Number: i, Title: "Issue"})
		selected = append([]int{i}, selected...)
	}
	repoDir := setupAutoSelectionRepo(t, jsonInts(selected...))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{searchIssuesResult: candidates}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(0)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 75 {
		t.Fatalf("expected 75 issues (unlimited), got %d", len(spy.req.Issues))
	}
}

func TestRun_AutoFlag_NotEnoughCandidatesStillDelegatesAll(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(1, 2))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 2, Title: "Feature B"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "--count", "5"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{1, 2}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
}

func TestRun_AutoFlag_WithLabelUsesLabelSearch(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(1))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{searchIssuesResult: []github.Issue{{Number: 1, Title: "Bug A"}}}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "--label", "bug"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_AutoFlag_WithQueryUsesRawQuery(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(3))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{searchIssuesResult: []github.Issue{{Number: 3, Title: "Feature A"}}}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "--query", "label:bug is:open"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if gh.searchIssuesQuery != "label:bug is:open" {
		t.Errorf("expected search query 'label:bug is:open', got %q", gh.searchIssuesQuery)
	}
}

func TestRun_AutoFlag_AcceptsExplicitIssueArgs(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(42, 43, 44))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, State: "open", Title: "Issue 42"},
			43: {Number: 43, State: "open", Title: "Issue 43"},
			44: {Number: 44, State: "open", Title: "Issue 44"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "42", "43", "44"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 43, 44}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_AutoFlag_ExplicitArgsAndCountCaps(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(42, 43))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, State: "open", Title: "Issue 42"},
			43: {Number: 43, State: "open", Title: "Issue 43"},
			44: {Number: 44, State: "open", Title: "Issue 44"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "--count", "2", "42", "43", "44"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 2 {
		t.Fatalf("expected 2 issues (count cap), got %d: %v", len(spy.req.Issues), spy.req.Issues)
	}
}

func TestRun_AutoFlag_AcceptsExplicitArgsAndLabel(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(42, 44))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, State: "open", Title: "Issue 42", Labels: []string{"bug"}},
			43: {Number: 43, State: "open", Title: "Issue 43", Labels: []string{"enhancement"}},
			44: {Number: 44, State: "open", Title: "Issue 44", Labels: []string{"bug"}},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "--label", "bug", "42", "43", "44"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{42, 44}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
}

func TestRun_AutoFlag_SetsConservativeDefaults(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(1, 5))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		searchIssuesResult: []github.Issue{
			{Number: 5, Title: "Feature E"},
			{Number: 1, Title: "Feature A"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "--count", "1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if spy.req.Parallel != 1 {
		t.Errorf("expected parallel=1, got %d", spy.req.Parallel)
	}
	if spy.req.ContainerCapacity != 1 {
		t.Errorf("expected container-capacity=1, got %d", spy.req.ContainerCapacity)
	}
	if spy.req.MaxContainers != 1 {
		t.Errorf("expected max-containers=1, got %d", spy.req.MaxContainers)
	}
	if spy.req.Retries != 3 {
		t.Errorf("expected retries=3, got %d", spy.req.Retries)
	}
}

func TestRun_AutoFlag_CapAppliesAfterExpansion(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(10))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD", Body: "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n- #11\n- #12\n", State: "open"},
			10: {Number: 10, Title: "Child 1", Body: "## Parent\n\n#1\n\n## What\n\n", State: "open"},
			11: {Number: 11, Title: "Child 2", Body: "## Parent\n\n#1\n\n## What\n\n", State: "open"},
			12: {Number: 12, Title: "Child 3", Body: "## Parent\n\n#1\n\n## What\n\n", State: "open"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "--count", "1", "1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	if len(spy.req.Issues) != 1 {
		t.Fatalf("expected 1 issue (cap=1 applied post-expansion), got %d: %v", len(spy.req.Issues), spy.req.Issues)
	}
	if spy.req.Issues[0] == 1 {
		t.Errorf("expected the post-expansion cap to drop the PRD, but #1 made it into the batch: %v", spy.req.Issues)
	}
	if spy.req.Issues[0] != 10 {
		t.Errorf("expected the smallest child #10 (numeric sort after cap), got #%d", spy.req.Issues[0])
	}
}

func TestRun_AutoFlag_ExpandedListReachesBatchRunner(t *testing.T) {
	repoDir := setupAutoSelectionRepo(t, jsonInts(10, 11, 12))
	spy := &spyBatchRunner{result: &batch.Result{}}
	gh := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			1:  {Number: 1, Title: "PRD", Body: "## Problem Statement\n\nP.\n\n## Solution\n\nS.\n\n## User Stories\n\n1. U.\n\n## Child Issues\n\n- #10\n- #11\n- #12\n", State: "open"},
			10: {Number: 10, Title: "Child 1", Body: "## Parent\n\n#1\n\n## What\n\n", State: "open"},
			11: {Number: 11, Title: "Child 2", Body: "## Parent\n\n#1\n\n## What\n\n", State: "open"},
			12: {Number: 12, Title: "Child 3", Body: "## Parent\n\n#1\n\n## What\n\n", State: "open"},
		},
	}
	deps := Dependencies{
		BatchRunner:  spy,
		ConfigStore:  &fakeStore{config: autoSelectionConfig(50)},
		EventLog:     &fakeEventLog{},
		GitHubClient: gh,
		IsTTY:        func() bool { return false },
		RepoRoot:     repoDir,
	}

	t.Chdir(repoDir)
	if _, err := runAutoSelectionCmd(t, deps, "--auto", "1", "10", "11", "12"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spy.called {
		t.Fatal("expected batch runner to be called")
	}
	want := []int{10, 11, 12}
	if len(spy.req.Issues) != len(want) {
		t.Fatalf("expected issues %v, got %v", want, spy.req.Issues)
	}
	for i, v := range want {
		if spy.req.Issues[i] != v {
			t.Errorf("expected issue %d at index %d, got %d", v, i, spy.req.Issues[i])
		}
	}
	for _, n := range spy.req.Issues {
		if n == 1 {
			t.Errorf("expected PRD #1 to NOT be in req.Issues, but it is: %v", spy.req.Issues)
		}
	}
	seen := make(map[int]struct{}, len(spy.req.Issues))
	for _, n := range spy.req.Issues {
		if _, dup := seen[n]; dup {
			t.Errorf("expected no duplicates in req.Issues, got %v", spy.req.Issues)
		}
		seen[n] = struct{}{}
	}
}
