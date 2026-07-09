# Testing

This page is for contributors modifying Sandman itself. For using Sandman, see [Get Started](../get-started/README.md) and [Using Sandman](../usage/README.md).

Sandman's default test command is the normal contributor gate. Smoke and end-to-end tests are opt-in because they create real sandbox resources and may need a configured agent runtime.

## Default check

Run the standard local gate before submitting a change:

```bash
make check
```

That runs:

```bash
gofmt -w .
go vet ./...
go test -race -v ./...
```

For a faster targeted loop while editing one package, run the smallest relevant `go test` command first, then finish with `make check` when the change is ready.

## Smoke tests

Smoke tests run a single agent session end-to-end and verify the core run loop. They are fast compared with full e2e tests and are disabled unless a provider allowlist is set.

```bash
SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke
```

`SANDMAN_TEST_PROVIDERS` accepts a comma-separated list of provider names, `all`, or `*`. When unset, smoke tests skip themselves.

### Smoke image prewarm

Smoke tests prebuild the container images they need on first use, then reuse those images during the same test process. To force each smoke test to build its own image, disable prewarm:

```bash
SANDMAN_SMOKE_PREFETCH=0 SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke
```

## E2E tests

E2E tests exercise multi-session behavior such as continuing a previous run, batch orchestration, and subagent permission boundaries. They require the `e2e` build tag and are slower than smoke tests.

```bash
SANDMAN_TEST_PROVIDERS=opencode go test -tags e2e ./internal/cmd -run PRFlow
```

## Gated scenarios

Some expensive scenarios run without a build tag and are selected with `SANDMAN_E2E_GATES`. The value can be a single scenario, a comma-separated list, `all`, or `*`.

### Scenario reference

| Scenario | Package | Test selector |
|----------|---------|---------------|
| `batch` | `internal/batch` | `TestRunBatch_EndToEnd` |
| `continue_multi` | `internal/cmd` | `TestContinueFlow_PodmanSandboxBinarySupportsMultipleIssues` |
| `opencode_subagent` | `internal/cmd` | `TestOpencodeSubagentPermissionAllowAll` |
| `badge` | `internal/cmd` | badge scenario tests |
| `pathlen` | `internal/cmd` | path-length scenario tests |
| `batch_id_rules` | `internal/cmd` | `TestSlice10` |

```bash
# Single scenario
SANDMAN_E2E_GATES=batch go test -run TestRunBatch_EndToEnd ./internal/batch

# Multiple scenarios
SANDMAN_E2E_GATES=batch,continue_multi,opencode_subagent go test ./...

# All scenarios
SANDMAN_E2E_GATES=all go test ./...
```

## Per-agent model override

By default, smoke and e2e tests use the model baked into each test case. To target a different model for a specific agent, set `SANDMAN_TEST_MODEL_<AGENT>` using the uppercased agent name.

```bash
SANDMAN_TEST_MODEL_OPENCODE=opencode/gpt-5-nano \
  SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke
```

## Cleanup after interrupted tests

Smoke and e2e tests can create worktrees, containers, batch directories, temp directories, and shim state. If a run is interrupted before cleanup executes, remove residue with `sandman clean`.

```bash
# Preview what would be removed
sandman clean --dry-run --orphaned

# Remove orphaned test batch directories
sandman clean --orphaned

# Recover stale run state and clean active batch resources
sandman clean --stale

# Full cleanup
sandman clean
```

For stranded worktrees, see [`sandman stranded`](../usage/commands.md#sandman-stranded) or use [`sandman clean`](../usage/commands.md#sandman-clean).

## Deeper test infrastructure

For hermetic `gh` shims, fast-mode blocking shims, short Unix-socket paths, canonical test env vars, parallel test rules, and portal live-run invariants, see [Test Infrastructure](test-infrastructure.md).
