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

Use `codeindex` CLI before grepping for symbols or assessing blast radius.

### Core commands

| Command | Purpose |
|---------|---------|
| `codeindex lookup <symbol>` | Find where a function/class is defined (file + line) |
| `codeindex impact <file>` | Blast-radius report: how many files break if this changes |
| `codeindex dependencies <file>` | Imports and imported-by for a file |
| `codeindex high-blast --threshold N` | List riskiest files (blast score ≥ N) |
| `codeindex search "natural language"` | Hybrid semantic + keyword + graph symbol search |

## Agent skills

- Issue tracker: `docs/agents/issue-tracker.md`
- Triage labels: `docs/agents/triage-labels.md`
- Domain vocabulary: `CONTEXT.md` (read for domain terms)
- ADRs: `docs/adr/` (architectural decisions)
