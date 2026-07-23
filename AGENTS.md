# Sandman

CLI tool for orchestrating AFK coding agents in isolated sandboxes.

## Purpose

This file provides operating instructions for coding agents working in this repository. Follow the architecture notes and workflow rules below before making changes.

## Architecture

- **Event-sourced state**: Run status is a projection over the append-only `.sandman/events.jsonl`, not mutable records. `events.RunState` folds events into current state. If a status looks wrong, start by tracing the relevant event types and the fold/projection logic rather than searching for mutable status fields.
- **Factory seams**: `cmd.Dependencies` (`internal/cmd/root.go`) is the top-level dependency injection struct. The per-run seam is `RunExecutor{Execute(ctx, row RowSpec)}` (`internal/batch/row_spec.go`); its implementation holds a `runDeps` struct (github.Client, prompt.IssueRenderer, events.EventLog, RunnableFactory, SandboxFactory, paths.Layout, heartbeatTickInterval, errorLog, runSessionOptions, verifyPath) and a narrow `runCoordination` interface (`*Orchestrator` implements it; RunBatch owns the state). Test injection of the orchestrator's behavior goes through `OrchestratorOpt` options on `NewOrchestrator` — `WithRunnableFactory`, `WithSandboxFactory`, `WithContainerRuntimeFactory`, `WithErrorLog`, `WithRunSessionOpts`, `WithHeartbeatTickInterval`, `WithVerifyPath` (and `WithBadgeHooker` for production). `batch.Request` is the public batch input (issues, config, flags) — it does **not** carry factories. Inject fakes at the `runDeps`/`OrchestratorOpt` seams rather than mocking deep concrete types.
- **Sandbox interface** (`internal/sandbox/sandbox.go`): the abstract isolation contract implemented by `WorktreeSandbox` and `ContainerSandbox`. After the parent-map consolidation (#2244), the interface has 9 methods and `Start(opts SandboxStart) error` is the only configuration entry point — the previous 4-setter pre-Start dance (SetOverride / SetStrandedReconcile / SetGitIdentity / SetContinue) is gone. `SandboxStart{Override, Continue, StrandedReconcile, Identity}` plus `SandboxIdentity{Name, Email}` are the value types callers build per row; production builds them via `runSession.startOptsFor(branch)`. `RestoreHostPaths()` is a separate post-Start protocol and stays unchanged.
- **Filesystem as data store**: There is no database. State lives in flat files under `.sandman/` (manifests, logs, review state), written atomically via temp-file + `os.Rename`. IPC uses Unix domain sockets.

## Sandman task routing

Canonical on-disk layout reference: `docs/architecture/disk-layout.md`.

Start from the most likely architectural seam for the problem.

### Status and run-state bugs

If a bug involves status, lifecycle, or current run state:

- Start with event types and `events.RunState`.
- Trace the fold/projection path from `.sandman/events.jsonl`.
- Do not assume there is a canonical mutable status record.

### Command wiring and dependency injection

If the task involves CLI wiring, root command setup, or substitution in tests:

- Start with `cmd.Dependencies` in `internal/cmd/root.go`.
- Then inspect the relevant command constructors and injected dependencies.
- Prefer changing seams over introducing new hidden globals.

### Batch and sandbox orchestration

If a task involves execution flow, runners, or sandbox lifecycle:

- Start with `RunExecutor` and `RowSpec` (`internal/batch/row_spec.go`); the per-run seam is `Execute(ctx, row RowSpec)`. `batch.Request` is the public batch input that feeds it.
- Trace `runDeps` for the test/constructor seam and `OrchestratorOpt` (`WithRunnableFactory`, `WithSandboxFactory`, `WithContainerRuntimeFactory`, `WithErrorLog`, `WithRunSessionOpts`, ...) for test injection. `RunnableFactory` and `SandboxFactory` are the factory interface types inside `runDeps`.
- Keep orchestration logic testable by preserving the `RunExecutor`/`runDeps`/`OrchestratorOpt` interface seams.

### Persistence and file safety

If the task involves manifests, logs, review state, or other persisted data:

- Inspect `.sandman/` writers and readers first.
- Preserve atomic write behavior using temp-file + `os.Rename`.
- Do not introduce partial-write or in-place mutation patterns where existing code expects atomic replacement.

### IPC and socket behavior

If the task involves coordination between processes:

- Inspect Unix domain socket code paths first.
- Prefer changes that preserve clear ownership and cleanup of socket lifecycle.

## Change-safety rules

Before editing shared or central code, assess downstream impact first.

Always run dependency or blast-radius checks before changing:

- Event definitions or event-fold/projection logic.
- Shared command wiring or `cmd.Dependencies`.
- The per-run seam: `RunExecutor` interface, `RowSpec`/`BatchConfig`, `runDeps` (incl. `RunnableFactory`/`SandboxFactory` and their implementations), and `runCoordination`.
- `OrchestratorOpt` options on `NewOrchestrator` (`WithRunnableFactory`, `WithSandboxFactory`, `WithContainerRuntimeFactory`, `WithErrorLog`, `WithRunSessionOpts`, `WithHeartbeatTickInterval`, `WithVerifyPath`, `WithBadgeHooker`).
- Files with high blast radius.
- Persistence code under `.sandman/`.
- IPC or socket lifecycle code.

If a file has high blast radius, prefer the smallest safe change and inspect dependent paths before editing.

## Testing guidance

When writing or updating tests:

- Mock or fake at the documented seams: `cmd.Dependencies` (top level); `runDeps` fields and `OrchestratorOpt` options (`WithRunnableFactory`, `WithSandboxFactory`, `WithContainerRuntimeFactory`, `WithErrorLog`, `WithRunSessionOpts`, `WithHeartbeatTickInterval`, `WithVerifyPath`) for orchestrator-level injection; `RunnableFactory`/`SandboxFactory` fakes that satisfy the factory interfaces.
- Do not mock deep concrete internals when an interface seam already exists.
- Preserve event-sourced behavior in tests; verify projections/folds rather than inventing alternate mutable-state shortcuts.

### User command: "run full regression suite"

Trigger phrases: `run full regression suite`, `run all tests`, `run the full suite`, `run regression tests`. When the user asks for any of these, do **not** fall through to `make check` (the default unit suite). Run every test tier — unit, smoke, e2e — with the canonical env vars and timeouts from `docs/development/testing.md`:

```bash
# 1. Default unit + race (matches `make test`)
go test -race -v ./...

# 2. Smoke — every provider, every buildTools preset, full timeout
SANDMAN_TEST_PROVIDERS=all SANDMAN_RUN_SMOKE_E2E=1 \
  go test -tags smoke -timeout 60m ./internal/cmd -run Smoke

# 3. E2E — every scenario gate, no real-agent sub-tests
SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all \
  go test -tags e2e -timeout 30m ./...

# 4. Real-agent preset matrix (opt-in; needs host opencode auth +
#    a working podman/docker. SKIP if either is unavailable.)
if [ -f "$HOME/.local/share/opencode/auth.json" ] && command -v podman >/dev/null 2>&1; then
  SANDMAN_RUN_AGENT_E2E=1 SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all \
    go test -tags e2e -timeout 90m ./internal/cmd -run TestPresetMatrixHarness
fi
```

Skip a tier with a one-line note (e.g. `skipping tier 4: no opencode auth at $HOME/.local/share/opencode/auth.json`) rather than blocking on it. Aggregate the per-tier PASS/FAIL/SKIP counts and surface a final summary, never silently swallow a failure. Timeouts are tuned per tier — do not lower them; lowering reintroduces the `batch aborted by operator` false-positive from under-budgeted test processes.

## Implementation constraints

- Preserve event-sourced reasoning. Do not add mutable status fields as shortcuts if status should be derived from events.
- Preserve atomic filesystem semantics. Prefer temp-file + rename over direct in-place writes.
- Preserve DI seams. Prefer injecting dependencies over hard-coding globals or constructing deep concrete dependencies inline.
- Keep IPC changes compatible with Unix domain socket assumptions already used in the repo.

## Branching and versioning rules

These rules apply to every change-request an agent opens against this repository. They mirror what the GitHub Ruleset on `main` and the CI status checks enforce, and what the release-please automation reads back when computing the next SemVer. Skipping them now means a CI failure later — read them before opening a change request.

- **Trunk-based on `main`.** No `develop` branch exists. Every change lands via change request against `main`. Direct pushes to `main` are blocked by the GitHub Ruleset.
- **Change-request titles follow Conventional Commits.** The full regex enforced by the `CI / semantic-pull-request` status check is:
  ```
  ^(feat|fix|perf|chore|docs|refactor|test|build|ci|revert|feat!|fix!|perf!)(\([a-z0-9-]+\))?!?: .+
  ```
  Allowed types: `feat`, `fix`, `perf`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`, `revert`. Use a trailing `!` (for example `feat!:`) only when the change is breaking. Keep the title to one short imperative sentence with no trailing period.
- **SemVer is derived from the merged change-request title.** `feat:` → minor bump, `fix:` / `perf:` → patch, `feat!:` / `fix!:` → major. Everything else is changelog-only. The release-please automation aggregates the merged change-request type stack and opens a Release change request that applies the next SemVer tag.
- **Required status checks on `main`:** `CI / build` (matrix `ubuntu-latest` + `macos-latest`, `pull_request` and `push` triggers from `.github/workflows/go.yml`) and `CI / semantic-pull-request`. Both must pass before the GitHub merge button enables.

### When opening a change request

1. Branch from `main`.
2. Pick the most accurate Conventional Commits type. `Update README` fails the gate; `docs: explain the branching and versioning rules in AGENTS.md` passes.
3. Keep the title short — one sentence, no trailing period, no issue number in the title.
4. Run `make check` locally before pushing; `CI / build` runs the same suite.
5. Link issues in the body (`Closes #<n>`, `Fixes #<n>`, or `Resolves #<n>`), not in the title.

### When the change touches CI, versioning, or repository-level agent docs

- **CI gate change** → update `.github/workflows/go.yml` and `.github/rulesets/main.json`.
- **Versioning or SemVer rule change** → update this section and `CONTRIBUTING.md`.
- **Repository-level agent-docs change** → update `AGENTS.md` and the relevant `docs/development/` file.

### Feature branches

When several related issues need to ship together as one initiative, group them under a feature branch cut from `main`. The feature branch is the merge target for each issue's change request, and is itself change-requested back to `main` once the last issue lands.

- **Feature branch name** — `feat/<feature-name>`. Lowercase, hyphen-separated. Examples: `feat/release-pipeline-2026q3`, `feat/badge-mark-pagination`.
- **Issue branch base** — issue branches named `sandman/<issue-no>-<slug>` (the runtime's worktree branch name) are cut from the feature branch, not from `main`. The Sandman Go runtime resolves the base via `git.base_branch`; override the default `main` with `sandman run --base-branch <feature>` (or `.sandman/config.yaml:git.base_branch`) when running an issue that belongs to a feature-branch initiative.
- **Change-request target** — issue change requests target the feature branch. The feature branch's own change request targets `main`.
- **Branch lifecycle** — once a feature branch merges to `main`, delete it. Issue branches are deleted on their own merge.
- **Conventional Commits title** — every change request, issue or feature, carries a Conventional Commits header. The `feat:` prefix on the feature branch's change request indicates a SemVer minor bump; the issue change requests can use any allowed type.

Example initiative:

```
main
└── feat/release-pipeline-2026q3
    ├── feat(skill): 955-conventional-pull-request-gate
    ├── fix(workflow): 956-rename-go-to-ci
    ├── docs(contributor): 957-conventional-commits-in-templates
    ├── refactor(prompt): drop-rolled-back-auto-fields
    ├── test(prflow): 955-conventional-titles-in-e2e-fixtures
    ├── build(release): 956-goreleaser-multi-arch-config
    └── ci(ruleset): 955-protect-main-with-required-status-checks
```

The seven change requests cover seven allowed types. Issue change requests PR back to `feat/release-pipeline-2026q3`. Once all seven merge, the feature branch's own change request (titled e.g. `feat: ship the release pipeline initiative`) lands on `main`, and `feat/release-pipeline-2026q3` is deleted. The SemVer bump on `main` is the aggregate of the seven issue change requests plus the feature-branch change request.

## Skill content constraints

Skills under `internal/skill/sandman/` describe how coding agents work with the **user-facing** concepts (`.sandman/` state files, public CLI, review commands, worktrees, ADRs). They must not reference Sandman's **internals** — Go package paths under `internal/`, Go type and function names like `processPR` / `MarkSeen` / `launchReview`, or other implementation details that may shift.

Skills also must not mention the GitHub issue tracker directly (issue numbers, kanban labels, or triage vocabulary). When a skill needs to refer to a project decision or when it needs to refer to the user's work item, describe it behaviorally ("the implementor's open work item") rather than by tracker coordinates or references to ADR (`docs/adr/`) or `CONTEXT.md`; These skills are also going to be used independently.

Concretely: a contributor reviewing a skill should be able to read it without knowing Sandman's package layout or workflow automation. If a paragraph needs re-reading after the user-facing vocab is internalized, it shouldn't name internals.

**Violations** to be caught during `sandman-self-review` and `sandman-pr-review`. The regression net is `internal/skill/sandman/skill_hygiene_test.go`, which scans all skill prose for forbidden internal package paths, forbidden internal Go identifiers, and tracker jargon.

## Before committing

Run:

```bash
gofmt -w . && go vet ./...
```

If the change affects behavior materially, also run the most relevant targeted tests for the touched package(s) before finalizing.

When changing `docs/adr/`, run the structural ADR assertion script to verify index integrity:

```bash
go run scripts/check_adr_index.go
```

## Agent skills and repository references

Use these repository-specific references when appropriate:

- Issue tracker: `docs/agents/issue-tracker.md`
- Triage labels: `docs/agents/triage-labels.md`
- Domain vocabulary: `CONTEXT.md`
- ADRs: `docs/adr/`

## Preferred operating pattern

For most non-trivial tasks, follow this order:

1. Read this file.
2. Read only the narrowed code paths.
3. Read `CONTEXT.md` or ADRs if domain or architectural intent matters.
4. Make the smallest coherent change.
5. Run formatting, vetting, and relevant tests.
6. Summarize what changed, what was verified, and any remaining risk.

