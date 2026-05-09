# Sandman

A terminal-native CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Agent skills

### Issue tracker

Issues and PRDs live as GitHub issues in `rafaelromao/sandman`. Use the `gh` CLI for all operations. See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical triage roles using default label strings. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context repo. Read `CONTEXT.md` at the repo root and `docs/adr/` for architectural decisions. See `docs/agents/domain.md`.

## Pre-commit checklist

Before committing any Go code changes, always run `gofmt` to ensure the code is properly formatted. The CI pipeline enforces this check and will fail the build if any files need formatting.

```bash
gofmt -w .
```
