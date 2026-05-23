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
			sockPath, err := findDaemonSocket(".sandman")
			if err != nil {
				return err
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

func findDaemonSocket(baseDir string) (string, error) {
	runsDir := filepath.Join(baseDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no sandman daemon is running")
		}
		return "", err
	}

	var sockets []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sockPath := filepath.Join(runsDir, entry.Name(), "run.sock")
		if _, err := os.Stat(sockPath); err == nil {
			sockets = append(sockets, sockPath)
		}
	}

	switch len(sockets) {
	case 0:
		return "", fmt.Errorf("no sandman daemon is running")
	case 1:
		return sockets[0], nil
	default:
		return "", fmt.Errorf("multiple sandman daemons are running; specify a run directory under .sandman/runs/")
	}
}
