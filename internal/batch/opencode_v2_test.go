package batch

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// Execute the rendered shell command, so a v1 flag or broken quoting cannot
// pass simply because a recording sandbox accepts arbitrary strings.
type fakeV2CLISandbox struct{ fakeSandbox }

func (s *fakeV2CLISandbox) Exec(ctx context.Context, command string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = s.workDir, stdout, stderr
	return cmd.Run()
}

func TestOpenCodeV2_FakeCLIRunAndReview(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = --version ]; then echo 'opencode v2.1.2'; exit; fi
[ "$1" = run ] || exit 40
shift
model=
auto=
while [ "$#" -gt 0 ]; do
 case "$1" in
 --variant|--dangerously-skip-permissions) echo "v1 flag rejected" >&2; exit 41 ;;
 --model) model="$2"; shift ;;
 --format) [ "$2" = json ] || exit 42; shift ;;
 --auto) auto=yes ;;
 --title|--session) shift ;;
 --continue) ;;
 *) case "$1" in 'a prompt'*) ;; *) exit 43 ;; esac ;;
 esac
 shift
done
[ "$model" = 'openai/test#high effort' ] || exit 44
[ "$auto" = yes ] || exit 45
printf '%s\n' '{"type":"text","sessionID":"ses-v2","part":{"text":"done"}}'
`
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, review := range []bool{false, true} {
		t.Run(map[bool]string{false: "prompt", true: "review"}[review], func(t *testing.T) {
			root := t.TempDir()
			layout := paths.NewLayout(&config.Config{}, root)
			sb := &fakeV2CLISandbox{fakeSandbox{workDir: root}}
			run := NewAgentRunWithLayout(nil, "test", sb, layout)
			run.preset, run.review = "opencode-v2", review
			run.model, run.variant = "openai/test#old", "high effort"
			run.dangerouslySkipPermissions = goldenBool(true)
			run.runFolder, run.outputWriter = filepath.Join(root, "run"), &bytes.Buffer{}
			result := run.Run(context.Background(), &spyRenderer{}, config.BuiltInAgentPresets["opencode-v2"].Command, prompt.RenderConfig{TaskPrompt: "a prompt"})
			if result.Status != "success" {
				t.Fatalf("fake v2 rejected launch: %+v", result)
			}
			data, err := os.ReadFile(filepath.Join(run.runFolder, "session.json"))
			if err != nil || !strings.Contains(string(data), "ses-v2") {
				t.Fatalf("v2 session not captured: %s %v", data, err)
			}
		})
	}
}

func TestOpenCodeV2_RendersChunkedToolEventsToLiveAndSavedLogs(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, root)
	var live bytes.Buffer
	const (
		firstChunk   = `{"type":"tool_use","sessionID":"ses-v2-log","part":{"tool":"shell","state":{"status":"completed","input":{"command":"git status --short --branch && git branch --show-current`
		lastChunk    = ` && git log -1 --oneline","workdir":"/workspace/repo"}}}}` + "\n"
		want         = "$ git status --short --branch && git branch --show-current && git log -1 --oneline @ /workspace/repo"
		wantRead     = "→ Read /workspace/repo/AGENTS.md (offset 1, limit 200)"
		wantSkill    = `→ Skill "sandman-run"`
		wantPatch    = "→ Edit /workspace/repo/main.go"
		wantAgent    = "→ Subagent explore: Audit tool schemas"
		wantWrite    = "→ Write /workspace/repo/new.txt"
		wantEdit     = "→ Edit /workspace/repo/changed.txt"
		wantGrep     = `✱ Grep "TODO" in /workspace/repo (include **/*.go)`
		wantGlob     = `✱ Glob "**/*.go" in /workspace/repo`
		wantQuestion = "→ Task Choose a recovery strategy"
		wantError    = "→ Read /workspace/repo/private.txt (offset 2, limit 10) (error: permission denied)"
		wantUnknown  = `future_tool {"query":"`
	)
	readEvent := `{"type":"tool_use","part":{"tool":"read","state":{"status":"completed","input":{"path":"/workspace/repo/AGENTS.md","offset":1,"limit":200}}}}` + "\n"
	skillEvent := `{"type":"tool_use","part":{"tool":"skill","state":{"status":"completed","input":{"id":"sandman-run"}}}}` + "\n"
	patchEvent := `{"type":"tool_use","part":{"tool":"patch","state":{"status":"completed","input":{"patchText":"*** Begin Patch\n*** Update File: /workspace/repo/main.go\n@@\n-old\n+new\n*** End Patch"}}}}` + "\n"
	agentEvent := `{"type":"tool_use","part":{"tool":"subagent","state":{"status":"completed","input":{"agent":"explore","description":"Audit tool schemas","prompt":"Inspect the supported OpenCode v2 tool inputs."}}}}` + "\n"
	writeEvent := `{"type":"tool_use","part":{"tool":"write","state":{"status":"completed","input":{"path":"/workspace/repo/new.txt","content":"file contents"}}}}` + "\n"
	editEvent := `{"type":"tool_use","part":{"tool":"edit","state":{"status":"completed","input":{"path":"/workspace/repo/changed.txt","oldString":"before","newString":"after"}}}}` + "\n"
	grepEvent := `{"type":"tool_use","part":{"tool":"grep","state":{"status":"completed","input":{"pattern":"TODO","path":"/workspace/repo","include":"**/*.go","literal":true,"caseSensitive":false,"limit":20}}}}` + "\n"
	globEvent := `{"type":"tool_use","part":{"tool":"glob","state":{"status":"completed","input":{"pattern":"**/*.go","path":"/workspace/repo","hidden":true,"limit":25}}}}` + "\n"
	questionEvent := `{"type":"tool_use","part":{"tool":"question","state":{"status":"completed","input":{"questions":[{"question":"Choose a recovery strategy","header":"Recovery","options":[{"label":"Retry","description":"Try again"}]}]}}}}` + "\n"
	errorFirstChunk := `{"type":"tool_use","part":{"tool":"read","state":{"status":"error","input":{"path":"/workspace/repo/private.txt","offset":2`
	errorLastChunk := `,"limit":10},"error":"permission denied"}}}` + "\n"
	unknownInput := strings.Repeat("x", 300)
	unknownEvent := `{"type":"tool_use","part":{"tool":"future_tool","state":{"status":"completed","input":{"query":"` + unknownInput + `"}}}}` + "\n"
	sb := &fakeSandbox{workDir: root}
	sb.execFunc = func(_ context.Context, _ string, stdout, _ io.Writer) error {
		for _, chunk := range []string{firstChunk, lastChunk, readEvent, skillEvent, patchEvent, agentEvent, writeEvent, editEvent, grepEvent, globEvent, questionEvent, errorFirstChunk, errorLastChunk, unknownEvent} {
			if _, err := io.WriteString(stdout, chunk); err != nil {
				return err
			}
		}
		return nil
	}
	run := NewAgentRunWithLayout(nil, "test", sb, layout)
	run.preset = "opencode-v2"
	run.runFolder = filepath.Join(root, "run")
	run.outputWriter = &live

	result := run.Run(context.Background(), &spyRenderer{result: "prompt"}, config.BuiltInAgentPresets["opencode-v2"].Command, prompt.RenderConfig{})
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	logData, err := os.ReadFile(filepath.Join(run.runFolder, "run.log"))
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	for name, output := range map[string]string{"saved run.log": string(logData), "live output": live.String()} {
		if !strings.Contains(output, want) {
			t.Errorf("%s = %q, want readable tool event %q", name, output, want)
		}
		if !strings.Contains(output, wantRead) {
			t.Errorf("%s = %q, want readable read event %q", name, output, wantRead)
		}
		if !strings.Contains(output, wantSkill) {
			t.Errorf("%s = %q, want readable skill event %q", name, output, wantSkill)
		}
		if !strings.Contains(output, wantPatch) {
			t.Errorf("%s = %q, want readable patch event %q", name, output, wantPatch)
		}
		if !strings.Contains(output, wantAgent) {
			t.Errorf("%s = %q, want readable delegated-agent event %q", name, output, wantAgent)
		}
		if !strings.Contains(output, wantWrite) || !strings.Contains(output, wantEdit) {
			t.Errorf("%s = %q, want readable V2 filesystem paths %q and %q", name, output, wantWrite, wantEdit)
		}
		if !strings.Contains(output, wantGrep) {
			t.Errorf("%s = %q, want readable V2 search details %q", name, output, wantGrep)
		}
		if !strings.Contains(output, wantGlob) {
			t.Errorf("%s = %q, want readable V2 glob details %q", name, output, wantGlob)
		}
		if !strings.Contains(output, wantQuestion) {
			t.Errorf("%s = %q, want readable V2 question details %q", name, output, wantQuestion)
		}
		if !strings.Contains(output, wantError) {
			t.Errorf("%s = %q, want readable error event %q", name, output, wantError)
		}
		if !strings.Contains(output, wantUnknown) || strings.Contains(output, unknownInput) {
			t.Errorf("%s = %q, want a useful but bounded unknown-tool diagnostic", name, output)
		}
		if strings.Contains(output, `\u0026\u0026`) || strings.Contains(output, `"command"`) || strings.Contains(output, `"path"`) || strings.Contains(output, `"id"`) || strings.Contains(output, `"patchText"`) || strings.Contains(output, `"prompt"`) || strings.Contains(output, `"content"`) || strings.Contains(output, `"oldString"`) {
			t.Errorf("%s contains raw JSON tool input: %q", name, output)
		}
	}
	identity, err := os.ReadFile(filepath.Join(run.runFolder, "session.json"))
	if err != nil || !strings.Contains(string(identity), `"session_id": "ses-v2-log"`) {
		t.Fatalf("session metadata = %s, %v; want captured v2 session", identity, err)
	}
}

func TestOpenCodeV2_DoesNotReuseV1Identity(t *testing.T) {
	root := t.TempDir()
	layout := paths.NewLayout(&config.Config{}, root)
	if err := writeAgentSession(layout.RunSessionPath("prior", "run"), opencodeSessionKind, "ses-v1"); err != nil {
		t.Fatal(err)
	}
	run := NewAgentRunWithLayout(nil, "", nil, layout)
	run.reuseSession, run.previousBatchID, run.previousRunID = true, "prior", "run"
	run.sessionWarning = &bytes.Buffer{}
	strategy := strategyForPreset("opencode-v2")
	session, useContinue := strategy.PrepareLaunch(run)
	if session != "" || !useContinue {
		t.Fatalf("v2 reused v1 identity: %q %t", session, useContinue)
	}
}

// Model the container's mount namespace by choosing its most specific mount.
// An individual DB bind does not cover a sidecar that did not exist at startup.
func v2MountedFile(t *testing.T, mounts []sandbox.ConfigMount, target string) string {
	t.Helper()
	var source string
	longest := -1
	for _, mount := range mounts {
		if target == mount.Target && len(mount.Target) > longest {
			source, longest = mount.Source, len(mount.Target)
		} else if rel, ok := strings.CutPrefix(target, strings.TrimSuffix(mount.Target, "/")+"/"); ok && len(mount.Target) > longest {
			source, longest = filepath.Join(mount.Source, filepath.FromSlash(rel)), len(mount.Target)
		}
	}
	if source == "" {
		t.Fatalf("no mount covers %s: %v", target, mounts)
	}
	return source
}

func TestOpenCodeV2_NewDatabaseAndSidecarsSurviveSnapshotCleanup(t *testing.T) {
	for _, existingDB := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-home", true: "stopped-host-service"}[existingDB], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dataDir := filepath.Join(home, ".local", "share", "opencode")
			if existingDB {
				if err := os.MkdirAll(dataDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dataDir, "opencode.db"), []byte("database"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			opts, err := buildStartOptions(config.BuiltInAgentPresets["opencode-v2"].Agent("opencode-v2"), lookupEnvFrom(nil))
			if err != nil {
				t.Fatal(err)
			}
			cleanup, err := PrepareContainerConfigMounts(context.Background(), t.TempDir(), "", &opts, func(context.Context) (string, error) { return "", nil })
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			// No WAL/SHM existed when mounts were prepared. They must be created
			// in host storage, not beside a snapshot's individually mounted DB.
			for _, name := range []string{"opencode.db", "opencode.db-wal", "opencode.db-shm"} {
				path := v2MountedFile(t, opts.ConfigMounts, "/.local/share/opencode/"+name)
				if err := os.WriteFile(path, []byte("session-update:"+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Replacement is another directory operation individual file binds miss.
			replacement := v2MountedFile(t, opts.ConfigMounts, "/.local/share/opencode/opencode.db.new")
			if err := os.WriteFile(replacement, []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, v2MountedFile(t, opts.ConfigMounts, "/.local/share/opencode/opencode.db")); err != nil {
				t.Fatal(err)
			}
			cleanup()
			for _, name := range []string{"opencode.db", "opencode.db-wal", "opencode.db-shm"} {
				data, err := os.ReadFile(filepath.Join(dataDir, name))
				want := "session-update:" + name
				if name == "opencode.db" {
					want = "replacement"
				}
				if err != nil || string(data) != want {
					t.Fatalf("%s was lost on cleanup: %q %v", name, data, err)
				}
			}
		})
	}
}
