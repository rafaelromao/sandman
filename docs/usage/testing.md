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
