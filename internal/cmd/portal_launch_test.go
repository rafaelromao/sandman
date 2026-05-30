package cmd

import (
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestPortalLaunchDataFromConfigUsesRalphLoopLabel(t *testing.T) {
	data := portalLaunchDataFromConfig(nil)
	got := string(data.SelectionModeOptionsHTML)
	if !strings.Contains(got, "Ralph Loop") {
		t.Fatalf("expected Ralph Loop label, got %q", got)
	}
	if strings.Contains(got, "Next ready issue") {
		t.Fatalf("expected old label to be removed, got %q", got)
	}
}

func TestBuildPortalRunArgsQuerySelectionUsesQueryOnly(t *testing.T) {
	args, err := buildPortalRunArgs(t.TempDir(), &config.Config{Agent: "opencode"}, portalLaunchRequest{
		SelectionMode: "query",
		Query:         "is:open label:bug",
		Ralph:         ptrInt(3),
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsContainSequence(t, args, []string{"--query", "is:open label:bug"})
	assertArgsDoNotContain(t, args, "--ralph")
}

func TestBuildPortalRunArgsLabelSelectionUsesLabelOnly(t *testing.T) {
	args, err := buildPortalRunArgs(t.TempDir(), &config.Config{Agent: "opencode"}, portalLaunchRequest{
		SelectionMode: "label",
		Label:         "bug",
		Ralph:         ptrInt(3),
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsContainSequence(t, args, []string{"--label", "bug"})
	assertArgsDoNotContain(t, args, "--ralph")
}

func TestBuildPortalRunArgsRalphSelectionIncludesLabelAndQuery(t *testing.T) {
	args, err := buildPortalRunArgs(t.TempDir(), &config.Config{Agent: "opencode"}, portalLaunchRequest{
		SelectionMode: "ralph",
		Label:         "bug",
		Query:         "is:open label:bug",
		Ralph:         ptrInt(3),
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsContainSequence(t, args, []string{"--ralph", "3", "--label", "bug", "--query", "is:open label:bug"})
}

func ptrInt(v int) *int {
	return &v
}

func assertArgsContainSequence(t *testing.T, args []string, want []string) {
	t.Helper()
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return
		}
	}
	t.Fatalf("expected args to contain sequence %v, got %v", want, args)
}

func assertArgsDoNotContain(t *testing.T, args []string, want string) {
	t.Helper()
	for _, arg := range args {
		if arg == want {
			t.Fatalf("expected args not to contain %q, got %v", want, args)
		}
	}
}
