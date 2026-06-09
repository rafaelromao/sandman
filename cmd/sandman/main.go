package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/cmd"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/spf13/cobra"
)

func isStdoutTTY() bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(os.Stdout.Fd()), &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}

func executeRoot(rootCmd *cobra.Command, stderr io.Writer, exit func(int)) {
	if err := rootCmd.Execute(); err != nil {
		var coded *cmd.ExitCodedError
		if errors.As(err, &coded) {
			exit(coded.Code)
			return
		}
		var ue *cmd.UsageError
		if errors.As(err, &ue) {
			fmt.Fprintln(stderr, "Error:", err)
			fmt.Fprintln(stderr, rootCmd.UsageString())
			exit(1)
			return
		}
		fmt.Fprintln(stderr, err)
		exit(1)
	}
}

func main() {
	cfgStore := &config.FileStore{Path: ".sandman/config.yaml"}
	ghClient := &github.CLIClient{}
	renderer := &prompt.Engine{}
	eventLog := &events.JSONLLogger{Path: ".sandman/events.jsonl"}

	deps := cmd.Dependencies{
		BatchRunner:    batch.NewOrchestrator(ghClient, renderer, cfgStore, eventLog),
		ConfigStore:    cfgStore,
		EventLog:       eventLog,
		GitHubClient:   ghClient,
		PromptRenderer: renderer,
		IssuePicker:    &cmd.SimpleIssuePicker{},
		IsTTY:          isStdoutTTY,
	}

	rootCmd := cmd.NewRootCmd(deps)
	executeRoot(rootCmd, os.Stderr, os.Exit)
}
