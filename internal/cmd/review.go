package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	ghcli "github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/review"
	"github.com/spf13/cobra"
)

// reviewDaemonRunner is the function used to build and run the review
// daemon. Tests override it to avoid actually polling GitHub.
var reviewDaemonRunner = runReviewDaemon

// NewReviewCmd creates the `sandman review` daemon command. It polls open PRs
// every 30s for `/sandman review` comments and launches review agents up to
// parallel_reviews at a time. The daemon writes its log lines to stderr and to
// .sandman/reviews/review.sock (exposed via `sandman attach`), and shuts down
// cleanly on SIGINT/SIGTERM.
func NewReviewCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Run the Sandman pull-request review daemon",
		Long:  "Run the review daemon that polls open PRs every 30s for /sandman review comments and launches review agents.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := deps.ConfigStore.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			sandboxFlag, _ := cmd.Flags().GetString("sandbox")
			parallelFlag, _ := cmd.Flags().GetInt("parallel")
			ccFlag, _ := cmd.Flags().GetInt("container-capacity")
			ccSet := cmd.Flags().Changed("container-capacity")
			mcFlag, _ := cmd.Flags().GetInt("max-containers")
			mcSet := cmd.Flags().Changed("max-containers")
			agentFlag, _ := cmd.Flags().GetString("agent")
			modelFlag, _ := cmd.Flags().GetString("model")
			variantFlag, _ := cmd.Flags().GetString("variant")
			variantFlag = strings.TrimSpace(variantFlag)

			if parallelFlag < 0 {
				return MarkUsage(fmt.Errorf("parallel must be 0 or greater"))
			}
			if ccSet && ccFlag < 0 {
				return MarkUsage(fmt.Errorf("container_capacity must be 0 or greater"))
			}
			if mcSet && mcFlag < 0 {
				return MarkUsage(fmt.Errorf("max_containers must be 0 or greater"))
			}

			reviewAgentName := strings.TrimSpace(agentFlag)
			if reviewAgentName == "" {
				reviewAgentName = cfg.EffectiveReviewAgent()
			}

			repoRoot := deps.RepoRoot
			if repoRoot == "" {
				resolved, err := resolveRepoRoot()
				if err != nil {
					return fmt.Errorf("resolve repo root: %w", err)
				}
				repoRoot = resolved
			}

			// Issue #2212: warn once at startup if the opencode version pinned
			// in the sandbox differs from the host. The daemon only warns at
			// process startup, not per tick, because the
			// existing `d.busy` serializes per-tick scans and the
			// warning is informational.
			warnOpencodeVersionMismatch(cmd, reviewAgentName, sandboxFlag, repoRoot, cfg)

			parallelSet := cmd.Flags().Changed("parallel")
			opts := reviewDaemonOptions{Sandbox: sandboxFlag, ContainerCapacity: ccFlag, ContainerCapacitySet: ccSet,
				MaxContainers: mcFlag, MaxContainersSet: mcSet, Agent: agentFlag, Model: modelFlag,
				Variant: variantFlag, VariantSet: cmd.Flags().Changed("variant"), Parallel: parallelFlag, ParallelSet: parallelSet}
			return reviewDaemonRunner(cmd.Context(), deps, cfg, opts, cmd.ErrOrStderr())
		},
	}

	cmd.Flags().String("agent", "", "Override default_review_agent for this run")
	cmd.Flags().String("model", "", "Override default_review_model for this run")
	cmd.Flags().String("variant", "", "Override review_variant for this run")
	cmd.Flags().String("sandbox", "", "Sandbox mode: podman (default), docker, or worktree")
	cmd.Flags().Int("parallel", 0, "Override parallel_reviews for this run; 0 uses the configured value")
	cmd.Flags().Int("container-capacity", 0, "Maximum concurrent agent runs per container; 0 means unlimited")
	cmd.Flags().Int("max-containers", 0, "Maximum number of containers to run at once; 0 means no cap (unbounded pool)")

	return cmd
}

// runReviewDaemon wires and runs the review daemon. The cmd layer owns
// the SIGINT/SIGTERM signal handling; the daemon handles the polling
// loop and the in-flight batch cancellation.
func runReviewDaemon(parent context.Context, deps Dependencies, cfg *config.Config, opts reviewDaemonOptions, stderr io.Writer) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// The daemon's socket and prompt template live under
	// .sandman/reviews/ so the daemon's on-disk footprint is just two
	// files at that location plus run folders under .sandman/batches/.
	// Issue #1224 acceptance criteria.
	repoRoot, err := resolveRepoRoot()
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	return newReviewDaemon(deps, cfg, paths.NewLayout(cfg, repoRoot), opts, stderr).Run(ctx)
}

// reviewDaemonOptions are the `sandman review` flag values the daemon uses.
type reviewDaemonOptions struct {
	Sandbox                                string
	ContainerCapacity, MaxContainers       int
	ContainerCapacitySet, MaxContainersSet bool
	Agent, Model, Variant                  string
	VariantSet                             bool
	Parallel                               int
	ParallelSet                            bool
}

// newReviewDaemon builds the review daemon without starting it: no socket is
// bound until Run. Daemon log lines go to stderr (the terminal running the
// command) and to the broadcaster that `sandman attach` clients read.
func newReviewDaemon(deps Dependencies, cfg *config.Config, layout paths.Layout, opts reviewDaemonOptions, stderr io.Writer) *review.Daemon {
	broadcaster := daemon.NewBroadcaster()
	ctlSocket := daemon.NewControlSocketWithName(layout.ReviewsDir(), "review.sock", broadcaster)
	poster := deps.CommentPoster
	if poster == nil {
		poster = ghCommentPosterFromDeps(deps)
	}
	d := review.New(layout.SandmanDir, deps.GitHubClient, deps.Renderer, deps.BatchRunner, cfg, broadcaster, opts.Parallel, opts.ParallelSet, poster)
	d.Console = stderr
	d.Layout = layout
	d.Sandbox = opts.Sandbox
	d.ContainerCapacity = opts.ContainerCapacity
	d.ContainerCapacitySet = opts.ContainerCapacitySet
	d.MaxContainers = opts.MaxContainers
	d.MaxContainersSet = opts.MaxContainersSet
	d.Agent = opts.Agent
	d.Model = opts.Model
	d.Variant = opts.Variant
	d.VariantSet = opts.VariantSet
	d.SetSocket(ctlSocket)
	return d
}

// ghCommentPosterFromDeps is a fallback that builds a
// GHCommentPoster from deps.GitHubClient when the deps wiring did
// not already provide a CommentPoster. The production path
// (cmd/sandman/main.go) pre-builds and assigns the poster, so this
// fallback only fires for tests / non-production wiring that pass a
// fake GitHubClient. Returning nil lets the daemon's nil-safe
// default take over; see review.New's nil-to-nop fallback.
func ghCommentPosterFromDeps(deps Dependencies) review.CommentPoster {
	if deps.GitHubClient == nil {
		return nil
	}
	cli, ok := deps.GitHubClient.(*ghcli.CLIClient)
	if !ok {
		return nil
	}
	return ghcli.NewGHCommentPoster(cli)
}
