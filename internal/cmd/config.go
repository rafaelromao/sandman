package cmd

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/spf13/cobra"
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
func NewConfigSetCmd(path string) *cobra.Command {
	return &cobra.Command{
		Use:                "set <key> <value>",
		Short:              "Set a config value",
		Args:               cobra.ExactArgs(2),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}

			if err := cfg.SetValue(args[0], args[1]); err != nil {
				return err
			}

			return config.Save(path, cfg)
		},
	}
}
