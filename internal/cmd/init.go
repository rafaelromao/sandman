package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/rafaelromao/sandman/internal/config"
)

// NewInitCmd creates the init command.
func NewInitCmd(loader config.Loader) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize a new Sandman project in the current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := loader.Load()
			if err != nil {
				// For the placeholder, we ignore the error and print the message
			}
			fmt.Fprintln(cmd.OutOrStdout(), "init is not yet implemented")
			return nil
		},
	}
}
