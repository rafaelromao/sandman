//go:build smoke || e2e

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const scriptedOpenCodeModel = "fixture/scripted"

func localLiveProvider(provider string) bool {
	return os.Getenv("CI") == "" && os.Getenv("GITHUB_ACTIONS") == "" &&
		os.Getenv("SANDMAN_LOCAL_LIVE_PROVIDER") == provider
}

func TestLiveProvidersRequireExplicitLocalSelection(t *testing.T) {
	for _, env := range []string{"CI", "GITHUB_ACTIONS", "SANDMAN_LOCAL_LIVE_PROVIDER"} {
		t.Setenv(env, "")
	}
	t.Setenv("SANDMAN_TEST_PROVIDERS", "all")
	t.Setenv("SANDMAN_FULL_REGRESSION", "1")
	if localLiveProvider("opencode") || localLiveProvider("claude") {
		t.Fatal("full regression must not activate a hosted provider")
	}
	for _, provider := range []string{"opencode", "claude"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("SANDMAN_LOCAL_LIVE_PROVIDER", provider)
			if !localLiveProvider(provider) {
				t.Fatal("explicit local opt-in must work")
			}
			for _, env := range []string{"CI", "GITHUB_ACTIONS"} {
				t.Run(env, func(t *testing.T) {
					t.Setenv(env, "true")
					if localLiveProvider(provider) {
						t.Fatal("live provider must remain disabled in CI")
					}
				})
			}
		})
	}
}

func requireScriptedOpenCodeE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("SANDMAN_RUN_AGENT_E2E") != "1" {
		t.Skip("set SANDMAN_RUN_AGENT_E2E=1 for the scripted OpenCode E2Es")
	}
}

type scriptedOpenCodeTurn struct {
	Role     string `json:"role"`
	Contains any    `json:"contains"` // a string or ordered list of required substrings
	Reply    any    `json:"reply"`
}

func scriptedOpenCodeTool(name string, args map[string]any) any {
	return map[string]any{"name": name, "arguments": args}
}

func scriptedOpenCodeBash(command string) any {
	return scriptedOpenCodeTool("bash", map[string]any{
		"command": command, "description": "Execute the fixture workflow", "timeout": 10000,
	})
}

func openCodeFixtureSource() string {
	_, source, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(source), "../../scripts/opencode_fixture.py")
}

func scriptedOpenCodePin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(openCodeFixtureSource()), "release_scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Providers map[string]struct {
			CLI string `json:"cli"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	pin := manifest.Providers["opencode"].CLI
	if pin == "" {
		t.Fatal("missing OpenCode CLI pin")
	}
	return pin
}

// Keep the CLI and its test-owned HTTP server in the same network namespace.
// The CI host tier already has only loopback. Direct tagged Linux tests re-run
// only their own test in a fresh user/network namespace if needed.
func runInLoopbackNamespace(t *testing.T) bool {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	isolated := len(interfaces) == 1 && interfaces[0].Flags&(net.FlagLoopback|net.FlagUp) == net.FlagLoopback|net.FlagUp
	if isolated {
		version, err := exec.CommandContext(t.Context(), "opencode", "--version").Output()
		if pin := scriptedOpenCodePin(t); err != nil || strings.TrimSpace(string(version)) != pin {
			t.Fatalf("need OpenCode CLI %s for scripted host coverage: got %q (%v)", pin, version, err)
		}
		return true
	}
	if runtime.GOOS != "linux" {
		t.Skip("scripted host OpenCode E2Es require an isolated Linux network namespace")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SANDMAN_FIXTURE_NETWORK_CHILD") == "1" {
		t.Fatal("scripted host namespace has network interfaces beyond loopback")
	}
	t.Setenv("SANDMAN_FIXTURE_NETWORK_CHILD", "1")
	cmd := exec.CommandContext(t.Context(), "unshare", "-Urn", "sh", "-ec",
		`ip link set lo up; exec "$@"`, "sh", binary, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scripted host E2E in loopback namespace: %v\n%s", err, output)
	}
	t.Logf("scripted host E2E in loopback namespace:\n%s", output)
	return false
}

const containerModelCommand = "python3 /sandman-model/fixture.py /sandman-model/turns.json "

// Scope networking and the read-only fixture mount to the runtime launch, never
// to image builds. The process in the tested container talks to its own loopback.
func prepareContainerModel(t *testing.T, turns []scriptedOpenCodeTurn) {
	t.Helper()
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source, err := os.ReadFile(openCodeFixtureSource())
	if err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(map[string]any{
		"turns": turns, "permissions": map[string]string{"bash": "allow"}, "cli": scriptedOpenCodePin(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"fixture.py": source, "turns.json": spec} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := `#!/bin/sh
set -f
old_ifs=$IFS
IFS=:
for directory in $PATH; do
  case "$directory" in ""|/sandman-model) continue;; esac
  binary=$directory/opencode
  if [ -x "$binary" ]; then
    IFS=$old_ifs
    case "$1" in run) exec ` + containerModelCommand + `"$binary" "$@";; esac
    exec "$binary" "$@"
  fi
done
printf '%s\n' 'Missing OpenCode executable in the generated image PATH' >&2
exit 127
`
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(wrapper), 0755); err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	state := t.TempDir()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
	script := "#!/bin/sh\nset -eu\n" +
		"if [ \"${1:-}\" = run ] && [ \"${2:-}\" = -d ]; then\n" +
		"  for arg do image=${command-}; command=${flag-}; flag=${target-}; target=$arg; done\n" +
		"  test \"$command $flag $target\" = 'tail -f /dev/null'\n" +
		"  image_env=$(" + quote(podman) + " image inspect \"$image\" --format '{{range .Config.Env}}{{println .}}{{end}}')\n" +
		"  image_path=$(printf '%s\\n' \"$image_env\" | sed -n 's/^PATH=//p')\n" +
		"  test -n \"$image_path\"\n" +
		"  shift\n  set -- run --network none --env \"PATH=/sandman-model:$image_path\"" +
		" -v " + quote(dir+":/sandman-model:ro,Z") +
		" -v " + quote(state+":/sandman-model-state:Z") + " \"$@\"\nfi\n" +
		"exec " + quote(podman) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "podman"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The Python fixture owns scripted model traffic only. The test continues to
// observe the managed command, GitHub mutations and real OpenCode session data.
func startScriptedOpenCode(t *testing.T, permissions map[string]string, turns []scriptedOpenCodeTurn) (string, func() error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, "python3", "-u", openCodeFixtureSource())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			_ = cmd.Wait()
			if stderr.Len() > 0 {
				t.Logf("scripted OpenCode fixture: %s", &stderr)
			}
		}
	})
	if err := json.NewEncoder(stdin).Encode(map[string]any{
		"permissions": permissions, "turns": turns,
	}); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		waited = true
		_ = cmd.Wait()
		t.Fatalf("start scripted OpenCode fixture: %v\n%s", err, &stderr)
	}
	if !json.Valid(line) {
		t.Fatal("scripted OpenCode fixture returned invalid configuration")
	}
	return string(bytes.TrimSpace(line)), func() error {
		_, writeErr := fmt.Fprintln(stdin, "verify")
		_ = stdin.Close()
		waited = true
		if err := cmd.Wait(); err != nil || writeErr != nil {
			return fmt.Errorf("scripted OpenCode fixture: %v / %v\n%s", writeErr, err, &stderr)
		}
		return nil
	}
}
