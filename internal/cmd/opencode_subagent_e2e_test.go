//go:build e2e

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/testenv"
)

const subagentPermissionE2EBranch = "1-fix-failing-test"

const subagentMarkerEnv = "FAKE_OPENCODE_SUBAGENT"

func TestOpencodeSubagentPermissionAllowAll(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioOpencodeSubagent) {
		t.Skip("set SANDMAN_E2E_GATES=opencode_subagent (or all) to run opencode subagent permission e2e")
	}
	// FIX: merged gh shim is prepended on the host PATH even though its relative
	// paths only resolve inside the container. The follow-up branch should split
	// host/container shims before this E2E is brought back into CI. See #2459.
	if os.Getenv("CI") != "" && !testenv.FullRegression() {
		t.Skip("skip e2e in CI")
	}
	if !podmanAvailable(t) {
		return
	}

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	_ = initRunIntegrationRepoWithRemote(t, repoDir)

	// Note: this test deliberately does NOT isolate HOME/XDG_CONFIG_HOME.
	// The earlier isolated-HOME shape gave rootless podman a cold container
	// store on a brand-new $HOME/.local/share/containers tree, and the
	// orchestrator's BuildImage inside `sandman run` blew the run budget
	// re-pulling debian + opencode under that cold store. Sharing the
	// runner's HOME lets the orchestrator reuse the podman layer cache
	// warmed by the smoke and sandbox tiers that ran earlier in the same
	// job. GitHub calls are still mocked by the gh shim prepended to
	// PATH below, so the loss of HOME isolation does not re-introduce
	// real-network calls. The host/container shim split (#2459) is
	// preserved by the PATH ordering (ghShimDir first, .sandman/bin last)
	// applied in runSandmanBinaryWithTimeout.

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
	installPermissionFakeOpenCodeForContainer(t, repoDir)

	out, err = runSandmanBinaryWithTimeout(t, binPath, repoDir, 10*time.Minute, "run", "--sandbox", "podman", "1")
	if err != nil {
		ghLog, _ := os.ReadFile(filepath.Join(ghShimDir, "gh-calls.log"))
		t.Fatalf("sandman run failed: %v\noutput:\n%s\ngh log:\n%s", err, out, ghLog)
	}
	if !strings.Contains(out, "Summary: 1 succeeded") {
		t.Fatalf("expected run success summary, got:\n%s", out)
	}

	logPath := findIssueRunLogUnderBatches(t, repoDir, 1)
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
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

func findIssueRunLogUnderBatches(t *testing.T, repoDir string, issue int) string {
	t.Helper()
	batchesDir := filepath.Join(repoDir, ".sandman", "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil {
		t.Fatalf("read batches dir %s: %v", batchesDir, err)
	}
	suffix := fmt.Sprintf("-%d", issue)
	for _, batch := range entries {
		if !batch.IsDir() {
			continue
		}
		runsDir := filepath.Join(batchesDir, batch.Name(), "runs")
		runs, err := os.ReadDir(runsDir)
		if err != nil {
			continue
		}
		for _, run := range runs {
			if !run.IsDir() || !strings.HasSuffix(run.Name(), suffix) {
				continue
			}
			logPath := filepath.Join(runsDir, run.Name(), "run.log")
			if _, err := os.Stat(logPath); err == nil {
				return logPath
			}
		}
	}
	t.Fatalf("run log for issue %d not found under %s", issue, batchesDir)
	return ""
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

func writeMergedFakeGHShimForContainer(t *testing.T, dir string) {
	t.Helper()
	// The container shim already answers `pr list` with state "merged"
	// deterministically, so no marker patch is needed.
	writeFakeGHShimForContainer(t, dir)
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

func runSandmanBinaryWithTimeout(t *testing.T, binPath, workDir string, timeout time.Duration, args ...string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ghBin := filepath.Join(workDir, ".sandman", "bin")
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		// Host-side sandman should resolve the dedicated host gh shim first.
		// The container-only gh shim under .sandman/bin is copied into the image
		// separately, and prepending it here makes the host process try to use a
		// script whose shim_dir is /workspace/.sandman/bin (container path), which
		// fails before the container even starts.
		"PATH="+os.Getenv("PATH")+string(os.PathListSeparator)+ghBin,
		"GH_TOKEN=fake",
		"GITHUB_TOKEN=fake",
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), context.DeadlineExceeded
	}
	return string(out), err
}
