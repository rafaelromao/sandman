# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Portal "Active Batches" filter now checks daemon control socket liveness (100ms `net.DialTimeout`) instead of relying solely on event-log state. Runs whose socket is dead are downgraded from `kind: "active"` to `kind: "completed"`. Covers both live-batch and historical-run paths. (#726, #740)

### Added

- `SANDMAN_TEST_MODEL_<AGENT>` env vars (e.g. `SANDMAN_TEST_MODEL_OPENCODE`, `SANDMAN_TEST_MODEL_PI`) override the model the smoke and prflow e2e tests target per agent. When unset, the tests use the literal model baked into their case lists. Resolved through `testenv.ResolveTestModel` (testenv_test.go covers empty/set/trim/agent-scoped paths).
- `sandman init` gains `--retries` and `--run-idle-timeout` flags that persist `retries` and `run_idle_timeout` in the scaffolded `.sandman/config.yaml`. Sentinel `-1` keeps the built-in default (`3` for `retries`, `1800` for `run_idle_timeout`); `0` disables retries / the heartbeat watchdog respectively.
- Pi preset now snapshots `~/.pi/` but keeps `~/.pi/agent/npm` (npm cache) and `~/.pi/agent/sessions` (mutable per-run sessions) mounted live. Mirrors the OpenCode split using the same mechanism; no new fields or code paths (ADR-0017).
- `run.idle_timeout` event type: documented in `events.go`, `monitoring.md`, and `configuration.md`. The heartbeat watchdog emits this event when an agent produces no log output for `run_idle_timeout` seconds (default: 1800, configurable via `run_idle_timeout` in config or `--run-idle-timeout` on the CLI). `0` disables the watchdog.
- CLI summary line for `sandman run` and `sandman continue` now includes a non-zero `aborted` bucket (`Summary: N succeeded, N failed, N aborted, N blocked`), and emits only the buckets whose count is non-zero. A new `cmd.ExitCodedError` carries the process exit code for the abort path: when `RunBatch` returns `batch.ErrAborted`, the CLI prints `batch aborted by operator` to stderr and the process exits with code 130 (the standard Unix code for SIGINT). Real run failures keep the existing `run batch: ...` message and non-zero exit.
- Cascade abort: when an in-batch blocker finishes with status `"aborted"`, its dependents are emitted as `run.aborted` (not `run.blocked`) with an `aborted_by` payload naming the upstream blocker. The `RunID` on the cascade `run.aborted` matches the `RunID` on the prior `run.queued` event so projection collapses to a single `RunState`.
- `Aborted` as a first-class terminal AgentRun status in the events vocabulary. A new `run.aborted` event is emitted when an agent run is interrupted by context cancellation, and `RunState.Status()` returns `"aborted"` for it. The abort path now also covers runs that were still queued (waiting on the turn gate or the start gate) at the moment of cancellation; the `RunID` on the abort event matches the prior `run.queued` event for the same issue.
- `batch.ErrAborted` sentinel error. `RunBatch` now returns an error wrapping `ErrAborted` (matchable with `errors.Is`) when context cancellation interrupted one or more in-flight AgentRuns, so the CLI layer can distinguish operator-initiated abort from a genuine run failure.
- Standard open-source project files: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `CHANGELOG.md`
- `Makefile` with common development tasks
- GitHub issue templates for bug reports, feature requests, and agent improvements
- GitHub pull request template
- Architectural Decision Record (ADR) convention with template
- Container capacity and max_containers configuration model
- Pooled container sandbox with auto-scaling (`max_containers=0`) and queueing
- Dependency-aware batch execution with topological sort and cycle detection
- `--include-dependencies` CLI flag for transitive blocker expansion
- `run.blocked` event type and blocked status
- Built-in agent presets (opencode, pi) with config resolver
- `--prompt`, `--template`, `--prompt-arg` CLI flags for prompt customization
- BuildToolsPreset scaffold-time recipe with pinned versioning and mise
- Node BuildToolsPreset with repo hint detection and pinned container scaffolding
- `--sandbox`, `--container-capacity`, `--max-containers` CLI flags
- Event log: `run.warning` event type

### Changed

- Breaking schema rename (lands alongside #640): the legacy `default_` prefix is dropped from three config keys — `default_agent` → `agent`, `default_model` → `model`, `default_parallel` → `parallel`. Hard cutover with no shim; once #640 ships, repos that still use the old keys silently fall back to project defaults. Migrate existing `.sandman/config.yaml` files with `sed -i -e 's/^default_agent:/agent:/' -e 's/^default_model:/model:/' -e 's/^default_parallel:/parallel:/' .sandman/config.yaml` after upgrading to a release that contains both #640 and #641.
- Breaking default change: `DefaultRetries = 3` is now applied by `Load()` when the YAML `retries:` key is absent (was implicitly `0`). This is the new ralph-style default and flows through `resolveRetries` to runtime behaviour. To preserve the prior no-retry behaviour, set `retries: 0` explicitly in `.sandman/config.yaml` (or pass `sandman init --retries 0` on first scaffold).
- Renamed the `run.cancelled` event type to `run.aborted`. New runs emit `run.aborted` with `status: aborted`. The projected status for a cancellation event is now `"aborted"` (was `"failure"`), and `sandman clean --failed` now removes aborted runs. Legacy `run.cancelled` events in existing `events.jsonl` files continue to project as `"aborted"`, so the cut-over is lossless for the abort semantic.
- Dependency resolution now treats in-batch success as sufficient to unblock dependents. When a blocker AgentRun in the same batch finishes with status `success`, its dependents start immediately without re-fetching the blocker's GitHub issue state. Only external blockers (issues not in the current batch) are still re-checked against GitHub closure right before a dependent starts, preserving the existing gate when an external blocker has not closed yet.

## [0.1.0] - 2026-05-09

### Added

- Initial release of Sandman
- CLI commands: `init`, `run`, `status`, `history`, `continue`, `clean`, `config`
- Git worktree sandboxing for isolated agent execution
- Parallel batch execution with configurable concurrency
- Event logging to JSONL
- Integration with `gh` CLI for issue fetching
- Prompt template rendering for AI agents
- Support for custom agent providers via configuration

[Unreleased]: https://github.com/rafaelromao/sandman/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/rafaelromao/sandman/releases/tag/v0.1.0
