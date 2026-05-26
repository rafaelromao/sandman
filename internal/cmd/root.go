package cmd

import (
	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/spf13/cobra"
)

// IssuePicker selects issues interactively from a list.
type IssuePicker interface {
	Select(issues []github.Issue) ([]int, error)
}

// Dependencies holds the domain adapters injected into CLI commands.
type Dependencies struct {
	BatchRunner    batch.Runner
	ConfigStore    config.Store
	EventLog       events.EventLog
	GitHubClient   github.Client
	PromptRenderer prompt.Renderer
	IssuePicker    IssuePicker
	IsTTY          func() bool
	GitRunner      gitRunner
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

	root.AddCommand(NewInitCmd())
	root.AddCommand(NewRunCmd(deps))
	root.AddCommand(NewStatusCmd(deps.EventLog))
	root.AddCommand(NewHistoryCmd(deps.EventLog))
	root.AddCommand(NewContinueCmd(deps))
	root.AddCommand(NewCleanCmd(deps))
	root.AddCommand(NewConfigCmd(deps.ConfigStore))
	root.AddCommand(NewAttachCmd())
	root.AddCommand(NewPortalCmd(deps))

	return root
}
