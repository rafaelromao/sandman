package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewVersionCmd returns the `version` subcommand. It prints
// "sandman <version>\n" using the value returned by getVersion, so tests
// can inject a deterministic value without touching the package-level
// ldflags seam in cmd/sandman/main.go.
func NewVersionCmd(getVersion func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the sandman version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "sandman %s\n", getVersion())
			return nil
		},
	}
}
