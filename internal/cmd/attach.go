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
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
			repoRoot, err := findRepoRoot(cwd)
			if err != nil {
				return err
			}
			sandmanDir := filepath.Join(repoRoot, ".sandman")
			sockPath, err := findDaemonSocket(sandmanDir)
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
// under baseDir. It looks for batch sockets (.sandman/batches/<id>/batch.sock)
// and the review daemon socket (.sandman/review.sock). If exactly one is
// live, it is returned. Multiple live sockets is a hard error because it
// is ambiguous which daemon the operator wants to attach to.
func findDaemonSocket(baseDir string) (string, error) {
	candidates := []string{}

	reviewSock := ReviewSocketPath(baseDir)
	if _, err := os.Stat(reviewSock); err == nil {
		candidates = append(candidates, reviewSock)
	}

	batchesDir := filepath.Join(baseDir, "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read batches dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sockPath := filepath.Join(batchesDir, entry.Name(), "batch.sock")
		if _, err := os.Stat(sockPath); err == nil {
			candidates = append(candidates, sockPath)
		}
	}

	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no sandman daemon is running")
	case 1:
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple sandman daemons are running; specify a batch directory under .sandman/batches/")
	}
}
