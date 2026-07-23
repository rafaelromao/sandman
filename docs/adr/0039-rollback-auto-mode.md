# ADR-0039: Roll back Auto Mode (`--auto`, `--count`, `auto_max_count`)

## Status

accepted. Supersedes [ADR-0022](0022-rename-ralph-to-auto-mode.md). This ADR records the rollback of the Auto Mode surface; future iterations may amend the rollback or reintroduce a successor flag under a new decision with its own context.

## Context and problem statement

[ADR-0022](0022-rename-ralph-to-auto-mode.md) records the rename from `--ralph` to `--auto` and the addition of the `auto_max_count` config key that bounded the agent's per-run issue picker. The runtime path that would consume those settings was scoped but never landed: `internal/prompt/prompt.go#RenderConfig.CandidateIssues` and `MaxCount`, the `{{CANDIDATE_ISSUES}}` / `{{MAX_COUNT}}` substitution mappings in `internal/prompt/engine.go`, and four test functions that exercised the rolled-back surface.

Concretely:

- `sandman run --auto` and `sandman run --count` are absent from `sandman run --help` today. `internal/cmd/run_test.go`'s `TestRun_HelpMentionsPromptOnlyMode` asserts both flags are absent.
- `internal/prompt/prompt.go`'s `RenderConfig.CandidateIssues` and `MaxCount` fields were never wired to a CLI flag because the implementation was cancelled before any release picked it up.
- `CHANGELOG.md` `## [1.0.0]` listed `auto_max_count` and the `--auto` / `--count` flags under `### Added`, even though the runtime never exposed them. The Added entry was the only evidence that the feature had shipped.

The conservative defaults that `--auto` would have applied (`--retries=3`, `--parallel=1`, `--container-capacity=1`, `--max-containers=1`) are the new defaults outright (see `CHANGELOG.md` `### Changed`), so dropping the surrounding `--auto` flag does not lose any behavior the defaults already covered. There is no operator-visible behavior attached to the `--auto` flag today; there is nothing to migrate.

## Decision

Remove the `--auto` and `--count` flags, the `auto_max_count` config key, the `.sandman/auto-selection-prompt.md` opt-in file, and the dormant `RenderConfig.CandidateIssues` / `MaxCount` prompt-engine fields. The conservative defaults absorb what `--auto` would have toggled, and the conservative defaults are the only operator-visible run knobs going forward until a successor decision introduces one. The `--ralph` flag (the predecessor) stays removed with no replacement, same as before.

A future proposal that wants a successor to Auto Mode — for example, a new issue-picker-over-many flag with its own context and shape — is welcome; it belongs in its own ADR with its own context. This ADR's status is `accepted`, not a stronger marker; it does not preclude re-introducing a similar feature under a different name or a different default policy. Operators who want to amend the rollback (e.g. re-introduce `--auto` with a different implementation) should open a new ADR that supersedes this one.

Concretely:

- `internal/prompt/prompt.go`: drop `RenderConfig.CandidateIssues` and `MaxCount`. The fields are dead code.
- `internal/prompt/engine.go`: drop the `{{CANDIDATE_ISSUES}}` and `{{MAX_COUNT}}` substitution mappings.
- `internal/prompt/engine_test.go`: drop `TestRender_CandidateIssuesSubstituted`, `TestRender_MaxCountSubstituted`, `TestRender_CandidateIssuesAndMaxCountBothSubstituted`.
- `internal/prompt/renderer_test.go`: drop `TestRenderer_ConfigMappingCandidateAndMaxCount`.
- `internal/cmd/run_test.go`: keep the `--auto` and `--count` absence assertions in `TestRun_HelpMentionsPromptOnlyMode`; add a leading comment block that names the test as a rollback sentinel.
- `CHANGELOG.md` `## [1.0.0]`: move the `--auto` / `--count` / `auto_max_count` entries out of `### Added` and into `### Removed`. Rewrite the adjacent `--ralph` migration note and the `priority-selection-prompt.md` / `auto-selection-prompt.md` removal notes so they no longer reference a feature that no longer exists.

## Consequences

### Easier

- The contributor-facing CHANGELOG no longer claims features the runtime never exposed.
- The conservative defaults are the only operator-visible knob; "auto mode" reduces to "the default run" without a flag, which is a smaller surface for new operators to learn.
- `internal/prompt.RenderConfig` shrinks to the actually-used surface (operator-controlled template paths plus the REVIEW_COMMAND substitution).
- The rollback is reversible: a future ADR may supersede this one and reintroduce a successor flag under a different name or a different default policy. Nothing in the runtime path is destructively removed in a way that blocks re-introduction.

### Harder

- Operator scripts that grep for `--auto` / `--count` or that read `auto_max_count` from `.sandman/config.yaml` will fail. There is no migration: the fields never worked, so the scripts were either never run or were running against v1.0.0 CHANGELOG-style stubs. Authors of those scripts own the cleanup.
- The conservative defaults are the only operator-visible run knobs going forward. Any future proposal that wants a new issue-picker-over-many flag (e.g. a renamed `--auto` with a different implementation) belongs in its own ADR with its own context; it does not need to anchor on this one.

## Supersedes

[ADR-0022](0022-rename-ralph-to-auto-mode.md). That ADR described the rename to `--auto`; this ADR records the rollback that supersedes the rename and the surrounding Auto Mode surface. A future ADR may supersede this one to re-introduce a successor.
