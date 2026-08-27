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

For OpenCode context rollover changes, the hermetic production-path coverage is:

```bash
go test -run 'ContextRollover|ContextRecoveryTask' ./internal/batch ./internal/prompt ./internal/events ./internal/cmd
```

For OpenCode session reuse, the focused production-path coverage is:

```bash
go test -run 'OpenCode|ContinueFlag' ./internal/batch ./internal/cmd
```

These tests cover structured output capture, atomic per-Run identity metadata,
fresh versus explicit continuation, exact-session selection, and the narrow
one-time fallback boundary.

## CI coverage

Sandman publishes four GoReleaser targets that match the four Unix platforms OpenCode supports (sans Windows): Linux amd64, Linux arm64, macOS amd64 (Intel), and macOS arm64 (Apple Silicon). The CI and release-validation tiers exercise those platforms as follows:

| Platform | Build + unit (race) | Podman-backed regression | Native Compatibility boundary |
|----------|---------------------|--------------------------|-------------------------------|
| Linux amd64 | `CI / build (ubuntu-latest)` (PR + push to `main`) | `Full Regression - Linux` (release-please branch + dispatch) | n/a |
| Linux arm64 | (no PR-time matrix entry today; covered by the released archive) | n/a | `Native Compatibility / (linux/arm64)` on `ubuntu-24.04-arm` (release-please branch + dispatch) |
| macOS amd64 (Intel) | n/a (covered transitively by `CI / build (macos-latest)` arm64 today) | n/a — release-please does not run Podman on macOS after #2459 | `Native Compatibility / (darwin/amd64)` on `macos-15-intel` (release-please branch + dispatch) |
| macOS arm64 (Apple Silicon) | `CI / build (macos-latest)` (PR + push to `main`) | n/a | `Native Compatibility / (darwin/arm64)` on `macos-14` (release-please branch + dispatch) |

The ordinary `CI` workflow runs the default untagged suite on the `ubuntu-latest` and `macos-latest` runners. It explicitly leaves `SANDMAN_TEST_PROVIDERS`, `SANDMAN_E2E_GATES`, `SANDMAN_RUN_SMOKE_E2E`, and `SANDMAN_RUN_AGENT_E2E` disabled, so pull requests do not run opt-in smoke or E2E scenarios.

Release Please branch updates add two release-validation workflows:

- `Full Regression - Linux` is the exhaustive authority for Linux. It runs the race-enabled unit suite, every smoke provider and build-tools preset, and every E2E gate including real-agent coverage with the canonical 60- and 90-minute budgets.
- `Native Compatibility` is a focused suite with three parallel arch-specific jobs — `Native Compatibility (darwin/amd64)` on `macos-15-intel`, `Native Compatibility (darwin/arm64)` on `macos-14`, and `Native Compatibility (linux/arm64)` on `ubuntu-24.04-arm`. Each job builds Sandman natively for its arch, then exercises Darwin/Linux socket paths plus portal and attach streaming, and verifies the native portal and attach streaming boundary through two hermetic portal E2E cases. Linux-container preset portability remains covered by the Linux workflow rather than being rebuilt inside the macOS Podman VM.

The contract test `TestPlatformCoverageMapMatchesWorkflowsAndGoReleaser` enforces that the CI matrix, the `Native Compatibility` workflow shape, the `Full Regression - Linux` exhaustive command, and the four `.goreleaser.yml` build IDs all stay aligned with the matrix above. A drift in any one of those four sources fails the contract.

### Full regression (`SANDMAN_FULL_REGRESSION`)

Tests that are tagged for a container runtime or a real provider typically skip
themselves when the generic `CI` env var is set (`os.Getenv("CI") != ""`), so
the ordinary PR-time `CI` workflow stays fast and never spends API quota on
real-agent runs. The `Full Regression - Linux` workflow is different: its whole
point is that nothing is skipped. It sets `SANDMAN_FULL_REGRESSION=1`, and every
CI-only skip guard is written as
`if os.Getenv("CI") != "" && !testenv.FullRegression() { t.Skip(...) }` so the
exhaustive job actually executes the real-agent, portal, preset-matrix, visual,
and heartbeat-timeout tests that ordinary CI only compiles.

The full-regression workflow installs the `opencode` CLI (`curl -fsSL
https://opencode.ai/install | bash`) and provisions `~/.local/share/opencode/auth.json`
from the `OPENCODE_API_KEY` secret. With auth present, the real-agent tests run
for real and a free-model quota exhaustion surfaces as a failed job instead of
a green-but-empty one. If the secret is absent, the auth-gated tests skip
cleanly with their normal "missing auth" message rather than failing.

`go.yml` must never set `SANDMAN_FULL_REGRESSION`; the contract test
`TestCIWorkflowKeepsOptInSuitesDisabled` enforces that so the CI-only skips
remain active on pull requests.

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
sub-test per buildTools variant — `generic`, `go`, `python`, `elixir`,
`node`, `dotnet`, `rust`, `java`, `ruby`), `-timeout 60m` is a safer
budget.

### Container build failure smoke test

`TestSmoke_ContainerBuildFailure` verifies that when the scaffolded container image cannot be built (e.g., an invalid Dockerfile instruction), the run fails with a clear build-error message and no stranded containers or worktrees are left behind. It is gated on `SANDMAN_RUN_SMOKE_E2E=1` because it needs a real container runtime.

```bash
SANDMAN_RUN_SMOKE_E2E=1 SANDMAN_TEST_PROVIDERS=opencode \
  go test -tags smoke -timeout 5m ./internal/cmd -run TestSmoke_ContainerBuildFailure
```

### Smoke image prewarm

Smoke tests skip the expensive real-agent cases unless `SANDMAN_RUN_SMOKE_E2E=1` is set. When enabled, they build the container images they need on first use, then reuse those images during the same test process. To enable the upfront prewarm fan-out instead of on-demand builds, set:

```bash
SANDMAN_RUN_SMOKE_E2E=1 SANDMAN_SMOKE_PREFETCH=1 SANDMAN_TEST_PROVIDERS=opencode \
  go test -tags smoke -timeout 30m ./internal/cmd -run Smoke
```

## E2E tests

E2E tests exercise multi-session behavior such as continuing a previous run, batch orchestration, and subagent permission boundaries. They require the `e2e` build tag and are slower than smoke tests.

Implementation pull-request lifecycle changes must keep the production-path
regression slice green with `go test ./internal/batch ./internal/cmd`. This
slice covers merged completion precedence, recoverable awaits, continuation
re-evaluation, retained review evidence, and portal projection.

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
| `batch_id_rules` | `internal/cmd` | `TestBatchIDRules_*` |
| `preset_matrix` | `internal/cmd` | preset-matrix scenario tests |
| `base_branch_feature` | `internal/batch` | `TestRunBatch_BaseBranchFeature_CutsWorktreeFromFeatureBranch` |
| `review_daemon` | `internal/cmd` | `TestReviewDaemonE2E_RealAgentInContainer` |
| `lifecycle_commands` | `internal/cmd` | `TestLifecycle_*` |
| `review_wait` | `internal/cmd` | `TestReviewWaitStabilization_*` |

The `review_wait` scenario is in the tagged E2E tier and requires
`go test -tags e2e`.

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
# Real-agent sub-tests execute as part of the E2E suite.
SANDMAN_RUN_AGENT_E2E=1 SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all \
  go test -tags e2e -timeout 90m ./...
```

`SANDMAN_RUN_AGENT_E2E=1` requires the host's opencode auth snapshot
(`~/.local/share/opencode/auth.json`) and a working `podman` or `docker`
runtime. With the opt-in set, the preset-matrix sub-tests are real-workflow
and need the wider `-timeout 90m` budget. Without it, the same tests skip with
a message naming the skipped provider and the missing opt-in, and the rest of
the suite runs as normal.

In addition to the `SANDMAN_RUN_AGENT_E2E` opt-in, the real-agent tests also
self-skip when the generic `CI` env var is set — unless `SANDMAN_FULL_REGRESSION=1`
is present (see [Full regression](#full-regression-sandman_full_regression)).
The `Full Regression - Linux` workflow sets both, so its real-agent coverage
genuinely exercises the free `opencode/big-pickle` model.

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
