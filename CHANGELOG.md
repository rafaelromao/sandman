# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `auto_max_count` config key (default 50) and `sandman config get/set auto_max_count` round-trip. `0` means unlimited. (#896)
- `sandman run --auto` boolean flag and `sandman run --count N` integer cap. Auto Mode accepts the same filters as regular Sandman runs (label, query, explicit issue args) and lets the agent pick which to implement up to the cap. (#896)

### Added (continued)

- `parallel_reviews` config key and `--parallel-reviews` init flag (default 1) controlling review-daemon concurrency. `EffectiveReviewParallel()` defaults to the constant when unset or invalid.
- `sandman run --continue --run-id` flag for prompt-only continuation. Mirrors `sandman run --run-id`: looks up the most recent prompt-only run (`Issue: 0`) from the event log, reads the task file from its worktree, and forwards it as the prompt for the new prompt-only run. Supports the same format validation and mutual-exclusivity with issue numbers. (#784)
- `scripts/reconcile-stranded-worktrees.sh` — standalone detection tool for stranded worktrees (prints remediation commands for the operator to run) [#733](https://github.com/rafaelromao/sandman/issues/733)
- `## Troubleshooting > Stranded worktrees` section in `docs/usage/commands.md` documenting the new script and `--override` reconciliation behavior
- `SANDMAN_TEST_MODEL_<AGENT>` env vars (e.g. `SANDMAN_TEST_MODEL_OPENCODE`) override the model the smoke and prflow e2e tests target per agent. When unset, the tests use the literal model baked into their case lists. Resolved through `testenv.ResolveTestModel` (testenv_test.go covers empty/set/trim/agent-scoped paths).
- `sandman init` gains `--retries` and `--run-idle-timeout` flags that persist `retries` and `run_idle_timeout` in the scaffolded `.sandman/config.yaml`. Sentinel `-1` keeps the built-in default (`3` for `retries`, `1800` for `run_idle_timeout`); `0` disables retries / the heartbeat watchdog respectively.
- `run.idle_timeout` event type: documented in `events.go`, `monitoring.md`, and `configuration.md`. The heartbeat watchdog emits this event when an agent produces no log output for `run_idle_timeout` seconds (default: 1800, configurable via `run_idle_timeout` in config or `--run-idle-timeout` on the CLI). `0` disables the watchdog.
- CLI summary line for `sandman run` and `sandman run --continue` now includes a non-zero `aborted` bucket (`Summary: N succeeded, N failed, N aborted, N blocked`), and emits only the buckets whose count is non-zero. A new `cmd.ExitCodedError` carries the process exit code for the abort path: when `RunBatch` returns `batch.ErrAborted`, the CLI prints `batch aborted by operator` to stderr and the process exits with code 130 (the standard Unix code for SIGINT). Real run failures keep the existing `run batch: ...` message and non-zero exit.
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
- Built-in agent presets (opencode) with config resolver
- `--prompt`, `--template`, `--prompt-arg` CLI flags for prompt customization
- BuildToolsPreset scaffold-time recipe with pinned versioning and mise
- Node BuildToolsPreset with repo hint detection and pinned container scaffolding
- `--sandbox`, `--container-capacity`, `--max-containers` CLI flags
- Event log: `run.warning` event type

### Changed

- `sandman run --continue` and the orchestrator's retry path now read `.sandman/task.md` verbatim instead of routing the file through a prompt parser/rewriter. The previous pipeline rewrote the file into a different scaffold and carried forward stale blocker state; the continuation seam now returns the file content as-is and falls back to `prompt.DefaultPrompt()` when the file is blank.
- `sandman tdd` skill now reuses an existing `## Plan` section in `.sandman/task.md` instead of drafting a new plan. The plan section stays in the task file for continuation runs, and the `## Next Step` heading remains part of the handoff/resume flow. Verification: Go unit tests cover the plan-reuse branches and prompt handoff behavior. (#912)
- **Sandman layout redesign**: The on-disk layout has been redesigned. The `batches.json` file now serves as the canonical index of all batches (replacing the former `.sandman/runs/` directory-based scanning). Archive directories live under `.sandman/archive/<batch-id>` instead of `.sandman/archive/<id>`. The `clean` command now uses `--archived` (remove archived batches) and `--stale` (recover stale runs in dead batches) instead of status-based flags. The old `.sandman/runs/` and `.sandman/logs/` directories are no longer used; all run artifacts now live under `.sandman/batches/<batch-id>/runs/<run-id>/`. See ADR-0032 for the full design rationale.

### Fixed

- `--continue` no longer carries forward a stale `## Blockers` section from a prior task.md. The section was freeform text the agent wrote on a previous run and was never revalidated against the live GitHub state — that is what caused issue #1193 to keep reporting "PR #1208 remains open, awaiting the unrelated CI failure" after the blocker was already resolved. `prompt.ContinuationTaskPrompt` now strips the `## Blockers` H2 block from the resumed prompt so the agent re-discovers blockers against the current state of the world instead of inheriting them from the file.
- **Breaking CLI change**: `sandman run --continue` no longer accepts a `<prompt-text>` trailing argument. The command now takes only issue numbers (`sandman run --continue <issue-number>...`). The resume prompt is read verbatim from `.sandman/task.md` in each issue's worktree, falling back to an empty task template when the file is missing. Portal continue preset updated accordingly.
- Breaking schema rename (lands alongside #640): the legacy `default_` prefix is dropped from three config keys — `default_agent` → `agent`, `default_model` → `model`, `default_parallel` → `parallel`. Hard cutover with no shim; once #640 ships, repos that still use the old keys silently fall back to project defaults. Migrate existing `.sandman/config.yaml` files with `sed -i -e 's/^default_agent:/agent:/' -e 's/^default_model:/model:/' -e 's/^default_parallel:/parallel:/' .sandman/config.yaml` after upgrading to a release that contains both #640 and #641.
- Breaking default change: `DefaultRetries = 3` is now applied by `Load()` when the YAML `retries:` key is absent (was implicitly `0`). This is the new ralph-style default and flows through `resolveRetries` to runtime behaviour. To preserve the prior no-retry behaviour, set `retries: 0` explicitly in `.sandman/config.yaml` (or pass `sandman init --retries 0` on first scaffold).
- Renamed the `run.cancelled` event type to `run.aborted`. New runs emit `run.aborted` with `status: aborted`. The projected status for a cancellation event is now `"aborted"` (was `"failure"`). Legacy `run.cancelled` events in existing `events.jsonl` files continue to project as `"aborted"`, so the cut-over is lossless for the abort semantic.
- Dependency resolution now treats in-batch success as sufficient to unblock dependents. When a blocker AgentRun in the same batch finishes with status `success`, its dependents start immediately without re-fetching the blocker's GitHub issue state. Only external blockers (issues not in the current batch) are still re-checked against GitHub closure right before a dependent starts, preserving the existing gate when an external blocker has not closed yet.

### Removed

- `--ralph` flag (was `sandman run --ralph N`). Migrate to `sandman run --auto --count N`. The `--ralph` selection behavior is now `--auto`; the conservative defaults (`--retries=3`, `--parallel=1`, `--container-capacity=1`, `--max-containers=1`) silently apply for both. (#896)
- `.sandman/priority-selection-prompt.md` is no longer recognized as the Auto Mode opt-in signal. Customized files are soft-migrated to `.sandman/auto-selection-prompt.md` on the next `sandman init`; the legacy file may be deleted at the operator's discretion. (#896)
- Pi agent preset, its `~/.pi/` snapshot split (see ADR-0017), and all Pi-specific branches; users must configure a custom provider or migrate to OpenCode. See ADR-0024.
- `internal/prompt/plan-template.md` and its Go embed. The plan output shape belongs to the `sandman-plan` skill, not the prompt package; the file should never have been created under `internal/prompt/`. Removes the `//go:embed plan-template.md` and `defaultPlanTemplate()` from `internal/prompt/engine.go`; the two `TestDefaultPlanTemplate_*` guards in `internal/prompt/engine_test.go` are kept as `t.Skip`-only regression markers documenting the deletion.

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
