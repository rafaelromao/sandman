package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/rafaelromao/sandman/internal/config"
)

// NewConfigCmd creates the config command.
func NewConfigCmd(loader config.Loader) *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Manage Sandman configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := loader.Load()
			if err != nil {
				// For the placeholder, we ignore the error
			}
			fmt.Fprintln(cmd.OutOrStdout(), "config is not yet implemented")
			return nil
		},
	}
}
