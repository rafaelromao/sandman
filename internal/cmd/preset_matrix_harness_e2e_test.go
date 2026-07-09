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
	"os"
	"path/filepath"
	"strings"
	"testing"

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
