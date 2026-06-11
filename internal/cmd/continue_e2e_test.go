//go:build e2e

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const continueE2EBranch = "sandman/1-fix-failing-test"

func TestContinueFlow_PodmanSandboxBinaryReusesContinuationContext(t *testing.T) {
	if !podmanAvailable(t) {
		return
	}

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	_ = initRunIntegrationRepoWithRemote(t, repoDir)

	isolatedHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(isolatedHome, ".ssh"), 0755); err != nil {
		t.Fatalf("create isolated ssh dir: %v", err)
	}
	t.Setenv("HOME", isolatedHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolatedHome, ".config"))

	ghShimDir := t.TempDir()
	writeMergedFakeGHShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "init")
	if err != nil {
		t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
	}

	if _, err := runSandmanBinary(t, binPath, repoDir, "config", "set", "review_command", "/oc review"); err != nil {
		t.Fatalf("sandman config set failed: %v", err)
	}
	forcePodmanSandbox(t, repoDir)
	writeMergedFakeGHShimForContainer(t, filepath.Join(repoDir, ".sandman", "bin"))
	installFakeOpenCodeForContainer(t, repoDir)

	// Pre-set the PR state to "merged" so checkPRMerged succeeds on first attempt.
	prStatePath := filepath.Join(repoDir, ".sandman", "bin", "pr-state-sandman_1-fix-failing-test")
	if err := os.WriteFile(prStatePath, []byte("merged\n"), 0644); err != nil {
		t.Fatalf("write pr state: %v", err)
	}

	out, err = runSandmanBinary(t, binPath, repoDir, "run", "--sandbox", "podman", "1")
	if err != nil {
		ghLog, _ := os.ReadFile(filepath.Join(ghShimDir, "gh-calls.log"))
		t.Fatalf("sandman run failed: %v\noutput:\n%s\ngh log:\n%s", err, out, ghLog)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected run success summary, got:\n%s", out)
	}

	worktreePath := filepath.Join(repoDir, ".sandman", "worktrees", continueE2EBranch)
	initialPrompt, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "rendered-prompt.md"))
	if err != nil {
		t.Fatalf("read rendered prompt: %v", err)
	}
	if !strings.Contains(string(initialPrompt), ".sandman/handoff.md") {
		t.Fatalf("expected rendered prompt to mention continuation context, got:\n%s", initialPrompt)
	}

	// Initial run succeeded and PR was merged, so handoff.md was cleaned up
	// by the orchestrator. Write a handoff.md manually to simulate the state
	// a previous run would have left before the first continue.
	contextPath := filepath.Join(worktreePath, ".sandman", "handoff.md")
	initialHandoffContent := fmt.Sprintf(`## Stage: plan-approved
## Source Prompt: .sandman/rendered-prompt.md
## Last Skill: sandman-tdd
## Last Skill Status: complete
## Completed
Initial run for %s.

## Pending
none

## Blockers
none

## Key Decisions
fake opencode for continuation e2e

## Next Step
continue the flow for %s
`, continueE2EBranch, continueE2EBranch)
	if err := os.WriteFile(contextPath, []byte(initialHandoffContent), 0644); err != nil {
		t.Fatalf("write initial handoff: %v", err)
	}
	// Reset the fake opencode step file so the first continue runs step 1.
	stepFilePath := filepath.Join(worktreePath, ".sandman", "fake-opencode-step-df007d4b37ed388b")
	if err := os.WriteFile(stepFilePath, []byte("1\n"), 0644); err != nil {
		t.Fatalf("write step file: %v", err)
	}

	cfgPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Git.BaseBranch = "trunk"
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	out, err = runSandmanBinary(t, binPath, repoDir, "continue", "1")
	if err != nil {
		t.Fatalf("first continue failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected first continue success summary, got:\n%s", out)
	}

	firstHandoffPrompt, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "handoff-prompt.md"))
	if err != nil {
		t.Fatalf("read first continue prompt: %v", err)
	}
	if !strings.Contains(string(firstHandoffPrompt), "Initial run for "+continueE2EBranch+".") {
		t.Fatalf("expected initial context in first continue prompt, got:\n%s", firstHandoffPrompt)
	}
	if !strings.Contains(string(firstHandoffPrompt), "## Prior Context") {
		t.Fatalf("expected prior context wrapper, got:\n%s", firstHandoffPrompt)
	}
	if !strings.Contains(string(firstHandoffPrompt), "## Source Prompt") {
		t.Fatalf("expected source prompt section, got:\n%s", firstHandoffPrompt)
	}
	if !strings.Contains(string(firstHandoffPrompt), "## Update Handoff Context") {
		t.Fatalf("expected update handoff context section, got:\n%s", firstHandoffPrompt)
	}
	if !strings.Contains(string(firstHandoffPrompt), ".sandman/rendered-prompt.md") {
		t.Fatalf("expected rendered-prompt.md reference, got:\n%s", firstHandoffPrompt)
	}
	if !strings.Contains(string(firstHandoffPrompt), "## Stage: plan-approved") {
		t.Fatalf("expected stage line preserved, got:\n%s", firstHandoffPrompt)
	}
	if !strings.Contains(string(firstHandoffPrompt), "## Last Skill: sandman-tdd") {
		t.Fatalf("expected last skill in first continue prompt, got:\n%s", firstHandoffPrompt)
	}
	if !strings.Contains(string(firstHandoffPrompt), "## Last Skill Status: complete") {
		t.Fatalf("expected last skill status in first continue prompt, got:\n%s", firstHandoffPrompt)
	}
	if strings.Contains(string(firstHandoffPrompt), string(initialPrompt)) {
		t.Fatalf("expected handoff-prompt.md to NOT inline rendered-prompt.md content, got:\n%s", firstHandoffPrompt)
	}

	firstHandoffContext, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatalf("read first continue context: %v", err)
	}
	if !strings.Contains(string(firstHandoffContext), "First continue for "+continueE2EBranch+".") {
		t.Fatalf("expected first continue context contents, got:\n%s", firstHandoffContext)
	}

	// Reset PR state so the second continue is not rejected as already merged.
	if err := os.WriteFile(prStatePath, []byte("open\n"), 0644); err != nil {
		t.Fatalf("reset pr state: %v", err)
	}

	out, err = runSandmanBinary(t, binPath, repoDir, "continue", "1")
	if err != nil {
		t.Fatalf("second continue failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected second continue success summary, got:\n%s", out)
	}

	secondHandoffPrompt, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "handoff-prompt.md"))
	if err != nil {
		t.Fatalf("read second continue prompt: %v", err)
	}
	if !strings.Contains(string(secondHandoffPrompt), "First continue for "+continueE2EBranch+".") {
		t.Fatalf("expected updated context in second continue prompt, got:\n%s", secondHandoffPrompt)
	}
	if !strings.Contains(string(secondHandoffPrompt), "## Last Skill: sandman-tdd") {
		t.Fatalf("expected last skill in second continue prompt, got:\n%s", secondHandoffPrompt)
	}
	if !strings.Contains(string(secondHandoffPrompt), "## Last Skill Status: complete") {
		t.Fatalf("expected last skill status in second continue prompt, got:\n%s", secondHandoffPrompt)
	}

	secondHandoffContext, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatalf("read second continue context: %v", err)
	}
	if !strings.Contains(string(secondHandoffContext), "Second continue for "+continueE2EBranch+".") {
		t.Fatalf("expected second continue context contents, got:\n%s", secondHandoffContext)
	}

	// Reset PR state again for the third continue.
	if err := os.WriteFile(prStatePath, []byte("open\n"), 0644); err != nil {
		t.Fatalf("reset pr state: %v", err)
	}

	out, err = runSandmanBinary(t, binPath, repoDir, "continue", "1")
	if err != nil {
		t.Fatalf("third continue failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected third continue success summary, got:\n%s", out)
	}

	thirdHandoffPrompt, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "handoff-prompt.md"))
	if err != nil {
		t.Fatalf("read third continue prompt: %v", err)
	}
	if !strings.Contains(string(thirdHandoffPrompt), "Second continue for "+continueE2EBranch+".") {
		t.Fatalf("expected second continue context in third continue prompt, got:\n%s", thirdHandoffPrompt)
	}
	if !strings.Contains(string(thirdHandoffPrompt), "## Prior Context") {
		t.Fatalf("expected prior context wrapper in third prompt, got:\n%s", thirdHandoffPrompt)
	}
	if !strings.Contains(string(thirdHandoffPrompt), "## Last Skill: sandman-implement") {
		t.Fatalf("expected last skill in third continue prompt, got:\n%s", thirdHandoffPrompt)
	}
	if !strings.Contains(string(thirdHandoffPrompt), "## Last Skill Status: complete") {
		t.Fatalf("expected last skill status in third continue prompt, got:\n%s", thirdHandoffPrompt)
	}

	thirdHandoffContext, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatalf("read third continue context: %v", err)
	}
	if !strings.Contains(string(thirdHandoffContext), "Third continue for "+continueE2EBranch+".") {
		t.Fatalf("expected third continue context contents, got:\n%s", thirdHandoffContext)
	}

	log := &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")}
	loggedEvents, err := log.Read()
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}

	var runEvents []events.Event
	for _, event := range loggedEvents {
		if event.Issue == 1 && (event.Type == "run.started" || event.Type == "run.continued") {
			runEvents = append(runEvents, event)
		}
	}
	if len(runEvents) != 4 {
		t.Fatalf("expected 4 run events, got %#v", runEvents)
	}
	if runEvents[0].Type != "run.started" || runEvents[1].Type != "run.continued" || runEvents[2].Type != "run.continued" || runEvents[3].Type != "run.continued" {
		t.Fatalf("unexpected run event sequence: %#v", []string{runEvents[0].Type, runEvents[1].Type, runEvents[2].Type, runEvents[3].Type})
	}
	if got, ok := payloadString(runEvents[1].Payload, "previous_run_id"); !ok || got != runEvents[0].RunID {
		t.Fatalf("expected first continue previous_run_id %q, got %#v", runEvents[0].RunID, runEvents[1].Payload["previous_run_id"])
	}
	if got, ok := payloadString(runEvents[1].Payload, "base_branch"); !ok || got != "main" {
		t.Fatalf("expected first continue base_branch to stay on original branch, got %#v", runEvents[1].Payload["base_branch"])
	}
	if got, ok := payloadString(runEvents[2].Payload, "previous_run_id"); !ok || got != runEvents[1].RunID {
		t.Fatalf("expected second continue previous_run_id %q, got %#v", runEvents[1].RunID, runEvents[2].Payload["previous_run_id"])
	}
	if got, ok := payloadString(runEvents[2].Payload, "base_branch"); !ok || got != "main" {
		t.Fatalf("expected second continue base_branch to stay on original branch, got %#v", runEvents[2].Payload["base_branch"])
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected preserved worktree, got: %v", err)
	}
}

func TestContinueFlow_PodmanSandboxBinarySupportsMultipleIssues(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioContinueMulti) {
		t.Skip("set SANDMAN_E2E_GATES=continue_multi (or all) to run multi-issue continue e2e")
	}
	if !podmanAvailable(t) {
		return
	}

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	_ = initRunIntegrationRepoWithRemote(t, repoDir)

	isolatedHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(isolatedHome, ".ssh"), 0755); err != nil {
		t.Fatalf("create isolated ssh dir: %v", err)
	}
	t.Setenv("HOME", isolatedHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolatedHome, ".config"))

	ghShimDir := t.TempDir()
	writeMergedFakeGHShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "init")
	if err != nil {
		t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
	}

	if _, err := runSandmanBinary(t, binPath, repoDir, "config", "set", "review_command", "/oc review"); err != nil {
		t.Fatalf("sandman config set failed: %v", err)
	}
	forcePodmanSandbox(t, repoDir)
	writeMergedFakeGHShimForContainer(t, filepath.Join(repoDir, ".sandman", "bin"))
	installFakeOpenCodeForContainer(t, repoDir)

	prStateDir := filepath.Join(repoDir, ".sandman", "bin")
	for _, pr := range []struct{ branch, hash string }{
		{"sandman/1-fix-failing-test", "sandman_1-fix-failing-test"},
		{"sandman/2-fix-failing-test", "sandman_2-fix-failing-test"},
	} {
		prStatePath := filepath.Join(prStateDir, "pr-state-"+pr.hash)
		if err := os.WriteFile(prStatePath, []byte("merged\n"), 0644); err != nil {
			t.Fatalf("write pr state for %s: %v", pr.branch, err)
		}
	}

	for _, issue := range []string{"1", "2"} {
		t.Logf("running issue %s", issue)
		out, err = runSandmanBinary(t, binPath, repoDir, "run", "--sandbox", "podman", issue)
		if err != nil {
			t.Fatalf("sandman run %s failed: %v\noutput:\n%s", issue, err, out)
		}
		if !strings.Contains(out, "Summary: 1 succeeded") {
			t.Fatalf("expected run success summary for issue %s, got:\n%s", issue, out)
		}
	}

	issueOneWorktree := filepath.Join(repoDir, ".sandman", "worktrees", "sandman/1-fix-failing-test")
	issueTwoWorktree := filepath.Join(repoDir, ".sandman", "worktrees", "sandman/2-fix-failing-test")

	initialPrompt1, err := os.ReadFile(filepath.Join(issueOneWorktree, ".sandman", "rendered-prompt.md"))
	if err != nil {
		t.Fatalf("read issue 1 rendered prompt: %v", err)
	}
	if !strings.Contains(string(initialPrompt1), ".sandman/handoff.md") {
		t.Fatalf("expected issue 1 rendered prompt to mention continuation context, got:\n%s", initialPrompt1)
	}

	initialPrompt2, err := os.ReadFile(filepath.Join(issueTwoWorktree, ".sandman", "rendered-prompt.md"))
	if err != nil {
		t.Fatalf("read issue 2 rendered prompt: %v", err)
	}
	if !strings.Contains(string(initialPrompt2), ".sandman/handoff.md") {
		t.Fatalf("expected issue 2 rendered prompt to mention continuation context, got:\n%s", initialPrompt2)
	}

	// Initial runs succeeded and PRs were merged, so handoff.md was cleaned up.
	// Write handoff.md and step files for both issues before the continue.
	// Per-issue Last Skill values diverge on purpose to test field independence.
	for _, wtPath := range []struct{ path, branch, hash, lastSkill string }{
		{issueOneWorktree, "sandman/1-fix-failing-test", "df007d4b37ed388b", "sandman-tdd"},
		{issueTwoWorktree, "sandman/2-fix-failing-test", "83f322e35451c018", "sandman-implement"},
	} {
		handoffContent := fmt.Sprintf(`## Stage: plan-approved
## Source Prompt: .sandman/rendered-prompt.md
## Last Skill: %s
## Last Skill Status: complete
## Completed
Initial run for %s.

## Pending
none

## Blockers
none

## Key Decisions
fake opencode for continuation e2e

## Next Step
continue the flow for %s
`, wtPath.lastSkill, wtPath.branch, wtPath.branch)
		handoffPath := filepath.Join(wtPath.path, ".sandman", "handoff.md")
		if err := os.WriteFile(handoffPath, []byte(handoffContent), 0644); err != nil {
			t.Fatalf("write handoff for %s: %v", wtPath.branch, err)
		}
		stepPath := filepath.Join(wtPath.path, ".sandman", "fake-opencode-step-"+wtPath.hash)
		if err := os.WriteFile(stepPath, []byte("1\n"), 0644); err != nil {
			t.Fatalf("write step file for %s: %v", wtPath.branch, err)
		}
	}

	out, err = runSandmanBinary(t, binPath, repoDir, "continue", "1", "2")
	if err != nil {
		t.Fatalf("multi-issue continue failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 2 succeeded") {
		t.Fatalf("expected multi-issue continue success summary, got:\n%s", out)
	}

	handoffPrompt1, err := os.ReadFile(filepath.Join(issueOneWorktree, ".sandman", "handoff-prompt.md"))
	if err != nil {
		t.Fatalf("read issue 1 continue prompt: %v", err)
	}
	if !strings.Contains(string(handoffPrompt1), "Initial run for sandman/1-fix-failing-test.") {
		t.Fatalf("expected issue 1 prompt to use its own verbatim context, got:\n%s", handoffPrompt1)
	}
	if !strings.Contains(string(handoffPrompt1), "## Prior Context") {
		t.Fatalf("expected issue 1 prompt to have prior context wrapper, got:\n%s", handoffPrompt1)
	}
	if !strings.Contains(string(handoffPrompt1), "## Source Prompt: .sandman/rendered-prompt.md") {
		t.Fatalf("expected issue 1 prompt to have source prompt, got:\n%s", handoffPrompt1)
	}
	if !strings.Contains(string(handoffPrompt1), "## Last Skill: sandman-tdd") {
		t.Fatalf("expected issue 1 prompt to have sandman-tdd, got:\n%s", handoffPrompt1)
	}
	if !strings.Contains(string(handoffPrompt1), "## Last Skill Status: complete") {
		t.Fatalf("expected issue 1 prompt to have complete status, got:\n%s", handoffPrompt1)
	}

	handoffPrompt2, err := os.ReadFile(filepath.Join(issueTwoWorktree, ".sandman", "handoff-prompt.md"))
	if err != nil {
		t.Fatalf("read issue 2 continue prompt: %v", err)
	}
	if !strings.Contains(string(handoffPrompt2), "Initial run for sandman/2-fix-failing-test.") {
		t.Fatalf("expected issue 2 prompt to use its own verbatim context, got:\n%s", handoffPrompt2)
	}
	if !strings.Contains(string(handoffPrompt2), "## Prior Context") {
		t.Fatalf("expected issue 2 prompt to have prior context wrapper, got:\n%s", handoffPrompt2)
	}
	if !strings.Contains(string(handoffPrompt2), "## Source Prompt: .sandman/rendered-prompt.md") {
		t.Fatalf("expected issue 2 prompt to have source prompt, got:\n%s", handoffPrompt2)
	}
	if !strings.Contains(string(handoffPrompt2), "## Last Skill: sandman-implement") {
		t.Fatalf("expected issue 2 prompt to have sandman-implement, got:\n%s", handoffPrompt2)
	}
	if !strings.Contains(string(handoffPrompt2), "## Last Skill Status: complete") {
		t.Fatalf("expected issue 2 prompt to have complete status, got:\n%s", handoffPrompt2)
	}

	handoffContext1, err := os.ReadFile(filepath.Join(issueOneWorktree, ".sandman", "handoff.md"))
	if err != nil {
		t.Fatalf("read issue 1 continuation context: %v", err)
	}
	if !strings.Contains(string(handoffContext1), "First continue for sandman/1-fix-failing-test.") {
		t.Fatalf("expected issue 1 context to advance independently, got:\n%s", handoffContext1)
	}

	handoffContext2, err := os.ReadFile(filepath.Join(issueTwoWorktree, ".sandman", "handoff.md"))
	if err != nil {
		t.Fatalf("read issue 2 continuation context: %v", err)
	}
	if !strings.Contains(string(handoffContext2), "First continue for sandman/2-fix-failing-test.") {
		t.Fatalf("expected issue 2 context to advance independently, got:\n%s", handoffContext2)
	}

	log := &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")}
	loggedEvents, err := log.Read()
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}

	initialRunIDs := make(map[int]string)
	continued := make(map[int]string)
	for _, event := range loggedEvents {
		if event.Type == "run.started" && (event.Issue == 1 || event.Issue == 2) {
			if _, ok := initialRunIDs[event.Issue]; !ok {
				initialRunIDs[event.Issue] = event.RunID
			}
		}
		if event.Type != "run.continued" {
			continue
		}
		if event.Issue != 1 && event.Issue != 2 {
			continue
		}
		previousRunID, ok := payloadString(event.Payload, "previous_run_id")
		if !ok {
			t.Fatalf("missing previous_run_id for issue %d: %#v", event.Issue, event.Payload)
		}
		continued[event.Issue] = previousRunID
	}
	if len(continued) != 2 {
		t.Fatalf("expected 2 continued issues, got %#v", continued)
	}
	if continued[1] != initialRunIDs[1] {
		t.Fatalf("expected issue 1 to continue its own prior run %q, got %q", initialRunIDs[1], continued[1])
	}
	if continued[2] != initialRunIDs[2] {
		t.Fatalf("expected issue 2 to continue its own prior run %q, got %q", initialRunIDs[2], continued[2])
	}
}

func forcePodmanSandbox(t *testing.T, repoDir string) {
	t.Helper()

	cfgPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Sandbox = "podman"
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

func installFakeOpenCodeForContainer(t *testing.T, repoDir string) {
	t.Helper()

	binDir := filepath.Join(repoDir, ".sandman", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("create fake opencode dir: %v", err)
	}

	script := `#!/bin/sh
set -eu

branch="$(git branch --show-current)"
	branch_key="$(printf '%s' "$branch" | sha256sum | cut -c1-16)"
	step_file=".sandman/fake-opencode-step-${branch_key}"
step=0
if [ -f "$step_file" ]; then
  step=$(cat "$step_file")
fi

mkdir -p .sandman

write_context() {
  completed="$1"
  stage="$2"
  last_skill="$3"
  cat > .sandman/handoff.md <<EOF
## Stage: $stage
## Source Prompt: .sandman/rendered-prompt.md
## Last Skill: $last_skill
## Last Skill Status: complete
## Completed
$completed

## Pending
none

## Blockers
none

## Key Decisions
fake opencode for continuation e2e

## Next Step
continue the flow for $branch
EOF
}

case "$step" in
  0)
    if [ -f double.go ]; then
      perl -0pi -e 's/return 0/return 4/' double.go
    fi
    write_context "Initial run for $branch." "plan-approved" "sandman-tdd"
    ;;
  1)
    write_context "First continue for $branch." "implementation-committed" "sandman-tdd"
    ;;
  2)
    write_context "Second continue for $branch." "pr-created" "sandman-implement"
    ;;
  3)
    write_context "Third continue for $branch." "pr-review-finished" "sandman-pr-review"
    ;;
  *)
    printf 'unexpected fake opencode step %s for %s\n' "$step" "$branch" >&2
    exit 1
    ;;
esac

printf '%s\n' $((step + 1)) > "$step_file"
printf 'fake opencode step %s for %s\n' "$step" "$branch"
printf '%s\n' '# Todos' "- [x] fake opencode step ${step} complete"
`

	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}

	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile = append(dockerfile, []byte("\nCOPY .sandman/bin/opencode /usr/local/bin/opencode\nRUN chmod +x /usr/local/bin/opencode\nENV PATH=\"/usr/local/bin:$PATH\"\n")...)
	if err := os.WriteFile(dockerfilePath, dockerfile, 0644); err != nil {
		t.Fatalf("append fake opencode to Dockerfile: %v", err)
	}
}

func writeMergedFakeGHShim(t *testing.T, dir string) {
	t.Helper()

	script := `#!/bin/sh
set -eu

printf '%s\n' "$*" >> "__LOG_FILE__"

case "$1" in
  issue)
    if [ "${2:-}" = "view" ]; then
      case "${3:-}" in
        1)
          issue_number="${3:-}"
          body="The repo has a tiny failing Go test. Make Double(2) return 4."
          cat <<JSON
{"number":$issue_number,"title":"Fix failing test","body":"$body"}
JSON
          exit 0
          ;;
        2)
          issue_number="${3:-}"
          body="The repo has a tiny failing Go test. Make Double(3) return 6."
          cat <<JSON
{"number":$issue_number,"title":"Fix failing test","body":"$body"}
JSON
          exit 0
          ;;
      esac
    fi
    ;;
  repo)
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"name":"sandbox","owner":{"login":"example"}}
JSON
      exit 0
    fi
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
      state_file="$shim_dir/pr-state"
      count_file="$shim_dir/pr-state-count"
      state="open"
      if [ -f "$state_file" ]; then
        state=$(cat "$state_file")
      fi
      cat <<JSON
[{"number":1,"state":"open","mergedAt":"","headRefName":"$head","headRefOid":""}]
JSON
      exit 0
    fi
    if [ "${2:-}" = "create" ]; then
      printf 'https://example.test/example/sandbox/pull/1\n'
      exit 0
    fi
    if [ "${2:-}" = "checks" ]; then
      printf 'all checks passed\n'
      exit 0
    fi
    if [ "${2:-}" = "comment" ]; then
      printf 'commented\n'
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
      printf 'https://example.test/example/sandbox/pull/1\n'
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
      repos/example/sandbox/issues/1)
        cat <<'JSON'
{"number":1,"title":"Fix failing test","body":"The repo has a tiny failing Go test. Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/2)
        cat <<'JSON'
{"number":2,"title":"Fix failing test","body":"The repo has a tiny failing Go test. Make Double(3) return 6.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/1/events)
        printf '[]\n'
        exit 0
        ;;
      repos/example/sandbox/issues/2/events)
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
	script = strings.ReplaceAll(script, "__LOG_FILE__", filepath.Join(dir, "gh-calls.log"))

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("create gh shim dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
}

func writeMergedFakeGHShimForContainer(t *testing.T, hostDir string) {
	t.Helper()

	containerShimDir := hostDir
	script := strings.ReplaceAll(`#!/bin/sh
set -eu

shim_dir="__SHIM_DIR__"

case "$1" in
  issue)
    if [ "${2:-}" = "view" ]; then
      case "${3:-}" in
        1)
          issue_number="${3:-}"
          body="The repo has a tiny failing Go test. Make Double(2) return 4."
          cat <<JSON
{"number":$issue_number,"title":"Fix failing test","body":"$body"}
JSON
          exit 0
          ;;
        2)
          issue_number="${3:-}"
          body="The repo has a tiny failing Go test. Make Double(3) return 6."
          cat <<JSON
{"number":$issue_number,"title":"Fix failing test","body":"$body"}
JSON
          exit 0
          ;;
      esac
    fi
    ;;
  repo)
    if [ "${2:-}" = "view" ]; then
      cat <<'JSON'
{"name":"sandbox","owner":{"login":"example"}}
JSON
      exit 0
    fi
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
      head_key="$(printf '%s' "${head:-default}" | tr '/ ' '__')"
      state_file="__SHIM_DIR__/pr-state-${head_key}"
      state="open"
      if [ -f "$state_file" ]; then
        state=$(cat "$state_file")
      fi
      merged_at=""
      pr_state="$state"
      if [ "$state" = "merged" ]; then
        merged_at="2026-06-05T00:00:00Z"
        next_state="open"
      else
        next_state="merged"
      fi
      printf '%s\n' "$next_state" > "$state_file"
      cat <<JSON
[{"number":1,"state":"$pr_state","mergedAt":"$merged_at","headRefName":"$head","headRefOid":""}]
JSON
      exit 0
    fi
    if [ "${2:-}" = "create" ]; then
      printf 'https://example.test/example/sandbox/pull/1\n'
      exit 0
    fi
    if [ "${2:-}" = "checks" ]; then
      printf 'all checks passed\n'
      exit 0
    fi
    if [ "${2:-}" = "comment" ]; then
      printf 'commented\n'
      exit 0
    fi
    if [ "${2:-}" = "view" ]; then
      printf 'https://example.test/example/sandbox/pull/1\n'
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
      repos/example/sandbox/issues/1)
        cat <<'JSON'
{"number":1,"title":"Fix failing test","body":"The repo has a tiny failing Go test. Make Double(2) return 4.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/2)
        cat <<'JSON'
{"number":2,"title":"Fix failing test","body":"The repo has a tiny failing Go test. Make Double(3) return 6.","labels":[{"name":"ready-for-agent"}]}
JSON
        exit 0
        ;;
      repos/example/sandbox/issues/1/events)
        printf '[]\n'
        exit 0
        ;;
      repos/example/sandbox/issues/2/events)
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
`, "__SHIM_DIR__", containerShimDir)
	if err := os.MkdirAll(hostDir, 0755); err != nil {
		t.Fatalf("create gh shim dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "gh"), []byte(script), 0755); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	repoDir := filepath.Dir(filepath.Dir(hostDir))
	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile = append(dockerfile, []byte("\nCOPY .sandman/bin/gh /usr/local/bin/gh\nRUN chmod +x /usr/local/bin/gh\n")...)
	if err := os.WriteFile(dockerfilePath, dockerfile, 0644); err != nil {
		t.Fatalf("append gh shim to Dockerfile: %v", err)
	}
}
