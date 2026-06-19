# Workflows

## Running specific issues

```bash
sandman run 42 43
```

Runs agents for issues #42 and #43. Issues run concurrently up to the `--parallel` limit.

## Running issue ranges

```bash
sandman run 42:45
```

Selects issues #42 through #45 — both bounds inclusive. Sandman resolves the requested issue numbers locally and can combine them with label/query filters:

```bash
sandman run 42:45 --label bug
```

Combines the range with a label filter while keeping only the requested issues.

You can also combine label and query filters directly:

```bash
sandman run --label bug --query "author:me"
```

Also supports omitting the lower bound to start from 1:

```bash
sandman run :10
```

Or an upper bound for open-ended ranges:

```bash
sandman run 42:
```

## Running by label

```bash
sandman run --label ready-for-agent
```

Selects all open issues with the given label and runs agents for each. Combine with positional arguments for a narrower selection:

```bash
sandman run 42:45 --label bug
```

## Running by query

```bash
sandman run --query "label:bug is:open"
```

Selects issues matching a GitHub search query. Any valid GitHub issue search query works.

## Running the next N issues

```bash
sandman run --auto --count 3
```

Auto Mode: Sandman selects up to 3 issues from the candidate pool (default `ready-for-agent` label) and runs them. Use `--label X` or `--query "..."` to scope the candidate set, or pass explicit issue numbers as the candidate pool. `auto_max_count` in config controls the default cap (50); set to `0` for unlimited.

## Running without an issue

```bash
sandman run --base-branch main --prompt "Return only OK."
```

When `sandman run` uses `--prompt` or `--template` without any issue selectors and the final prompt omits `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, and `{{ISSUE_BODY}}`, Sandman runs in prompt-only mode: it skips GitHub issue lookup, records `issue: null` in events and history, syncs the selected base branch before the run starts, and names the branch `sandman/<slug>-<timestamp>`.

## Watching runs in the browser

```bash
sandman portal
```

`sandman portal` gives you a repo-scoped view of current and recent runs plus launcher presets. It rescans `.sandman/runs/` on each poll, so multiple Sandman instances in the same repository show up as they start. This is useful when you want to watch live output, review logs, compare runs, and launch new Sandman commands without jumping between terminals.

## Continuing a previous run

```bash
sandman run --continue 42 --prompt "finish the tests"
```

Reuses the previously created branch for issue #42 and feeds the agent the stored task file plus any new prompt text. Useful when the original prompt stalled or drifted.

## Cleaning up

```bash
sandman clean --success     # Remove worktrees and logs for successful runs
sandman clean --failed      # Remove worktrees and logs for failed runs
sandman clean --all         # Remove all worktrees and logs
```

Containers are stopped automatically when a batch completes. No dedicated container cleanup command is needed.

## BlockedBy-aware execution

Sandman can detect BlockedBy relationships between issues and execute them in the correct order.

### How BlockedBy detection works

Sandman discovers `BlockedBy` relationships from two sources:

1. **Body references** — issue bodies containing `blocked by #N` or `depends on #N`
2. **GitHub REST API** — `gh api` issue and events responses that surface native blocker numbers

The union of both sources forms each issue's `BlockedBy` set.

### Strict mode (default)

```bash
sandman run 42 43
```

If issue #43 is blocked by #42 but #42 is not in the batch, Sandman errors out:

```
Error: missing blockers: #42
```

### Auto-expand mode

```bash
sandman run --include-dependencies 100
```

Recursively includes all transitive blockers of issue #100. If #100 is blocked by #42, and #42 is blocked by #10, all three issues are included.

- Cycles are detected and produce an error with the cycle path

### Execution semantics

Once the dependency graph is resolved:

- **Blocked issues** only start when all blocker issues have succeeded
- **Independent issues** run concurrently (up to `--parallel`)
- **Failed blockers** cause dependent issues to be marked as `blocked` (skipped)
- **Topological ordering** ensures blockers complete before dependents start

### Container scheduling examples

```bash
# Each agent in its own container
sandman run --container-capacity 1 42 43

# Up to 6 concurrent runs across up to 3 containers
sandman run --container-capacity 2 --max-containers 3 42 43 44 45 46

# Auto-mode container scaling
sandman config set max_containers 0
sandman run --parallel 8 42 43 44 45 46 47
```

Set `container_capacity` to `0` when you want the default container capacity behavior without pinning a per-container limit in config.
