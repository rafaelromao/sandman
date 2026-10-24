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
GOTOOLCHAIN=go1.25.0 go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...
go test -race -v ./...
```

Real OpenCode smoke and preset-matrix fixtures copy credentials into a test-owned
HOME, rather than importing the operator's live plugins, dependency installations,
or session database. Preset tests retain the real Podman data-store location and
allow Sandman's normal cancellation cleanup before deleting temporary artifacts.
This keeps a concurrent host agent or plugin update from changing their inputs.

The Staticcheck gate inherits the default checks and explicitly defers `U1000`
(unused code) to the v1.1 cleanup work. The analyzer is pinned to `v0.7.0`
and runs with Go `1.25.0` so newer local toolchains do not provide incompatible
export data.

For a faster targeted loop while editing one package, run the smallest relevant `go test` command first, then finish with `make check` when the change is ready.

For AgentRun lifecycle/artifact separation, the hermetic production-path coverage is:

```bash
go test ./internal/events ./internal/daemon ./internal/cmd -run 'AgentRunLifecycle|EventLifecycle|LifecycleIsNotInferred|EventAuthority|EventTerminality|RequiresEveryAgentRunTerminal|RecoverStaleRuns'
```

The CLI/Portal/archive/restart tracer bullet uses a real JSONL log and persisted
index with a deliberately stale `run.json.status`. Divergence tests preserve
event-terminal outcomes after artifact relocation/removal, abort dead active
work despite successful snapshots, preserve capacity-queued continuations, and
reject unknown lifecycle/read failures at archive and cleanup boundaries.
Events own lifecycle; index records and filesystem probes own artifact
location/availability. Snapshot identity/path validation still governs safe
artifact reclamation.

For request-scoped informal approval, the focused regression slice is:

```bash
go test -race ./internal/batch ./internal/prompt -run 'InformalApproval|TaskWithReviewEvidence'
```

It covers no-findings reviews with scope paths and metrics, ordinary and
COMMENTED approvals, request/head/window and real-feedback controls, pending-CI
observations without premature relaunches, and same-RunID approval continuation
through verified merge. Accepted informal approval is sufficient without a
formal GitHub approval; its continuation prompt must preserve that contract.
The captured reports in `internal/batch/testdata/approved-daemon-28-*.md` and
`approved-sandman-{2818,2886}.md` cover quality tags, no-author-change wording,
publication limitations and informational next steps. Classifier and lifecycle
tests assert approval precedes informal feedback; executor tests cover ordinary
`--continue` and capacity-ready re-entry through verified merge and issue closure.

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

For agent strategy or `claude` preset changes, the focused coverage is:

```bash
go test -run 'Strategy|AgentSelection|Claude|UsageLimit|RenderCommand' ./internal/batch
go test -run 'Claude|DefaultModel|BuiltInPresets|ReviewModel' ./internal/config ./internal/scaffold ./internal/review ./internal/skill ./internal/cmd
```

`TestAgentStrategyGolden_OpenCodeLaunches` pins the rendered OpenCode commands,
`session.json` writes, logs, and detector results captured before the agent
strategy seam existed; regenerate its golden with
`SANDMAN_AGENT_STRATEGY_UPDATE=1` only for an intended OpenCode behaviour change.
`TestAgentSelection_NoAgentNameComparisonsOutsideStrategy` fails when a new
`== "opencode"`-style agent-name comparison appears outside the strategy seam.
Session identity is agent-neutral in storage: each strategy validates its own
`session.json` protocol and provider, and the Portal reads any valid record, so
neither needs an agent-name comparison. The Claude launch tests in
`claude_strategy_test.go` pin fresh, exact-resume, invalid-identity, and
missing-session commands, in place of a Claude golden.
The hermetic `claude` preset path (a fake `claude` binary behind the real
worktree sandbox, usage-limit await, and exact `--resume` re-entry) runs with the
`batch` E2E gate:

```bash
SANDMAN_E2E_GATES=batch go test -tags e2e -run ClaudePresetWorktreeEndToEnd ./internal/batch
```

## CI coverage

Sandman publishes four GoReleaser targets that match the four Unix platforms OpenCode supports (sans Windows): Linux amd64, Linux arm64, macOS amd64 (Intel), and macOS arm64 (Apple Silicon). The CI and release-validation tiers exercise those platforms as follows:

| Platform | Build + unit (race) | Podman-backed regression | Native Compatibility boundary |
|----------|---------------------|--------------------------|-------------------------------|
| Linux amd64 | `CI / build (ubuntu-latest)` + `CI / portal-continuity-browser` (PR + push to `main`) | `Full Regression - Linux` (release-please branch + dispatch) | `Native Compatibility (linux/amd64)` on `ubuntu-latest` |
| Linux arm64 | (no PR-time matrix entry today; covered by the released archive) | n/a | `Native Compatibility / (linux/arm64)` on `ubuntu-24.04-arm` (release-please branch + dispatch) |
| macOS amd64 (Intel) | n/a (covered transitively by `CI / build (macos-latest)` arm64 today) | n/a — release-please does not run Podman on macOS after #2459 | `Native Compatibility / (darwin/amd64)` on `macos-15-intel` (release-please branch + dispatch) |
| macOS arm64 (Apple Silicon) | `CI / build (macos-latest)` (PR + push to `main`) | n/a | `Native Compatibility / (darwin/arm64)` on `macos-14` (release-please branch + dispatch) |

The ordinary `CI` workflow uses independent platform jobs to run the default untagged suite on the `ubuntu-latest` and `macos-latest` runners. It explicitly leaves `SANDMAN_TEST_PROVIDERS`, `SANDMAN_E2E_GATES`, `SANDMAN_RUN_SMOKE_E2E`, and `SANDMAN_RUN_AGENT_E2E` disabled, so pull requests do not run opt-in smoke or E2E scenarios.

The required `CI / portal-continuity-browser` job provisions Node and Chromium on Linux and runs the actual-page Portal log continuity browser scenarios. Its separate required status keeps the browser gate visible in branch protection on pull requests and pushes to `main`.

Release Please branch updates add two release-validation workflows:

- `Full Regression - Linux` is the exhaustive authority for Linux. It runs independent unit/browser, smoke container, credential-free integration, and live-provider jobs with the canonical 60- and 90-minute budgets.
- `Native Compatibility` is a focused suite with four parallel arch-specific jobs — `Native Compatibility (linux/amd64)` on `ubuntu-latest`, `Native Compatibility (darwin/amd64)` on `macos-15-intel`, `Native Compatibility (darwin/arm64)` on `macos-14`, and `Native Compatibility (linux/arm64)` on `ubuntu-24.04-arm`. Each job builds Sandman natively for its arch, then exercises Darwin/Linux socket paths plus portal and attach streaming, and verifies the native portal and attach streaming boundary through two hermetic portal E2E cases. Linux-container preset portability remains covered by the Linux workflow rather than being rebuilt inside the macOS Podman VM.

The contract test `TestPlatformCoverageMapMatchesWorkflowsAndGoReleaser` checks that required substrings are present in the workflow files, `scripts/release_tier.sh`, and `.goreleaser.yml`: the platform build jobs' runner labels and `runs-on`, the three native runner labels with their GOOS/GOARCH/`uname` assertions and two portal E2E test names, the exhaustive-regression environment (`SANDMAN_RUN_AGENT_E2E=1`, `SANDMAN_E2E_GATES: all`), and the four GoReleaser build IDs. It does not parse the matrix above, so edits to this table are not enforced; keep the table aligned with those files by review.

### Full regression (`SANDMAN_FULL_REGRESSION`)

Tests that are tagged for a container runtime or a real provider typically skip
themselves when the generic `CI` env var is set (`os.Getenv("CI") != ""`), so
the ordinary PR-time `CI` workflow stays fast and never spends API quota on
real-agent runs. The `Full Regression - Linux` workflow is different: it runs
every required scenario. It sets `SANDMAN_FULL_REGRESSION=1`, and CI-only skip
guards are written as
`if os.Getenv("CI") != "" && !testenv.FullRegression() { t.Skip(...) }` so the
exhaustive job actually executes the real-agent, portal, preset-matrix, visual,
and heartbeat-timeout tests that ordinary CI only compiles. Portal browser
guards also accept `SANDMAN_REQUIRE_PORTAL_BROWSER=1`, which overrides the CI
skip and turns a missing browser into a failure. Tests can still skip for
missing prerequisites (no container runtime, host CLI, or file-backed auth);
only the manifest-permitted skips below are acceptable for required scenarios.

The checked-in `scripts/release_scenarios.json` manifest defines required tests,
provider CLIs/models, native platform boundaries and narrowly permitted skips.
The only permitted required-scenario skips are the no-runtime negative control
when a runtime exists and two retired PR-flow scenarios whose replacement is
required. Skip reasons must match the manifest. Unrelated optional default tests
are reported but do not substitute for required scenario coverage. New tagged
E2E tests must enter the manifest; contracts reject missing entries.

The four independent jobs call `release-regression-tier.yml` and have no
dependency on each other. The final gate checks each job explicitly, preserving
sibling failures when only one tier is rerun.
Unit includes `visual` tests and Node production-module coverage. Smoke builds
all preset containers without provider calls. Integration excludes the manifest's
live selector and uses fake agents, local Git remotes and real containers.
The subagent permission fixture isolates HOME/config snapshots from runner state
while retaining the runner's XDG data directory for Podman's container store.
Live selects the explicit OpenCode scenarios and receives only OpenCode credentials.
Claude provider E2Es run in a separate manual tier when `Full Regression - Linux`
is dispatched with `run_claude` enabled; only that job receives Claude credentials. Ordinary PR CI runs only the single minimal OpenCode canary described below; the full provider suites remain release-only.

Every Linux tier provisions Node 22, Chromium and validated rootless Podman
with runc. Live additionally installs `opencode-ai@1.19.3` and requires
`OPENCODE_API_KEY`. The optional manual Claude tier installs
`@anthropic-ai/claude-code@2.1.283` and requires `CLAUDE_CODE_OAUTH_TOKEN`.
Each provider tier checks its own CLI version. OpenCode auth is
written as JSON with mode 0600. The selected model identifiers are
`opencode/big-pickle` and `claude-haiku-4-5-20251001`. Missing credentials,
tools, required tests, unexpected skips and duplicate executions fail validation.
Quota/provider failures remain failures; they cannot produce successful coverage.

`release-regression-<tier>-<sha>` artifacts contain the raw Go JSON stream,
process exit code, source SHA, observed tool versions and `report.json`.
Reports and Actions summaries count executions, passes, skips, failures,
missing scenarios and interrupted tests; they retain skip/failure reasons and
classify process failures versus timeouts. The evaluator runs with `always()`
even after setup failure, and the final release regression gate requires every
tier. GNU timeout kills the tier process group after a bounded grace period;
runner containers are removed during final cleanup. Native jobs similarly
publish `native-<platform>-<sha>` reports for all four published platforms.

To reproduce one tier after provisioning prerequisites, use:

```bash
mkdir -p regression-artifacts
export RELEASE_TIER=integration SANDMAN_FULL_REGRESSION=1
export SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all
bash scripts/release_tier.sh
GITHUB_SHA=$(git rev-parse HEAD) python3 scripts/release_report.py \
  "$RELEASE_TIER" regression-artifacts/events.jsonl \
  "$(cat regression-artifacts/exit-code)" regression-artifacts/report.json
```

Run `python3 scripts/release_report_test.py` for hermetic accounting tests.
Hosted live runs and native runners remain necessary to establish actual release
evidence; local contract tests alone cannot certify a release. The main ruleset
keeps ordinary PR contexts because release-only contexts do not exist on other
PRs; maintainers must inspect both release workflows at the exact release head
before merging. Do not alter runtime check-rollup selection for these jobs.

`go.yml` must never set `SANDMAN_FULL_REGRESSION`; the contract test
`TestCIWorkflowKeepsOptInSuitesDisabled` enforces that so the CI-only skips
remain active on pull requests.

## Smoke tests

Smoke tests run a single agent session end-to-end and verify the core run loop. They are fast compared with full e2e tests. They need the `smoke` (or `e2e`) build tag, `SANDMAN_RUN_SMOKE_E2E=1`, and a non-empty provider allowlist; without the opt-in or the allowlist they skip themselves.

```bash
SANDMAN_RUN_SMOKE_E2E=1 SANDMAN_TEST_PROVIDERS=opencode \
  go test -tags smoke -timeout 30m ./internal/cmd -run Smoke
```

`SANDMAN_TEST_PROVIDERS` accepts a comma-separated list of provider names, `all`, or `*`. Even when enabled, individual cases skip when no container runtime, host CLI, or file-backed agent auth is available, or when image build, config mount, container start, or worktree setup is unavailable on the host.

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

Lifecycle regression coverage follows the canonical
[transition contract](../architecture/run-state-machine.md). Exercise composed
CI → review → approval → occupied slot → merge; quota → recovery → CI/review;
deferred admission → explicit abort; and ownerless wait → different-batch
rehydration inside fixed grace. Assert emitted events and their CLI/Portal folds,
managed CI/review hard deadlines, session-local relaunch and invocation-local
standalone CI bounds, completed quota polling, cumulative RunID duration, exclusive claims,
held dependencies, terminal preservation, and launch-boundary quota revalidation.

E2E tests exercise multi-session behavior such as continuing a previous run, batch orchestration, and subagent permission boundaries. They require the `e2e` build tag and are slower than smoke tests.

Implementation pull-request lifecycle changes must keep the production-path
regression slice green with `go test ./internal/batch ./internal/cmd`. This
slice covers merged completion precedence, recoverable awaits, continuation
re-evaluation, retained review evidence, and portal projection. Wait admission
is restricted to current-head CI queued/running, a confirmed in-deadline
delegated-review request (which counts as active from delivery, before the
reviewer starts), or a recognised provider usage limit with quota-reset polling.
The suite also pins failure for absent PRs, idle/generic
gates, stale heads, lookup/state errors, and the historical exhausted in-session
relaunch allowance; verified
request-scoped review outcomes resume implementor-owned repair or merge work.
Scheduler coverage must show that legitimate awaits release execution
capacity, keep dependents queued, and resume automatically when external work
resolves and a slot is available. Portal aggregation remains `Reviewing` while
an associated review run is active, whether the implementation's runtime phase
is waiting or running.

`TestManagedReviewLifecycle_ProductionPathRecoversAndReleasesDependency` in
`internal/batch` is the focused vertical slice for request delivery, waiting,
restart recovery, same-RunID continuation, verified merge observation, and
dependent release. Its cancellation companion verifies that explicit abort
terminalizes the run and revokes recovery. Durable review publication and
canonical evidence migration remain covered at their daemon and registration
seams; run those focused package suites without cache when changing either
boundary.

The [AFK exit baseline](afk-exit-regression-baseline.md) distinguishes retained
historical exits from removed cumulative restrictions. Regression coverage must
show that stale/exhausted/corrupt repair ledgers cannot veto fresh execution;
reviewer failures remain retryable beyond three launches with backoff and single
publication; standalone CI permits pending checks after 30 but before 60 minutes,
preserves same-head counters, and resets on a new head or fresh invocation.
Quota coverage must include completed-poll accounting across executor/scheduler
reconstruction, downtime/capacity clock jumps, the final boundary probe, zero and
positive configured ordinary retries, invalid-accounting fallback, independent
sibling outcomes, and cancellation. Quota estimate normalization must preserve
existing recovery grace; CI/review hard deadlines remain enforced. Terminal
failure payloads carry no active `await`, `await_reason`, or `gate` markers.

```bash
SANDMAN_TEST_PROVIDERS=opencode \
  go test -tags e2e -timeout 30m ./internal/cmd -run PRFlow
```

For the full `-run TestPresetMatrixHarness` suite (every scaffold preset —
`go`, `node`, `dotnet`, `elixir`, `rust`, `java`, `ruby`, `python`,
`generic`), use `-timeout 90m`: each preset pays a fresh `podman build`
of the scaffolded image. The script `scripts/run-preset-matrix.sh` applies
that budget automatically.

Each real-agent preset-matrix CLI invocation allows up to 15 minutes for live
provider latency. Its deadline is capped by the remaining suite deadline minus
30 seconds for cancellation and cleanup. This avoids cutting off an actively
progressing agent after five minutes while retaining a bounded per-run budget;
the exhaustive E2E tier still uses its canonical 90-minute timeout.

## Gated scenarios

Expensive scenarios are selected with `SANDMAN_E2E_GATES`. The value can be a single scenario, a comma-separated list, `all`, or `*`. Only the `batch` gate's `TestRunBatch_EndToEnd` and the `base_branch_feature` test run without a build tag; every other gated test, including the `batch`-gated claude preset and context-rollover tests, lives in `//go:build e2e` files and also needs `-tags e2e`.

### Scenario reference

| Scenario | Package | Test selector |
|----------|---------|---------------|
| `batch` | `internal/batch` | `TestRunBatch_EndToEnd` |
| `continue_multi` | `internal/cmd` | none: no gated test currently references this scenario |
| `opencode_subagent` | `internal/cmd` | `TestOpencodeSubagentPermissionAllowAll` |
| `badge` | `internal/cmd` | badge scenario tests |
| `pathlen` | `internal/cmd` | path-length scenario tests |
| `batch_id_rules` | `internal/cmd` | `TestBatchIDRules_*` |
| `preset_matrix` | `internal/cmd` | none gated by this constant; the e2e-tagged preset-matrix harness (`preset_matrix_harness_e2e_test.go`) runs through `scripts/run-preset-matrix.sh` |
| `base_branch_feature` | `internal/batch` | `TestRunBatch_BaseBranchFeature_CutsWorktreeFromFeatureBranch` |
| `review_daemon` | `internal/cmd` | `TestReviewDaemonE2E_RealAgentInContainer` |
| `lifecycle_commands` | `internal/cmd` | `TestLifecycle_*` |
| `review_wait` | `internal/cmd` | `TestReviewWaitStabilization_*` |

All scenarios except the untagged `batch` and `base_branch_feature` tests
require `go test -tags e2e`.

```bash
# Single scenario
SANDMAN_E2E_GATES=batch go test -timeout 30m -run TestRunBatch_EndToEnd ./internal/batch

# Multiple scenarios (tagged tier)
SANDMAN_E2E_GATES=batch,opencode_subagent,review_wait \
  go test -tags e2e -timeout 30m ./...

# All scenarios
SANDMAN_E2E_GATES=all go test -tags e2e -timeout 90m ./...
```

## Per-agent model override

By default, smoke and e2e tests use the model baked into each test case. To target a different model for a specific agent, set `SANDMAN_TEST_MODEL_<AGENT>` using the uppercased agent name.

```bash
SANDMAN_TEST_MODEL_OPENCODE=opencode/gpt-5-nano \
  SANDMAN_RUN_SMOKE_E2E=1 SANDMAN_TEST_PROVIDERS=opencode \
  go test -tags smoke -timeout 30m ./internal/cmd -run Smoke
```

## Real-agent opt-in (`SANDMAN_RUN_AGENT_E2E`)

Real-agent E2E sub-tests, including PR-flow and preset-matrix scenarios, run
the **real** opencode or claude agent inside a real container against a real LLM
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
runtime. The Claude Code case (`TestPresetMatrixHarness_RealClaudeAgentReuseSession`)
runs only when `SANDMAN_TEST_PROVIDERS` names `claude` (or `all`), the `claude`
CLI is on `PATH`, and either `~/.claude/.credentials.json` exists or
`CLAUDE_CODE_OAUTH_TOKEN` is exported (the macOS path, from `claude setup-token`).
It uses the `haiku` alias unless `SANDMAN_TEST_MODEL_CLAUDE` is set, runs the
built-in preset command in a container, and continues the row with
`--reuse-session`. The first real run is also the place to capture one
usage-limit `result` line on a limited account and pin it as the golden for the
Claude usage-limit rule. With the opt-in set, the preset-matrix sub-tests are real-workflow
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

Keep Git cleanup scoped to the temporary worktree being removed. Never run repository-wide `git worktree prune` from a test or agent sandbox: its `.git` directory may be shared with host worktrees whose recorded paths are not visible in the sandbox.

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

## Quality gate and protection migration

`CI / quality-gate` runs with `always()` after `static-analysis`, `contracts`, `build-linux`, `build-macos`,
`portal-continuity-browser`, `goreleaser`, and `semantic-pull-request`.
Only success is accepted, except the intentional skipped title check on a main
push. The independent `build-linux` and `build-macos` jobs preserve the existing
`build (ubuntu-latest)` and `build (macos-latest)` check names. Each is a direct
gate dependency, so a partial rerun of one platform cannot hide the other
platform's failure through GitHub's matrix-result aggregation. The snapshot uses
Go from `go.mod` and GoReleaser v2.10.1 to package all four configured platforms
on PRs and main pushes. It never publishes a release.

Jobs are bounded at 25 minutes (build), 20 (packaging), 15 (browser), 5 (title),
and 2 (aggregate), keeping the parallel required path within the managed
30-minute observation budget under normal runner availability. Queue delays
remain outside job timeout control. PR events include opened, synchronize,
reopened, and edited, with no target-branch filter. Concurrency cancels only
superseded runs of the same PR; each main push has an independent group.

Run `go test ./scripts` for workflow contracts and the hermetic evaluator tests.
Those tests execute the same `scripts/quality_gate.sh` used by Actions with
failed, cancelled, skipped, absent, and unknown dependency results. Ordinary PR
CI retains the opt-in provider/smoke/E2E exclusions and existing runtime guards.

### Verify and apply active protection

The JSON in `.github/rulesets/main.json` is desired policy, not evidence of
active protection. On 2026-10-14 the connected GitHub API returned an empty
repository ruleset list, and reading legacy main branch protection returned
403 `Resource not accessible by integration`. Enforcement is therefore
**unverified**, not established as absent or successfully applied.

An authorized maintainer must perform the following migration:

1. Read both repository/organization rulesets and legacy protection for `main`
   in GitHub Settings (Rules / Rulesets and Branches), or with an administrative
   credential using `gh api repos/rafaelromao/sandman/rulesets`,
   `gh api repos/rafaelromao/sandman/rules/branches/main`, and
   `gh api repos/rafaelromao/sandman/branches/main/protection`. Record the active
   rule IDs, enforcement, required context names and check-source integration,
   bypass permissions, and any access limitations. A 403/404 is not verification.
2. Observe a PR run and a main-push run of the new workflow. Confirm exact check
   names from GitHub, both matrix entries, packaging, and the aggregate result;
   confirm title edits rerun validation and invalid titles block the gate.
3. Add the observed `quality-gate` context to the existing active policy in the
   UI, preserving every currently required context and restriction. The checked-in
   JSON retains the previous four contexts plus `CI / quality-gate`; use the
   actual emitted check context (and Actions integration) shown by GitHub.
   Do not overwrite existing protection by blindly importing this JSON. If no
   policy exists, create an active ruleset for `refs/heads/main` using this file
   as the baseline and preserve any applicable organization requirements.
4. Re-read the effective rules and verify the merge UI blocks a failed aggregate
   gate for a maintainer without bypass. Record the policy/run links and result.
   Keep the old contexts until the aggregate gate is proven and active; removing
   them is a separate reviewed migration. If access is unavailable, leave the
   verification pending and document the limitation explicitly.


### Separate CI validation and reports

`make check` validates without editing files; use `make fmt` explicitly to fix
formatting. CI uses the same `fmt-check`, `build-check`, `vet`, `staticcheck`,
`contracts`, `test-ci`, and `test-browser` commands. Local `make test` retains
all default race tests, including browser tests when Chromium is available.

The aggregate additionally requires `static-analysis` and `contracts`. Existing
platform status names and required protection contexts remain unchanged. Static
analysis, contracts (including the ADR index and skill hygiene), Linux/macOS
build/race tests, Portal browser scenarios, and snapshot packaging run in parallel.
Only the browser job installs Chromium. `scripts/portal_browser_tests.txt` lists
all required Chromium-backed tests (continuity, subject switching, page rendering,
and visual layout). `test-browser` enables the `visual` build tag and requires a
browser; `test-ci` excludes that same exact inventory. A Go AST contract checks
the inventory against browser helper callers, preventing silently skipped scenarios.
The built-binary Portal browser test retains its separate
`SANDMAN_RUN_PORTAL_BINARY_E2E=1` opt-in; it was never part of default PR coverage. Node remains installed in both platform
jobs so production-module tests and JavaScript assertions remain in the race suite.

Linux retains Podman with runc and its runtime probe. Default container tests are
mixed with hermetic tests in the command, batch, and sandbox packages; tests using
real containers and cancellation must remain in the Linux race tier until an
explicit inventory and tier migration preserves them. Provider, smoke and real-agent
opt-ins remain disabled on PRs. No additional API-quota tests are introduced.

`scripts/ci_report.sh` captures raw logs, Go JSON events, a structured JSON report,
and Markdown under `reports/`, preserving command failure status. Job summaries
identify category, failing package/test, artifact name, and elapsed seconds;
compiler/setup failures refer to the raw log. Artifacts upload with `always()`.
Packaging retains `dist/` and points to GoReleaser diagnostics. Snapshot and release
both use Go from `go.mod` and GoReleaser `v2.10.1` for four-platform archives.

Race jobs have a 25-minute limit (tests 20 minutes); static/contracts/browser have
15-minute limits, packaging 20, and aggregate 2. Measure the first PR's elapsed
critical path including setup and artifact upload against the managed 30-minute
CI-wait budget; local timing alone cannot verify hosted Linux/macOS duration.

Security and workflow validation are mandatory dependencies of `CI / quality-gate`.
Run `make security-check workflow-check` with govulncheck v1.1.4, Gitleaks
v8.24.3, Actionlint v1.7.7 and ShellCheck v0.10.0 on PATH. The reusable security
workflow also runs every Monday and manually; inspect Scheduled Security in
Actions for failures. Scanner errors fail closed. Public output contains status
only, never raw secret findings; reproduce locally to investigate. No scanner
artifacts are uploaded. The reviewed baseline contains 28 exact historical false positives in Portal
run/batch identifier fixtures (`.github/security-exceptions.json`), owned by
rafaelromao and expiring 2027-01-16. No actual secrets are accepted. Never regenerate a baseline
to accept new findings. Secret exceptions require an exact Gitleaks fingerprint,
owner, rationale and ISO expiry date; expired entries fail validation. Reachable
vulnerabilities must be fixed, with no blanket vulnerability suppression.
Dependabot opens weekly Go module and Actions updates; review those updates and
retain full action commit pins with readable version comments. Release automation
retains write permissions; validation defaults to contents read and semantic title
validation gets pull-request read. Existing protection contexts remain in place.

The Go language minimum remains 1.25; `toolchain go1.26.10` selects a patched
standard library for normal builds; `.go-version` selects the same patched
toolchain explicitly in setup-go and security scans. Keep automatic
toolchain selection enabled. Staticcheck retains its isolated Go 1.25.0 runner
for export-data compatibility; it does not build release binaries. The checked-in
ruleset also requires workflow-validation explicitly; apply/verify live protection
using the existing documented procedure.

### Go coverage rollout

The existing Linux and macOS `test-ci` race runs each collect an atomic Go
profile. There is no second complete test run. `coverage-linux` and
`coverage-macos` artifacts contain the raw profile, package statement totals,
changed-line result, head/base SHA, and policy decision in JSON and Markdown.
Profiles are never summed across platforms; duplicate blocks within a profile
are counted once. These measurements do not measure Portal JavaScript or
browser coverage. The dedicated browser gate and Node tests retain ownership.

`python3 scripts/coverage_report.py --platform linux` reproduces the report after
`make test-ci`. Locally it compares HEAD with its parent; in PR CI it compares
the merge base of the actual PR base SHA and tested merge SHA, including stacked PRs.
CI retains GitHub's synthetic merge checkout so validation covers integration with
the target branch. Reports identify the exact tested SHA.
Main pushes compare with their before SHA. Deleted lines are excluded. Renames
are conservatively treated as deletion/addition, including unchanged executable
lines at the destination. Changed executable lines mean changed lines intersecting
Go coverage block spans, not AST statement counts; comments or braces inside a
block span can therefore be included. Overlapping spans require every block to
be covered. No intersecting executable changes gives n/a, never 0% or 100%.

The baseline is a same-platform artifact from a successful CI main push at the
exact comparison SHA. Stacked bases without such artifacts explicitly report
baseline unavailable; there is no fallback to an unrelated main baseline.
Expired artifacts and API failures also report unavailable. The first rollout
runs establish the measured baselines; no repository-wide floor is imposed.

`.github/coverage-policy.json` starts in `report` mode. The 80% changed-line
proposal and 1 percentage point protected-package regression tolerance are
calibration candidates, not approved requirements. Advisory findings exit zero
so Sandman's overall check rollup continues to work. To enable enforcement,
review the platform baseline reports and CI timings, agree thresholds in a PR,
set `approval_url` to that review and change `mode` to `enforce`. Then violations
(including missing baselines) fail the platform job and existing aggregate gate.
No new ruleset context is needed. Legacy low coverage alone never fails policy.

Protected packages are events, batch, review, daemon and sandbox. Exemptions
require `package`, exact package `source_tree`, `expires_on` (ISO date), a concrete
`reason`, and HTTPS `review_url` in the policy file. Compute the package tree with
`git rev-parse HEAD:internal/events` (substitute the package). Unlike a containing
commit SHA, this can be recorded before committing the policy. Source changes
invalidate it; unchanged code remains exempt only through its expiration date.
They waive only that package regression, not changed-line policy or missing
baseline evidence. Require maintainer review for policy
and exemption changes. Prioritize behavioral ownership, recovery, event
projection and cancellation tests over tests written only to increase coverage.

Coverage reporting runs after failed tests when a profile exists. Reports mark
`collection_status`; only `success` profiles with matching schema, platform and
tested SHA qualify as baselines. Failed/unverified collections are diagnostic
only. CI explicitly grants read access to Actions artifacts; signed download
redirects discard the GitHub authorization header.

### Publication authorization

Release publication uses same-invocation reusable workflows from the exact main
commit: CI (including security, coverage policy and four-platform snapshot), Full
Regression (all manifest tiers), and Native Compatibility (all four platforms).
`Release / publication-gate` rejects any missing, skipped, failed or cancelled
result. No check name, external status, earlier workflow run or release-branch SHA
can authorize publication. GitHub's reusable-workflow dependency graph establishes
provenance; the gate also checks the caller repository, event, main ref, workflow
source SHA and candidate SHA. Do not replace these dependencies with status-name
lookups. A merged release commit always receives fresh validation, even if its
release branch passed. Workflow dispatch is permitted on main for recovery.

The checked-in main ruleset now sets
`strict_required_status_checks_policy: true`: concurrent merges require the PR to
be current with main before merging. Apply and read back protection using the
procedure above; the file alone does not enforce it. On 2026-10-16 the rulesets
API returned an empty list; legacy protection/admin access remains unverified.
An authorized repository administrator must apply/verify strict required checks.
A merge queue would also require `merge_group` CI and title-gate support first.

Regular CI runs one tool-free OpenCode canary request on Linux (60-second
provider deadline, three-minute job, no containers or retries), using the release
manifest's exact CLI/model. A provider deprecation failure instructs the implementor
to update the scaffold fallback, release manifest and release workflow pin together.
The canary is mandatory in the aggregate quality gate; full provider E2Es remain
release-only, and Claude provider E2Es remain manual-only. The existing OPENCODE_API_KEY repository secret is required and is forwarded
by the Release reusable-CI caller.

The regular Linux build also runs the credential-free fake subagent permission
scenario (`TestOpencodeSubagentPermissionAllowAll`) with Podman. Its minimal image
installs both shell fakes (`gh` and `opencode`) on the image PATH, so container
auth setup succeeds before the agent adds the worktree's shim directory. This
exercises the real container startup and permission forwarding without a provider
request. Diagnostics are included as `container-permissions` in the existing
Linux build reports; the broader provider suites remain release-only.

Python toolchain image builds mount an optional `github_token` build secret for
mise downloads and artifact-attestation verification. Image builders share the
same argument helper; token values never become build arguments or image ENV.
The release runner supplies `MISE_GITHUB_TOKEN` independently of fake GitHub CLI
credentials. Local builds without a token still work subject to GitHub API limits.
Regular Linux CI tests the generated install instruction with a fake installer,
both with and without authentication, and checks that the secret is absent from
subsequent layers, runtime environment, and image history.

Maintainers can add the `release-validation` label to a same-repository PR to
run the full Linux release regression tiers before merging a release repair.
New commits rerun that opt-in validation. Fork PRs receive no provider secrets;
Claude remains excluded and requires the existing manual workflow input.
