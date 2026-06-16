package cmd

import "testing"

func TestBuildPortalCommandArgsContinuePresetSingleIssue(t *testing.T) {
	args, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset: "continue",
		Issues: []int{42},
	})
	if err != nil {
		t.Fatalf("build command args: %v", err)
	}

	want := []string{"run", "--continue", "42"}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %#v", len(want), args)
	}
	for i, wantArg := range want {
		if args[i] != wantArg {
			t.Fatalf("arg %d: got %q want %q", i, args[i], wantArg)
		}
	}
}

func TestBuildPortalCommandArgsContinuePresetMultipleIssues(t *testing.T) {
	args, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset: "continue",
		Issues: []int{1, 42},
	})
	if err != nil {
		t.Fatalf("build command args: %v", err)
	}

	want := []string{"run", "--continue", "1", "42"}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %#v", len(want), args)
	}
	for i, wantArg := range want {
		if args[i] != wantArg {
			t.Fatalf("arg %d: got %q want %q", i, args[i], wantArg)
		}
	}
}

func TestBuildPortalCommandArgsContinuePresetIgnoresPrompt(t *testing.T) {
	args, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset: "continue",
		Issues: []int{42},
		Prompt: "finish the tests",
	})
	if err != nil {
		t.Fatalf("build command args: %v", err)
	}

	want := []string{"run", "--continue", "42"}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %#v", len(want), args)
	}
}

func TestBuildPortalCommandArgsContinuePresetRequiresIssues(t *testing.T) {
	_, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset: "continue",
		Issues: []int{},
	})
	if err == nil {
		t.Fatal("expected error when no issues provided")
	}
}

func TestBuildPortalCommandArgsCleanPresetRequiresConfirmation(t *testing.T) {
	_, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset:    "clean",
		CleanMode: "success",
	})
	if err == nil {
		t.Fatal("expected clean preset to require confirmation")
	}
}

func TestBuildPortalCommandArgsArchiveRunPreset(t *testing.T) {
	args, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset:       "archive",
		ArchiveMode:  "run",
		ArchiveRunID: "abc",
		Confirmed:    true,
	})
	if err != nil {
		t.Fatalf("build command args: %v", err)
	}

	want := []string{"archive", "run", "abc"}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %#v", len(want), args)
	}
	for i, wantArg := range want {
		if args[i] != wantArg {
			t.Fatalf("arg %d: got %q want %q", i, args[i], wantArg)
		}
	}
}

func TestBuildPortalCommandArgsArchiveRunPresetRequiresID(t *testing.T) {
	_, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset:      "archive",
		ArchiveMode: "run",
	})
	if err == nil {
		t.Fatal("expected archive run preset to require a run id")
	}
}

func TestBuildPortalCommandArgsArchiveOlderThanPreset(t *testing.T) {
	args, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset:               "archive",
		ArchiveMode:          "older-than",
		ArchiveOlderThanDays: "7",
		Confirmed:            true,
	})
	if err != nil {
		t.Fatalf("build command args: %v", err)
	}

	want := []string{"archive", "older-than", "7"}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %#v", len(want), args)
	}
	for i, wantArg := range want {
		if args[i] != wantArg {
			t.Fatalf("arg %d: got %q want %q", i, args[i], wantArg)
		}
	}
}

func TestBuildPortalCommandArgsArchiveOlderThanPresetRejectsInvalid(t *testing.T) {
	t.Run("non-numeric", func(t *testing.T) {
		_, err := buildPortalCommandArgs(portalCommandLaunchRequest{
			Preset:               "archive",
			ArchiveMode:          "older-than",
			ArchiveOlderThanDays: "abc",
		})
		if err == nil {
			t.Fatal("expected archive older-than preset to reject non-numeric days")
		}
	})
	t.Run("negative", func(t *testing.T) {
		_, err := buildPortalCommandArgs(portalCommandLaunchRequest{
			Preset:               "archive",
			ArchiveMode:          "older-than",
			ArchiveOlderThanDays: "-1",
		})
		if err == nil {
			t.Fatal("expected archive older-than preset to reject negative days")
		}
	})
	t.Run("missing", func(t *testing.T) {
		_, err := buildPortalCommandArgs(portalCommandLaunchRequest{
			Preset:      "archive",
			ArchiveMode: "older-than",
		})
		if err == nil {
			t.Fatal("expected archive older-than preset to require a day count")
		}
	})
}

func TestBuildPortalCommandArgsArchiveStalePreset(t *testing.T) {
	args, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset:      "archive",
		ArchiveMode: "stale",
		Confirmed:   true,
	})
	if err != nil {
		t.Fatalf("build command args: %v", err)
	}

	want := []string{"archive", "stale"}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %#v", len(want), args)
	}
	for i, wantArg := range want {
		if args[i] != wantArg {
			t.Fatalf("arg %d: got %q want %q", i, args[i], wantArg)
		}
	}
}

func TestBuildPortalCommandArgsArchiveUnknownMode(t *testing.T) {
	_, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset:      "archive",
		ArchiveMode: "bogus",
		Confirmed:   true,
	})
	if err == nil {
		t.Fatal("expected archive preset to reject unknown mode")
	}
}

func TestBuildPortalCommandArgsArchiveRequiresConfirmation(t *testing.T) {
	cases := []portalCommandLaunchRequest{
		{Preset: "archive", ArchiveMode: "run", ArchiveRunID: "abc"},
		{Preset: "archive", ArchiveMode: "older-than", ArchiveOlderThanDays: "7"},
		{Preset: "archive", ArchiveMode: "stale"},
	}
	for _, req := range cases {
		_, err := buildPortalCommandArgs(req)
		if err == nil {
			t.Fatalf("expected archive preset to require confirmation (mode=%q)", req.ArchiveMode)
		}
	}
}
