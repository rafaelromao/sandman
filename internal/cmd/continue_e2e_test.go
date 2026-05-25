//go:build e2e

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
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
	writeFakeGHShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "init")
	if err != nil {
		t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
	}

	forcePodmanSandbox(t, repoDir)
	writeFakeGHShimForContainer(t, filepath.Join(repoDir, ".sandman", "bin"))
	installFakeOpenCodeForContainer(t, repoDir)

	out, err = runSandmanBinary(t, binPath, repoDir, "run", "--sandbox", "podman", "1")
	if err != nil {
		t.Fatalf("sandman run failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected run success summary, got:\n%s", out)
	}

	worktreePath := filepath.Join(repoDir, ".sandman", "worktrees", continueE2EBranch)
	initialPrompt, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "rendered-prompt.md"))
	if err != nil {
		t.Fatalf("read rendered prompt: %v", err)
	}
	if !strings.Contains(string(initialPrompt), ".sandman/continuation-context.md") {
		t.Fatalf("expected rendered prompt to mention continuation context, got:\n%s", initialPrompt)
	}

	contextPath := filepath.Join(worktreePath, ".sandman", "continuation-context.md")
	initialContext, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatalf("read initial continuation context: %v", err)
	}
	if !strings.Contains(string(initialContext), "# Continuation Context") {
		t.Fatalf("expected initial context header, got:\n%s", initialContext)
	}
	if !strings.Contains(string(initialContext), "Initial run.") {
		t.Fatalf("expected initial context contents, got:\n%s", initialContext)
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

	out, err = runSandmanBinary(t, binPath, repoDir, "continue", "1", "finish the tests")
	if err != nil {
		t.Fatalf("first continue failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected first continue success summary, got:\n%s", out)
	}

	firstContinuePrompt, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "continue-prompt.md"))
	if err != nil {
		t.Fatalf("read first continue prompt: %v", err)
	}
	if !strings.Contains(string(firstContinuePrompt), "## Prior Context") {
		t.Fatalf("expected prior context section, got:\n%s", firstContinuePrompt)
	}
	if strings.Contains(string(firstContinuePrompt), "# Continuation Context") {
		t.Fatalf("expected context header stripped, got:\n%s", firstContinuePrompt)
	}
	if !strings.Contains(string(firstContinuePrompt), "Initial run.") {
		t.Fatalf("expected initial context in first continue prompt, got:\n%s", firstContinuePrompt)
	}
	if !strings.Contains(string(firstContinuePrompt), "finish the tests") {
		t.Fatalf("expected new instruction in first continue prompt, got:\n%s", firstContinuePrompt)
	}
	if !strings.Contains(string(firstContinuePrompt), "overwrite `.sandman/continuation-context.md`") {
		t.Fatalf("expected context overwrite instruction, got:\n%s", firstContinuePrompt)
	}

	firstContinueContext, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatalf("read first continue context: %v", err)
	}
	if !strings.Contains(string(firstContinueContext), "First continue.") {
		t.Fatalf("expected first continue context contents, got:\n%s", firstContinueContext)
	}

	out, err = runSandmanBinary(t, binPath, repoDir, "continue", "1", "push the PR")
	if err != nil {
		t.Fatalf("second continue failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Summary: 1 succeeded, 0 failed") {
		t.Fatalf("expected second continue success summary, got:\n%s", out)
	}

	secondContinuePrompt, err := os.ReadFile(filepath.Join(worktreePath, ".sandman", "continue-prompt.md"))
	if err != nil {
		t.Fatalf("read second continue prompt: %v", err)
	}
	if !strings.Contains(string(secondContinuePrompt), "First continue.") {
		t.Fatalf("expected updated context in second continue prompt, got:\n%s", secondContinuePrompt)
	}
	if !strings.Contains(string(secondContinuePrompt), "push the PR") {
		t.Fatalf("expected second instruction in continue prompt, got:\n%s", secondContinuePrompt)
	}

	secondContinueContext, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatalf("read second continue context: %v", err)
	}
	if !strings.Contains(string(secondContinueContext), "Second continue.") {
		t.Fatalf("expected second continue context contents, got:\n%s", secondContinueContext)
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
	if len(runEvents) != 3 {
		t.Fatalf("expected 3 run events, got %#v", runEvents)
	}
	if runEvents[0].Type != "run.started" || runEvents[1].Type != "run.continued" || runEvents[2].Type != "run.continued" {
		t.Fatalf("unexpected run event sequence: %#v", []string{runEvents[0].Type, runEvents[1].Type, runEvents[2].Type})
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

step_file=".sandman/fake-opencode-step"
step=0
if [ -f "$step_file" ]; then
  step=$(cat "$step_file")
fi

mkdir -p .sandman

write_context() {
  completed="$1"
  cat > .sandman/continuation-context.md <<EOF
# Continuation Context

## Completed
$completed

## Pending
none

## Blockers
none

## Key Decisions
fake opencode for continuation e2e

## Next Step
continue the flow
EOF
}

case "$step" in
  0)
    write_context "Initial run."
    ;;
  1)
    write_context "First continue."
    ;;
  2)
    write_context "Second continue."
    ;;
  *)
    printf 'unexpected fake opencode step %s\n' "$step" >&2
    exit 1
    ;;
esac

printf '%s\n' $((step + 1)) > "$step_file"
printf 'fake opencode step %s\n' "$step"
`

	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}

	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile = append(dockerfile, []byte("\nCOPY .sandman/bin/opencode /usr/local/bin/opencode\nRUN chmod +x /usr/local/bin/opencode\n")...)
	if err := os.WriteFile(dockerfilePath, dockerfile, 0644); err != nil {
		t.Fatalf("append fake opencode to Dockerfile: %v", err)
	}
}
