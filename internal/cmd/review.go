package cmd

import (
	"context"
	"fmt"
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
// parallel_reviews at a time. The daemon writes log lines to .sandman/review.sock
// (exposed via `sandman attach`) and shuts down cleanly on SIGINT/SIGTERM.
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
			return reviewDaemonRunner(cmd.Context(), deps, cfg, sandboxFlag, ccFlag, ccSet, mcFlag, mcSet, agentFlag, modelFlag, parallelFlag, parallelSet)
		},
	}

	cmd.Flags().String("agent", "", "Override default_review_agent for this run")
	cmd.Flags().String("model", "", "Override default_review_model for this run")
	cmd.Flags().String("sandbox", "", "Sandbox mode: podman (default), docker, or worktree")
	cmd.Flags().Int("parallel", 0, "Override parallel_reviews for this run; 0 uses the configured value")
	cmd.Flags().Int("container-capacity", 0, "Maximum concurrent agent runs per container; 0 means unlimited")
	cmd.Flags().Int("max-containers", 0, "Maximum number of containers to run at once; 0 means no cap (unbounded pool)")

	return cmd
}

// runReviewDaemon wires and runs the review daemon. The cmd layer owns
// the SIGINT/SIGTERM signal handling; the daemon handles the polling
// loop and the in-flight batch cancellation.
func runReviewDaemon(parent context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool, agentFlag string, modelFlag string, parallel int, parallelSet bool) error {
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
	layout := paths.NewLayout(cfg, repoRoot)
	sandmanDir := layout.SandmanDir
	socketDir := layout.ReviewsDir()
	broadcaster := daemon.NewBroadcaster()
	ctlSocket := daemon.NewControlSocketWithName(socketDir, "review.sock", broadcaster)
	poster := deps.CommentPoster
	if poster == nil {
		poster = ghCommentPosterFromDeps(deps)
	}
	d := review.New(sandmanDir, deps.GitHubClient, deps.Renderer, deps.BatchRunner, cfg, broadcaster, parallel, parallelSet, poster)
	d.Layout = layout
	d.Sandbox = sandbox
	d.ContainerCapacity = cc
	d.ContainerCapacitySet = ccSet
	d.MaxContainers = mc
	d.MaxContainersSet = mcSet
	d.Agent = agentFlag
	d.Model = modelFlag
	d.SetSocket(ctlSocket)
	return d.Run(ctx)
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
