# Disk layout

## Intro

Every persisted Sandman artifact lives under `<repo>/.sandman/` (with two documented exceptions: out-of-repo tempdirs used as fallback parents for config snapshots, and the shared `~/.agents/skills/sandman/**` skill tree installed into the user's home).

## Canonical tree

```
.sandman/
├── Dockerfile                          # scaffold (init only)
├── config.yaml                         # scaffold (init only)
├── prompt.md                           # scaffold (init only)
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
    ├── <N>.head_sha                    # per-PR head SHA tracker
    └── <N>.addressed_comments          # per-PR addressed-comment list
```

## Per-artifact table

| Path | Layout method | Writer | Reader | Cleanup owner | Lifecycle |
|------|---------------|--------|--------|---------------|-----------|
| `Dockerfile` | scaffold | `sandman init` | container runtime | repo (manual) | init only |
| `config.yaml` | scaffold | `sandman init` | Sandman (config load) | repo (manual) | init only |
| `prompt.md` | scaffold | `sandman init` | prompt renderer | repo (manual) | init only |
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
| `state/<N>.head_sha` | runtime, atomic-rename | review daemon (per-PR head SHA tracker) | review daemon (dedup gate) | review daemon (rotates on PR close) | per PR |
| `state/<N>.addressed_comments` | runtime, atomic-rename | review daemon (per-PR addressed-comment list) | review daemon (dedup gate) | review daemon (rotates on PR close) | per PR |

## Out-of-layout

The following artefacts do **not** live under `<repo>/.sandman/` and are documented here for completeness. They are out-of-layout by design — they are either shared across repos (skills) or out-of-repo tempdirs (config-snapshot fallback).

- **`os.MkdirTemp("", "sandman-config-*")`** (in `internal/batch/config_mounts.go:29`) — the **config-snapshot parent tempdir**, used by `PrepareContainerConfigMounts` when the caller passes an empty `runDir`. This is an out-of-repo tempdir because the agent's `ConfigDirs` / `ConfigFiles` mount resolution needs a parent to host the resolved snapshot tree when no run-owned parent is available. The directory is removed by the cleanup function returned from `prepareSnapshotParent`.
- **`os.MkdirTemp(parentDir, ".config-*")`** (in `internal/sandbox/container.go:310`) — the **config-snapshot subdir**, atomically renamed to `<parentDir>/config/` immediately after creation. When `parentDir` is a run-owned path, this lands back under `.sandman/batches/<batchID>/runs/<runID>/config/` and is on-layout; when `parentDir` is the tempdir parent above, this is the out-of-layout half of the same pair.
- **`~/.agents/skills/sandman/**`** — the **shared Sandman skill tree**, installed by `sandman init` into the user's home directory. This is repo-scoped *content* but not repo-scoped *location*: it is a single installed tree shared across every repo on the host, so it is intentionally out of the per-repo `.sandman/` layout. See `CONTEXT.md` §Sandman Skill.

## Upgrades

Sandman does not migrate on-disk state across version upgrades. After upgrading, clear `.sandman/` and re-run `sandman init`. See [Troubleshooting](../help/troubleshooting.md#portal-shows-unknown-rows-after-upgrading-sandman) for the symptom and fix.
