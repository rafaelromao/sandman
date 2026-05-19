package cmd

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func NewAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach",
		Short: "Attach to a running sandman daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			sockPath := filepath.Join(".sandman", "run.sock")
			if _, err := os.Stat(sockPath); os.IsNotExist(err) {
				return fmt.Errorf("no sandman daemon is running.")
			}

			conn, err := net.Dial("unix", sockPath)
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer conn.Close()

			_, err = io.Copy(cmd.OutOrStdout(), conn)
			return err
		},
	}
}
