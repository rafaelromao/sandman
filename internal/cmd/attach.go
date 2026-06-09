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

// findDaemonSocket returns the path of the only active sandman socket
// under baseDir. It looks for run sockets (.sandman/runs/<id>/run.sock)
// and the review daemon socket (.sandman/review.sock). If exactly one is
// live, it is returned. Multiple live sockets is a hard error because it
// is ambiguous which daemon the operator wants to attach to.
func findDaemonSocket(baseDir string) (string, error) {
	candidates := []string{}

	reviewSock := filepath.Join(baseDir, "review.sock")
	if _, err := os.Stat(reviewSock); err == nil {
		candidates = append(candidates, reviewSock)
	}

	runsDir := filepath.Join(baseDir, "runs")
	if entries, err := os.ReadDir(runsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			sockPath := filepath.Join(runsDir, entry.Name(), "run.sock")
			if _, err := os.Stat(sockPath); err == nil {
				candidates = append(candidates, sockPath)
			}
		}
	}

	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no sandman daemon is running")
	case 1:
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple sandman daemons are running; specify a run directory under .sandman/runs/")
	}
}
