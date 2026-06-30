# Sandman

Domain vocabulary for Sandman, a CLI tool that orchestrates AFK coding agents in isolated sandboxes.

## Language

**BlockedBy**:
The set of issue numbers that must complete successfully before an AgentRun for this issue can start. Derived from the union of body references and GitHub native dependency fields. An external blocker (not in the current batch) must still be closed on GitHub immediately before start time. An in-batch blocker only needs to reach status `success` within the batch — its GitHub issue may still be open at that instant.
_Avoid_: dependencies, prerequisites.

**In-batch blocker**:
A blocker that is itself a member of the current Batch. Its terminal batch status (`success`, `failure`, `aborted`, or `blocked`) is the single source of truth for whether the dependent may start; the corresponding GitHub issue's `state` is not consulted.
_Avoid_: local blocker, sibling blocker.

**External blocker**:
A blocker named in an AgentRun's BlockedBy that is not a member of the current Batch. The dependent may only start once GitHub reports the external blocker's issue as `closed` at the instant just before start time.
_Avoid_: outside blocker, third-party blocker.

**Agent**:
An external AI coding tool (OpenCode) invoked by Sandman via `os/exec`. Sandman does not contain the agent; it renders a command template and executes it.
_Avoid_: AI model, LLM, copilot.

**AgentPreset**:
A built-in command, config source, and auth profile for a known AI coding tool keyed by name (currently `opencode`). Declared in `config.BuiltInAgentPresets` and resolved by `config.ResolveAgentProvider`.
_Avoid_: Provider template, agent type.

**Agent Provider**:
A configured agent preset or custom provider definition. Sandman supports the built-in preset (`opencode`) and optional repo-local custom providers via the `agents` config map.
_Avoid_: Agent type, runner.

**AgentModel**:
A built-in agent model identifier overridden via `sandman run --model`.
_Avoid_: agent model, default model.

**AgentRun**:
One execution of an agent against one issue, producing commits on a branch. The unit of work within a batch.
_Avoid_: Run, job, task.

**Prompt-only run**:
A Batch execution that runs without fetching a GitHub Issue. Also called a no-issue run. Prompt-only runs use a `sandman/<slug>-<timestamp>` branch name and carry a null issue in events and result output; human-facing summaries label them `prompt-only`.
_Avoid_: synthetic issue run.

**DependencyResolver**:
The component that fetches issues, extracts their BlockedBy relationships, validates the dependency graph (detecting cycles and missing blockers), and produces a topologically sorted ResolvedBatch.
_Avoid_: scheduler, planner.

**PRD** (Product Requirements Document):
A GitHub issue whose body contains the H2 sections `## Problem Statement`, `## Solution`, and `## User Stories`. A PRD is a container for a set of vertical slices; Sandman recognizes it structurally and resolves it into its child issues before execution. Detection is body-based, not label-based.
_Avoid_: spec, epic, umbrella issue.

**PRDResolver**:
The component that detects PRD issues, discovers their child issues (from the body, comments, and a fallback mention search), verifies each candidate by its `## Parent` backlink, and replaces the PRD with its accepted children in the input batch. Runs in `sandman run` after issue selection and before `DependencyResolver.Resolve`. A PRD with no accepted children fails the resolution; a harvested child that is itself a PRD (nested PRD) also fails. Candidates that are also in the user-typed input list bypass the `## Parent` and nested-PRD checks (the user owns the choice), as documented in ADR-0025 §3a. User-typed numbers are otherwise accepted unconditionally, except when the number is itself a PRD, in which case the resolver still runs its own expansion pass on it.
_Avoid_: PRD expander, PRD flattener.

**`## Parent` backlink**:
A H2 section in an issue body of the form `## Parent` followed by either `#N` shorthand or a full GitHub issue URL (`https://github.com/<owner>/<repo>/issues/N`). A candidate child of PRD `#N` is accepted only when its `## Parent` section cites `#N`.
_Avoid_: parent reference, parent link, parent header.

**Batch**:
The folder `.sandman/batches/<batch-id>/` plus all child run folders. Contains the daemon sockets (`batch.sock`, `run.sock`), the `batch.json` manifest, and the `config/` host snapshot at the batch root. Each run folder (`.sandman/batches/<batch-id>/runs/<run-id>/`) holds its own `run.json`, `run.log`, and per-run command socket. One daemon process, one `batch.json`. References ADR-0032.
_Avoid_: Batch run, invocation.

**Batches index**:
The master list at `.sandman/batches.json` recording every batch ever created with its on-disk path frozen at write time. Entries carry `status` (`active` / `archived` / `unavailable`), `kind`, `issues`, and lifecycle timestamps. Atomic write: write to `.sandman/batches.json.tmp`, then `os.Rename`. The portal reads the index first and only probes the filesystem for live-socket state. Renaming `.sandman/` or `batches/` requires only a batched index rewrite — no code changes. References ADR-0032.
_Avoid_: index, master index.

**Run**:
One folder under `.sandman/batches/<batch-id>/runs/<run-id>/` containing `run.json`, `run.log`, `run.sock`, and (for review runs) `review-state.json`. Identified by the per-row RunID produced by `runid.NewRunID`. Each Run represents a single AgentRun within a Batch. References ADR-0032.
_Avoid_: run folder, run directory.

**Review daemon state**:
Flat files under `.sandman/reviews/` for daemon-level state: `review.sock` (daemon command socket) and `review-prompt.md` (shared prompt template). No per-PR subdirectories. Per-run review state (`review-state.json` with seen comments and claim locks) lives inside the batch run folder at `.sandman/batches/<batch-id>/runs/<run-id>/review-state.json`. Dedup key is `(prNumber, commentID)`. The daemon additionally maintains a per-process in-memory `seenCache` (`prNumber → map[commentID]bool`) hydrated at construction from the on-disk batches index and refreshed on every successful `ReviewStateStore.MarkSeen` (status ∈ `{success, superseded}`); the cache is advisory and must be invalidated on every `MarkSeen` so the per-tick scan can short-circuit without re-reading `.sandman/batches.json` or any `review-state.json`. Under the lazy-verify contract (issue #1482 slice D), the daemon adds a third per-process in-memory structure, `pendingReviews` (`prNumber → []pendingReviewEntry`), which holds triggers that have been launched but whose agent-posted review comment has not yet been observed. Each tick's `promotePendingReviews` step walks this map against `ListPRComments`, promotes the entry to `success` when the review comment is found, or to `failure` after `pendingMaxCycles` (default 3) cycles without observation. The `review-state.json` `seenComments` list therefore gains a new `pending` status (NOT in `shouldSkipDedupStatus` — it remains retryable from the in-process pending map's perspective) that promotes to `success` or `failure` via `promotePendingReviews` on subsequent ticks. References ADR-0034 and issues #1480 (slice A), #1479 (slice B), #1482 (slice D).
_Avoid_: review state, PR state.

**Review daemon tick**:
One scan cycle of the review daemon's polling loop. A tick acquires the `busy` semaphore (buffer size 1, unconditional) before scanning open PRs; if already held, the tick returns immediately ("previous tick still running, skipping"). This `busy=1` invariant means at most one tick runs at a time regardless of `parallel_reviews`. The inner `sem` channel (sized to `EffectiveReviewParallel()`) still allows a single tick to launch reviews for multiple PRs concurrently. Within a tick, `processPR` uses `ReviewStateStore.TryClaim` to acquire an in-memory claim lock before adding reactions or calling `launchReview`; the claim is in-memory only and is released if `launchReview` fails before the state is written, allowing retry on the next tick. As the first step of every tick (after acquiring `busy`, before `ListOpenPRs`) the daemon runs `promotePendingReviews` which walks the in-memory `pendingReviews` map for triggers that were launched by a prior tick but whose agent-posted review comment has not yet been observed; the entries are promoted to `success` or `failure` per ADR-0034 §Verify off the critical path. References ADR-0034 §Cross-tick claim lock and §Verify off the critical path.
_Avoid_: scan cycle, polling loop.

**Branch**:
A git branch named `sandman/<issue-number>-<slugified-title>` for issue-driven AgentRuns, or `sandman/<slug>-<timestamp>` for prompt-only runs.
_Avoid_: Feature branch, PR branch.

**SidecarBranch**:
A git branch initiated by Sandman's own post-batch side effects rather than by a user-supplied issue or prompt. The badge branch `sandman/built-with-sandman` is the first such SidecarBranch. Unlike issue-driven branches (which carry a GitHub issue) or prompt-only branches (which carry a timestamp suffix for uniqueness), a SidecarBranch carries no issue and no timestamp — its shape is determined by the sidecar that created it.
_Avoid_: Bot branch, auto branch.

**BuildToolsPreset**:
A scaffold-time recipe chosen during `sandman init` that seeds a pinned container image definition with shared baseline tools and built-in agent installation defaults.
_Avoid_: language, stack, base image.

**ConfigDirs**:
Directories resolved into a container sandbox via a temporary copy for agent configuration. Before mounting, Sandman creates a batch-scoped copy of each directory with symlinks resolved (following ADR-0008). Paths starting with `~` are expanded to the user's home directory. Missing directories are silently skipped.
_Avoid_: mounted config directories.

**ConfigFiles**:
Individual files resolved into a container sandbox via a temporary copy for agent configuration. Before mounting, Sandman creates a batch-scoped copy of each file with symlinks resolved (following ADR-0008). Paths starting with `~` are expanded to the user's home directory. Missing files are silently skipped.
_Avoid_: config paths, settings files.

**SnapshotExcludes**:
Paths (typically subtrees of a `ConfigDir`) skipped during the copy-resolve snapshot step (ADR-0008/ADR-0015). Used to keep large mutable runtime state out of the run-owned snapshot (ADR-0016). Paths starting with `~` are expanded.
_Avoid_: snapshot blocklist, copy excludes.

**LiveMount**:
A path bind-mounted directly from the host into a ContainerSandbox at its corresponding HOME=/ container path, instead of being copied into the snapshot. Used when host-side state must remain inspectable after the container run completes — e.g. OpenCode's `opencode.db` session database (ADR-0016). A LiveMount is implicitly a `SnapshotExclude` of the same path.
_Avoid_: bind mount (too generic), shared mount.

**ContainerSandbox**:
A Docker or Podman container providing filesystem and process isolation for one or more AgentRuns. A Batch may scale a pool of ContainerSandboxes up or down within configured limits.
_Avoid_: Docker container, sandbox container.

**ContainerCapacity**:
The maximum number of AgentRuns that may execute concurrently inside one ContainerSandbox. `0` means unlimited: any number of AgentRuns may execute inside one ContainerSandbox. When capacity is reached (or when containers are full), additional AgentRuns wait for another container or for capacity to free up.
_Avoid_: shared mode, isolated mode.

**StartDelay**:
A batch pacing delay in seconds. After any AgentRun finishes, the Batch waits this long before starting the next AgentRun. `0` disables pacing.
_Avoid_: cooldown, throttle.

**MaxContainers**:
The maximum number of ContainerSandboxes Sandman may create for one Batch. `max_containers=0` means auto mode: create up to the minimum number of containers needed for the currently active AgentRuns, based on ContainerCapacity.
_Avoid_: isolated container toggle, fixed pool size.

**Event**:
A single structured log entry in the append-only JSONL event log (`.sandman/events.jsonl`). Examples: `run.started`, `run.continued`, `run.queued`, `run.blocked`, `run.warning`, `run.finished`, `run.aborted`. A `run.queued` event is emitted when an issue enters the wait queue due to having unresolved blockers or due to parallel capacity constraints (i.e., when effective parallelism is less than the total number of issues in the batch).
_Avoid_: Log line, record.

**Aborted**:
A first-class terminal AgentRun status indicating the run was interrupted by context cancellation (e.g. SIGINT/SIGTERM) before it could finish on its own merits. Emitted as a `run.aborted` event with `status: aborted`. `RunState.Status()` returns `"aborted"` for any `run.aborted` event and, for backwards compatibility, for legacy `run.cancelled` events in older `events.jsonl` files.
_Avoid_: cancelled, killed, terminated.

**Issue**:
A GitHub issue fetched via `gh` CLI. The unit of work delegated to an agent.
_Avoid_: Ticket, story.

**Prompt**:
The instruction content passed to an Agent after template rendering. See `Task` for the on-disk file used for one AgentRun.
_Avoid_: Instructions, query.

**Task**:
The generated instruction file (`.sandman/task.md`) passed to an Agent for one AgentRun. It contains issue metadata, plan output, and the execution checklist. It replaces `rendered-prompt.md` and subsumes the handoff document's role.
_Avoid_: Prompt file, handoff doc.

**Default Task Prompt**:
The canonical bootstrap prompt template embedded in Sandman at `internal/prompt/default-task-prompt.md`.
_Avoid_: Base prompt, stock prompt.

**Sandman Skill**:
The shared skill folder installed by `sandman init` into `~/.agents/skills/sandman/` and used by Sandman agents for the full plan/implement/review/merge/continue flow.
_Avoid_: Prompt workflow, local prompt copy.

**Project Prompt Template**:
The repo-local `.sandman/prompt.md` template created from the Default Task Prompt by `sandman init` and materialized on run when missing.
_Avoid_: User prompt, custom prompt.

**Prompt keys**:
The built-in substitution keys available in prompt templates: `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, `{{ISSUE_BODY}}`, `{{SOURCE_BRANCH}}`, `{{BASE_BRANCH}}`, `{{BRANCH}}`, `{{REVIEW_COMMAND}}`. Custom keys are supported via the `--prompt-arg KEY=VALUE` CLI flag.

**Command template key**:
The substitution keys available in agent command templates: `{{.PromptFile}}` (relative path of `.sandman/task.md`) and `{{.SessionName}}` (pre-formatted session display title supplied by the caller, e.g. `"Sandman run-42-1712345678901: "`). `SessionName` must not contain single quotes — the template shells it as `--title '{{.SessionName}}'` and the renderer rejects any value containing `'` with an error. Templates that reference `{{.SessionName}}` should guard the substitution with `{{if .SessionName}}` to avoid emitting a bare `--title ''` when the field is empty.

**ResolvedBatch**:
A batch where all issues have been fetched, their BlockedBy relationships resolved, and the execution order topologically sorted. Ready for the Orchestrator.
_Avoid_: planned batch, execution plan.

**RunID**:
A string that uniquely identifies one AgentRun within a batch. Persisted in the `RunID` field of every event in `events.jsonl` and used as the row key in the portal. Generated by `internal/runid.NewRunID` using the per-batch `<shortid>-<ts>` prefix and a per-row subject suffix. References ADR-0030.
_Avoid_: run identifier.

**Sandbox**:
The abstract isolation mechanism within which an AgentRun executes. Hides whether isolation is provided by a worktree or by a container-backed sandbox strategy.
_Avoid_: Environment, boundary, boundary context.

**Sandbox.Exec contract (Setpgid invariant)**:
The `Sandbox.Exec` method requires the spawned OS command to be its own process-group leader. Any new `Sandbox` implementation must set `Setpgid: true` on the spawned command, so that the shared `waitCmd` helper's `syscall.Kill(-cmd.Process.Pid, …)` lands on the right group on context cancel. Without this, the cancel silently no-ops and `cmd.Wait()` blocks forever, surfacing as a "no-op" abort in the portal. This invariant prevents a bug where an in-flight AgentRun in container mode stays `active` after the user clicks Abort, because the `kill(-PID, SIGKILL)` in `waitCmd` targets a non-existent PGID and returns `ESRCH`. A separate follow-up issue (#1113) addresses the remaining problem that killing the host-side `docker exec` / `podman exec` wrapper does not propagate to the in-container AgentRun.

**Worktree**:
A git worktree under `.sandman/worktrees/`, created per AgentRun, providing a dedicated checkout for the agent to modify. Not a sandbox on its own; a sandbox may contain one or more worktrees.
_Avoid_: Working directory, checkout, clone.

**WorktreeSandbox**:
A sandbox adapter that uses only a git worktree for isolation, with no container. One worktree per AgentRun.
_Avoid_: Local sandbox.

**Stranded worktree**:
A sandman-managed git worktree whose HEAD points to a different branch than its directory name expects. Stranded worktrees can result from interrupted runs. `sandman run --override` and `sandman run --continue` (on a fresh issue with no prior run) auto-recover from stranded worktrees by default (ADR-0027), including the case where the main repo itself is checked out on a `sandman/N-…` branch; pass `--no-reconcile-stranded` to opt out. A *prunable* worktree is a related but distinct state: the worktree registration exists in `git worktree list` but the `.git` gitlink points to a non-existent directory, making git consider it prunable. Prunable worktrees are also auto-recovered on `--continue` (ADR-0027 strategy 0): the stale registration is pruned and the existing directory is re-registered without deleting its contents. `scripts/reconcile-stranded-worktrees.sh` provides a standalone detection tool that prints remediation commands for the operator to run.
_Avoid_: Orphaned worktree, lost worktree.
_See_: Branch, Worktree.

**Archive**:
The on-disk resting place for completed batch directories at `.sandman/archive/<batch-id>/`, populated by `sandman archive run <batch-id>` or by `sandman archive older-than <days>` for bulk archival of every dead batch whose manifest `CreatedAt` (or directory mtime when the manifest is missing) is older than the given cutoff. Archiving relocates the batch directory tree from `.sandman/batches/<batch-id>/` (its live-and-during-run home) to `.sandman/archive/<batch-id>/` so the batches directory stays scoped to currently-relevant batches. The daemon is forbidden from writing to an archived batch; the batch is treated as read-only historical state once moved. References ADR-0032.
_Avoid_: trash, graveyard, old runs, retired runs.

**Daemon Process**:
A long-lived sandman process executing a Batch in the background. Listens on the control socket.
_Avoid_: Background job, server.

**Control Socket**:
A Unix domain socket at `<batch>/batch.sock` that accepts attach client connections. Created when a daemon starts, removed when it stops.
_Avoid_: IPC socket, management socket.

**Saved Run Log**:
The persisted twin of the live attach stream, written to `.sandman/batches/<batch-id>/runs/<run-id>/run.log` for each AgentRun. Each line carries the same `[<runID>] HH:MM:SS ` prefix as the live stream, where `<runID>` is the per-run identifier produced by `runid.NewRunID`. The file is opened with `O_APPEND` during `AgentRun.Execute` and is read by `readPortalTextFile` in the portal; it is never truncated mid-run. Pre-change log files may contain un-prefixed lines or the older `[issue-<N>]`/`[prompt-only]` labels. References ADR-0032.
_Avoid_: Log file, run log.

**Command Server**:
A Unix domain socket at `<batch>/runs/<runID>/run.sock` that accepts one-shot JSON command requests from outside the daemon process. First supported command is `{"action":"abort","issue":<n>}`, dispatched to the orchestrator's per-issue cancel API. Created per-AgentRun when the orchestrator's per-row execution path starts, removed when the run completes. Distinct from the **Control Socket**, which streams daemon output to **Attach** clients.
_Avoid_: management socket, IPC socket.

**Issue Commander**:
The seam the command server uses to cancel a single in-flight AgentRun without affecting siblings. The Orchestrator implements this interface so an external caller can address one issue at a time even when the batch is still running.
_Avoid_: per-issue abort handle (that's the operation, not the seam).

**Attach**:
Connect a terminal to a running daemon via the control socket to stream its output. Invoked via `sandman attach`.
_Avoid_: Tail, follow.

**Portal**:
A repo-scoped local HTTP dashboard started by `sandman portal` that rescans the current repository's `.sandman/batches/` tree on each poll and shows active and recent Sandman instances. It does not manage daemon lifecycle. (Note: the preset launcher was removed in #1204.)
_Avoid_: dashboard, monitor, control panel.

**Reviewing**:
The in-flight portal status for an active review run (a run whose `run.started` event carried `payload.review = true`). Displayed in the status badge as `● reviewing`. The `Status` field is set to `"reviewing"` by `statusOrDefault` when `active && isReview` is true. Terminal review runs use `success`, `failure`, or `aborted` like any other run.
_Avoid_: reviewing status, review-in-progress. No secondary-row review chip.

**Auto-selecting**:
The in-flight portal status for an active auto-select run (a run whose `run.started` event carried `payload.run_kind = "auto-select"`). Displayed in the status badge as `● auto-selecting`. The `Status` field is set to `"auto-selecting"` by `statusOrDefault` when `active && isAutoSelect` is true. Terminal auto-select runs use `success`, `failure`, or `aborted` like any other run.
_Avoid_: selecting, auto-select status.

**Candidates**:
The list of issue numbers considered by the auto-select agent during the selection phase. Emitted in `run.started` as `payload.candidates` and projected into `portalRun.Candidates`. The portal renders this as a secondary-row chip (`Auto-select candidates: #N, #N`) for active and terminal auto-select runs. The final *selected* subset is emitted in `run.finished.payload.selected` and surfaced in the agent log, not in the chip.
_Avoid_: candidate list, selection candidates.

**Continue**:
The `--continue` flag on `sandman run` re-runs the latest AgentRun for one or more issues while reusing each issue's prior branch, base branch, agent, and review command. Continuation reads the existing `.sandman/task.md` directly rather than rendering a fresh prompt. Multi-issue `sandman run --continue <issue>...` submits a single Batch with per-issue `Branches`, `BaseBranch`, and `PreviousRunIDs` maps so the orchestrator parallelizes across issues. `--continue` keeps branch checkout unchanged, resolves the model from `--model` or `model`, and uses the stored base branch for prompt rendering and event metadata only. Per-issue prompt rendering is built on top of this surface by #443. When `.sandman/task.md` is present in the worktree, the resumed run consumes it instead of starting from a blank prompt.
_Avoid_: Retry.

**Continuation**:
An AgentRun or Batch request mode behind the `sandman run --continue` flag that skips prompt template rendering and reads raw prompt text from `.sandman/task.md` inside the worktree.
_Avoid_: Replay mode.

**Override**:
The `--override` flag on `sandman run`. Clears prior run artifacts before starting by deleting the existing worktree, logs, and events, then force-checks out the expected branch when the current checkout is mismatched or detached. Use it to reconcile stranded worktrees before a fresh run.
_Avoid_: Clean reset, manual checkout.

**Handoff**:
Deprecated: use `Task` (`.sandman/task.md`) instead. The task checklist now carries the checkpointed workflow state that used to live in the separate handoff file.
_Avoid_: Continuation context (legacy filename), continuation file.

## Relationships

- A **Daemon Process** is created by `sandman run`, which starts a **Control Socket**
- A **Control Socket** at `<batch>/batch.sock` accepts **Attach** connections for the duration of the **Batch**
- A **Daemon Process** also starts a **Command Server** at `<batch>/runs/<runID>/run.sock` for surgical per-issue control requests; the **Command Server** is created per-AgentRun in the orchestrator's per-row execution path
- An **Issue Commander** is the orchestrator seam the **Command Server** dispatches to; it maps an external cancel to a single **AgentRun**
- A **Daemon Process** stops the **Control Socket** when its **Batch** completes; per-row **Command Server** instances are stopped when each **AgentRun** completes
- An **Attach** client connects to the **Control Socket** and reads the daemon's output until EOF
- A **Portal** is repo-scoped and can show multiple **Daemon Process** instances from the same repository at once
- A **Portal** rescans the current repository's `.sandman/batches/` tree on each poll so newly started **Daemon Process** instances appear without restarting the portal
- An **Archive** entry under `.sandman/archive/<batch-id>/` is the relocated home of a batch directory whose **Daemon Process** is no longer live; `sandman archive run <batch-id>` performs the move after the liveness check fails
- The batch directory (`.sandman/batches/<batch-id>/`) is the **Daemon Process**'s home; once it has been moved to `.sandman/archive/<batch-id>/` the directory is no longer owned by any **Daemon Process**
- A **RunID** is generated by `NewRunID` for each **AgentRun** within a **Batch** and is persisted as the row key in the portal and in `events.jsonl`
- A **Batches index** at `.sandman/batches.json` is the master list; the **Portal** reads it first and only probes the filesystem for live-socket state

- A **Batch** contains zero or more **AgentRuns**
- An **AgentRun** targets exactly one **Issue** and produces exactly one **Branch**
- An **Issue** may have **BlockedBy** relationships to other **Issues**
- A **DependencyResolver** produces a **ResolvedBatch** from a set of **Issues**
- An **Orchestrator** executes a **ResolvedBatch**, respecting **BlockedBy** ordering
- A **PRDResolver** runs before a **DependencyResolver**, replacing any **PRD** in the input with its child **Issues** so the orchestrator never sees the PRD itself
- An **AgentRun** may be **blocked** if any of its in-batch **BlockedBy** issues did not finish with status `success`, or if any of its external **BlockedBy** issues is still open on GitHub when the run is about to start
- A **Sandbox** provides isolation for one or more **AgentRuns**
- In `sandbox: worktree`, each **AgentRun** gets its own **Sandbox** (a **WorktreeSandbox**)
- In a container-backed sandbox strategy, each **ContainerSandbox** may host up to **ContainerCapacity** **AgentRuns** at once
- A **Batch** may create multiple **ContainerSandboxes**, up to **MaxContainers**, to satisfy concurrent **AgentRuns**
- If all containers are full and the **Batch** is already at **MaxContainers**, additional **AgentRuns** wait in a queue until capacity becomes available
- If `max_containers=0`, Sandman auto-scales the container pool up to the minimum number of containers needed for active **AgentRuns**
- Container pooling is batch-scoped: idle **ContainerSandboxes** may be reused by later **AgentRuns** in the same **Batch**, and are stopped when that **Batch** completes
- A **Batch** may apply **StartDelay** pacing between **AgentRun** starts; the delay is batch-local and does not change container capacity
- An **AgentRun** generates many **Events**
- A **Prompt** is rendered per **AgentRun** from the selected built-in **AgentPreset**
- A **Prompt** is rendered per **AgentRun** from the selected built-in **AgentPreset**, and the shared **Sandman Skill** provides the rest of the workflow guidance

## Example dialogue

> **Dev:** "When a **Batch** contains three **AgentRuns**, do they all share one **Sandbox**?"
> **Domain expert:** "Not necessarily. With `sandbox: worktree`, each **AgentRun** gets its own **WorktreeSandbox**. With container-backed sandboxing, Sandman packs runs into **ContainerSandboxes** up to **ContainerCapacity**, then creates more containers up to **MaxContainers**. If `max_containers=0`, the pool auto-scales to the minimum number of containers needed for the active **AgentRuns**."

> **Dev:** "What happens if the batch has more runnable work than the container pool can hold right now?"
> **Domain expert:** "Extra **AgentRuns** wait in a queue. Idle containers can be reused later in the same **Batch**, but Sandman stops them when the **Batch** finishes. Cross-batch warm-container reuse is out of scope for this model."

> **Dev:** "Does the **Sandbox** interface expose the worktree path?"
> **Domain expert:** "Yes — the **Sandbox** contract returns a working directory path, but callers must not assume it is a git worktree. That detail belongs to the adapter."

## Flagged ambiguities

- "run" was used to mean both the CLI command (`sandman run`) and a single agent execution. Resolved: the CLI command triggers a **Batch**; each execution is an **AgentRun**.
- "sandbox" was used interchangeably with "worktree" and "container." Resolved: **Sandbox** is the abstract isolation contract; **WorktreeSandbox** and **ContainerSandbox** are the concrete adapters. A **Worktree** is a git concept — a dedicated checkout that lives inside a sandbox.
- "language" was used to mean both repo detection and scaffold recipe choice. Resolved: **BuildToolsPreset** is the scaffold-time recipe term; avoid "language" for that choice.
- "running process" was used to mean both a **Daemon Process** (background sandman) and an **AgentRun** (agent execution). Resolved: **Daemon Process** is the long-lived sandman process; an **AgentRun** is a single agent execution within a batch.
