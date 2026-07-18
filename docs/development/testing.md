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
SANDMAN_TEST_PROVIDERS=opencode \
  go test -tags smoke -timeout 30m ./internal/cmd -run Smoke
```

`SANDMAN_TEST_PROVIDERS` accepts a comma-separated list of provider names, `all`, or `*`. When unset, smoke tests skip themselves.

The `-timeout 30m` budget is required because each smoke sub-test pays a real
`podman build` of the per-provider / per-buildTools image plus a real
`opencode run` agent invocation; the cumulative wall time of the smoke suite
exceeds Go's 10-minute default timeout. For the full preset matrix (one
sub-test per buildTools variant — `generic`, `go`, `python`, `elixir`),
`-timeout 60m` is a safer budget.

### Smoke image prewarm

Smoke tests skip the expensive real-agent cases unless `SANDMAN_RUN_SMOKE_E2E=1` is set. When enabled, they build the container images they need on first use, then reuse those images during the same test process. To enable the upfront prewarm fan-out instead of on-demand builds, set:

```bash
SANDMAN_RUN_SMOKE_E2E=1 SANDMAN_SMOKE_PREFETCH=1 SANDMAN_TEST_PROVIDERS=opencode \
  go test -tags smoke -timeout 30m ./internal/cmd -run Smoke
```

## E2E tests

E2E tests exercise multi-session behavior such as continuing a previous run, batch orchestration, and subagent permission boundaries. They require the `e2e` build tag and are slower than smoke tests.

```bash
SANDMAN_TEST_PROVIDERS=opencode \
  go test -tags e2e -timeout 30m ./internal/cmd -run PRFlow
```

For the full `-run TestPresetMatrixHarness` suite (every scaffold preset —
`go`, `node`, `dotnet`, `elixir`, `rust`, `java`, `ruby`, `python`,
`generic`), use `-timeout 90m`: each preset pays a fresh `podman build`
of the scaffolded image. The script `scripts/run-preset-matrix.sh` applies
that budget automatically.

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
| `preset_matrix` | `internal/cmd` | preset-matrix scenario tests |

```bash
# Single scenario
SANDMAN_E2E_GATES=batch go test -timeout 30m -run TestRunBatch_EndToEnd ./internal/batch

# Multiple scenarios
SANDMAN_E2E_GATES=batch,continue_multi,opencode_subagent \
  go test -timeout 30m ./...

# All scenarios
SANDMAN_E2E_GATES=all go test -timeout 30m ./...
```

## Per-agent model override

By default, smoke and e2e tests use the model baked into each test case. To target a different model for a specific agent, set `SANDMAN_TEST_MODEL_<AGENT>` using the uppercased agent name.

```bash
SANDMAN_TEST_MODEL_OPENCODE=opencode/gpt-5-nano \
  SANDMAN_TEST_PROVIDERS=opencode \
  go test -tags smoke -timeout 30m ./internal/cmd -run Smoke
```

## Real-agent opt-in (`SANDMAN_RUN_AGENT_E2E`)

Real-agent E2E sub-tests, including PR-flow and preset-matrix scenarios, run
the **real** opencode agent inside a real container against a real LLM
provider. Those sub-tests are gated behind a runtime opt-in so the `-tags e2e`
suite stays runnable on developer machines and CI without live agent
credentials:

```bash
# Default: agent sub-tests skip cleanly with a clear message.
SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all \
  go test -tags e2e -timeout 30m ./internal/cmd

# Opt in: agent sub-tests actually execute.
SANDMAN_RUN_AGENT_E2E=1 SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all \
  go test -tags e2e -timeout 90m ./internal/cmd
```

`SANDMAN_RUN_AGENT_E2E=1` requires the host's opencode auth snapshot
(`~/.local/share/opencode/auth.json`) and a working `podman` or `docker`
runtime. With the opt-in set, the preset-matrix sub-tests are real-workflow
and need the wider `-timeout 90m` budget. Without it, the same tests skip with
a message naming the skipped provider and the missing opt-in, and the rest of
the suite runs as normal.

## Cleanup after interrupted tests

Smoke and e2e tests can create worktrees, containers, batch directories, temp directories, and shim state. If a run is interrupted before cleanup executes, remove residue with `sandman clean --all` (or pick a specific mode flag from the recipes below — bare `sandman clean` is a hard error).

```bash
# Preview what would be removed
sandman clean --dry-run --orphaned

# Remove orphaned test batch directories
sandman clean --orphaned

# Recover stale run state and clean active batch resources
sandman clean --stale

# Full cleanup
sandman clean --all
```

For stranded worktrees, see [`sandman stranded`](../usage/commands.md#sandman-stranded) or use [`sandman clean --all`](../usage/commands.md#sandman-clean) (or a specific mode flag).

## Deeper test infrastructure

For hermetic `gh` shims, fast-mode blocking shims, short Unix-socket paths, canonical test env vars, parallel test rules, and portal live-run invariants, see [Test Infrastructure](test-infrastructure.md).
