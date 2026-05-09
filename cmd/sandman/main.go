package main

import (
	"fmt"
	"os"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/cmd"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

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
	}

	rootCmd := cmd.NewRootCmd(deps)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
