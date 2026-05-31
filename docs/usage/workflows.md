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

Expands `42:45` into issues #42, #43, #44, #45 — both bounds inclusive. Combine ranges with individual numbers:

```bash
sandman run 10 42:45 50
```

Also supports omitting the lower bound to start from 1:

```bash
sandman run :10
```

Expands to issues #1 through #10.

Ranges are capped at 100 issues to prevent accidental massive batches.

## Running by label

```bash
sandman run --label ready-for-agent
```

Selects all open issues with the given label and runs agents for each.

## Running by query

```bash
sandman run --query "label:bug is:open"
```

Selects issues matching a GitHub search query. Any valid GitHub issue search query works.

## Running the next N issues

```bash
sandman run --ralph 3
```

Selects the 3 lowest-numbered open issues labeled `ready-for-agent`. This is useful for CI/CD pipelines or automated triage workflows.

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

## Continuing a failed run

```bash
sandman continue 42 "finish the tests"
```

Reuses the previously created branch for issue #42 and feeds the agent a new prompt plus prior continuation context. Useful when the original prompt stalled or drifted.

## Cleaning up

```bash
sandman clean --success     # Remove worktrees and logs for successful runs
sandman clean --failed      # Remove worktrees and logs for failed runs
sandman clean --all         # Remove all worktrees and logs
```

Containers are stopped automatically when a batch completes. No dedicated container cleanup command is needed.

## Dependency-aware execution

Sandman can detect dependencies between issues and execute them in the correct order.

### How dependency detection works

Sandman discovers `BlockedBy` relationships from two sources:

1. **Body references** — issue bodies containing `blocked by #N` or `depends on #N`
2. **GitHub REST API** — native `issue.blocked_by` and `issue.issue_dependencies.blocked_by` fields

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
