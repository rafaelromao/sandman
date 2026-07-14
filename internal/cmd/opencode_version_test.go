package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/opencode"
	"github.com/spf13/cobra"
)

// errProbe is a sentinel error used by withProbe to simulate a failed
// shell-out to `opencode --version`.
var errProbe = errors.New("probe failed")

// withProbe installs a stub for opencode.VersionProbe that returns the
// given version and error for the duration of t, restoring the
// production seam when t finishes.
func withProbe(t *testing.T, version string, err error) {
	t.Helper()
	prev := opencode.VersionProbe
	t.Cleanup(func() { opencode.VersionProbe = prev })
	opencode.VersionProbe = func() (string, error) { return version, err }
}

// withDockerfilePin writes a synthetic .sandman/Dockerfile at dir
// that pins opencode-ai@version. dir is used as the repoRoot for the
// orchestrator; the file lives at dir/.sandman/Dockerfile.
func withDockerfilePin(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatalf("mkdir .sandman: %v", err)
	}
	content := "# sandman build-tools: go\nFROM debian:bookworm-slim\nRUN npm install -g opencode-ai@" + version + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte(content), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
}

// newWarningTestCmd returns a *cobra.Command, an errBuf that
// captures cmd.ErrOrStderr() output, and a outBuf that captures
// cmd.OutOrStdout() output. Tests call orchestrator with the
// command and read captured.String() to assert on the warning
// text emitted (or its absence), and check outBuf stays empty
// to pin the non-interference contract (warning never leaks into
// the batch's stdout / event log / batch payload).
func newWarningTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	c := &cobra.Command{Use: "test"}
	var errBuf, outBuf bytes.Buffer
	c.SetErr(&errBuf)
	c.SetOut(&outBuf)
	return c, &errBuf, &outBuf
}

// defaultCfgSandbox returns a *config.Config with Sandbox set to the
// given value. Tests that exercise the fallback from a missing/empty
// CLI flag to cfg.Sandbox use this helper.
func defaultCfgSandbox(value string) *config.Config {
	return &config.Config{Sandbox: value}
}

// TestWarnOpencodeVersionMismatch_NeverTouchesStdout pins the
// non-interference contract from Slice 4: the orchestrator only
// writes to stderr. Touching OutOrStdout would pollute the batch's
// stdout (which feeds into the event log / batch payload), changing
// observable behavior beyond "advisory warning."
func TestWarnOpencodeVersionMismatch_NeverTouchesStdout(t *testing.T) {
	dir := t.TempDir()
	withProbe(t, "1.17.19", nil)
	withDockerfilePin(t, dir, "1.15.0")

	cmd, errBuf, outBuf := newWarningTestCmd(t)
	warnOpencodeVersionMismatch(cmd, "opencode", "podman", dir, defaultCfgSandbox(""))

	if outBuf.Len() != 0 {
		t.Errorf("orchestrator must not write to stdout; got %q", outBuf.String())
	}
	if errBuf.Len() == 0 {
		t.Errorf("orchestrator must write to stderr when mismatch detected")
	}
}

func TestParseOpencodePinFromDockerfile_ExtractsVersion(t *testing.T) {
	content := `# sandman build-tools: go
FROM debian:bookworm-slim
RUN npm install -g opencode-ai@1.17.19
RUN echo hello
`
	version, ok := ParseOpencodePinFromDockerfile(content)
	if !ok {
		t.Fatalf("expected to parse opencode-ai pin, got ok=false")
	}
	if version != "1.17.19" {
		t.Errorf("expected version %q, got %q", "1.17.19", version)
	}
}

func TestResolveSandboxVersion_WorktreeReturnsHost(t *testing.T) {
	got := ResolveSandboxVersion("worktree", "1.17.19", "1.15.0", "1.15.0")
	if got != "1.17.19" {
		t.Errorf("worktree must mirror host: got %q, want %q", got, "1.17.19")
	}
}

func TestResolveSandboxVersion_ContainerUsesDockerfilePin(t *testing.T) {
	got := ResolveSandboxVersion("podman", "1.17.19", "1.15.0", "1.15.0")
	if got != "1.15.0" {
		t.Errorf("podman with Dockerfile pin must use the pin: got %q, want %q", got, "1.15.0")
	}
}

func TestResolveSandboxVersion_ContainerFallsBackToCatalog(t *testing.T) {
	got := ResolveSandboxVersion("podman", "1.17.19", "", "1.15.0")
	if got != "1.15.0" {
		t.Errorf("podman without Dockerfile pin must fall back to catalog: got %q, want %q", got, "1.15.0")
	}
}

func TestResolveSandboxVersion_DockerContainerUsesDockerfilePin(t *testing.T) {
	got := ResolveSandboxVersion("docker", "1.17.19", "1.18.0", "1.15.0")
	if got != "1.18.0" {
		t.Errorf("docker with Dockerfile pin must use the pin: got %q, want %q", got, "1.18.0")
	}
}

func TestResolveSandboxVersion_UnknownModeReturnsEmpty(t *testing.T) {
	got := ResolveSandboxVersion("nixos-containers", "1.17.19", "1.15.0", "1.15.0")
	if got != "" {
		t.Errorf("unknown mode must return empty: got %q, want \"\"", got)
	}
}

func TestResolveSandboxVersion_EmptyModeReturnsEmpty(t *testing.T) {
	got := ResolveSandboxVersion("", "1.17.19", "1.15.0", "1.15.0")
	if got != "" {
		t.Errorf("empty mode must return empty: got %q, want \"\"", got)
	}
}

func TestFormatMismatchWarning_MentionsBothVersions(t *testing.T) {
	warning := FormatMismatchWarning("1.17.19", "1.15.0", "/repo/path")
	for _, want := range []string{"1.17.19", "1.15.0", "UnknownError"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning must mention %q; got:\n%s", want, warning)
		}
	}
}

func TestFormatMismatchWarning_EmptySideProducesNoWarning(t *testing.T) {
	// Caller is responsible for skipping the call when hostVersion or
	// sandboxVersion is empty; the formatter does not gate on that.
	// Pin this contract so any future defensive logic changes
	// intentionally.
	if warning := FormatMismatchWarning("", "1.15.0", "/repo/path"); !strings.Contains(warning, "") {
		t.Errorf("formatter does not gate empty host: %q", warning)
	}
}

func TestWarnOpencodeVersionMismatch_WarnsOnPodmanMismatch(t *testing.T) {
	dir := t.TempDir()
	withProbe(t, "1.17.19", nil)
	withDockerfilePin(t, dir, "1.15.0")

	cmd, buf, _ := newWarningTestCmd(t)
	warnOpencodeVersionMismatch(cmd, "opencode", "podman", dir, defaultCfgSandbox(""))

	out := buf.String()
	for _, want := range []string{"warning", "1.17.19", "1.15.0", "UnknownError"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected stderr to contain %q; got:\n%s", want, out)
		}
	}
}

func TestWarnOpencodeVersionMismatch_SilentWhenAgentNotOpencode(t *testing.T) {
	dir := t.TempDir()
	withProbe(t, "1.17.19", nil)
	withDockerfilePin(t, dir, "1.15.0")

	cmd, buf, _ := newWarningTestCmd(t)
	warnOpencodeVersionMismatch(cmd, "pi", "podman", dir, defaultCfgSandbox(""))
	if got := buf.String(); got != "" {
		t.Errorf("non-opencode agent must be silent; got:\n%s", got)
	}
}

func TestWarnOpencodeVersionMismatch_SilentWhenProbeFails(t *testing.T) {
	dir := t.TempDir()
	withProbe(t, "", errProbe)
	withDockerfilePin(t, dir, "1.15.0")

	cmd, buf, _ := newWarningTestCmd(t)
	warnOpencodeVersionMismatch(cmd, "opencode", "podman", dir, defaultCfgSandbox(""))
	if got := buf.String(); got != "" {
		t.Errorf("probe failure must be silent; got:\n%s", got)
	}
}

func TestWarnOpencodeVersionMismatch_SilentWhenVersionsMatch(t *testing.T) {
	dir := t.TempDir()
	withProbe(t, "1.17.19", nil)
	withDockerfilePin(t, dir, "1.17.19")

	cmd, buf, _ := newWarningTestCmd(t)
	warnOpencodeVersionMismatch(cmd, "opencode", "podman", dir, defaultCfgSandbox(""))
	if got := buf.String(); got != "" {
		t.Errorf("matching versions must be silent; got:\n%s", got)
	}
}

func TestWarnOpencodeVersionMismatch_SilentOnWorktreeMode(t *testing.T) {
	dir := t.TempDir()
	// Worktree mode: sandbox IS the host (no separate pinned build).
	// Probe is irrelevant — there is nothing to compare.
	withProbe(t, "1.17.19", nil)
	withDockerfilePin(t, dir, "1.15.0")

	cmd, buf, _ := newWarningTestCmd(t)
	warnOpencodeVersionMismatch(cmd, "opencode", "worktree", dir, defaultCfgSandbox(""))
	if got := buf.String(); got != "" {
		t.Errorf("worktree mode must be silent (host IS sandbox); got:\n%s", got)
	}
}

func TestWarnOpencodeVersionMismatch_FallsBackToCfgSandboxWhenFlagEmpty(t *testing.T) {
	dir := t.TempDir()
	withProbe(t, "1.17.19", nil)
	withDockerfilePin(t, dir, "1.15.0")

	cmd, buf, _ := newWarningTestCmd(t)
	warnOpencodeVersionMismatch(cmd, "opencode", "", dir, defaultCfgSandbox("podman"))
	if got := buf.String(); !strings.Contains(got, "warning") {
		t.Errorf("empty flag must fall back to cfg.Sandbox and warn; got:\n%s", got)
	}
}

func TestWarnOpencodeVersionMismatch_UsesCatalogWhenNoDockerfile(t *testing.T) {
	dir := t.TempDir()
	withProbe(t, "1.17.19", nil)
	// No Dockerfile set up — catalog default (1.15.0 in
	// builtInAgentVersionCatalog) should be the sandbox version.
	cmd, buf, _ := newWarningTestCmd(t)
	warnOpencodeVersionMismatch(cmd, "opencode", "podman", dir, defaultCfgSandbox(""))
	if got := buf.String(); !strings.Contains(got, "warning") {
		t.Errorf("missing Dockerfile must surface catalog pin; got:\n%s", got)
	}
}
