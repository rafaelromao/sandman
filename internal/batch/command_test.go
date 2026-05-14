package batch

import "testing"

func TestRenderCommand_SubstitutesWorktree(t *testing.T) {
	got, err := RenderCommand("opencode --worktree {{.Worktree}}", CommandData{
		Worktree: "/tmp/sandman/worktrees/fix-bug",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "opencode --worktree /tmp/sandman/worktrees/fix-bug"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderCommand_InvalidTemplateReturnsError(t *testing.T) {
	_, err := RenderCommand("opencode {{.Unknown", CommandData{})
	if err == nil {
		t.Fatal("expected error for invalid template syntax")
	}
}

func TestRenderCommand_UnknownFieldReturnsError(t *testing.T) {
	_, err := RenderCommand("opencode --worktree {{.Typo}}", CommandData{
		Worktree: "/tmp/worktree",
	})
	if err == nil {
		t.Fatal("expected error for unknown template field")
	}
}

func TestRenderCommand_SubstitutesPromptFile(t *testing.T) {
	got, err := RenderCommand("opencode --prompt-file {{.PromptFile}}", CommandData{
		Worktree:   "/tmp/sandman/worktrees/fix-bug",
		PromptFile: ".sandman/prompt.md",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "opencode --prompt-file .sandman/prompt.md"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderCommand_PlainCommandPassesThrough(t *testing.T) {
	got, err := RenderCommand("opencode", CommandData{
		Worktree: "/tmp/worktree",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "opencode" {
		t.Errorf("got %q, want %q", got, "opencode")
	}
}
