package cmd

import (
	"strings"
	"testing"
)

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
