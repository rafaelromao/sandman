# Smoke and E2E Tests

Sandman's integration test suite has two tiers: smoke tests and end-to-end (e2e) tests. Both are opt-in and disabled by default in the normal test run.

## Smoke tests

Smoke tests run a single agent session end-to-end and verify the core run loop. They are fast (~30 s) and cover the happy path.

```bash
SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke
```

`SANDMAN_TEST_PROVIDERS` is an allowlist: comma-separated provider names, `all`, or `*`. When unset, smoke tests skip themselves.

### Smoke image prewarm

On first invocation of any smoke test, `TestMain` runs the prewarm phase: it scaffolds a throwaway repository for each of the four `(provider, buildTools)` variants (`opencode/generic`, `opencode/go`, `opencode/python`, `opencode/elixir`) and builds a container image for each one. The image tags are stored in a package-level map so subsequent smoke test invocations reuse the cached images instead of rebuilding on every test.

The prewarm builds all four variants **concurrently** using goroutines with a semaphore-capped fan-out. Wall-clock time for a fresh prewarm is therefore bounded by the slowest single variant rather than the sum of all four. A per-variant build failure is tolerated: the failing variant falls back to the per-test build path and does not block the others.

To disable the prewarm and force every smoke test to build its own image (useful when iterating on the Dockerfile or when you want each test to be hermetic):

```bash
SANDMAN_SMOKE_PREFETCH=0 SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke
```

## E2E tests

E2E tests exercise multi-session scenarios such as continuing from a previous run, batch orchestration, and subagent permission boundaries. They require the `-tags e2e` build tag and are slower than smoke tests.

```bash
SANDMAN_TEST_PROVIDERS=opencode go test -tags e2e ./internal/cmd -run PRFlow
```

## Gated scenarios

Gated scenarios (`SANDMAN_E2E_GATES`) run without a build tag and select individual scenarios by name. `SANDMAN_E2E_GATES` accepts a single scenario, a comma list, `all`, or `*`.

### Scenario reference

| Scenario | Package | Test selector |
|----------|---------|---------------|
| `batch` | `internal/batch` | `TestRunBatch_EndToEnd` |
| `continue_multi` | `internal/cmd` | `TestContinueFlow_PodmanSandboxBinarySupportsMultipleIssues` |
| `opencode_subagent` | `internal/cmd` | `TestOpencodeSubagentPermissionAllowAll` |
| `badge` | `internal/cmd` | badge scenario tests |
| `pathlen` | `internal/cmd` | path-length scenario tests |

```bash
# Single scenario
SANDMAN_E2E_GATES=batch go test -run TestRunBatch_EndToEnd ./internal/batch

# Multiple scenarios
SANDMAN_E2E_GATES=batch,continue_multi,opencode_subagent go test ./...

# All scenarios
SANDMAN_E2E_GATES=all go test ./...
```

The canonical list of supported scenario identifiers is in [`internal/testenv/testenv.go:39-47`](../../internal/testenv/testenv.go).

## Per-agent model override

By default, smoke and e2e tests use the model baked into each test case. To target a different model for a specific agent, set `SANDMAN_TEST_MODEL_<AGENT>` (uppercased, e.g. `SANDMAN_TEST_MODEL_OPENCODE`). The helper that resolves the override is `testenv.ResolveTestModel`.

```bash
SANDMAN_TEST_MODEL_OPENCODE=opencode/gpt-5-nano \
  SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke
```

## Fast-mode test harness

Some e2e tests use blocking agent shims that simulate long-running agent behavior (e.g., a fake `opencode` that sleeps for 600 seconds to mimic a real agent task). When `SANDMAN_TEST_FAST=1` is set, these shims enter fast mode: instead of sleeping, they poll a wakeup file (`$WAKEUP_DIR/wakeup`) and return immediately when it appears.

This shortens e2e test runs from minutes to seconds. When the env var is unset or empty, the shims use real sleeps and behave as in previous versions.

```bash
# Fast mode (default for go test when SANDMAN_TEST_PROVIDERS is set)
SANDMAN_TEST_PROVIDERS=opencode go test -tags e2e ./internal/cmd -run PRFlow

# Explicit fast mode
SANDMAN_TEST_FAST=1 SANDMAN_TEST_PROVIDERS=opencode go test -tags e2e ./internal/cmd -run PRFlow

# Slow mode (real sleeps)
SANDMAN_TEST_FAST= SANDMAN_TEST_PROVIDERS=opencode go test -tags e2e ./internal/cmd -run PRFlow
```

### Implementing a fast-mode-aware blocking shim

When writing a new blocking shim that needs fast mode, use the `fast_test_wait` helper inside the shell script:

```sh
fast_test_wait() {
    _duration="$1"
    _wakeup_dir="${WAKEUP_DIR:-}"
    if [ "${SANDMAN_TEST_FAST:-}" = "1" ] && [ -n "$_wakeup_dir" ] && [ -d "$_wakeup_dir" ]; then
        _deadline=$(($(date +%s) + _duration))
        while [ $(date +%s) -lt $_deadline ]; do
            if [ -f "$_wakeup_dir/wakeup" ]; then
                return 0
            fi
            sleep 0.1
        done
    fi
    sleep "$_duration"
}

# Then instead of: sleep 600
# Use:           fast_test_wait 600
```

The test that exercises the shim sets `SANDMAN_TEST_FAST=1` and `WAKEUP_DIR` to a temp directory it owns, then creates `$WAKEUP_DIR/wakeup` to signal the shim to wake.

For container-based shims, mount the wakeup directory into the container at the same path and pass `WAKEUP_DIR` as an environment variable to the container run command.

## Test infrastructure

Platform helpers for Unix-socket path length (`MkdirShort`) and capability gates are documented in [`docs/agents/testenv.md`](../agents/testenv.md). Do not duplicate that content here; link to it instead.

## Side effects and cleanup

E2E and smoke tests create real sandbox resources (worktrees, containers, batch directories) and use real temp directories under `/tmp/`. When a test is interrupted or fails before its cleanup runs, residue can accumulate on disk and in the `.sandman/` state directory.

### What accumulates after an interrupted run

- **Worktrees** under `.sandman/worktrees/` — each test that creates a worktree leaves it behind if the test is killed mid-flight
- **Orphaned batch directories** under `.sandman/batches/<ts>-<sid>-.../` — per-test batch dirs with no matching `run.started` event and no live daemon socket; these are not removed by normal batch cleanup
- **Temp directories** — tests use Sandman-prefixed temp paths (`sm-*`, `sandman-config-*`) under `/tmp/`; when a test process is killed before `t.Cleanup` fires, these directories are left behind
- **GH shim state and opencode session DB rows** — the hermetic `gh` shim directory and any opencode session state written during a test can persist after the test exits

### Environments most likely to hit failures

These side effects are most problematic in:

- **CI environments with disk quotas** — repeated e2e runs accumulate temp directories and orphaned batches that can exhaust the quota, causing subsequent runs to fail
- **Worktree-based sandboxes** — the worktree-per-run model means interrupted runs leave worktrees on disk that accumulate over time
- **Shared or constrained `/tmp`** — environments where `/tmp` is a tmpfs with a quota (common in containerised CI) are especially affected by `sm-*` temp dir accumulation
- **Repeated test runs without cleanup between runs** — residue compounds across runs, increasing the risk of hitting disk or state conflicts

### Smoke test auth layout copy policy

Smoke tests copy the opencode auth directory (`~/.config/opencode`, `~/.local/share/opencode`) into the test temp directory, but exclude the opencode SQLite database (`opencode.db`, `opencode.db-shm`, `opencode.db-wal`) to avoid exhausting disk quotas on constrained CI runners. This mirrors the snapshot-exclude behavior described in ADR-0016.

### Cleaning up

Run `sandman clean` after any interrupted or failed e2e run:

```bash
# Preview what would be removed
sandman clean --dry-run --orphaned

# Remove orphaned test batch directories (no live socket, no run.started event)
sandman clean --orphaned

# Remove active batches and their worktrees; also recover stale run state
# and sweep stale config snapshots (ADR-0015)
sandman clean --stale

# Full cleanup: active batches + orphaned test dirs + stale snapshots
# (--stale is mutually exclusive with --orphaned)
sandman clean
```

`--orphaned` targets test batch directories that have no live daemon socket and no corresponding `run.started` event — the exact residue a failed or interrupted e2e run leaves behind.

The `--stale` path sweeps stale config snapshots left after a crash (the `sandman-config-*` temp directories introduced by ADR-0008) and also emits `run.aborted` events for runs in dead batches to correct the event log. See ADR-0008 and ADR-0015 for the underlying cleanup mechanism.

For stranded worktrees (a worktree whose HEAD does not match its directory name), see [`sandman stranded`](commands.md#sandman-stranded) or use [`sandman clean`](commands.md#sandman-clean).
