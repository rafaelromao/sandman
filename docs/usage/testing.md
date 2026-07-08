# Smoke and E2E Tests

Sandman's integration test suite has two tiers: smoke tests and end-to-end (e2e) tests. Both are opt-in and disabled by default in the normal test run.

## Smoke tests

Smoke tests run a single agent session end-to-end and verify the core run loop. They are fast (~30 s) and cover the happy path.

```bash
SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke
```

`SANDMAN_TEST_PROVIDERS` is an allowlist: comma-separated provider names, `all`, or `*`. When unset, smoke tests skip themselves.

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
