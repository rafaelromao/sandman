package cmd

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/spf13/cobra"
)

func readTaskPrompt(cmd *cobra.Command, taskPath string) (string, error) {
	content, exists, err := batch.ReadTaskContent(taskPath)
	if err != nil {
		return "", err
	}
	if !exists {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: no task found at %s; using empty task template\n", taskPath)
	}
	return content, nil
}
