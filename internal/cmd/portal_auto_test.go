package cmd

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestPortalLaunchDataFromConfigAutoMaxCountDefaultsFromConfig(t *testing.T) {
	cfg := &config.Config{AutoMaxCount: 17}
	data := portalLaunchDataFromConfig(cfg)
	if data.AutoMaxCount != 17 {
		t.Fatalf("expected AutoMaxCount=17 from config, got %d", data.AutoMaxCount)
	}
}

func TestPortalLaunchDataFromConfigAutoMaxCountFallsBackToDefault(t *testing.T) {
	data := portalLaunchDataFromConfig(nil)
	// The nil-guarded path sets AutoMaxCount to DefaultAutoMaxCount so the form
	// prefill matches the runtime default. Load() sets a concrete value for
	// loaded configs, and explicit cfg.AutoMaxCount <= 0 also falls back here.
	if data.AutoMaxCount != config.DefaultAutoMaxCount {
		t.Fatalf("expected AutoMaxCount=%d (default), got %d", config.DefaultAutoMaxCount, data.AutoMaxCount)
	}
}

func TestBuildPortalRunArgsAutoSelectionEmitsAutoFlag(t *testing.T) {
	args, err := buildPortalRunArgs(t.TempDir(), &config.Config{Agent: "opencode"}, portalLaunchRequest{
		SelectionMode: "auto",
		AutoMaxCount:  ptrInt(5),
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsContainSequence(t, args, []string{"--auto", "--count", "5"})
}

func TestBuildPortalRunArgsAutoSelectionIncludesLabelAndQuery(t *testing.T) {
	args, err := buildPortalRunArgs(t.TempDir(), &config.Config{Agent: "opencode"}, portalLaunchRequest{
		SelectionMode: "auto",
		Label:         "bug",
		Query:         "is:open label:bug",
		AutoMaxCount:  ptrInt(3),
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsContainSequence(t, args, []string{"--auto", "--count", "3", "--label", "bug", "--query", "is:open label:bug"})
}

func TestBuildPortalRunArgsAutoSelectionDefaultsToConfigCount(t *testing.T) {
	cfg := &config.Config{Agent: "opencode", AutoMaxCount: 25}
	args, err := buildPortalRunArgs(t.TempDir(), cfg, portalLaunchRequest{
		SelectionMode: "auto",
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsContainSequence(t, args, []string{"--auto", "--count", "25"})
}

func TestBuildPortalRunArgsAutoSelectionFallsBackToDefault(t *testing.T) {
	cfg := &config.Config{Agent: "opencode", AutoMaxCount: config.DefaultAutoMaxCount}
	args, err := buildPortalRunArgs(t.TempDir(), cfg, portalLaunchRequest{
		SelectionMode: "auto",
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsContainSequence(t, args, []string{"--auto", "--count", "50"})
}

func TestBuildPortalRunArgsAutoSelectionRespectsExplicitZeroAsUnlimited(t *testing.T) {
	cfg := &config.Config{Agent: "opencode", AutoMaxCount: 0}
	args, err := buildPortalRunArgs(t.TempDir(), cfg, portalLaunchRequest{
		SelectionMode: "auto",
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsContainSequence(t, args, []string{"--auto"})
	assertArgsDoNotContain(t, args, "--count")
}

func TestBuildPortalRunArgsLabelSelectionDoesNotEmitAuto(t *testing.T) {
	args, err := buildPortalRunArgs(t.TempDir(), &config.Config{Agent: "opencode"}, portalLaunchRequest{
		SelectionMode: "label",
		Label:         "bug",
		AutoMaxCount:  ptrInt(3),
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsDoNotContain(t, args, "--auto")
}

func TestBuildPortalRunArgsQuerySelectionDoesNotEmitAuto(t *testing.T) {
	args, err := buildPortalRunArgs(t.TempDir(), &config.Config{Agent: "opencode"}, portalLaunchRequest{
		SelectionMode: "query",
		Query:         "is:open label:bug",
		AutoMaxCount:  ptrInt(3),
	})
	if err != nil {
		t.Fatalf("build run args: %v", err)
	}

	assertArgsDoNotContain(t, args, "--auto")
}
