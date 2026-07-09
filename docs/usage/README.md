# Sandman Guides

How to install, configure, run, and operate Sandman end to end.

## First time here

- [Getting Started](getting-started.md) — prerequisites, install, init, first run
- [Concepts](concepts.md) — the Batch / AgentRun / Sandbox / Prompt-only model in prose
- [Workflows](workflows.md) — by issue, label, query, range, auto, prompt-only

## Run a batch

- [Commands Reference](commands.md) — every CLI flag and subcommand
- [Configuration](configuration.md) — full config schema and `config get` / `config set`
- [Sandbox Modes](sandbox-modes.md) — worktree vs podman vs docker

## Watch and debug

- [Portal](portal.md) — repo-scoped browser dashboard and its HTTP API
- [Monitoring and Debugging](monitoring.md) — `status`, `history`, `events.jsonl`, idle timeout
- [Troubleshooting](troubleshooting.md) — common failure modes and first thing to try
- [Smoke and E2E Tests](testing.md) — opt-in integration tiers

## Customize

- [Default Task Prompt](default-task-prompt.md) — bootstrap template and the human-prose companion to `internal/prompt/default-task-prompt.md`
- [Sandman Skills](skills.md) — the installed workflow skills under `~/.agents/skills/sandman/`
- [Agent Compatibility](agent-compatibility.md) — the `opencode` preset and the `opencode-shell-strategy` plugin

## Operations

- [Badge](badge.md) — the "Built with Sandman" badge sidecar
- [FAQ](faq.md) — questions people ask before installing
