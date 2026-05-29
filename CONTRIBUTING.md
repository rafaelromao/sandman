# Contributing to Sandman

Thank you for your interest in contributing! Sandman has two first-class contribution surfaces: **Go code** and **agent-facing documentation** (prompts, domain vocabulary, agent instructions). Both are equally welcome.

## Code of Conduct

This project and everyone participating in it is governed by the [Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code.

## Table of Contents

- [How Can I Contribute?](#how-can-i-contribute)
- [Code Contributions](#code-contributions)
- [Non-Code Contributions](#non-code-contributions)
- [Getting Help](#getting-help)

## How Can I Contribute?

### Reporting Bugs

Before creating a bug report, please check the [existing issues](https://github.com/rafaelromao/sandman/issues) to see if the problem has already been reported. If it has and the issue is still open, add a comment to the existing issue instead of opening a new one.

When you are creating a bug report, please use the [Bug Report template](https://github.com/rafaelromao/sandman/issues/new?template=bug_report.md) and include as many details as possible.

### Suggesting Features

Feature requests are tracked as [GitHub issues](https://github.com/rafaelromao/sandman/issues). Please use the [Feature Request template](https://github.com/rafaelromao/sandman/issues/new?template=feature_request.md).

For suggestions related to agent behavior, prompts, or domain vocabulary, use the [Agent Improvement template](https://github.com/rafaelromao/sandman/issues/new?template=agent_improvement.md) instead.

### Pull Requests

- Fill in the [required template](.github/PULL_REQUEST_TEMPLATE.md).
- Ensure the PR description clearly describes the problem and solution.
- Include the relevant issue number if applicable.
- Ensure all CI checks pass.

## Code Contributions

### Prerequisites

- Go 1.24 or later
- `gh` CLI installed and authenticated
- Git

### Setting Up Your Development Environment

1. Fork the repository on GitHub.
2. Clone your fork locally:
   ```bash
   git clone https://github.com/<your-username>/sandman.git
   cd sandman
   ```
3. Install dependencies:
   ```bash
   go mod download
   ```

### Building and Testing

Use the Makefile for common tasks:

```bash
# Format code, run vet, and run tests
make check

# Build the binary
make build

# Install to $GOPATH/bin
make install

# Format only
make fmt
```

Before submitting a PR, always run `make check` locally.

### Coding Standards

- Follow standard Go conventions (formatting is enforced by `gofmt`).
- Run `go vet ./...` and resolve all warnings.
- Write tests for new functionality.
- Keep the domain vocabulary in `CONTEXT.md` in mind when naming things.

### Commit Messages

- Use the present tense ("Add feature" not "Added feature").
- Use the imperative mood ("Move cursor to..." not "Moves cursor to...").
- Reference issues and pull requests where appropriate.

## Non-Code Contributions

Sandman's agent-facing documentation (`CLAUDE.md`, `CONTEXT.md`, `docs/agents/`, prompts, and ADRs) is a first-class contribution surface. Improvements to these files help both human contributors and automated agents work more effectively with the codebase.

### Agent Docs and Prompts

- Agent docs are explicitly contributor-welcome.
- All agent-doc changes require maintainer review.
- Include a clear rationale for any change that affects agent behavior.
- If you're modifying prompt templates, explain how you've verified the change produces better agent output.

### Domain Vocabulary (`CONTEXT.md`)

- Use the glossary terms precisely; avoid the listed synonyms.
- If you need a term that isn't in the glossary, that's a signal — either reconsider or propose adding it.
- Changes to domain vocabulary can have wide-reaching effects; these especially need maintainer review.

### Architectural Decision Records (ADRs)

- New ADRs should follow the MADR-style template in `docs/adr/0000-use-adr-template.md`.
- ADRs are sequential; use the next available number.
- ADR status starts as `proposed` and moves to `accepted` or `rejected` after discussion.

## Project Structure

Sandman follows a ports-and-adapters (hexagonal) architecture. The core domain lives in `internal/`:

```
cmd/sandman/main.go          # Composition root — wires interfaces to concrete adapters
internal/
  cmd/                       # Cobra CLI commands (run, init, status, history, continue, clean, config)
  batch/                     # Core domain: Orchestrator, AgentRun, DependencyResolver
  sandbox/                   # Sandbox interface + WorktreeSandbox and ContainerSandbox adapters
  config/                    # Config model, file store, built-in agent presets
  github/                    # GitHub client interface + gh CLI implementation
  prompt/                    # Prompt template engine and renderers
  events/                    # Event log interface + JSONL implementation
  scaffold/                  # sandman init scaffolding logic
```

### Key interfaces

| Interface | Package | Purpose |
|-----------|---------|---------|
| `Runner` | `batch` | Coordinates parallel execution of `AgentRun`s |
| `Runnable` | `batch` | Single agent execution lifecycle |
| `Sandbox` | `sandbox` | Execution isolation (worktree or container) |
| `ContainerStarter` | `sandbox` | Starts Docker/Podman containers |
| `Store` | `config` | Loads and saves `.sandman/config.yaml` |
| `Client` | `github` | Fetches issues and dependencies from GitHub |
| `Renderer` | `prompt` | Renders agent prompt templates |
| `EventLog` | `events` | Append-only structured event log |

### Data flow

1. `sandman run` parses CLI flags, selects issues (by number, label, query, `--ralph`, or interactive picker)
2. `DependencyResolver` fetches each issue's `BlockedBy` relationships, validates the graph (cycle detection, missing blockers), and produces a topologically sorted `ResolvedBatch`
3. `Orchestrator` executes the `ResolvedBatch` — creating sandboxes, running agents, respecting `BlockedBy` ordering and `Parallel`/`ContainerCapacity`/`MaxContainers` limits
4. Each `AgentRun` renders a prompt, executes the agent command inside its sandbox, and logs structured events
5. Results are written to `.sandman/events.jsonl` for status/history queries

## Getting Help

If you have questions about contributing, feel free to open a [discussion](https://github.com/rafaelromao/sandman/discussions) or ask in an existing issue.
