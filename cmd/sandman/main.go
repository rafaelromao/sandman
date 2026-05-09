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
	"github.com/rafaelromao/sandman/internal/sandbox"
)

func main() {
	// Composition root: wire real adapters
	deps := cmd.Dependencies{
		BatchRunner:    &batch.Orchestrator{},
		ConfigLoader:   &config.FileLoader{Path: ".sandman/config.yaml"},
		EventLog:       &events.JSONLLogger{Path: ".sandman/events.jsonl"},
		SandboxManager: &sandbox.WorktreeSandbox{},
		GitHubClient:   &github.CLIClient{},
		PromptRenderer: &prompt.Engine{},
	}

	rootCmd := cmd.NewRootCmd(deps)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
