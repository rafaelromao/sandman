package cmd

import (
	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/spf13/cobra"
)

// Dependencies holds the domain adapters injected into CLI commands.
type Dependencies struct {
	BatchRunner    batch.Runner
	ConfigLoader   config.Loader
	EventLogger    events.Logger
	EventReader    events.Reader
	SandboxManager sandbox.Sandbox
	GitHubClient   github.Client
	PromptRenderer prompt.Renderer
}

// NewRootCmd constructs the command tree with injected dependencies.
func NewRootCmd(deps Dependencies) *cobra.Command {
	root := &cobra.Command{
		Use:   "sandman",
		Short: "Sandman orchestrates AFK coding agents in isolated sandboxes",
		Long: `Sandman is a terminal-native CLI tool for orchestrating AFK coding agents
in isolated sandboxes. It manages issue tracking, worktrees, containerized
execution, and event logging for automated coding workflows.`,
	}

	root.AddCommand(NewInitCmd(deps.ConfigLoader))
	root.AddCommand(NewRunCmd(deps))
	root.AddCommand(NewStatusCmd(deps.EventReader))
	root.AddCommand(NewHistoryCmd(deps.EventReader))
	root.AddCommand(NewRetryCmd(deps.BatchRunner))
	root.AddCommand(NewCleanCmd(deps.SandboxManager))
	root.AddCommand(NewConfigCmd(deps.ConfigLoader, ".sandman/config.yaml"))

	return root
}
