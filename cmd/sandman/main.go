package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/cmd"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func isStdoutTTY() bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(os.Stdout.Fd()), &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}

func main() {
	// Composition root: wire real adapters
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
	if err := rootCmd.Execute(); err != nil {
		var coded *cmd.ExitCodedError
		if errors.As(err, &coded) {
			os.Exit(coded.Code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
