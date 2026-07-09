# Test Infrastructure

This page is for contributors modifying Sandman itself. For using Sandman, see [Get Started](../get-started/README.md) and [Using Sandman](../usage/README.md).

This is the advanced reference for Sandman's test harness. For the common commands and scenario gates, start with [Testing](testing.md).

## Canonical test env vars

| Var | Purpose |
|-----|---------|
| `SANDMAN_TEST_PROVIDERS` | Provider allowlist for smoke and e2e tests |
| `SANDMAN_E2E_GATES` | Stable e2e scenario identifiers |
| `SANDMAN_TEST_MODEL_<AGENT>` | Per-agent model override for smoke and e2e tests |
| `SANDMAN_TEST_FAST` | Enables fast-mode behavior for blocking shims |

When gate vars are unset, expensive tests should skip themselves instead of doing real work.

## Fast-mode blocking shims

Some e2e tests use blocking agent shims that simulate long-running agent behavior. When `SANDMAN_TEST_FAST=1` is set, those shims should wait on a wakeup file instead of sleeping for the full duration.

Use this helper inside shell shims that need fast mode:

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
```

Then use `fast_test_wait 600` instead of `sleep 600`.

The test owns `WAKEUP_DIR`, sets `SANDMAN_TEST_FAST=1`, and creates `$WAKEUP_DIR/wakeup` when the shim should return. For container-based shims, mount the wakeup directory into the container at the same path and pass `WAKEUP_DIR` into the container environment.

## Hermetic `gh` shims

E2E tests use a fake `gh` command to avoid calling the real GitHub API. The shim should respond to the subcommands the code path under test needs, with JSON shaped like the real command output.

Common subcommands:

| Subcommand | Purpose | Required response fields |
|------------|---------|-------------------------|
| `gh repo view` | Resolve repo owner/name | `owner.login`, `name` |
| `gh api repos/{owner}/{repo}/issues/{n}` | Fetch work item data | `number`, `title`, `body`, `labels`, `blocked_by` |
| `gh api repos/{owner}/{repo}/issues/{n}/events` | Fetch dependency events | Array of event objects, often `[]` |
| `gh pr list` | List change requests | `number`, `state`, `mergedAt`, `headRefName`, `headRefOid` |
| `gh pr create` | Create a change request | Writes args/body to test-owned state, outputs a URL |
| `gh pr checks` | Check status | Text output used to simulate passing checks |
| `gh pr comment` | Post a review command | Text output |
| `gh auth token` | Fake auth token | Token string |
| `gh auth status` | Auth status | Text output showing logged-in state |

When dependency relationships matter, include the `blocked_by` JSON field. If there are no blockers, omit the field instead of returning an empty array unless the test specifically needs that shape.

For container-based tests, write the shim into the path visible inside the container and keep the same command contract.

## Short Unix-socket paths

When a test binds a Unix domain socket, use `testenv.MkdirShort(t, dirHint)` instead of `t.TempDir()`.

`t.TempDir()` can create paths that exceed Unix socket path limits on macOS. `MkdirShort` creates a short directory under `/tmp/` and removes it with `t.Cleanup`.

Use a readable `dirHint` prefix so `/tmp` dumps are easy to inspect:

| Area | Prefix |
|------|--------|
| Review tests | `sm-review-` |
| Daemon/socket tests | `sm-daemon-` |
| Batch/orchestrator tests | `sm-orch-` |
| Portal tests | `sm-portal-` |
| Abort/stop tests | `sm-abort-` or `sm-stop-` |
| Auto-mode tests | `sm-auto-` |

```go
func TestSomething(t *testing.T) {
    dir := testenv.MkdirShort(t, "sm-review-")
    sock := NewControlSocket(dir, NewBroadcaster())
    if err := sock.Start(); err != nil {
        t.Fatalf("Start: %v", err)
    }
    defer sock.Stop()
}
```

## Parallel test safety

Add `t.Parallel()` only when the test is isolated from process-global state and shared resources.

Safe candidates:

- Pure function tests
- Tests using per-test temp directories
- Tests using fake clients or injected stubs
- Tests with distinct sandbox layouts

Avoid `t.Parallel()` when the test uses:

- `t.Setenv()` or `t.Chdir()`
- Package-level mutable state
- Hard-coded paths outside a temp directory
- Shared network ports
- Pre-warmed shared resources
- Container runtime availability gates

Before adding `t.Parallel()`, check whether the test mutates globals, changes environment, writes outside its own temp directory, or depends on a shared external capability.

## Portal live-run atomicity invariant

Portal tests that exercise concurrent or parent-child run scenarios rely on a consistent snapshot of event-log state and active run discovery.

Two behaviors depend on that invariant:

1. When a parent run is terminal and a live review child exists, the parent keeps its terminal status while the child surfaces as a review row.
2. When two runs are live concurrently, the portal snapshot includes both runs with distinct identities.

Tests in this area should verify the rendered summary behavior, not just the presence of individual event records.
