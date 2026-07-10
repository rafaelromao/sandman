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
//   - runs `sandman run` in container mode driving the REAL opencode
//     agent (installed by the scaffolded image, using the host's opencode
//     auth snapshotted into the container) against one faked GitHub issue,
//     with a canonical language-agnostic task body that is identical for
//     every preset;
//   - asserts the run advanced the agent branch (the agent committed real
//     work, observable in the branch tree) and wrote the expected
//     run.started/run.finished events.
//
// Only `gh` is faked (host + in-container shims); no real GitHub repo,
// issue, or PR is ever created. The `generic` preset is the canonical
// carrier for both tool-add paths (CLI-options and edited-Dockerfile) per
// the parent issue (#2056); the language-specific children (#2059+)
// reuse the same harness helpers.
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

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/scaffold"
	"github.com/rafaelromao/sandman/internal/testenv"
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

// TestPresetMatrixHarness_GenericRunExecutesRealTask pins the end-to-end
// `generic` CLI-options path: scaffold → `podman build` → `sandman run 1`
// driving the REAL opencode agent (installed by the scaffolded image, host
// opencode auth snapshotted in) against the canonical language-agnostic
// task. The agent branch advances beyond baseline (it committed real work)
// and the events log has exactly one run.started and one run.finished. Only
// `gh` is faked; no real GitHub repo, issue, or PR is created.
//
// This is the canonical carrier for the CLI-options tool-add path
// (issue #2056 user story 12). The edited-Dockerfile variant is pinned by
// TestPresetMatrixHarness_GenericRunWithEditedDockerfile.
func TestPresetMatrixHarness_GenericRunExecutesRealTask(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "generic", "", "")
	baseline := runE2ERun(t, binPath, repoDir, "generic")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}

// TestPresetMatrixHarness_GenericRunWithEditedDockerfile pins the
// end-to-end `generic` edited-Dockerfile path: scaffold → append a
// known `RUN` line to `.sandman/Dockerfile` → `podman build` (which
// must still succeed) → `sandman run 1` with the REAL opencode agent
// → the agent branch advances and the events log has the expected
// events.
//
// This is the canonical carrier for the edited-Dockerfile tool-add
// path (issue #2056 user story 11). The CLI-options variant is
// pinned by TestPresetMatrixHarness_GenericRunExecutesRealTask.
func TestPresetMatrixHarness_GenericRunWithEditedDockerfile(t *testing.T) {
	// The harness uses a touch marker so the edit is observably
	// present in the built image without dragging in a
	// network-dependent apt-get install (the shared packages
	// list already installs the most common tools; the harness
	// is exercising the "edited Dockerfile still builds" path,
	// not the network).
	binPath, repoDir := runE2EScaffold(t, "generic", "", "RUN touch /etc/sandman-preset-matrix-edited")
	baseline := runE2ERun(t, binPath, repoDir, "generic")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
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

// TestPresetMatrixHarness_GoRunExecutesRealTask pins the end-to-end
// `go` CLI-options path: scaffold with a bare go.mod → `podman build` →
// `sandman run 1` driving the REAL opencode agent against the canonical
// task. The agent branch advances beyond baseline and the events log has
// exactly one run.started and one run.finished. Only `gh` is faked.
//
// This mirrors TestPresetMatrixHarness_GenericRunExecutesRealTask but for
// the Go preset instead of generic.
func TestPresetMatrixHarness_GoRunExecutesRealTask(t *testing.T) {
	binPath, repoDir := runE2EScaffoldWithGoMod(t, "go", "")
	baseline := runE2ERun(t, binPath, repoDir, "go")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}

// TestPresetMatrixHarness_GoRunWithEditedDockerfile pins the end-to-end
// `go` edited-Dockerfile path: scaffold with a bare go.mod → append a
// `RUN` line to `.sandman/Dockerfile` → `podman build` (still succeeds)
// → `sandman run 1` with the REAL opencode agent → the agent branch
// advances and the events log has the expected events.
//
// This mirrors TestPresetMatrixHarness_GenericRunWithEditedDockerfile
// but for the Go preset instead of generic.
func TestPresetMatrixHarness_GoRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldWithGoMod(t, "go", "RUN touch /etc/sandman-preset-matrix-edited")
	baseline := runE2ERun(t, binPath, repoDir, "go")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
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

// TestPresetMatrixHarness_NodeRunExecutesRealTask pins the
// end-to-end `node` CLI-options path: scaffold with node+lts → podman
// build → sandman run 1 driving the REAL opencode agent against the
// canonical task. The agent branch advances beyond baseline and the
// events log has exactly one run.started and one run.finished. Only
// `gh` is faked.
func TestPresetMatrixHarness_NodeRunExecutesRealTask(t *testing.T) {
	binPath, repoDir := runE2EScaffoldNode(t, "node", "")
	baseline := runE2ERun(t, binPath, repoDir, "node")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}

// TestPresetMatrixHarness_NodeRunWithEditedDockerfile pins the
// end-to-end `node` edited-Dockerfile path: scaffold with node+lts,
// append a RUN line to the Dockerfile → podman build (still succeeds)
// → sandman run 1 with the REAL opencode agent → the agent branch
// advances and the events log has the expected events.
func TestPresetMatrixHarness_NodeRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldNode(t, "node", "RUN touch /etc/sandman-preset-matrix-node-edited")
	baseline := runE2ERun(t, binPath, repoDir, "node")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
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

// presetMatrixBranch is the agent branch sandman creates for issue #1
// (title "Fix failing test" from the faked gh shim). The slug is stable
// and matches the opencode_subagent_e2e_test.go convention, so the
// post-run assertions can read the agent's committed work from the
// branch tip (the orchestrator removes the worktree after the run).
const presetMatrixBranch = "sandman/1-fix-failing-test"

// requirePresetMatrixE2E gates the real-opencode preset-matrix run tests.
// They need a container runtime AND a working opencode install with auth on
// the host (the host opencode config is snapshotted into the container), and
// they never run in CI (CI does not build the e2e tag and has no provider
// auth). Each precondition skips cleanly so the suite degrades gracefully on
// a machine that cannot run the real agent.
func requirePresetMatrixE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Skip("skip preset-matrix e2e in CI")
	}
	containerRuntimeAvailable(t)
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("skip preset-matrix e2e: opencode unavailable on PATH: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	authPath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if _, err := os.Stat(authPath); err != nil {
		t.Skipf("skip preset-matrix e2e: no opencode auth at %s", authPath)
	}
}

// customizePresetMatrixOpencodeAgent rewires the scaffolded opencode agent
// for a real in-container run, mirroring prflow_e2e_test.go's
// customizeOpenCodeAgentForContainer: --pure (no host sessions/config leak
// into the run), --dangerously-skip-permissions (the container is already the
// sandbox), the in-container gh shim first on PATH, and the resolved test
// model. The model comes from SANDMAN_TEST_MODEL_OPENCODE when set, else the
// sandman default (opencode/big-pickle).
func customizePresetMatrixOpencodeAgent(t *testing.T, repoDir string) {
	t.Helper()
	model := testenv.ResolveTestModel("opencode", config.DefaultModel)
	cfgPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	agent, err := cfg.ResolveAgentProvider("opencode")
	if err != nil {
		t.Fatalf("resolve opencode agent: %v", err)
	}
	agent.Model = model
	agent.Command = fmt.Sprintf(`PATH=/workspace/.sandman/bin:${PATH} opencode run --pure --dangerously-skip-permissions -m %s "$(cat {{.PromptFile}})"`, model)
	if cfg.AgentProviders == nil {
		cfg.AgentProviders = map[string]config.Agent{}
	}
	cfg.AgentProviders["opencode"] = agent
	if cfg.Agents == nil {
		cfg.Agents = map[string]config.Agent{}
	}
	cfg.Agents["opencode"] = agent
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// writePresetMatrixTaskTemplate writes the canonical language-agnostic task
// the REAL opencode agent executes for EVERY preset to a dedicated template
// file and returns its absolute path. runE2ERun passes it via `sandman run
// --template`, which the prompt engine reads directly (prompt/engine.go) and
// which makes MaterializePromptFile a no-op — so the lean task template is
// used verbatim regardless of the scaffolded .sandman/prompt.md (which the
// orchestrator would otherwise re-materialize with the heavy default
// workflow). The task body is identical across presets; the faked gh issue
// only supplies the title/number the prompt engine substitutes. The prompt
// tells the agent to commit on the current branch (so the harness can assert
// the branch advanced) and forbids pushing or opening a PR.
func writePresetMatrixTaskTemplate(t *testing.T, repoDir string) string {
	t.Helper()
	templatePath := filepath.Join(repoDir, ".sandman", "preset-matrix-task.md")
	prompt := `# Task

Issue #{{ISSUE_NUMBER}}: {{ISSUE_TITLE}}

This repository's main language has a source file that currently prints the wrong value (0). Change it so it prints 42 instead.

Then create a file named answer.txt at the root of the repository whose entire contents are exactly 42 (just those two digits, no newline, no other text).

When you are done, stage every change and create exactly one commit on the current branch. Do not push, do not run gh, and do not open a pull request.
`
	if err := os.WriteFile(templatePath, []byte(prompt), 0644); err != nil {
		t.Fatalf("write task template: %v", err)
	}
	abs, err := filepath.Abs(templatePath)
	if err != nil {
		t.Fatalf("resolve task template path: %v", err)
	}
	return abs
}

// seedPresetMatrixProject writes a single source file in the preset's language
// that prints 0, giving the real opencode agent a concrete, language-specific
// starting point for the canonical task. The seed is never compiled by the
// scaffolded image build (the Dockerfile only installs the toolchain), so a
// root-level source file that a given language's build would not pick up is
// fine — opencode edits it as part of the task, which is what proves the
// preset's container + toolchain + opencode path works end to end.
func seedPresetMatrixProject(t *testing.T, repoDir, preset string) {
	t.Helper()
	var path, body string
	switch preset {
	case "generic":
		path, body = "answer.sh", "#!/bin/sh\necho 0\n"
	case "go":
		path, body = "answer.go", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(0) }\n"
	case "node":
		path, body = "answer.js", "console.log(0)\n"
	case "dotnet":
		path, body = "answer.cs", "using System;\n\nclass Answer\n{\n\tstatic void Main()\n\t{\n\t\tConsole.WriteLine(0);\n\t}\n}\n"
	case "python":
		path, body = "answer.py", "print(0)\n"
	case "elixir":
		path, body = "answer.exs", "IO.puts(0)\n"
	case "rust":
		path, body = "answer.rs", "fn main() { println!(\"0\"); }\n"
	case "ruby":
		path, body = "answer.rb", "puts 0\n"
	case "java":
		path, body = "Answer.java", "public class Answer {\n\tpublic static void main(String[] args) {\n\t\tSystem.out.println(0);\n\t}\n}\n"
	default:
		t.Fatalf("unknown preset %q for seedPresetMatrixProject", preset)
	}
	if err := os.WriteFile(filepath.Join(repoDir, path), []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
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
// and the real `prompt.Engine`. The fake GitHub surface comes from the
// `gh` shim that `runE2ERun` installs on PATH (host) and into
// `.sandman/bin` (container) before driving `sandman` through the public
// CLI binary, so the orchestrator's `FetchIssue` path goes through the
// shim and the harness never has to pre-seed a fake GitHub client. The
// real opencode agent is driven by the canonical task body in
// `.sandman/prompt.md`, which the production prompt engine renders, so
// the prompt engine still exercises the real seam.
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

// runE2ERun drives a REAL opencode agent against issue #1 in a container,
// mirroring the prflow real-opencode harness. Only `gh` is faked (host +
// in-container shims); the agent itself is the real opencode binary installed
// by the scaffolded image, using the host's opencode auth snapshotted into
// the container. The canonical task lives in .sandman/prompt.md so it is
// identical for every preset. The image is built explicitly first so a broken
// Dockerfile fails fast before the orchestrator is invoked. The returned
// baseline is the repo HEAD before the run; callers assert the agent advanced
// its branch beyond it.
func runE2ERun(t *testing.T, binPath, repoDir, preset string) string {
	t.Helper()
	requirePresetMatrixE2E(t)
	seedPresetMatrixProject(t, repoDir, preset)
	customizePresetMatrixOpencodeAgent(t, repoDir)
	taskTemplate := writePresetMatrixTaskTemplate(t, repoDir)

	ghShimDir := t.TempDir()
	writeFakeGHShim(t, ghShimDir)
	writeFakeGHShimForContainer(t, filepath.Join(repoDir, ".sandman", "bin"))
	prependPath(t, ghShimDir)

	// Podman stores its image layers under the user's container store, but its
	// build temp (blob extraction) follows TMPDIR — which defaults to /tmp, a
	// small tmpfs on most dev machines. Big toolchain images (rust, java,
	// dotnet) overflow it with "disk quota exceeded". Relocate TMPDIR onto a
	// larger filesystem for this test so both the harness build and the
	// orchestrator's build (sandman run inherits the env) have room.
	if home, err := os.UserHomeDir(); err == nil {
		largeTemp := filepath.Join(home, ".cache", "sandman-e2e-podman")
		if os.MkdirAll(largeTemp, 0755) == nil {
			t.Setenv("TMPDIR", largeTemp)
		}
	}

	imageTag := "sandman-preset-matrix-" + strings.ToLower(t.Name())
	// Each preset image carries a full language toolchain (rust/java/dotnet
	// images are multiple GB), and the matrix builds one per test. Reclaim all
	// unused images before each build so the 18-image matrix does not exhaust
	// podman's storage mid-suite (observed as "disk quota exceeded" on the
	// rust preset). The just-built image is removed by the cleanup below.
	_ = exec.Command("podman", "image", "prune", "-f").Run()
	t.Cleanup(func() {
		_ = exec.Command("podman", "rmi", "-f", imageTag).Run()
		_ = exec.Command("podman", "image", "prune", "-f").Run()
	})
	buildCmd := exec.Command("podman", "build", "-t", imageTag, "-f",
		filepath.Join(repoDir, ".sandman", "Dockerfile"), repoDir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build image %q: %v\n%s", imageTag, err, out)
	}

	baselineOut, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD before run: %v", err)
	}
	baseline := strings.TrimSpace(string(baselineOut))

	runArgs := []string{"run", "--sandbox", "podman", "--agent", "opencode", "--template", taskTemplate, "1"}
	out, err := runSandmanBinaryWithEnv(t, binPath, repoDir, ghShimDir, runArgs)
	t.Logf("sandman run returned err=%v\noutput:\n%s", err, out)
	if err != nil {
		t.Fatalf("sandman run failed: %v\noutput:\n%s", err, out)
	}
	return baseline
}

// assertPresetMatrixAgentWorked asserts the real opencode run did real work:
// the agent branch advanced beyond the pre-run baseline (the agent committed),
// and the canonical answer.txt artifact (content "42") is present in that
// branch's tree. The orchestrator removes the worktree after the run, so the
// assertions read from the branch tip rather than the worktree directory.
// Together with assertRunStartedAndFinished this proves the full
// init->build->run path worked end to end for the preset under test.
func assertPresetMatrixAgentWorked(t *testing.T, repoDir, baseline string) {
	t.Helper()
	branchOut, err := exec.Command("git", "-C", repoDir, "rev-parse", presetMatrixBranch).Output()
	if err != nil {
		t.Fatalf("rev-parse agent branch %s: %v (did the run create it?)", presetMatrixBranch, err)
	}
	branchHash := strings.TrimSpace(string(branchOut))
	if branchHash == "" || branchHash == baseline {
		t.Fatalf("expected agent branch %s to advance beyond baseline %s; got %q (the agent did not commit)", presetMatrixBranch, baseline, branchHash)
	}
	artifact, err := exec.Command("git", "-C", repoDir, "show", presetMatrixBranch+":answer.txt").Output()
	if err != nil {
		t.Fatalf("read answer.txt from branch %s: %v (did the agent create and commit it?)", presetMatrixBranch, err)
	}
	if !strings.Contains(string(artifact), "42") {
		t.Fatalf("agent branch %s answer.txt = %q, want it to contain %q", presetMatrixBranch, string(artifact), "42")
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

// TestPresetMatrixHarness_DotnetRunExecutesRealTask pins the
// end-to-end `dotnet` CLI-options path: scaffold with dotnet+lts →
// podman build → sandman run 1 driving the REAL opencode agent against
// the canonical task. The agent branch advances beyond baseline and the
// events log has exactly one run.started and one run.finished. Only `gh`
// is faked.
func TestPresetMatrixHarness_DotnetRunExecutesRealTask(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "dotnet", "lts", "")
	baseline := runE2ERun(t, binPath, repoDir, "dotnet")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}

// TestPresetMatrixHarness_DotnetRunWithEditedDockerfile pins the
// end-to-end `dotnet` edited-Dockerfile path: scaffold with dotnet+lts,
// append a RUN line to the Dockerfile → podman build (still succeeds) →
// sandman run 1 with the REAL opencode agent → the agent branch advances
// and the events log has the expected events.
func TestPresetMatrixHarness_DotnetRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "dotnet", "lts", "RUN touch /etc/sandman-preset-matrix-edited")
	baseline := runE2ERun(t, binPath, repoDir, "dotnet")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
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

// TestPresetMatrixHarness_ElixirRunExecutesRealTask pins the
// end-to-end `elixir` CLI-options path: scaffold with elixir+lts →
// podman build → sandman run 1 driving the REAL opencode agent against
// the canonical task. The agent branch advances beyond baseline and the
// events log has exactly one run.started and one run.finished. Only `gh`
// is faked.
func TestPresetMatrixHarness_ElixirRunExecutesRealTask(t *testing.T) {
	binPath, repoDir := runE2EScaffoldElixir(t, "elixir", "")
	baseline := runE2ERun(t, binPath, repoDir, "elixir")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}

// TestPresetMatrixHarness_ElixirRunWithEditedDockerfile pins the
// end-to-end `elixir` edited-Dockerfile path: scaffold with elixir+lts,
// append a RUN line to the Dockerfile → podman build (still succeeds) →
// sandman run 1 with the REAL opencode agent → the agent branch advances
// and the events log has the expected events.
func TestPresetMatrixHarness_ElixirRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldElixir(t, "elixir", "RUN touch /etc/sandman-preset-matrix-elixir-edited")
	baseline := runE2ERun(t, binPath, repoDir, "elixir")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
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

// TestPresetMatrixHarness_RustRunExecutesRealTask pins the
// end-to-end `rust` CLI-options path: scaffold with rust+lts →
// podman build → sandman run 1 driving the REAL opencode agent against
// the canonical task. The agent branch advances beyond baseline and the
// events log has exactly one run.started and one run.finished. Only `gh`
// is faked.
func TestPresetMatrixHarness_RustRunExecutesRealTask(t *testing.T) {
	binPath, repoDir := runE2EScaffoldRust(t, "rust", "")
	baseline := runE2ERun(t, binPath, repoDir, "rust")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}

// TestPresetMatrixHarness_RustRunWithEditedDockerfile pins the
// end-to-end `rust` edited-Dockerfile path: scaffold with rust+lts,
// append a RUN line to the Dockerfile → podman build (still succeeds) →
// sandman run 1 with the REAL opencode agent → the agent branch advances
// and the events log has the expected events.
func TestPresetMatrixHarness_RustRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldRust(t, "rust", "RUN touch /etc/sandman-preset-matrix-rust-edited")
	baseline := runE2ERun(t, binPath, repoDir, "rust")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
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

// TestPresetMatrixHarness_JavaRunExecutesRealTask pins the
// end-to-end `java` CLI-options path: scaffold with pom.xml+lts →
// podman build → sandman run 1 driving the REAL opencode agent against
// the canonical task. The agent branch advances beyond baseline and the
// events log has exactly one run.started and one run.finished. Only `gh`
// is faked.
func TestPresetMatrixHarness_JavaRunExecutesRealTask(t *testing.T) {
	binPath, repoDir := runE2EScaffoldJava(t, "java", "")
	baseline := runE2ERun(t, binPath, repoDir, "java")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}

// TestPresetMatrixHarness_JavaRunWithEditedDockerfile pins the
// end-to-end `java` edited-Dockerfile path: scaffold with pom.xml+lts,
// append a RUN line to the Dockerfile → podman build (still succeeds) →
// sandman run 1 with the REAL opencode agent → the agent branch advances
// and the events log has the expected events.
func TestPresetMatrixHarness_JavaRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldJava(t, "java", "RUN touch /etc/sandman-preset-matrix-java-edited")
	baseline := runE2ERun(t, binPath, repoDir, "java")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
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

// TestPresetMatrixHarness_RubyRunExecutesRealTask pins the
// end-to-end `ruby` CLI-options path: scaffold with Gemfile → podman
// build → sandman run 1 driving the REAL opencode agent against the
// canonical task. The agent branch advances beyond baseline and the
// events log has exactly one run.started and one run.finished. Only `gh`
// is faked.
func TestPresetMatrixHarness_RubyRunExecutesRealTask(t *testing.T) {
	binPath, repoDir := runE2EScaffoldRuby(t, "ruby", "")
	baseline := runE2ERun(t, binPath, repoDir, "ruby")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}

// TestPresetMatrixHarness_RubyRunWithEditedDockerfile pins the
// end-to-end `ruby` edited-Dockerfile path: scaffold with Gemfile,
// append a RUN line to the Dockerfile → podman build (still succeeds)
// → sandman run 1 with the REAL opencode agent → the agent branch
// advances and the events log has the expected events.
func TestPresetMatrixHarness_RubyRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffoldRuby(t, "ruby", "RUN touch /etc/sandman-preset-matrix-ruby-edited")
	baseline := runE2ERun(t, binPath, repoDir, "ruby")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
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

// TestPresetMatrixHarness_PythonRunExecutesRealTask pins the
// end-to-end `python` CLI-options path: scaffold with python+lts →
// podman build → sandman run 1 driving the REAL opencode agent against
// the canonical task. The agent branch advances beyond baseline and the
// events log has exactly one run.started and one run.finished. Only `gh`
// is faked.
func TestPresetMatrixHarness_PythonRunExecutesRealTask(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "python", "lts", "")
	baseline := runE2ERun(t, binPath, repoDir, "python")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}

// TestPresetMatrixHarness_PythonRunWithEditedDockerfile pins the
// end-to-end `python` edited-Dockerfile path: scaffold with python+lts,
// append a RUN line to the Dockerfile → podman build (still succeeds) →
// sandman run 1 with the REAL opencode agent → the agent branch advances
// and the events log has the expected events.
func TestPresetMatrixHarness_PythonRunWithEditedDockerfile(t *testing.T) {
	binPath, repoDir := runE2EScaffold(t, "python", "lts", "RUN touch /etc/sandman-preset-matrix-python-edited")
	baseline := runE2ERun(t, binPath, repoDir, "python")
	assertRunStartedAndFinished(t, repoDir, 1)
	assertPresetMatrixAgentWorked(t, repoDir, baseline)
}
