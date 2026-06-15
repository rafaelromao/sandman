# ADR-0025: Rename Ralph to Auto Mode

## Status

proposed

## Context

ADR-0012 introduced the Ralph Loop — an agent-driven two-phase issue selection process. The naming has aged poorly: the "Ralph" and "Ralph Loop" labels are not self-explanatory, they don't match what the feature actually does, and the issue has been the source of repeated user confusion in support requests.

Parent PRD #895 captures the broader product intent: PRDs and selection semantics should align with how a normal `sandman run` works, and Auto Mode is the user-facing label for the selection feature.

## Decision

Rename the user-facing surface from "Ralph" / "Ralph Loop" to "Auto Mode":

- `--ralph` becomes `--auto` (a boolean flag, not an int flag).
- `--count` becomes the candidate cap on `--auto` runs; `0` means unlimited.
- `auto_max_count` is the new config key. Default is `50`. `0` means unlimited.
- The scaffolded prompt file `.sandman/priority-selection-prompt.md` is renamed to `.sandman/auto-selection-prompt.md`. The embedded default in `internal/prompt/auto_selection_prompt.md` is updated to mention "Auto Mode" in its header.
- The portal "Ralph Loop" radio option becomes "Auto Mode" with the value `auto`. The form field `ralph` is renamed to `autoMaxCount` (JSON tag `autoMaxCount`).
- Selection semantics: Auto Mode accepts the same filters as regular Sandman runs (label, query, explicit issue args). It does not reject explicit issue args; the args form the candidate pool, optionally narrowed by label/query and capped to `--count`.
- Conservative defaults (`--parallel=1`, `--container-capacity=1`, `--max-containers=1`, `--retries=3`) silently apply when `--auto` is set, mirroring the Ralph Loop behavior.
- `priority-selection-prompt.md` is no longer recognized as the opt-in signal. If a project has a customized `priority-selection-prompt.md`, `sandman init` copies its content to `auto-selection-prompt.md` so the user's customization is preserved (soft migration).
- The `--ralph` flag is removed. Operators must migrate scripts to `--auto` (with `--count` if they need a non-default cap).

### Key design choices and rationale

#### `--auto` is a bool, `--count` is the cap

Splitting the boolean mode (`--auto`) from the integer cap (`--count`) keeps the CLI surface orthogonal with the other run-time flags and lets the config default (`auto_max_count`) be the fallback when neither is set. The previous `--ralph=N` form is dropped because it conflated "is the feature on?" with "how many?". The acceptance criterion "`--count` controls the candidate cap for Auto Mode" made this split the cleanest mapping.

#### `0` means unlimited in both places

Both `--count 0` and `auto_max_count: 0` mean "no cap". This lets operators opt out of the cap without changing the CLI surface. A hand-built `*config.Config` with `AutoMaxCount: 0` is treated as the explicit-unlimited signal; the `*int` raw YAML field keeps the "absent" and "explicit 0" cases distinct during `Load()`.

#### Same filters as regular runs

Auto Mode reuses the existing filter pipeline (`parseIssueSelection`, `resolveIssuesLocally`, `searchIssues`, `querySupportsLocalFiltering`). This means explicit issue args, label filters, query filters, and the `--label`+`--query` mutual exclusion all behave identically between `sandman run 42 43` and `sandman run --auto 42 43`. Operators don't have to learn a second set of filter rules.

#### Soft-migration of the prompt filename

If a project has `.sandman/priority-selection-prompt.md` (from a prior `sandman init`) but no `auto-selection-prompt.md`, the scaffold copies the legacy file's content to the new name. This preserves user customizations without requiring them to manually rename the file or re-edit the prompt. The new `auto-selection-prompt.md` then opts the project into Auto Mode's selection phase.

#### ADR-0012 is not renamed

ADR-0012 is an immutable historical record of the original decision. We add ADR-0025 as a new file that supersedes 0012. The index in `docs/adr/README.md` is updated to point at 0025 for the current surface.

## Consequences

### Positive

- The naming now matches the feature's behavior. "Auto Mode" reads as "Sandman auto-selects issues", which is what it does.
- The CLI surface is cleaner: `--auto` is a bool like `--continue` and `--override`; `--count` is an int like `--parallel` and `--retries`.
- The filter pipeline is unified. Operators learning `sandman run` learn `sandman run --auto` for free.
- Soft migration preserves user customizations of the prompt file.

### Negative

- `--ralph` is removed. Scripts that called `sandman run --ralph N` must change to `sandman run --auto --count N`. This is a breaking change but follows the same precedent as the prior `--next` → `--ralph` rename.
- Projects with a customized `priority-selection-prompt.md` will see their file copied to `auto-selection-prompt.md`. They can delete the legacy file once they have confirmed the new one is in place, but the scaffold does not auto-delete it.

### Neutral

- The conservative defaults block is unchanged in shape — only the flag name it triggers on changes.
- The portal HTML form field is renamed; existing portal localStorage state that stores a `ralph` key is ignored (the form reads `autoMaxCount` only). This is a one-time UX transition for users who had previously submitted a Ralph run from the portal.

## Blocked by

None - can start immediately

## Runtime Context

- Issue: #896
- Parent PRD: #895
- Supersedes: ADR-0012 (Ralph Loop)
