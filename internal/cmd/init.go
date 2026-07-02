package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/scaffold"
	"github.com/spf13/cobra"
)

type cliPrompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (c *cliPrompter) Confirm(msg string) (bool, error) {
	fmt.Fprintf(c.out, "%s [y/N]: ", msg)
	line, err := c.in.ReadString('\n')
	if err != nil {
		return false, err
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes", nil
}

func (c *cliPrompter) Select(msg string, options []string) (string, error) {
	fmt.Fprintln(c.out, msg)
	for i, opt := range options {
		fmt.Fprintf(c.out, "  %d) %s\n", i+1, opt)
	}
	fmt.Fprint(c.out, "Enter number: ")
	line, err := c.in.ReadString('\n')
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(options) {
		return "", fmt.Errorf("invalid selection")
	}
	return options[n-1], nil
}

// NewInitCmd creates the init command.
func NewInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new Sandman project in the current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			buildTools, _ := cmd.Flags().GetString("build-tools")
			toolVersion, _ := cmd.Flags().GetString("tool-version")
			agent, _ := cmd.Flags().GetString("agent")
			model, _ := cmd.Flags().GetString("model")
			parallel, _ := cmd.Flags().GetInt("parallel")
			parallelReviews, _ := cmd.Flags().GetInt("parallel-reviews")
			reviewCommand, _ := cmd.Flags().GetString("review-command")

			retriesOverride, err := resolveInitInt(cmd, "retries")
			if err != nil {
				return err
			}
			runIdleTimeoutOverride, err := resolveInitInt(cmd, "run-idle-timeout")
			if err != nil {
				return err
			}

			s := &scaffold.Scaffolder{}
			prompter := &cliPrompter{
				in:  bufio.NewReader(cmd.InOrStdin()),
				out: cmd.OutOrStdout(),
			}

			wd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			if err := s.Scaffold(wd, scaffold.Options{
				BuildTools:      buildTools,
				ToolVersion:     toolVersion,
				Agent:           agent,
				Model:           model,
				Parallel:        parallel,
				ParallelReviews: parallelReviews,
				ReviewCommand:   reviewCommand,
				Retries:         retriesOverride,
				RunIdleTimeout:  runIdleTimeoutOverride,
			}, prompter); err != nil {
				return err
			}
			syncOpts, err := sandmanSkillSyncOptions(cmd, reviewCommand)
			if err != nil {
				return fmt.Errorf("resolve skill sync options: %w", err)
			}
			if syncOpts.ReviewCommand == "" {
				syncOpts.ReviewCommand = config.DefaultReviewCommand
			}
			if err := syncSandmanSkill(syncOpts); err != nil {
				return fmt.Errorf("install sandman skill: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().String("build-tools", "", fmt.Sprintf("Build tools preset (%s)", strings.Join(scaffold.KnownBuildToolsPresets, ", ")))
	cmd.Flags().String("tool-version", "", "Logical version selector (repo, latest, lts, or semver shorthand)")
	cmd.Flags().String("agent", "", "Default built-in agent preset (opencode)")
	cmd.Flags().String("model", "", "Default model for the agent")
	cmd.Flags().Int("parallel", -1, fmt.Sprintf("Default parallel container count (-1 = use config default %d)", config.DefaultParallel))
	cmd.Flags().Int("parallel-reviews", -1, fmt.Sprintf("Default parallel reviews count (-1 = use built-in default of %d)", config.DefaultReviewParallel))
	cmd.Flags().String("review-command", "", "Review command to store in config and install into shared skills")
	cmd.Flags().Int("retries", -1, fmt.Sprintf("Persist `retries` in scaffolded config (-1 = use built-in default of %d)", config.DefaultRetries))
	cmd.Flags().Int("run-idle-timeout", -1, fmt.Sprintf("Persist `run_idle_timeout` in scaffolded config (-1 = use built-in default of %d)", config.DefaultRunIdleTimeout))

	return cmd
}

// resolveInitInt reads an `init` int flag and returns nil when unset or set to
// the sentinel `-1`, a pointer to the user-supplied value when `>= 0`, or an
// error when the flag value is `< -1` (invalid).
func resolveInitInt(cmd *cobra.Command, name string) (*int, error) {
	flag := cmd.Flags().Lookup(name)
	if flag == nil || !flag.Changed {
		return nil, nil
	}
	value, _ := cmd.Flags().GetInt(name)
	if value == -1 {
		return nil, nil
	}
	if value < 0 {
		return nil, fmt.Errorf("%s must be 0 or greater", strings.ReplaceAll(name, "-", "_"))
	}
	return &value, nil
}
