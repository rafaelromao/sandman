package cmd

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/spf13/cobra"
)

// NewInitCmd creates the init command.
func NewInitCmd(store config.Store) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize a new Sandman project in the current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := store.Load()
			if err != nil {
				// For the placeholder, we ignore the error and print the message
			}
			fmt.Fprintln(cmd.OutOrStdout(), "init is not yet implemented")
			return nil
		},
	}
}
