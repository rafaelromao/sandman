//go:build e2e

// e2e harness for the scaffold preset matrix (issue #2057).
//
// This file extends the existing `//go:build e2e` test surface in
// `internal/cmd` (where the rest of the e2e tests live) so the rest of
// the preset matrix (#2059+) has a single scaffolding helper to reuse.
// The harness:
//
//   - probes the available container runtime (podman or docker) and
//     skips with a clear message when neither is available;
//   - runs `sandman init --build-tools <preset>` through the public CLI
//     entry point so the test exercises the same seam as the user;
//   - supports both the CLI-options path and the edited-Dockerfile path
//     for adding extra tools to a scaffold;
//   - runs `sandman run` in container mode against a fake GitHub issue
//     and a canonical fake task body;
//   - asserts the run produced the fake task's artifact and wrote the
//     expected events to the event log.
//
// The `generic` preset is the canonical carrier for both tool-add paths
// (CLI-options and edited-Dockerfile) per the parent issue (#2056). It
// is covered end to end in this file; the language-specific children
// (#2059+) reuse the same harness helpers.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/scaffold"
)

// TestPresetMatrixHarness_SkipsWhenNoContainerRuntime pins the
// runtime-probe slice of the harness: the test self-skips with a
// clear message when no container runtime is available, matching the
// existing `containerRuntimeAvailable` helper in portal_e2e_test.go.
//
// The probe path is exercised directly so the test stays hermetic and
// does not depend on the rest of the harness being wired up yet.
func TestPresetMatrixHarness_SkipsWhenNoContainerRuntime(t *testing.T) {
	if containerRuntimeAvailable(t) {
		t.Skip("container runtime is available; the no-runtime skip path is not reachable here")
	}
}

// TestPresetMatrixHarness_GenericScaffolds pins the scaffold-only
// slice of the harness: running `sandman init --build-tools generic`
// through the real binary produces `.sandman/{config.yaml,Dockerfile}`
// in the test repo. This proves the harness path works end to end
// without needing a `sandman run` step, so the rest of the matrix can
// layer on the same seam.
func TestPresetMatrixHarness_GenericScaffolds(t *testing.T) {
	containerRuntimeAvailable(t)

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "generic")
	if err != nil {
		t.Fatalf("sandman init --build-tools generic failed: %v\noutput:\n%s", err, out)
	}
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	// The generic preset must surface the live MISE version in the
	// scaffolded Dockerfile header so the harness can read it back
	// without duplicating a literal.
	dockerfile, err := os.ReadFile(filepath.Join(repoDir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	want := "# sandman mise-version: " + scaffold.DefaultMISEVersion
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("scaffolded Dockerfile missing %q (the toolchain-version-from-catalog AC):\n%s", want, dockerfile)
	}
}

// TestPresetMatrixHarness_GenericRunProducesFakeArtifact pins the
// end-to-end `generic` CLI-options path: scaffold → fake opencode
// shim installed into `.sandman/bin/opencode` and COPY'd into the
// container → `podman build` → `sandman run 1` with a fake GitHub
// issue whose body is the canonical fake task body → the per-issue
// run log carries the canonical fake-task marker, and the events
// log has exactly one `run.started` and one `run.finished` for the
// fake issue.
//
// This is the canonical carrier for the CLI-options tool-add path
// (issue #2056 user story 12). The edited-Dockerfile variant is
// pinned by `TestPresetMatrixHarness_GenericRunWithEditedDockerfile`.
func TestPresetMatrixHarness_GenericRunProducesFakeArtifact(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "generic", "", "")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_GenericRunWithEditedDockerfile pins the
// end-to-end `generic` edited-Dockerfile path: scaffold → append a
// known `RUN` line to `.sandman/Dockerfile` → fake opencode shim
// installed → `podman build` (which must still succeed) → `sandman
// run 1` → the per-issue run log carries the canonical fake-task
// marker and the events log has the expected events.
//
// This is the canonical carrier for the edited-Dockerfile tool-add
// path (issue #2056 user story 11). The CLI-options variant is
// pinned by `TestPresetMatrixHarness_GenericRunProducesFakeArtifact`.
func TestPresetMatrixHarness_GenericRunWithEditedDockerfile(t *testing.T) {
	// The harness uses a touch marker so the edit is observably
	// present in the built image without dragging in a
	// network-dependent apt-get install (the shared packages
	// list already installs the most common tools; the harness
	// is exercising the "edited Dockerfile still builds" path,
	// not the network).
	binPath, repoDir := runE2EScaffold(t, "generic", "", "RUN touch /etc/sandman-preset-matrix-edited")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_GoScaffolds pins the scaffold-only slice
// of the Go preset: running `sandman init --build-tools go` through
// the real binary with `--tool-version latest` produces
// `.sandman/{config.yaml,Dockerfile}` with the go-version header
// set to the bundled catalog's latest. This proves the Go preset
// scaffold path works end to end and that the version is resolved
// from the catalog rather than hardcoded.
func TestPresetMatrixHarness_GoScaffolds(t *testing.T) {
	containerRuntimeAvailable(t)

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "go", "--tool-version", "latest")
	if err != nil {
		t.Fatalf("sandman init --build-tools go failed: %v\noutput:\n%s", err, out)
	}
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(repoDir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	want := "# sandman go-version: " + scaffold.BundledGoVersionLatest()
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("scaffolded Dockerfile missing %q (version must be resolved from catalog, not hardcoded):\n%s", want, dockerfile)
	}
}

// TestPresetMatrixHarness_GoRunProducesFakeArtifact pins the end-to-end
// `go` CLI-options path: scaffold with bare go.mod → fake opencode shim
// installed → `podman build` → `sandman run 1` → the per-issue run log
// carries the canonical fake-task marker, and the events log has exactly
// one `run.started` and one `run.finished` for the fake issue.
//
// This mirrors TestPresetMatrixHarness_GenericRunProducesFakeArtifact
// but for the Go preset instead of generic.
func TestPresetMatrixHarness_GoRunProducesFakeArtifact(t *testing.T) {
	binPath, repoDir := runE2EScaffoldWithGoMod(t, "go", "")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_GoRunWithEditedDockerfile pins the end-to-end
// `go` edited-Dockerfile path: scaffold with bare go.mod → append a
// known `RUN` line to `.sandman/Dockerfile` → fake opencode shim
// installed → `podman build` (which must still succeed) → `sandman
// run 1` → the per-issue run log carries the canonical fake-task
// marker and the events log has the expected events.
//
// This mirrors TestPresetMatrixHarness_GenericRunWithEditedDockerfile
// but for the Go preset instead of generic.
func TestPresetMatrixHarness_GoRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldWithGoMod(t, "go", "RUN touch /etc/sandman-preset-matrix-edited")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// runE2EScaffoldWithGoMod is like runE2EScaffold but writes a
// go.mod with a go 1.24 directive before scaffolding so the Go
// preset has a version hint to resolve from.
func runE2EScaffoldWithGoMod(t *testing.T, preset, dockerfileAppend string) (string, string) {
	t.Helper()
	containerRuntimeAvailable(t)
	binPath := buildSandmanBinary(t)
	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)

	os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/hello\n\ngo 1.24\n"), 0644)

	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "init", "--build-tools", preset)
	forcePodmanSandbox(t, repoDir)
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "config", "set", "review_command", "/oc review")
	if dockerfileAppend != "" {
		appendDockerfileRun(t, repoDir, dockerfileAppend)
	}
	return binPath, repoDir
}

// TestPresetMatrixHarness_NodeScaffolds pins the scaffold-only
// slice of the node harness: running `sandman init --build-tools
// node --tool-version lts` through the real binary produces
// `.sandman/{config.yaml,Dockerfile}` in the test repo and the
// Dockerfile contains the pinned node version from the catalog.
func TestPresetMatrixHarness_NodeScaffolds(t *testing.T) {
	containerRuntimeAvailable(t)

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "node", "--tool-version", "lts")
	if err != nil {
		t.Fatalf("sandman init --build-tools node --tool-version lts failed: %v\noutput:\n%s", err, out)
	}
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(repoDir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	wantVersion := scaffold.DefaultNodeLTSVersion()
	if wantVersion == "" {
		t.Fatalf("DefaultNodeLTSVersion() returned empty; catalog may have drifted")
	}
	want := "# sandman node-version: " + wantVersion
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("scaffolded Dockerfile missing %q (the toolchain-version-from-catalog AC):\n%s", want, dockerfile)
	}
}

// TestPresetMatrixHarness_NodeRunProducesFakeArtifact pins the
// end-to-end `node` CLI-options path: scaffold with node+lts,
// fake opencode shim installed → podman build → sandman run 1
// → per-issue run log carries the canonical fake-task marker,
// and the events log has exactly one run.started and one
// run.finished for the fake issue.
func TestPresetMatrixHarness_NodeRunProducesFakeArtifact(t *testing.T) {
	binPath, repoDir := runE2EScaffoldNode(t, "node", "")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_NodeRunWithEditedDockerfile pins the
// end-to-end `node` edited-Dockerfile path: scaffold with
// node+lts, append a RUN line to the Dockerfile, fake opencode
// shim installed → podman build → sandman run 1 → per-issue run
// log carries the canonical fake-task marker and the events log
// has the expected events.
func TestPresetMatrixHarness_NodeRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldNode(t, "node", "RUN touch /etc/sandman-preset-matrix-node-edited")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// runE2EScaffoldNode scaffolds a repo for the node preset with
// --tool-version lts. It follows the same pattern as runE2EScaffold
// but adds the tool-version flag so the scaffolder resolves the node
// version through the catalog rather than prompting.
func runE2EScaffoldNode(t *testing.T, preset, dockerfileAppend string) (string, string) {
	t.Helper()
	containerRuntimeAvailable(t)
	binPath := buildSandmanBinary(t)
	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "init", "--build-tools", preset, "--tool-version", "lts")
	forcePodmanSandbox(t, repoDir)
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "config", "set", "review_command", "/oc review")
	if dockerfileAppend != "" {
		appendDockerfileRun(t, repoDir, dockerfileAppend)
	}
	return binPath, repoDir
}

// presetMatrixHarnessFakeTaskMarker is the canonical fake-task
// marker string the shim prints to stdout. The orchestrator
// captures the agent's stdout into the per-issue run log under
// `.sandman/logs/<n>.log`, so the marker ends up host-visible
// regardless of worktree cleanup. Every preset child reuses the
// same marker string so the harness assertions stay uniform.
const presetMatrixHarnessFakeTaskMarker = "fake-task-ok: canonical fake task body executed"

// installFakeTaskOpenCodeForContainer writes the canonical fake
// `opencode` shim into `<repo>/.sandman/bin/opencode` and appends
// the COPY/ENV block to the scaffolded Dockerfile so the shim is
// available in the container at /usr/local/bin/opencode. The shim
// honors SANDMAN_TEST_FAST=1 + WAKEUP_DIR to short-circuit in fast
// mode (matching the existing `opencode_subagent_e2e_test.go`
// pattern), prints the canonical fake-task marker to stdout, and
// writes the canonical artifact to /workspace/ (bind-mounted to
// the worktree on the host) before exiting 0. The harness
// asserts the marker in the host-visible per-run log; the
// worktree file is the canonical "fake task's artifact" the
// spec asks for but is transient because the orchestrator cleans
// up the worktree after the run.
func installFakeTaskOpenCodeForContainer(t *testing.T, repoDir string) {
	t.Helper()

	binDir := filepath.Join(repoDir, ".sandman", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("create fake opencode dir: %v", err)
	}

	script := `#!/bin/sh
# Canonical fake opencode shim for the e2e preset matrix.
# Honors SANDMAN_TEST_FAST=1 + WAKEUP_DIR to short-circuit in fast
# mode (otherwise it would sleep 600s like the real blocking shim).
# Writes the canonical fake-task artifact to /workspace/ (bind-mounted
# to the worktree on the host) and prints the canonical fake-task
# marker to stdout (captured by the orchestrator into the per-run
# log, which the harness asserts against).

_wakeup_dir="${WAKEUP_DIR:-}"
if [ "${SANDMAN_TEST_FAST:-}" = "1" ] && [ -n "$_wakeup_dir" ] && [ -d "$_wakeup_dir" ]; then
    while [ ! -f "$_wakeup_dir/wakeup" ]; do
        sleep 0.1
    done
fi

printf 'fake-task-ok\n' > /workspace/sandman-fake-task-marker
echo "` + presetMatrixHarnessFakeTaskMarker + `"
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
	appendOpencode := "\nCOPY .sandman/bin/opencode /usr/local/bin/opencode\nRUN chmod +x /usr/local/bin/opencode\nENV PATH=\"/usr/local/bin:$PATH\"\n"
	if err := os.WriteFile(dockerfilePath, append(dockerfile, []byte(appendOpencode)...), 0644); err != nil {
		t.Fatalf("append fake opencode to Dockerfile: %v", err)
	}
}

// appendDockerfileRun appends a single RUN line to the scaffolded
// Dockerfile. The text is appended verbatim with a leading newline so
// it always lands on its own line.
func appendDockerfileRun(t *testing.T, repoDir, runLine string) {
	t.Helper()
	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	body, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	appendLine := "\n" + strings.TrimSpace(runLine) + "\n"
	if err := os.WriteFile(dockerfilePath, append(body, []byte(appendLine)...), 0644); err != nil {
		t.Fatalf("append RUN to Dockerfile: %v", err)
	}
}

// assertRunStartedAndFinished reads <repo>/.sandman/events.jsonl and
// asserts it contains exactly one `run.started` and one `run.finished`
// for the given issue. No internal payload assertions: only
// externally observable events, per the issue AC.
func assertRunStartedAndFinished(t *testing.T, repoDir string, issue int) {
	t.Helper()
	logPath := filepath.Join(repoDir, ".sandman", "events.jsonl")
	logger := &events.JSONLLogger{Path: logPath}
	written, err := logger.Read()
	if err != nil {
		t.Fatalf("read events log %s: %v", logPath, err)
	}
	var started, finished int
	for _, e := range written {
		if e.Issue != issue {
			continue
		}
		switch e.Type {
		case "run.started":
			started++
		case "run.finished":
			finished++
		}
	}
	if started != 1 {
		t.Fatalf("expected exactly 1 run.started event for issue %d, got %d (events=%s)", issue, started, summarizeEvents(written, issue))
	}
	if finished != 1 {
		t.Fatalf("expected exactly 1 run.finished event for issue %d, got %d (events=%s)", issue, finished, summarizeEvents(written, issue))
	}
}

// summarizeEvents returns a short, human-readable summary of the
// events the test saw for the given issue. Used in failure messages
// so the operator can see what was emitted without dumping the full
// event log.
func summarizeEvents(all []events.Event, issue int) string {
	type minimal struct {
		Type      string `json:"type"`
		Timestamp string `json:"ts"`
		RunID     string `json:"run_id"`
	}
	var kept []minimal
	for _, e := range all {
		if e.Issue != issue {
			continue
		}
		kept = append(kept, minimal{Type: e.Type, Timestamp: e.Timestamp.Format(time.RFC3339Nano), RunID: e.RunID})
	}
	data, _ := json.Marshal(kept)
	return string(data)
}

// prFlowDepsForPresetMatrix returns the Dependencies wired the way
// prflow_e2e_test.go wires its prflow e2e surface: the real
// `github.CLIClient` (no new fake), the real `events.JSONLLogger`,
// and the real `prompt.Engine`. The fake GitHub surface comes
// from the `gh` shim that `runE2ERun` installs on PATH before
// driving `sandman` through the public CLI binary, so the
// orchestrator's `FetchIssue` path goes through the shim and the
// harness never has to pre-seed a fake GitHub client. The
// canonical fake task body lives in the in-container shim, which
// the production prompt engine never sees — the shim ignores the
// derived prompt and runs the canonical body verbatim, so the
// prompt engine still exercises the real seam with the issue body
// the `gh` shim returns.
func prFlowDepsForPresetMatrix(t *testing.T, repoDir string) Dependencies {
	t.Helper()
	return prFlowDeps(repoDir)
}

// runSandmanBinaryWithEnv is runSandmanBinary plus a per-call env
// override (PATH, GH_TOKEN, GITHUB_TOKEN). It is the same shape as
// runSandmanBinary in prflow_e2e_test.go but exposes the env so the
// harness can prepend its gh shim without mutating the process env
// for the whole test binary.
func runSandmanBinaryWithEnv(t *testing.T, binPath, workDir, ghBinDir string, args []string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"PATH="+ghBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_TOKEN=fake",
		"GITHUB_TOKEN=fake",
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), context.DeadlineExceeded
	}
	return string(out), err
}

// runE2EScaffold sets up a fresh e2e repo for the preset matrix:
// it inits the integration repo, runs `sandman init --build-tools
// <preset> --tool-version <toolVersion>`, forces `sandbox: podman`,
// and sets the review daemon guard the way prflow_e2e_test.go does
// it. The returned dir is the repo root on disk. The CLI-options
// path passes an empty dockerfileAppend; the edited-Dockerfile path
// passes a single `RUN ...` line to append after scaffolding. The
// toolVersion param is passed as --tool-version when non-empty.
func runE2EScaffold(t *testing.T, preset, toolVersion, dockerfileAppend string) (string, string) {
	t.Helper()
	containerRuntimeAvailable(t)
	binPath := buildSandmanBinary(t)
	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)
	initArgs := []string{"init", "--build-tools", preset}
	if toolVersion != "" {
		initArgs = append(initArgs, "--tool-version", toolVersion)
	}
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), initArgs...)
	forcePodmanSandbox(t, repoDir)
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "config", "set", "review_command", "/oc review")
	if dockerfileAppend != "" {
		appendDockerfileRun(t, repoDir, dockerfileAppend)
	}
	return binPath, repoDir
}

// runE2ERun installs the fake opencode + gh shims, builds the image
// explicitly (so the test fails fast on a broken Dockerfile before
// the orchestrator gets involved), and runs `sandman run 1` in
// container mode. It is the seam the language-specific children
// reuse to drive the orchestrator end to end. Returns the gh shim
// dir and the run output for callers that need to inspect the
// summary or the per-issue log.
func runE2ERun(t *testing.T, binPath, repoDir string) {
	t.Helper()
	installFakeTaskOpenCodeForContainer(t, repoDir)
	ghShimDir := t.TempDir()
	writeFakeGHShim(t, ghShimDir)
	writeFakeGHShimForContainer(t, filepath.Join(repoDir, ".sandman", "bin"))
	prependPath(t, ghShimDir)
	imageTag := "sandman-preset-matrix-" + strings.ToLower(t.Name())
	t.Cleanup(func() { _ = exec.Command("podman", "rmi", "-f", imageTag).Run() })
	buildCmd := exec.Command("podman", "build", "-t", imageTag, "-f",
		filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build image %q: %v\n%s", imageTag, err, out)
	}
	wakeupDir := t.TempDir()
	t.Setenv("SANDMAN_TEST_FAST", "1")
	t.Setenv("WAKEUP_DIR", wakeupDir)
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(wakeupDir, "wakeup"), []byte("ok"), 0644) })
	runArgs := []string{"run", "--sandbox", "podman", "--agent", "opencode", "1"}
	out, err := runSandmanBinaryWithEnv(t, binPath, repoDir, ghShimDir, runArgs)
	if err != nil {
		t.Fatalf("sandman run failed: %v\noutput:\n%s", err, out)
	}
	t.Logf("sandman run output:\n%s", out)
}

// findRunLogUnderBatches walks `.sandman/batches/` for the run.log
// of a single run for issue <issue>. The orchestrator places each
// run's log at `.sandman/batches/<batchID>/runs/<runID>/run.log`.
// The harness returns the path of the first matching run.log so
// callers can assert the agent actually emitted the expected
// output. It is fatal if no run log is found.
func findRunLogUnderBatches(t *testing.T, repoDir string, issue int) string {
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
			if !run.IsDir() {
				continue
			}
			if !strings.HasSuffix(run.Name(), suffix) {
				continue
			}
			logPath := filepath.Join(runsDir, run.Name(), "run.log")
			if _, err := os.Stat(logPath); err == nil {
				return logPath
			}
		}
	}
	t.Fatalf("run log for issue %d not found under %s", issue, batchesDir)
	// Unreachable: t.Fatalf calls runtime.Goexit, so the compiler
	// requires a return for the function signature.
	return ""
}

// assertFakeTaskMarkerInRunLog asserts the per-run log for issue 1
// contains the canonical fake-task marker string. The orchestrator
// captures the agent's stdout into `.sandman/batches/<batchID>/runs/
// <runID>/run.log`, which is host-visible after the run completes
// (the orchestrator does not delete the run folder, only the
// worktree at `.sandman/worktrees/<branch>`).
func assertFakeTaskMarkerInRunLog(t *testing.T, repoDir string, issue int) {
	t.Helper()
	logPath := findRunLogUnderBatches(t, repoDir, issue)
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read per-run log %s: %v", logPath, err)
	}
	if !strings.Contains(string(logData), presetMatrixHarnessFakeTaskMarker) {
		t.Fatalf("per-run log %s missing canonical fake-task marker, got:\n%s", logPath, string(logData))
	}
}

// TestPresetMatrixHarness_DotnetScaffolds pins the scaffold-only
// slice of the dotnet harness: running `sandman init --build-tools
// dotnet --tool-version lts` through the real binary produces
// `.sandman/{config.yaml,Dockerfile}` in the test repo and the
// Dockerfile contains the pinned dotnet version from the catalog.
func TestPresetMatrixHarness_DotnetScaffolds(t *testing.T) {
	containerRuntimeAvailable(t)

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "dotnet", "--tool-version", "lts")
	if err != nil {
		t.Fatalf("sandman init --build-tools dotnet --tool-version lts failed: %v\noutput:\n%s", err, out)
	}
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(repoDir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	wantVersion := scaffold.BundledDotnetVersion("lts")
	if wantVersion == "" {
		t.Fatalf("BundledDotnetVersion(\"lts\") returned empty; catalog may have drifted")
	}
	want := "# sandman dotnet-version: " + wantVersion
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("scaffolded Dockerfile missing %q (the toolchain-version-from-catalog AC):\n%s", want, dockerfile)
	}
}

// TestPresetMatrixHarness_DotnetRunProducesFakeArtifact pins the
// end-to-end `dotnet` CLI-options path: scaffold with dotnet+lts,
// fake opencode shim installed → podman build → sandman run 1
// → per-issue run log carries the canonical fake-task marker,
// and the events log has exactly one run.started and one
// run.finished for the fake issue.
func TestPresetMatrixHarness_DotnetRunProducesFakeArtifact(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "dotnet", "lts", "")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_DotnetRunWithEditedDockerfile pins the
// end-to-end `dotnet` edited-Dockerfile path: scaffold with
// dotnet+lts, append a RUN line to the Dockerfile, fake opencode
// shim installed → podman build → sandman run 1 → per-issue run
// log carries the canonical fake-task marker and the events log
// has the expected events.
func TestPresetMatrixHarness_DotnetRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "dotnet", "lts", "RUN touch /etc/sandman-preset-matrix-edited")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// runE2EScaffoldElixir scaffolds a repo for the elixir preset with
// --tool-version lts. It follows the same pattern as runE2EScaffoldNode
// but writes a mix.exs fixture before scaffolding so the elixir
// preset has a version hint to resolve from.
func runE2EScaffoldElixir(t *testing.T, preset, dockerfileAppend string) (string, string) {
	t.Helper()
	containerRuntimeAvailable(t)
	binPath := buildSandmanBinary(t)
	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)

	mixExs := `defmodule Demo.MixProject do
  use Mix.Project
  def project do
    [app: :demo, version: "0.1.0", elixir: "~> 1.18"]
  end
end
`
	if err := os.WriteFile(filepath.Join(repoDir, "mix.exs"), []byte(mixExs), 0644); err != nil {
		t.Fatalf("write mix.exs: %v", err)
	}

	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "init", "--build-tools", preset, "--tool-version", "lts")
	forcePodmanSandbox(t, repoDir)
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "config", "set", "review_command", "/oc review")
	if dockerfileAppend != "" {
		appendDockerfileRun(t, repoDir, dockerfileAppend)
	}
	return binPath, repoDir
}

// TestPresetMatrixHarness_ElixirScaffolds pins the scaffold-only
// slice of the elixir harness: running `sandman init --build-tools
// elixir --tool-version lts` through the real binary produces
// `.sandman/{config.yaml,Dockerfile}` in the test repo and the
// Dockerfile contains the pinned elixir version from the catalog.
func TestPresetMatrixHarness_ElixirScaffolds(t *testing.T) {
	containerRuntimeAvailable(t)

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	mixExs := `defmodule Demo.MixProject do
  use Mix.Project
  def project do
    [app: :demo, version: "0.1.0", elixir: "~> 1.18"]
  end
end
`
	if err := os.WriteFile(filepath.Join(repoDir, "mix.exs"), []byte(mixExs), 0644); err != nil {
		t.Fatalf("write mix.exs: %v", err)
	}

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "elixir", "--tool-version", "lts")
	if err != nil {
		t.Fatalf("sandman init --build-tools elixir --tool-version lts failed: %v\noutput:\n%s", err, out)
	}
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(repoDir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	wantVersion := scaffold.BundledElixirVersion("lts")
	if wantVersion == "" {
		t.Fatalf("BundledElixirVersion(\"lts\") returned empty; catalog may have drifted")
	}
	want := "# sandman elixir-version: " + wantVersion
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("scaffolded Dockerfile missing %q (the toolchain-version-from-catalog AC):\n%s", want, dockerfile)
	}
}

// TestPresetMatrixHarness_ElixirRunProducesFakeArtifact pins the
// end-to-end `elixir` CLI-options path: scaffold with elixir+lts,
// fake opencode shim installed → podman build → sandman run 1
// → per-issue run log carries the canonical fake-task marker,
// and the events log has exactly one run.started and one
// run.finished for the fake issue.
func TestPresetMatrixHarness_ElixirRunProducesFakeArtifact(t *testing.T) {
	binPath, repoDir := runE2EScaffoldElixir(t, "elixir", "")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_ElixirRunWithEditedDockerfile pins the
// end-to-end `elixir` edited-Dockerfile path: scaffold with
// elixir+lts, append a RUN line to the Dockerfile, fake opencode
// shim installed → podman build → sandman run 1 → per-issue run
// log carries the canonical fake-task marker and the events log
// has the expected events.
func TestPresetMatrixHarness_ElixirRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldElixir(t, "elixir", "RUN touch /etc/sandman-preset-matrix-elixir-edited")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_RustScaffolds pins the scaffold-only
// slice of the rust harness: running `sandman init --build-tools
// rust --tool-version lts` through the real binary produces
// `.sandman/{config.yaml,Dockerfile}` in the test repo and the
// Dockerfile contains the pinned rust version from the catalog.
func TestPresetMatrixHarness_RustScaffolds(t *testing.T) {
	containerRuntimeAvailable(t)

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	if err := os.WriteFile(filepath.Join(repoDir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\n"), 0644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
	}

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "rust", "--tool-version", "lts")
	if err != nil {
		t.Fatalf("sandman init --build-tools rust --tool-version lts failed: %v\noutput:\n%s", err, out)
	}
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(repoDir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	wantVersion := scaffold.BundledRustVersion("lts")
	if wantVersion == "" {
		t.Fatalf("BundledRustVersion(\"lts\") returned empty; catalog may have drifted")
	}
	want := "# sandman rust-version: " + wantVersion
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("scaffolded Dockerfile missing %q (the toolchain-version-from-catalog AC):\n%s", want, dockerfile)
	}
}

// TestPresetMatrixHarness_RustRunProducesFakeArtifact pins the
// end-to-end `rust` CLI-options path: scaffold with rust+lts,
// fake opencode shim installed → podman build → sandman run 1
// → per-issue run log carries the canonical fake-task marker,
// and the events log has exactly one run.started and one
// run.finished for the fake issue.
func TestPresetMatrixHarness_RustRunProducesFakeArtifact(t *testing.T) {
	binPath, repoDir := runE2EScaffoldRust(t, "rust", "")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_RustRunWithEditedDockerfile pins the
// end-to-end `rust` edited-Dockerfile path: scaffold with
// rust+lts, append a RUN line to the Dockerfile, fake opencode
// shim installed → podman build → sandman run 1 → per-issue run
// log carries the canonical fake-task marker and the events log
// has the expected events.
func TestPresetMatrixHarness_RustRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldRust(t, "rust", "RUN touch /etc/sandman-preset-matrix-rust-edited")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// runE2EScaffoldRust scaffolds a repo for the rust preset with
// --tool-version lts. It follows the same pattern as runE2EScaffoldNode
// but writes a Cargo.toml fixture before init so the scaffolder
// detects the repo as a Rust project and activates the rust preset.
func runE2EScaffoldRust(t *testing.T, preset, dockerfileAppend string) (string, string) {
	t.Helper()
	containerRuntimeAvailable(t)
	binPath := buildSandmanBinary(t)
	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\n"), 0644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
	}
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "init", "--build-tools", preset, "--tool-version", "lts")
	forcePodmanSandbox(t, repoDir)
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "config", "set", "review_command", "/oc review")
	if dockerfileAppend != "" {
		appendDockerfileRun(t, repoDir, dockerfileAppend)
	}
	return binPath, repoDir
}

// TestPresetMatrixHarness_JavaScaffolds pins the scaffold-only
// slice of the java harness: running `sandman init --build-tools
// java --tool-version lts` through the real binary produces
// `.sandman/{config.yaml,Dockerfile}` in the test repo and the
// Dockerfile contains the pinned java version from the catalog.
func TestPresetMatrixHarness_JavaScaffolds(t *testing.T) {
	containerRuntimeAvailable(t)

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	if err := os.WriteFile(filepath.Join(repoDir, "pom.xml"), []byte("<project></project>\n"), 0644); err != nil {
		t.Fatalf("write pom.xml: %v", err)
	}

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "java", "--tool-version", "lts")
	if err != nil {
		t.Fatalf("sandman init --build-tools java --tool-version lts failed: %v\noutput:\n%s", err, out)
	}
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(repoDir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	wantVersion := scaffold.BundledJavaVersion("lts")
	if wantVersion == "" {
		t.Fatalf("BundledJavaVersion(\"lts\") returned empty; catalog may have drifted")
	}
	want := "# sandman java-version: " + wantVersion
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("scaffolded Dockerfile missing %q (the toolchain-version-from-catalog AC):\n%s", want, dockerfile)
	}
}

// TestPresetMatrixHarness_JavaRunProducesFakeArtifact pins the
// end-to-end `java` CLI-options path: scaffold with pom.xml+lts,
// fake opencode shim installed → podman build → sandman run 1
// → per-issue run log carries the canonical fake-task marker,
// and the events log has exactly one run.started and one
// run.finished for the fake issue.
func TestPresetMatrixHarness_JavaRunProducesFakeArtifact(t *testing.T) {
	binPath, repoDir := runE2EScaffoldJava(t, "java", "")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_JavaRunWithEditedDockerfile pins the
// end-to-end `java` edited-Dockerfile path: scaffold with
// pom.xml+lts, append a RUN line to the Dockerfile, fake opencode
// shim installed → podman build → sandman run 1 → per-issue run
// log carries the canonical fake-task marker and the events log
// has the expected events.
func TestPresetMatrixHarness_JavaRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldJava(t, "java", "RUN touch /etc/sandman-preset-matrix-java-edited")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// runE2EScaffoldJava scaffolds a repo for the java preset with
// --tool-version lts. It follows the same pattern as runE2EScaffoldRust
// but writes a pom.xml fixture before init so the scaffolder
// detects the repo as a Java project and activates the java preset.
func runE2EScaffoldJava(t *testing.T, preset, dockerfileAppend string) (string, string) {
	t.Helper()
	containerRuntimeAvailable(t)
	binPath := buildSandmanBinary(t)
	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "pom.xml"), []byte("<project></project>\n"), 0644); err != nil {
		t.Fatalf("write pom.xml: %v", err)
	}
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "init", "--build-tools", preset, "--tool-version", "lts")
	forcePodmanSandbox(t, repoDir)
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "config", "set", "review_command", "/oc review")
	if dockerfileAppend != "" {
		appendDockerfileRun(t, repoDir, dockerfileAppend)
	}
	return binPath, repoDir
}

// TestPresetMatrixHarness_RubyScaffolds pins the scaffold-only slice
// of the Ruby preset: running `sandman init --build-tools ruby` through
// the real binary with `--tool-version lts` produces
// `.sandman/{config.yaml,Dockerfile}` with the ruby-version header
// set to the bundled catalog's lts. This proves the Ruby preset
// scaffold path works end to end and that the version is resolved
// from the catalog rather than hardcoded.
func TestPresetMatrixHarness_RubyScaffolds(t *testing.T) {
	containerRuntimeAvailable(t)

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	os.WriteFile(filepath.Join(repoDir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644)

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "ruby", "--tool-version", "lts")
	if err != nil {
		t.Fatalf("sandman init --build-tools ruby failed: %v\noutput:\n%s", err, out)
	}
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(repoDir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	wantVersion := scaffold.BundledRubyVersion("lts")
	if wantVersion == "" {
		t.Fatalf("BundledRubyVersion(\"lts\") returned empty; catalog may have drifted")
	}
	want := "# sandman ruby-version: " + wantVersion
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("scaffolded Dockerfile missing %q (the toolchain-version-from-catalog AC):\n%s", want, dockerfile)
	}
}

// TestPresetMatrixHarness_RubyRunProducesFakeArtifact pins the
// end-to-end `ruby` CLI-options path: scaffold with Gemfile,
// fake opencode shim installed → podman build → sandman run 1
// → per-issue run log carries the canonical fake-task marker,
// and the events log has exactly one run.started and one
// run.finished for the fake issue.
func TestPresetMatrixHarness_RubyRunProducesFakeArtifact(t *testing.T) {
	binPath, repoDir := runE2EScaffoldRuby(t, "ruby", "")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_RubyRunWithEditedDockerfile pins the
// end-to-end `ruby` edited-Dockerfile path: scaffold with Gemfile,
// append a RUN line to the Dockerfile, fake opencode shim installed
// → podman build → sandman run 1 → per-issue run log carries the
// canonical fake-task marker and the events log has the expected
// events.
func TestPresetMatrixHarness_RubyRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldRuby(t, "ruby", "RUN touch /etc/sandman-preset-matrix-ruby-edited")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// runE2EScaffoldRuby scaffolds a repo for the ruby preset with
// --tool-version lts. It follows the same pattern as runE2EScaffoldNode
// but writes a Gemfile fixture before init so the scaffolder detects
// the repo as a Ruby project and activates the ruby preset.
func runE2EScaffoldRuby(t *testing.T, preset, dockerfileAppend string) (string, string) {
	t.Helper()
	containerRuntimeAvailable(t)
	binPath := buildSandmanBinary(t)
	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "Gemfile"), []byte("source 'https://rubygems.org'\ngem 'rails'\n"), 0644); err != nil {
		t.Fatalf("write Gemfile: %v", err)
	}
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "init", "--build-tools", preset, "--tool-version", "lts")
	forcePodmanSandbox(t, repoDir)
	runRootCommand(t, prFlowDepsForPresetMatrix(t, repoDir), "config", "set", "review_command", "/oc review")
	if dockerfileAppend != "" {
		appendDockerfileRun(t, repoDir, dockerfileAppend)
	}
	return binPath, repoDir
}

// TestPresetMatrixHarness_PythonScaffolds pins the scaffold-only
// slice of the python harness: running `sandman init --build-tools
// python --tool-version lts` through the real binary produces
// `.sandman/{config.yaml,Dockerfile}` in the test repo and the
// Dockerfile contains the pinned python version from the catalog,
// resolved via the ltsFromLatest hook (one minor back from latest).
func TestPresetMatrixHarness_PythonScaffolds(t *testing.T) {
	containerRuntimeAvailable(t)

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	out, err := runSandmanBinary(t, binPath, repoDir, "init", "--build-tools", "python", "--tool-version", "lts")
	if err != nil {
		t.Fatalf("sandman init --build-tools python --tool-version lts failed: %v\noutput:\n%s", err, out)
	}
	for _, rel := range []string{".sandman/config.yaml", ".sandman/Dockerfile", ".sandman/prompt.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, rel)); err != nil {
			t.Fatalf("expected scaffolded %s: %v", rel, err)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(repoDir, ".sandman", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	wantVersion := scaffold.DefaultPythonLTSVersion()
	if wantVersion == "" {
		t.Fatalf("DefaultPythonLTSVersion() returned empty; catalog may have drifted")
	}
	want := "# sandman python-version: " + wantVersion
	if !strings.Contains(string(dockerfile), want) {
		t.Fatalf("scaffolded Dockerfile missing %q (the toolchain-version-from-catalog AC):\n%s", want, dockerfile)
	}
}

// TestPresetMatrixHarness_PythonRunProducesFakeArtifact pins the
// end-to-end `python` CLI-options path: scaffold with python+lts,
// fake opencode shim installed → podman build → sandman run 1
// → per-issue run log carries the canonical fake-task marker,
// and the events log has exactly one run.started and one
// run.finished for the fake issue.
func TestPresetMatrixHarness_PythonRunProducesFakeArtifact(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "python", "lts", "")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}

// TestPresetMatrixHarness_PythonRunWithEditedDockerfile pins the
// end-to-end `python` edited-Dockerfile path: scaffold with
// python+lts, append a RUN line to the Dockerfile, fake opencode
// shim installed → podman build → sandman run 1 → per-issue run
// log carries the canonical fake-task marker and the events log
// has the expected events.
func TestPresetMatrixHarness_PythonRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "python", "lts", "RUN touch /etc/sandman-preset-matrix-python-edited")
	runE2ERun(t, binPath, repoDir)
	assertFakeTaskMarkerInRunLog(t, repoDir, 1)
	assertRunStartedAndFinished(t, repoDir, 1)
}
