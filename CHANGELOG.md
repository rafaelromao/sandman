# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Pi preset now snapshots `~/.pi/` but keeps `~/.pi/agent/npm` (npm cache) and `~/.pi/agent/sessions` (mutable per-run sessions) mounted live. Mirrors the OpenCode split using the same mechanism; no new fields or code paths (ADR-0017).
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
