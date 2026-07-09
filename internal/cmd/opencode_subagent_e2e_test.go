//go:build e2e

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const subagentPermissionE2EBranch = "sandman/1-fix-failing-test"

const subagentMarkerEnv = "FAKE_OPENCODE_SUBAGENT"

func TestOpencodeSubagentPermissionAllowAll(t *testing.T) {
	// CI: JUSTIFIED — calls podman to build and run a container image
	// and exercises the real opencode CLI inside the sandbox. The gh
	// shim makes the network side hermetic; no real GitHub auth is
	// needed.
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}
	if !testenv.E2EGateAllowed(testenv.E2EScenarioOpencodeSubagent) {
		t.Skip("set SANDMAN_E2E_GATES=opencode_subagent (or all) to run opencode subagent permission e2e")
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

	// Ensure no real GitHub auth from the host leaks through to the
	// sandman subprocess. The gh shim returns a fake auth status; the
	// production code should never need to reach a real github.com.
	scrubGitHubEnv(t)

	ghShimDir := t.TempDir()
	// Install the host-side gh shim without pre-seeding merged state
	// so the orchestrator's pre-flight "is this PR already merged?"
	// check returns false and the agent actually runs. The
	// fake-opencode writes the already-resolved marker on exit so the
	// post-run check promotes the run to "success".
	writeFakeGHShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "init")
	if err != nil {
		t.Fatalf("sandman init failed: %v\noutput:\n%s", err, out)
	}

	if _, err := runSandmanBinary(t, binPath, repoDir, "config", "set", "review_command", "/oc review"); err != nil {
		t.Fatalf("sandman config set failed: %v", err)
	}

	forcePodmanSandbox(t, repoDir)
	// Install the fake gh shim into the container image so the agent's
	// in-container `gh auth setup-git` and `gh pr list` calls never
	// reach the host network. The shim starts with no pre-seeded state
	// so the orchestrator's pre-flight `prMerged` check is false and
	// the agent actually runs. The fake-opencode marks the task as
	// "already resolved" on exit so the post-run check promotes the
	// run to "success".
	writeFakeGHShimForContainer(t, filepath.Join(repoDir, ".sandman", "bin"))
	installPermissionFakeOpenCodeForContainer(t, repoDir)
	// Prepend the container-side shim dir to the agent's PATH so the
	// in-container `gh` resolves to the shim, not the real binary.
	customizeOpenCodeAgentContainerPath(t, repoDir)
	// Build the image explicitly so the Dockerfile COPY .sandman/bin/gh
	// line is actually applied; the sandman container runtime caches
	// images and would otherwise reuse a build that pre-dates the shim.
	buildCmd := exec.Command("podman", "build", "-t", "sandman-e2e-subagent", "-f",
		filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build subagent image: %v: %s", err, out)
	}
	assertContainerShimFresh(t, "sandman-e2e-subagent", filepath.Join(repoDir, ".sandman", "bin"))

	out, err = runSandmanBinaryWithTimeout(t, binPath, repoDir, 120*time.Second, "run", "--sandbox", "podman", "1")
	if err != nil {
		ghLog, _ := os.ReadFile(filepath.Join(ghShimDir, "gh-calls.log"))
		eventsData, _ := os.ReadFile(filepath.Join(repoDir, ".sandman", "events.jsonl"))
		t.Fatalf("sandman run failed: %v\noutput:\n%s\ngh log:\n%s\nevents:\n%s", err, out, ghLog, eventsData)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
		eventsData, _ := os.ReadFile(filepath.Join(repoDir, ".sandman", "events.jsonl"))
		t.Fatalf("expected run success summary, got:\n%s\nevents:\n%s", out, eventsData)
	}

	// The orchestrator writes the agent's per-run log under the
	// batches layout: <BatchesDir>/<batchID>/runs/<runID>/run.log.
	// Walk the batches tree to find the run.log for the attempt the
	// test just drove; the legacy .sandman/logs/<issue>.log path no
	// longer exists in the new layout.
	eventsPath := filepath.Join(repoDir, ".sandman", "events.jsonl")
	eventsData, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var runID string
	for _, line := range strings.Split(strings.TrimSpace(string(eventsData)), "\n") {
		if line == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if evt["type"] == "run.finished" {
			if id, ok := evt["run_id"].(string); ok {
				runID = id
			}
		}
	}
	if runID == "" {
		t.Fatalf("could not extract runID from events; events:\n%s", eventsData)
	}
	batchesDir := filepath.Join(repoDir, ".sandman", "batches")
	logPath := filepath.Join(batchesDir, runID[:len(runID)-2], "runs", runID, "run.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		// The orchestrator writes to a per-row run folder derived
		// from the runID; if the legacy layout is in play, fall back
		// to the pre-ADR-0032 path so the test still observes the
		// agent's stdout.
		if alt, walkErr := findRunLog(batchesDir, runID); walkErr == nil {
			logPath = alt
			logData, err = os.ReadFile(logPath)
		}
	}
	if err != nil {
		t.Fatalf("read log: %v (path=%s)\nevents:\n%s", err, logPath, eventsData)
	}
	log := string(logData)
	if strings.Contains(log, "permission.asked") {
		t.Fatalf("unexpected permission.asked event in log, got:\n%s", log)
	}
	if !strings.Contains(log, "fake-opencode: parent invoked subagent") {
		t.Fatalf("expected log to show parent invoked subagent, got:\n%s", log)
	}
	if !strings.Contains(log, "fake-opencode: subagent completed read") {
		t.Fatalf("expected log to show subagent completed the read (no permission.asked hang), got:\n%s", log)
	}
	if strings.Contains(log, "fake-opencode: subagent hanging") {
		t.Fatalf("subagent hung despite OPENCODE_PERMISSION being set; log:\n%s", log)
	}

	worktreePath := filepath.Join(repoDir, ".sandman", "worktrees", subagentPermissionE2EBranch)
	subagentEnvPath := filepath.Join(worktreePath, ".sandman", "subagent-env.txt")
	envData, err := os.ReadFile(subagentEnvPath)
	if err != nil {
		t.Fatalf("read subagent env dump: %v", err)
	}
	if !strings.Contains(string(envData), `"external_directory":"allow"`) {
		t.Fatalf("expected subagent to observe OPENCODE_PERMISSION with external_directory:allow, got:\n%s", envData)
	}
}

func installPermissionFakeOpenCodeForContainer(t *testing.T, repoDir string) {
	t.Helper()

	binDir := filepath.Join(repoDir, ".sandman", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("create fake opencode dir: %v", err)
	}

	script := `#!/bin/sh
set -eu

# Dump the OPENCODE_PERMISSION value the subagent sees so the test can assert
# the parent process forwarded it through the in-container export.
mkdir -p .sandman
if [ "${` + subagentMarkerEnv + `:-}" = "1" ]; then
  printf '%s\n' "${OPENCODE_PERMISSION:-<unset>}" > .sandman/subagent-env.txt
  perm="${OPENCODE_PERMISSION:-}"
  case "$perm" in
    *'"external_directory":"allow"'*)
      echo "fake-opencode: subagent attempting to read /etc/passwd (external_directory)"
      head -n 1 /etc/passwd >/dev/null
      echo "fake-opencode: subagent completed read"
      exit 0
      ;;
    *)
      # Mirror the real opencode behaviour: when the parent forgets the override,
      # the subagent's external_directory tool call emits permission.asked and
      # the host CLI's auto-reply handler ignores it, hanging the tool call.
      echo "fake-opencode: subagent hanging (OPENCODE_PERMISSION not allow-all)"
      _wakeup_dir="${WAKEUP_DIR:-}"
      if [ "${SANDMAN_TEST_FAST:-}" = "1" ] && [ -n "$_wakeup_dir" ] && [ -d "$_wakeup_dir" ]; then
        while [ ! -f "$_wakeup_dir/wakeup" ]; do
          sleep 0.1
        done
      else
        sleep 600
      fi
      exit 124
      ;;
  esac
fi

echo "fake-opencode: parent invoked subagent for external read"
` + subagentMarkerEnv + `=1 "$0" subagent-args
echo "fake-opencode: parent observed subagent return"
# The test exercises a pre-resolved scenario: the fake gh shim is seeded
# with a merged PR for this branch, so the production code's pre-flight
# check would treat the issue as already resolved. Mirror that by writing
# the canonical already-resolved marker into .sandman/task.md so the
# orchestrator's post-run check promotes the run to "success" without
# requiring a real agent loop.
mkdir -p .sandman
if [ -f .sandman/task.md ]; then
  printf '\n## Status: already resolved\n' >> .sandman/task.md
fi
exit 0
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

	writeFakeGHShim(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "pr-state"), []byte("merged\n"), 0644); err != nil {
		t.Fatalf("seed merged pr state: %v", err)
	}
}

// writeMergedFakeGHShimForContainer installs the fake gh shim into the
// container image with a pre-seeded "merged" PR state. It is kept for
// future tests that need the orchestrator to short-circuit on a
// merged PR; the subagent test deliberately does not use it because
// the agent must run to exercise the OPENCODE_PERMISSION plumbing.
func writeMergedFakeGHShimForContainer(t *testing.T, dir string) {
	t.Helper()

	writeFakeGHShimForContainer(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "pr-state"), []byte("merged\n"), 0644); err != nil {
		t.Fatalf("seed merged pr state in container shim: %v", err)
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

// customizeOpenCodeAgentContainerPath prepends the container-side shim
// directory (/workspace/.sandman/bin) to the opencode agent's command
// PATH. Without this, the in-container `gh` resolves to the real
// binary on the host and the test would hit real github.com. The shim
// is what makes the test hermetic.
func customizeOpenCodeAgentContainerPath(t *testing.T, repoDir string) {
	t.Helper()

	cfgPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	agent, err := cfg.ResolveAgentProvider("opencode")
	if err != nil {
		t.Fatalf("resolve opencode agent: %v", err)
	}
	if !strings.HasPrefix(agent.Command, "PATH=") {
		agent.Command = `PATH=/workspace/.sandman/bin:${PATH} ` + agent.Command
	}
	if cfg.AgentProviders == nil {
		cfg.AgentProviders = map[string]config.Agent{}
	}
	cfg.AgentProviders["opencode"] = agent
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// findRunLog walks the batches directory looking for a run.log under
// any runs/<runID> folder. Used as a fallback when the test's computed
// path does not match the orchestrator's path because the test
// re-executes the binary with a different cwd.
func findRunLog(batchesDir, runID string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(batchesDir, "**", "runs", runID, "run.log"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", os.ErrNotExist
	}
	return matches[0], nil
}

func runSandmanBinaryWithTimeout(t *testing.T, binPath, workDir string, timeout time.Duration, args ...string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ghBin := filepath.Join(workDir, ".sandman", "bin")
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"PATH="+ghBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_TOKEN=fake",
		"GITHUB_TOKEN=fake",
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), context.DeadlineExceeded
	}
	return string(out), err
}
