package cmd

import "testing"

func TestBuildPortalCommandArgsContinuePreset(t *testing.T) {
	args, err := buildPortalCommandArgs(portalCommandLaunchRequest{
		Preset: "continue",
		Issue:  42,
		Prompt: "finish the tests",
	})
	if err != nil {
		t.Fatalf("build command args: %v", err)
	}

	want := []string{"continue", "42", "finish the tests"}
	if len(args) != len(want) {
		t.Fatalf("expected %d args, got %#v", len(want), args)
	}
	for i, wantArg := range want {
		if args[i] != wantArg {
			t.Fatalf("arg %d: got %q want %q", i, args[i], wantArg)
		}
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
