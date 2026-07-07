package cmd

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/spf13/cobra"
)

func NewAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach",
		Short: "Attach to a running sandman daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot, err := resolveRepoRoot()
			if err != nil {
				return fmt.Errorf("resolve repo root: %w", err)
			}

			sockPath, err := findDaemonSocket(repoRoot)
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

func resolveRepoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return os.Getwd()
}

// findDaemonSocket returns the path of the only active sandman socket
// under baseDir (repo root). It looks for batch sockets
// (<baseDir>/.sandman/batches/<id>/batch.sock) and the review daemon
// socket (<baseDir>/.sandman/reviews/review.sock). If exactly one is
// live, it is returned. Multiple live sockets is a hard error because
// it is ambiguous which daemon the operator wants to attach to.
func findDaemonSocket(baseDir string) (string, error) {
	candidates := []string{}

	layout := paths.NewLayout(nil, baseDir)
	reviewSock := layout.ReviewSocketPath()
	if _, err := os.Stat(reviewSock); err == nil {
		candidates = append(candidates, reviewSock)
	}

	batchesDir := layout.BatchesDir
	entries, err := os.ReadDir(batchesDir)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read batches dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sockPath := layout.BatchSocketPath(entry.Name())
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
