package cmd

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/spf13/cobra"
)

// NewConfigCmd creates the config command with get/set subcommands.
func NewConfigCmd(store config.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage Sandman configuration",
	}
	cmd.AddCommand(NewConfigGetCmd(store))
	cmd.AddCommand(NewConfigSetCmd(store))
	return cmd
}

// NewConfigGetCmd creates the config get subcommand.
func NewConfigGetCmd(store config.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Get a config value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := store.Load()
			if err != nil {
				return err
			}

			value, err := cfg.GetValue(args[0])
			if err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), value)
			return nil
		},
	}
}

// NewConfigSetCmd creates the config set subcommand.
func NewConfigSetCmd(store config.Store) *cobra.Command {
	return &cobra.Command{
		Use:                "set <key> <value>",
		Short:              "Set a config value",
		Args:               cobra.ExactArgs(2),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := store.Load()
			if err != nil {
				return err
			}
			oldReviewCommand := cfg.EffectiveReviewCommand()

			if err := cfg.SetValue(args[0], args[1]); err != nil {
				return err
			}

			if args[0] == "review_command" {
				syncOpts, err := sandmanSkillSyncOptions(cmd, cfg.EffectiveReviewCommand())
				if err != nil {
					return fmt.Errorf("resolve skill sync options: %w", err)
				}
				if err := syncSandmanSkill(syncOpts); err != nil {
					return fmt.Errorf("sync sandman skill: %w", err)
				}
				if err := store.Save(cfg); err != nil {
					revertOpts, revertErr := sandmanSkillSyncOptions(cmd, oldReviewCommand)
					if revertErr == nil {
						_ = syncSandmanSkill(revertOpts)
					}
					return err
				}
				return nil
			}

			if err := store.Save(cfg); err != nil {
				return err
			}

			return nil
		},
	}
}
