package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/review"
	"github.com/spf13/cobra"
)

// reviewDaemonRunner is the function used to build and run the review
// daemon. Tests override it to avoid actually polling GitHub.
var reviewDaemonRunner = runReviewDaemon

// NewReviewCmd creates the `sandman review` command. When PR numbers
// are provided as positional args the command runs in one-shot mode
// (post a single review comment for each PR and exit). When no args
// are provided, the command starts the review daemon: it polls open
// PRs every 60s for `/sandman review` comments and launches review
// agents serially. The daemon writes log lines to .sandman/review.sock
// (exposed via `sandman attach`) and shuts down cleanly on SIGINT/SIGTERM.
func NewReviewCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review [pr-numbers...]",
		Short: "Run a Sandman agent to review a pull request",
		Long: "Run a Sandman agent to review a pull request. With PR numbers as positional " +
			"args, posts a single review comment for each and exits. Without args, starts " +
			"the review daemon that polls open PRs every 60s for /sandman review comments " +
			"and launches review agents.",
		Example: `  sandman review 42
  sandman review 42 43
  sandman review 42:45
  sandman review 42:
  sandman review :45
  sandman review 42 --agent opencode --model opencode/big-pickle`,
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

			if parallelFlag < 0 {
				return MarkUsage(fmt.Errorf("parallel must be 0 or greater"))
			}
			if ccSet && ccFlag < 0 {
				return MarkUsage(fmt.Errorf("container_capacity must be 0 or greater"))
			}
			if mcSet && mcFlag < 0 {
				return MarkUsage(fmt.Errorf("max_containers must be 0 or greater"))
			}

			if len(args) > 0 {
				return runReviewOneShotMulti(cmd, deps, cfg, args, parallelFlag)
			}
			if parallelFlag > 0 {
				cfg.DefaultReviewParallel = parallelFlag
			}
			return reviewDaemonRunner(cmd.Context(), deps, cfg, sandboxFlag, ccFlag, ccSet, mcFlag, mcSet)
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

// runReviewOneShot handles the one-shot review for a single PR number.
// Kept as a separate function so the daemon and multi-PR branches can be
// tested independently.
func runReviewOneShot(cmd *cobra.Command, deps Dependencies, cfg *config.Config, prNumber int, parallelFlag int) error {
	pr, err := deps.GitHubClient.FetchPR(prNumber)
	if err != nil {
		return fmt.Errorf("fetch PR #%d: %w", prNumber, err)
	}

	agentFlag, _ := cmd.Flags().GetString("agent")
	modelFlag, _ := cmd.Flags().GetString("model")
	sandboxFlag, _ := cmd.Flags().GetString("sandbox")
	ccFlag, _ := cmd.Flags().GetInt("container-capacity")
	mcFlag, _ := cmd.Flags().GetInt("max-containers")

	reviewAgentName := strings.TrimSpace(agentFlag)
	if reviewAgentName == "" {
		reviewAgentName = cfg.EffectiveReviewAgent()
	}
	if reviewAgentName == "" {
		return fmt.Errorf("review agent is not set; configure review_agent or agent in sandman config")
	}
	if _, err := cfg.ResolveAgentProvider(reviewAgentName); err != nil {
		return err
	}

	reviewModel := strings.TrimSpace(modelFlag)
	if reviewModel == "" {
		reviewModel = cfg.EffectiveReviewModel()
	}
	if reviewModel == "" {
		return fmt.Errorf("review model is not set; configure review_model or model in sandman config")
	}

	reviewParallel := parallelFlag
	if reviewParallel <= 0 {
		reviewParallel = cfg.EffectiveReviewParallel()
	}

	repoName, err := deps.GitHubClient.RepoName()
	if err != nil {
		return fmt.Errorf("get repo name: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "repo=%s agent=%s model=%s\n", repoName, reviewAgentName, reviewModel)

	rendered, err := deps.Renderer.RenderReview(prompt.RenderConfig{}, prompt.PRData{
		Number: pr.Number,
		Title:  pr.Title,
		Body:   pr.Body,
	})
	if err != nil {
		return fmt.Errorf("render review prompt: %w", err)
	}

	sandboxMode := strings.TrimSpace(sandboxFlag)
	if sandboxMode == "" {
		sandboxMode = cfg.Sandbox
	}

	repoRoot := deps.RepoRoot
	if repoRoot == "" {
		var err error
		repoRoot, err = resolveRepoRoot()
		if err != nil {
			return fmt.Errorf("resolve repo root: %w", err)
		}
	}
	sandmanDir := filepath.Join(repoRoot, ".sandman")

	rs := daemon.NewRunSession(sandmanDir, fmt.Sprintf("PR%d", pr.Number))
	manifest := daemon.BatchManifest{BatchId: fmt.Sprintf("PR%d", pr.Number), CreatedAt: time.Now(), RunKind: "review", PR: &pr.Number}
	if err := rs.Prepare(manifest, nil); err != nil {
		_ = rs.Close()
		return fmt.Errorf("bootstrap review session: %w", err)
	}
	defer rs.Close()

	relRunDir, err := filepath.Rel(repoRoot, rs.RunDir())
	if err != nil {
		return fmt.Errorf("rel run dir: %w", err)
	}

	if _, err := deps.BatchRunner.RunBatch(cmd.Context(), batch.Request{
		Agent:                reviewAgentName,
		Model:                reviewModel,
		Mode:                 map[int]batch.IssueMode{0: batch.ModeOverride},
		Sandbox:              sandboxMode,
		Parallel:             reviewParallel,
		ContainerCapacity:    ccFlag,
		ContainerCapacitySet: cmd.Flags().Changed("container-capacity"),
		MaxContainers:        mcFlag,
		MaxContainersSet:     cmd.Flags().Changed("max-containers"),
		PromptConfig: prompt.RenderConfig{
			PromptFlag: rendered,
			Branch:     fmt.Sprintf("sandman/review-%d-%d", pr.Number, time.Now().UnixNano()),
		},
		Review:       true,
		PRNumber:     pr.Number,
		RunID:        fmt.Sprintf("PR%d", pr.Number),
		OutputWriter: rs.Broadcaster(),
		RunDir:       relRunDir,
	}); err != nil {
		return fmt.Errorf("run review batch: %w", err)
	}
	return nil
}

// runReviewOneShotMulti parses positional args (bare numbers, N:M, N:,
// :M ranges) and runs a one-shot review for each resolved PR. For unbounded
// ranges (N: or :M) it fetches open PRs via ListOpenPRs and filters by PR number.
func runReviewOneShotMulti(cmd *cobra.Command, deps Dependencies, cfg *config.Config, args []string, parallelFlag int) error {
	prSet := make(map[int]struct{})
	hasUnbounded := false
	sel := issueSelection{exact: make(map[int]struct{})}

	for _, arg := range args {
		start, end, isRange, err := parseIssueRange(arg)
		if err != nil {
			return fmt.Errorf("invalid PR number %q: %w", arg, err)
		}
		if isRange {
			sel.ranges = append(sel.ranges, issueRangeSelection{start: start, end: end})
			if end == 0 || strings.HasPrefix(arg, ":") {
				hasUnbounded = true
				continue
			}
			if end-start >= 1000 {
				return fmt.Errorf("range %q expands to more than 1000 pull requests", arg)
			}
			for n := start; n <= end; n++ {
				prSet[n] = struct{}{}
			}
		} else {
			sel.exact[start] = struct{}{}
			prSet[start] = struct{}{}
		}
	}

	if hasUnbounded {
		openPRs, err := deps.GitHubClient.ListOpenPRs()
		if err != nil {
			return fmt.Errorf("list open PRs: %w", err)
		}
		for _, pr := range openPRs {
			if sel.matches(pr.Number) {
				prSet[pr.Number] = struct{}{}
			}
		}
	}

	prNumbers := make([]int, 0, len(prSet))
	for n := range prSet {
		prNumbers = append(prNumbers, n)
	}
	sort.Ints(prNumbers)

	for _, prNumber := range prNumbers {
		if err := runReviewOneShot(cmd, deps, cfg, prNumber, parallelFlag); err != nil {
			return err
		}
	}
	return nil
}

// runReviewDaemon wires and runs the review daemon. The cmd layer owns
// the SIGINT/SIGTERM signal handling; the daemon handles the polling
// loop and the in-flight batch cancellation.
func runReviewDaemon(parent context.Context, deps Dependencies, cfg *config.Config, sandbox string, cc int, ccSet bool, mc int, mcSet bool) error {
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
	sandmanDir := filepath.Join(repoRoot, ".sandman")
	socketDir := filepath.Join(sandmanDir, "reviews")
	broadcaster := daemon.NewBroadcaster()
	ctlSocket := daemon.NewControlSocketWithName(socketDir, "review.sock", broadcaster)
	d := review.New(sandmanDir, deps.GitHubClient, deps.Renderer, deps.BatchRunner, cfg, broadcaster)
	d.Sandbox = sandbox
	d.ContainerCapacity = cc
	d.ContainerCapacitySet = ccSet
	d.MaxContainers = mc
	d.MaxContainersSet = mcSet
	d.SetSocket(ctlSocket)
	return d.Run(ctx)
}
