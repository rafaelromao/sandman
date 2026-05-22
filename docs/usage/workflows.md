# Workflows

## Running specific issues

```bash
sandman run 42 43
```

Runs agents for issues #42 and #43. Issues run concurrently up to the `--parallel` limit.

Each `sandman run` invocation gets its own live run directory under `.sandman/runs/<run-id>/`, so multiple live daemons can coexist in one repo.

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
sandman run --next 3
```

Selects the 3 lowest-numbered open issues labeled `ready-for-agent`. This is useful for CI/CD pipelines or automated triage workflows.

## Interactive mode

```bash
sandman run --interactive 42
```

Runs the agent in interactive (TTY-attached) mode for a single issue. The agent receives full terminal capabilities rather than a non-interactive command invocation.

- Requires exactly one issue
- Mutually exclusive with `--include-dependencies`

## Retrying a failed run

```bash
sandman retry 42
```

Reruns the last agent for issue #42, reusing the previously created branch. If the prior run recorded prompt inputs, retry replays the same prompt/template source, prompt args, and review command. Useful after transient failures.

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

Sandman discovers `BlockedBy` relationships from three sources:

1. **Body references** — issue bodies containing `blocked by #N` or `depends on #N`
2. **GitHub REST API** — native `issue.blocked_by` and `issue.issue_dependencies.blocked_by` fields
3. **Issue events** — `blocked_by_added`, `blocked_by_removed`, and cross-reference events

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

- Mutually exclusive with `--interactive`
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
