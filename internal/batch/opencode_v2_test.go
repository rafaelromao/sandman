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
