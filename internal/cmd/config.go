package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/rafaelromao/sandman/internal/config"
)

// NewConfigCmd creates the config command with get/set subcommands.
func NewConfigCmd(loader config.Loader, path string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage Sandman configuration",
	}
	cmd.AddCommand(NewConfigGetCmd(loader))
	cmd.AddCommand(NewConfigSetCmd(path))
	return cmd
}

// NewConfigGetCmd creates the config get subcommand.
func NewConfigGetCmd(loader config.Loader) *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Get a config value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loader.Load()
			if err != nil {
				return err
			}

			key := args[0]
			value, err := getConfigValue(cfg, key)
			if err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), value)
			return nil
		},
	}
}

// NewConfigSetCmd creates the config set subcommand.
func NewConfigSetCmd(path string) *cobra.Command {
	return &cobra.Command{
		Use:                   "set <key> <value>",
		Short:                 "Set a config value",
		Args:                  cobra.ExactArgs(2),
		DisableFlagParsing:    true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}

			key := args[0]
			value := args[1]
			if err := setConfigValue(cfg, key, value); err != nil {
				return err
			}

			return config.Save(path, cfg)
		},
	}
}

func getConfigValue(cfg *config.Config, key string) (string, error) {
	switch strings.ToLower(key) {
	case "agent":
		return cfg.Agent, nil
	case "default_parallel":
		return fmt.Sprintf("%d", cfg.DefaultParallel), nil
	case "worktree_dir":
		return cfg.WorktreeDir, nil
	case "pr_template":
		return cfg.PRTemplate, nil
	case "sandbox":
		return cfg.Sandbox, nil
	case "git.author_name":
		return cfg.Git.AuthorName, nil
	case "git.author_email":
		return cfg.Git.AuthorEmail, nil
	default:
		return "", fmt.Errorf("unknown config key: %s", key)
	}
}

func setConfigValue(cfg *config.Config, key, value string) error {
	switch strings.ToLower(key) {
	case "agent":
		cfg.Agent = value
	case "default_parallel":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid value for default_parallel: %w", err)
		}
		if n <= 0 {
			return fmt.Errorf("default_parallel must be greater than 0")
		}
		cfg.DefaultParallel = n
	case "worktree_dir":
		cfg.WorktreeDir = value
	case "pr_template":
		cfg.PRTemplate = value
	case "sandbox":
		cfg.Sandbox = value
	case "git.author_name":
		cfg.Git.AuthorName = value
	case "git.author_email":
		cfg.Git.AuthorEmail = value
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}
