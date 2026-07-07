# Disk layout

## Intro

Every persisted Sandman artifact lives under `<repo>/.sandman/` (with two documented exceptions: out-of-repo tempdirs used as fallback parents for config snapshots, and the shared `~/.agents/skills/sandman/**` skill tree installed into the user's home).

This document captures the canonical on-disk layout that every slice of the on-disk-rename PRD is converging toward. It reflects the post-#1848 end state — `SelfPostStore` and `.sandman/reviews/self-posted.json` are gone — and lists the runtime sidecars that this slice introduces under `state/`.

## Canonical tree

```
.sandman/
├── Dockerfile                          # scaffold (init only)
├── config.yaml                         # scaffold (init only)
├── prompt.md                           # scaffold (init only)
├── auto-selection-prompt.md            # scaffold (init only)
├── events.jsonl                        # runtime, multi-writer (O_APPEND)
├── events.jsonl.malformed              # runtime sidecar (quarantined torn lines)
├── batches.json                        # runtime, atomic-rename
├── batches.json.bak                    # runtime sidecar (previous-good backup)
├── archive/                            # archived batches (after `sandman archive`)
├── batches/                            # per-batch + per-row tree
│   └── <batchID>/
│       ├── batch.json                  # batch manifest
│       ├── batch.sock                  # batch control socket
│       ├── config/                     # container config snapshot (batch-owned)
│       └── runs/<runID>/
│           ├── run.json                # row manifest (atomic-rename)
│           ├── run.log                 # O_APPEND prefixed stdout/stderr
│           ├── run.sock                # per-row control socket
│           ├── review-state.json       # review dedup state (atomic-rename)
│           └── config/                 # per-row container config snapshot
├── reviews/
│   ├── review-prompt.md                # scaffold (init only)
│   ├── quality-rules.md                # scaffold (init only)
│   └── review.sock                     # review-daemon control socket
├── worktrees/<branch>/                 # per-run worktree (git)
│   └── .sandman/task.md                # per-worktree rendered prompt
└── state/                              # runtime sidecars (NEW in this PRD)
    ├── .prompt-version                 # SHA-256 of materialized prompt template
    ├── .built_with_sandman             # empty control file (badge sidecar)
    ├── selected-issues.json            # auto-select output
    ├── <N>.head_sha                    # per-PR head SHA tracker
    └── <N>.addressed_comments          # per-PR addressed-comment list
```

## Per-artifact table

| Path | Layout method | Writer | Reader | Cleanup owner | Lifecycle |
|------|---------------|--------|--------|---------------|-----------|
| `Dockerfile` | scaffold | `sandman init` | container runtime | repo (manual) | init only |
| `config.yaml` | scaffold | `sandman init` | Sandman (config load) | repo (manual) | init only |
| `prompt.md` | scaffold | `sandman init` | prompt renderer | repo (manual) | init only |
| `auto-selection-prompt.md` | scaffold | `sandman init` | auto-select agent | repo (manual) | init only |
| `events.jsonl` | runtime, O_APPEND | orchestrator / run loop | `events.RunState` projection, portal, CLI status | never (append-only log) | continuous |
| `events.jsonl.malformed` | runtime sidecar | event writer (on torn line) | human / log triage | `sandman clean` (optional) | per torn line |
| `batches.json` | runtime, atomic-rename | orchestrator (on batch start, status change, archive) | portal, `sandman archive`, orchestrator | never (master index) | continuous |
| `batches.json.bak` | runtime sidecar | atomic-rename writer (previous-good backup) | human / disaster recovery | never (rotated on next atomic-rename) | per rewrite |
| `archive/` | runtime | `sandman archive` | portal, orchestrator (read-only) | `sandman archive` (move source), `sandman clean` (delete archived) | per archived batch |
| `archive/<batchID>/...` | runtime | `sandman archive run` / `sandman archive older-than` | portal, orchestrator (read-only after archive) | `sandman clean` | retired batch |
| `batches/<batchID>/` | runtime | orchestrator (batch start) | orchestrator, portal, attach client | orchestrator (on batch completion) | per batch |
| `batches/<batchID>/batch.json` | runtime, atomic-rename | orchestrator (batch start, status change) | orchestrator, portal | orchestrator (on batch completion, move to archive) | per batch |
| `batches/<batchID>/batch.sock` | runtime | daemon on start | attach client, orchestrator | daemon on stop | per batch |
| `batches/<batchID>/config/` | runtime, container snapshot | `PrepareContainerConfigMounts` | container runtime (bind-mount) | orchestrator (on batch completion) | per batch |
| `batches/<batchID>/runs/<runID>/` | runtime | orchestrator (per-row execution path) | orchestrator, portal | orchestrator (on run completion) | per AgentRun |
| `batches/<batchID>/runs/<runID>/run.json` | runtime, atomic-rename | orchestrator (run start, status change, finish) | orchestrator, portal, attach client | orchestrator (on run completion) | per AgentRun |
| `batches/<batchID>/runs/<runID>/run.log` | runtime, O_APPEND | `AgentRun.Execute` (prefixed stdout/stderr) | `readPortalTextFile`, attach client | never (run log is canonical per-run artefact) | per AgentRun |
| `batches/<batchID>/runs/<runID>/run.sock` | runtime | command server on per-row start | external caller (`{"action":"abort","issue":N}`) | command server on per-row completion | per AgentRun |
| `batches/<batchID>/runs/<runID>/review-state.json` | runtime, atomic-rename | review state store (dedup, claim lock) | review state store | orchestrator (on run completion) | per review AgentRun |
| `batches/<batchID>/runs/<runID>/config/` | runtime, container snapshot | `PrepareContainerConfigMounts` | container runtime (bind-mount) | orchestrator (on run completion) | per AgentRun |
| `reviews/review-prompt.md` | scaffold | `sandman init` | review daemon (prompt materialization) | repo (manual) | init only |
| `reviews/quality-rules.md` | scaffold | `sandman init` | review daemon (prompt materialization) | repo (manual) | init only |
| `reviews/review.sock` | runtime | review daemon on start | review daemon CLI | review daemon on stop | continuous |
| `worktrees/<branch>/` | runtime, git worktree | `git worktree add` (orchestrator) | agent, orchestrator | orchestrator (on run completion) | per AgentRun |
| `worktrees/<branch>/.sandman/task.md` | runtime, atomic-rename | prompt renderer (or `--continue` skips render and reads existing) | agent | orchestrator (on run completion) | per AgentRun |
| `state/.prompt-version` | runtime, atomic-rename | prompt materializer | prompt materializer (cache check) | `sandman clean` (optional) | per prompt template change |
| `state/.built_with_sandman` | runtime, empty control file | badge sidecar (post-batch) | portal / status badge | `sandman clean` (optional) | per post-batch badge |
| `state/selected-issues.json` | runtime, atomic-rename | auto-select agent output | orchestrator (consumes selection) | `sandman clean` (optional) | per auto-select run |
| `state/<N>.head_sha` | runtime, atomic-rename | review daemon (per-PR head SHA tracker) | review daemon (dedup gate) | review daemon (rotates on PR close) | per PR |
| `state/<N>.addressed_comments` | runtime, atomic-rename | review daemon (per-PR addressed-comment list) | review daemon (dedup gate) | review daemon (rotates on PR close) | per PR |

## Out-of-layout

The following artefacts do **not** live under `<repo>/.sandman/` and are documented here for completeness. They are out-of-layout by design — they are either shared across repos (skills) or out-of-repo tempdirs (config-snapshot fallback).

- **`os.MkdirTemp("", "sandman-config-*")`** (in `internal/batch/config_mounts.go:29`) — the **config-snapshot parent tempdir**, used by `PrepareContainerConfigMounts` when the caller passes an empty `runDir`. This is an out-of-repo tempdir because the agent's `ConfigDirs` / `ConfigFiles` mount resolution (ADR-0008) needs a parent to host the resolved snapshot tree when no run-owned parent is available. The directory is removed by the cleanup function returned from `prepareSnapshotParent`.
- **`os.MkdirTemp(parentDir, ".config-*")`** (in `internal/sandbox/container.go:310`) — the **config-snapshot subdir**, atomically renamed to `<parentDir>/config/` immediately after creation. When `parentDir` is a run-owned path, this lands back under `.sandman/batches/<batchID>/runs/<runID>/config/` and is on-layout; when `parentDir` is the tempdir parent above, this is the out-of-layout half of the same pair.
- **`~/.agents/skills/sandman/**`** — the **shared Sandman skill tree**, installed by `sandman init` into the user's home directory. This is repo-scoped *content* but not repo-scoped *location*: it is a single installed tree shared across every repo on the host, so it is intentionally out of the per-repo `.sandman/` layout. See `CONTEXT.md` §Sandman Skill.

## Migration note

Existing repos that predate this layout carry legacy files at the old paths and must move them manually per the CHANGELOG. This is not an automated migration — the rename is one-way and operator-driven because the source paths and target paths both exist on disk during the transition window.

At minimum, the following legacy paths must be moved (or removed if already retired):

- `.sandman/reviews/self-posted.json` — removed by #1848 (S5: SelfPostStore deletion). Repos that still carry this file from before #1848 must delete it; there is no successor location.
- `.sandman/reviews/self-posted.json.ignore-*.bak` rotations — removed alongside `self-posted.json` by #1848. Repos that still carry these `.bak` rotations must delete them.
- `.sandman/.prompt-version` — moved to `.sandman/state/.prompt-version` by slice 5 (#1865). Repos upgrading across slice 5 must `mv .sandman/.prompt-version .sandman/state/.prompt-version` (creating `state/` if needed).
- `.sandman/.built_with_sandman` — moved to `.sandman/state/.built_with_sandman` by slice 6 (#1866).
- `.sandman/selected-issues.json` — moved to `.sandman/state/selected-issues.json` by slice 6 (#1866).
- `.sandman/<N>.head_sha` and `.sandman/<N>.addressed_comments` — moved to `.sandman/state/<N>.head_sha` and `.sandman/state/<N>.addressed_comments` by slice 7 (#1867).

After the move, `git status` must show no `.sandman/` entries — slice 7's gitignore update covers the new `state/` subdir, so the moved files are untracked and never committed.

See `CHANGELOG.md` for the slice-by-slice migration timeline and any later moves.
