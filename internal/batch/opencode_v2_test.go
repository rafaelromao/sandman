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
