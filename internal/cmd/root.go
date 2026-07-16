package cmd

import (
	"errors"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/review"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// IssuePicker selects issues interactively from a list.
type IssuePicker interface {
	Select(issues []github.Issue) ([]int, error)
}

// Dependencies holds the domain adapters injected into CLI commands.
type Dependencies struct {
	BatchRunner      batch.Runner
	ConfigStore      config.Store
	EventLog         events.EventLog
	GitHubClient     github.Client
	CommentPoster    review.CommentPoster
	Renderer         prompt.IssueRenderer
	IssuePicker      IssuePicker
	IsTTY            func() bool
	GitRunner        gitRunner
	RunActivityProbe runActivityProbe
	TempCleaner      TempCleaner
	// Version returns the build/version string for `sandman --version` and
	// the `version` subcommand. Production wires the three-layer fallback
	// chain from cmd/sandman/main.go; tests inject a deterministic value.
	Version func() string
	// RepoRoot is the repository root the CLI commands operate on. When
	// empty, commands fall back to the current working directory, matching
	// the pre-Layout behaviour for callers that have not migrated.
	RepoRoot string
}

// NewRootCmd constructs the command tree with injected dependencies.
func NewRootCmd(deps Dependencies) *cobra.Command {
	root := &cobra.Command{
		Use:   "sandman",
		Short: "Sandman orchestrates AFK coding agents in isolated sandboxes",
		Long: `Sandman is a CLI tool for orchestrating AFK coding agents
in isolated sandboxes. It manages issue tracking, worktrees, containerized
execution, and event logging for automated coding workflows.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	if deps.Version != nil {
		root.Version = deps.Version()
		root.SetVersionTemplate("sandman {{.Version}}\n")
	}

	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if errors.Is(err, pflag.ErrHelp) {
			return err
		}
		return MarkUsage(err)
	})

	root.AddCommand(NewInitCmd())
	root.AddCommand(NewRunCmd(deps))
	root.AddCommand(NewStatusCmd(deps.EventLog))
	root.AddCommand(NewHistoryCmd(deps.EventLog))
	root.AddCommand(NewCleanCmd(deps))
	root.AddCommand(NewConfigCmd(deps.ConfigStore))
	root.AddCommand(NewAttachCmd())
	root.AddCommand(NewPortalCmd(deps))
	root.AddCommand(NewReviewCmd(deps))
	root.AddCommand(NewArchiveCmd(deps))
	root.AddCommand(NewStrandedCmd(deps))
	if deps.Version != nil {
		root.AddCommand(NewVersionCmd(deps.Version))
	}

	return root
}
