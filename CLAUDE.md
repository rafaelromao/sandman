# Sandman

CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Architecture

- **Event-sourced state**: Run status is a projection over the append-only `.sandman/events.jsonl`, not mutable records. `events.RunState` folds events into current state. Adding a new status or event type means updating the projection.
- **Factory seams**: `cmd.Dependencies` (`internal/cmd/root.go`) is the top-level DI struct. `batch.Request` accepts `RunnableFactory` and `SandboxFactory` interfaces. Tests inject fakes at these boundaries — mock there, not at concrete types.
- **Filesystem as data store**: No database. State is flat files under `.sandman/` (manifests, logs, review state), written atomically via temp-file + `os.Rename`. IPC is Unix domain sockets.

## Before committing

```bash
gofmt -w . && go vet ./...
```

## Symbol lookup

Use `codeindex` CLI before grepping: `codeindex lookup <symbol>`, `codeindex impact <file>`, `codeindex dependencies <file>`.
Regenerate: `codeindex analyze . && codeindex symbols . --inline`

## Agent skills

- Issue tracker: `docs/agents/issue-tracker.md`
- Triage labels: `docs/agents/triage-labels.md`
- Domain vocabulary: `CONTEXT.md` (read for domain terms)
- ADRs: `docs/adr/` (architectural decisions)
