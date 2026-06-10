package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
		Use:   "review [pr-number...]",
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

			if len(args) > 0 {
				return runReviewOneShotMulti(cmd, deps, cfg, args)
			}
			return reviewDaemonRunner(cmd.Context(), deps, cfg)
		},
	}

	cmd.Flags().String("agent", "", "Override default_review_agent for this run")
	cmd.Flags().String("model", "", "Override default_review_model for this run")
	cmd.Flags().String("sandbox", "", "Sandbox mode for the review run (default: worktree)")

	return cmd
}

// runReviewOneShot handles the one-shot review for a single PR number.
// Kept as a separate function so the daemon and multi-PR branches can be
// tested independently.
func runReviewOneShot(cmd *cobra.Command, deps Dependencies, cfg *config.Config, prNumber int) error {
	pr, err := deps.GitHubClient.FetchPR(prNumber)
	if err != nil {
		return fmt.Errorf("fetch PR #%d: %w", prNumber, err)
	}

	agentFlag, _ := cmd.Flags().GetString("agent")
	modelFlag, _ := cmd.Flags().GetString("model")
	sandboxFlag, _ := cmd.Flags().GetString("sandbox")

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

	repoName, err := deps.GitHubClient.RepoName()
	if err != nil {
		return fmt.Errorf("get repo name: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "repo=%s agent=%s model=%s\n", repoName, reviewAgentName, reviewModel)

	rendered, err := deps.PromptRenderer.RenderReview(prompt.RenderConfig{}, prompt.PRData{
		Number: pr.Number,
		Title:  pr.Title,
		Body:   pr.Body,
	})
	if err != nil {
		return fmt.Errorf("render review prompt: %w", err)
	}

	sandboxMode := strings.TrimSpace(sandboxFlag)
	if sandboxMode == "" {
		sandboxMode = "worktree"
	}

	if _, err := deps.BatchRunner.RunBatch(cmd.Context(), batch.Request{
		Agent:   reviewAgentName,
		Model:   reviewModel,
		Sandbox: sandboxMode,
		PromptConfig: prompt.RenderConfig{
			PromptFlag: rendered,
			Branch:     fmt.Sprintf("sandman/review-%d-%d", pr.Number, time.Now().UnixNano()),
		},
		Review:   true,
		PRNumber: pr.Number,
		RunID:    fmt.Sprintf("PR%d", pr.Number),
		RunDir:   daemon.RunDir(".sandman", nil, fmt.Sprintf("PR%d", pr.Number)),
	}); err != nil {
		return fmt.Errorf("run review batch: %w", err)
	}
	return nil
}

// rangeConstraint captures an unbounded range: end==0 means N: (all PRs >= N),
// start=1,end=M with the original arg having empty start means :M (all PRs <= M).
type rangeConstraint struct {
	start, end int
}

// runReviewOneShotMulti parses positional args (bare numbers, N:M, N:,
// :M ranges) and runs a one-shot review for each resolved PR. For unbounded
// ranges (N: or :M) it fetches open PRs via ListOpenPRs and filters by PR number.
func runReviewOneShotMulti(cmd *cobra.Command, deps Dependencies, cfg *config.Config, args []string) error {
	prSet := make(map[int]struct{})
	var constraints []rangeConstraint

	for _, arg := range args {
		start, end, isRange, err := parseIssueRange(arg)
		if err != nil {
			return fmt.Errorf("invalid argument %q: %w", arg, err)
		}
		if !isRange {
			prSet[start] = struct{}{}
			continue
		}
		if end == 0 {
			constraints = append(constraints, rangeConstraint{start: start, end: 0})
			continue
		}
		isEmptyStart := strings.HasPrefix(arg, ":")
		if isEmptyStart {
			constraints = append(constraints, rangeConstraint{start: 1, end: end})
			continue
		}
		for n := start; n <= end; n++ {
			prSet[n] = struct{}{}
		}
	}

	if len(constraints) > 0 {
		openPRs, err := deps.GitHubClient.ListOpenPRs()
		if err != nil {
			return fmt.Errorf("list open PRs: %w", err)
		}
		for _, pr := range openPRs {
			if _, ok := prSet[pr.Number]; ok {
				continue
			}
			for _, c := range constraints {
				if c.end == 0 {
					if pr.Number >= c.start {
						prSet[pr.Number] = struct{}{}
						break
					}
				} else if pr.Number <= c.end {
					prSet[pr.Number] = struct{}{}
					break
				}
			}
		}
	}

	prNumbers := make([]int, 0, len(prSet))
	for n := range prSet {
		prNumbers = append(prNumbers, n)
	}
	sort.Ints(prNumbers)

	for _, prNumber := range prNumbers {
		if err := runReviewOneShot(cmd, deps, cfg, prNumber); err != nil {
			return err
		}
	}
	return nil
}

// runReviewDaemon wires and runs the review daemon. The cmd layer owns
// the SIGINT/SIGTERM signal handling; the daemon handles the polling
// loop and the in-flight batch cancellation.
func runReviewDaemon(parent context.Context, deps Dependencies, cfg *config.Config) error {
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

	socketDir := ".sandman"
	broadcaster := daemon.NewBroadcaster()
	ctlSocket := daemon.NewControlSocketWithName(socketDir, "review.sock", broadcaster)
	d := review.New(socketDir, deps.GitHubClient, deps.PromptRenderer, deps.BatchRunner, cfg, broadcaster)
	d.SetSocket(ctlSocket)
	return d.Run(ctx)
}
