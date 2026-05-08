package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "sandman",
	Short: "Sandman orchestrates AFK coding agents in isolated sandboxes",
	Long: `Sandman is a terminal-native CLI tool for orchestrating AFK coding agents
in isolated sandboxes. It manages issue tracking, worktrees, containerized
execution, and event logging for automated coding workflows.`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(historyCmd)
	rootCmd.AddCommand(retryCmd)
	rootCmd.AddCommand(cleanCmd)
	rootCmd.AddCommand(configCmd)
}
