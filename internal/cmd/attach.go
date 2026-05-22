package cmd

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/spf13/cobra"
)

func NewAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach [run-id]",
		Short: "Attach to a running sandman daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			store := daemon.NewLiveRunStore(".sandman")
			runs, err := store.List()
			if err != nil {
				return fmt.Errorf("list live runs: %w", err)
			}

			var run *daemon.LiveRun
			switch len(args) {
			case 0:
				if len(runs) == 0 {
					return fmt.Errorf("no sandman daemon is running")
				}
				if len(runs) == 1 {
					run = &runs[0]
					break
				}
				runID, err := promptForLiveRun(cmd, runs)
				if err != nil {
					return err
				}
				for i := range runs {
					if runs[i].RunID == runID {
						run = &runs[i]
						break
					}
				}
				if run == nil {
					return fmt.Errorf("no live run %q", runID)
				}
			case 1:
				for i := range runs {
					if runs[i].RunID == args[0] {
						run = &runs[i]
						break
					}
				}
				if run == nil {
					return fmt.Errorf("no live run %q", args[0])
				}
			default:
				return fmt.Errorf("too many arguments")
			}

			conn, err := net.Dial("unix", store.SocketPath(run.RunID))
			if err != nil {
				return fmt.Errorf("connect to daemon %s: %w", run.RunID, err)
			}
			defer conn.Close()

			_, err = io.Copy(cmd.OutOrStdout(), conn)
			return err
		},
	}
}

func promptForLiveRun(cmd *cobra.Command, runs []daemon.LiveRun) (string, error) {
	writer := cmd.ErrOrStderr()
	fmt.Fprintln(writer, "Select live run:")
	for i, run := range runs {
		fmt.Fprintf(writer, "  [%d] %s  issues %s\n", i+1, run.RunID, formatIssueList(run.Issues))
	}
	fmt.Fprint(writer, "> ")

	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read selection: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("no live run selected")
	}
	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(runs) {
		return "", fmt.Errorf("invalid selection %q", line)
	}
	return runs[idx-1].RunID, nil
}
