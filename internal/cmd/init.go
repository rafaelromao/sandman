package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/rafaelromao/sandman/internal/scaffold"
	"github.com/spf13/cobra"
)

type cliPrompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (c *cliPrompter) Confirm(msg string) (bool, error) {
	fmt.Fprintf(c.out, "%s [y/N]: ", msg)
	line, err := c.in.ReadString('\n')
	if err != nil {
		return false, err
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes", nil
}

func (c *cliPrompter) Select(msg string, options []string) (string, error) {
	fmt.Fprintln(c.out, msg)
	for i, opt := range options {
		fmt.Fprintf(c.out, "  %d) %s\n", i+1, opt)
	}
	fmt.Fprint(c.out, "Enter number: ")
	line, err := c.in.ReadString('\n')
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(options) {
		return "", fmt.Errorf("invalid selection")
	}
	return options[n-1], nil
}

// NewInitCmd creates the init command.
func NewInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new Sandman project in the current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			lang, _ := cmd.Flags().GetString("lang")
			fromImage, _ := cmd.Flags().GetString("from-image")
			agent, _ := cmd.Flags().GetString("agent")

			s := &scaffold.Scaffolder{}
			prompter := &cliPrompter{
				in:  bufio.NewReader(cmd.InOrStdin()),
				out: cmd.OutOrStdout(),
			}

			wd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			return s.Scaffold(wd, scaffold.Options{
				Lang:      lang,
				FromImage: fromImage,
				Agent:     agent,
			}, prompter)
		},
	}

	cmd.Flags().String("lang", "", "Override language detection")
	cmd.Flags().String("from-image", "", "Custom base Docker image")
	cmd.Flags().String("agent", "", "Built-in agent preset (opencode, claude-code, codex, pi)")

	return cmd
}
