# Sandman

A terminal-native CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Agent skills

### Issue tracker

Issues and PRDs live as GitHub issues in `rafaelromao/sandman`. Use the `gh` CLI for all operations. See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical triage roles using default label strings. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context repo. Read `CONTEXT.md` at the repo root and `docs/adr/` for architectural decisions. See `docs/agents/domain.md`.

Never perform force push on git. Never push directly to main.
Never run grep, rg, find, or any recursive content/file search against directories outside the current working directory (e.g. /tmp, /var, /usr, /etc, /opt, /home, node_modules, .git, target, dist, build, vendor). Such searches return massive output that floods the context window. Restrict searches to the cwd or explicit sub-paths within it; use the Glob/Grep tools which already scope to the project by default.

Before committing any Go code changes, always run `gofmt` to ensure the code is properly formatted. The CI pipeline enforces this check and will fail the build if any files need formatting.

```bash
gofmt -w .
```

## Codeindex

Dependency index: `codeindex.json` — use `lookup_symbol`, `get_impact`, `get_dependencies` MCP tools before grepping.
Symbol index embedded in `codeindex.json` — use `lookup_symbol` MCP tool for O(1) symbol lookups.
Index stale or missing? Run `codeindex analyze . && codeindex symbols . --inline` to regenerate.
