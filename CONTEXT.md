# Sandman

Domain vocabulary for Sandman, a terminal-native CLI tool that orchestrates AFK coding agents in isolated sandboxes.

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
An external AI coding tool (OpenCode or Pi) invoked by Sandman via `os/exec`. Sandman does not contain the agent; it renders a command template and executes it.
_Avoid_: AI model, LLM, copilot.

**AgentPreset**:
A built-in command, config source, and auth profile for a known AI coding tool keyed by name (opencode, pi). Declared in `config.BuiltInAgentPresets` and resolved by `config.ResolveAgentProvider`.
_Avoid_: Provider template, agent type.

**Agent Provider**:
A configured agent preset or custom provider definition. Sandman supports built-in presets (`opencode`, `pi`) and optional repo-local custom providers via the `agents` config map.
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

**Batch**:
The set of AgentRuns triggered by a single `sandman run` invocation. Coordinated for parallel execution with a concurrency limit; prompt-only runs may execute without issue lookup and may emit null-issue run entries.
_Avoid_: Batch run, invocation.

**Branch**:
A git branch named `sandman/<issue-number>-<slugified-title>` for issue-driven AgentRuns, or `sandman/<slug>-<timestamp>` for prompt-only runs.
_Avoid_: Feature branch, PR branch.

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
Paths (typically subtrees of a `ConfigDir`) skipped during the copy-resolve snapshot step (ADR-0008/ADR-0015). Used to keep large mutable runtime state out of the run-owned snapshot (ADR-0016/ADR-0017). Paths starting with `~` are expanded.
_Avoid_: snapshot blocklist, copy excludes.

**LiveMount**:
A path bind-mounted directly from the host into a ContainerSandbox at its corresponding HOME=/ container path, instead of being copied into the snapshot. Used when host-side state must remain inspectable after the container run completes — e.g. OpenCode's `opencode.db` session database (ADR-0016) or Pi's `~/.pi/agent/{npm,sessions}` (ADR-0017). A LiveMount is implicitly a `SnapshotExclude` of the same path.
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
The generated instruction file passed to an Agent, rendered from a template with issue metadata and built-in substitutions.
_Avoid_: Instructions, query.

**Default Prompt**:
The canonical bootstrap prompt template embedded in Sandman at `internal/prompt/default_prompt.md`.
_Avoid_: Base prompt, stock prompt.

**Sandman Skill**:
The shared skill folder installed by `sandman init` into `~/.agents/skills/sandman/` and used by Sandman agents for the full plan/implement/review/merge/continuation flow.
_Avoid_: Prompt workflow, local prompt copy.

**Project Prompt Template**:
The repo-local `.sandman/prompt.md` template created from the Default Prompt by `sandman init` and materialized on run when missing.
_Avoid_: User prompt, custom prompt.

**Prompt keys**:
The built-in substitution keys available in prompt templates: `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, `{{ISSUE_BODY}}`, `{{SOURCE_BRANCH}}`, `{{BASE_BRANCH}}`, `{{BRANCH}}`, `{{REVIEW_COMMAND}}`. Custom keys are supported via the `--prompt-arg KEY=VALUE` CLI flag.

**Command template key**:
The `{{.PromptFile}}` key available in agent command templates, resolved to the relative path of the rendered prompt file.

**Portal launcher**:
A repo-scoped browser launcher in `sandman portal` that can start Sandman commands from typed presets while observing live runs in the current repository.
_Avoid_: run dashboard only.

**ResolvedBatch**:
A batch where all issues have been fetched, their BlockedBy relationships resolved, and the execution order topologically sorted. Ready for the Orchestrator.
_Avoid_: planned batch, execution plan.

**Sandbox**:
The abstract isolation mechanism within which an AgentRun executes. Hides whether isolation is provided by a worktree or by a container-backed sandbox strategy.
_Avoid_: Environment, boundary, boundary context.

**Worktree**:
A git worktree under `.sandman/worktrees/`, created per AgentRun, providing a dedicated checkout for the agent to modify. Not a sandbox on its own; a sandbox may contain one or more worktrees.
_Avoid_: Working directory, checkout, clone.

**WorktreeSandbox**:
A sandbox adapter that uses only a git worktree for isolation, with no container. One worktree per AgentRun.
_Avoid_: Local sandbox.

**Stranded worktree**:
A sandman-managed git worktree whose HEAD points to a different branch than its directory name expects. Stranded worktrees can result from interrupted runs. `sandman run --force` reconciles them automatically; `scripts/reconcile-stranded-worktrees.sh` provides a standalone detection tool that prints remediation commands for the operator to run.
_Avoid_: Orphaned worktree, lost worktree.
_See_: Branch, Worktree.

**Archive**:
The on-disk resting place for completed run directories at `.sandman/archive/<run-id>`, populated by `sandman archive run <run-id>` (which also accepts `batch <id>` as an alias since the run directory is the batch's on-disk home) or by `sandman archive older-than <days>` for bulk archival of every dead run whose manifest `CreatedAt` (or directory mtime when the manifest is missing) is older than the given cutoff. Archiving relocates the run directory tree from `.sandman/runs/<run-id>` (its live-and-during-run home) to `.sandman/archive/<run-id>` so the runs directory stays scoped to currently-relevant batches. The daemon is forbidden from writing to an archived run; the run is treated as read-only historical state once moved.
_Avoid_: trash, graveyard, old runs, retired runs.

**Daemon Process**:
A long-lived sandman process executing a Batch in the background. Listens on the control socket.
_Avoid_: Background job, server.

**Control Socket**:
A Unix domain socket at `.sandman/runs/<run-id>/run.sock` that accepts attach client connections. Created when a daemon starts, removed when it stops.
_Avoid_: IPC socket, management socket.

**Command Server**:
A Unix domain socket at `.sandman/runs/<run-id>/cmd.sock` that accepts one-shot JSON command requests from outside the daemon process. First supported command is `{"action":"abort","issue":<n>}`, dispatched to the orchestrator's per-issue cancel API. Created when a daemon starts, removed when it stops. Distinct from the **Control Socket**, which streams daemon output to **Attach** clients.
_Avoid_: management socket, IPC socket.

**Issue Commander**:
The seam the command server uses to cancel a single in-flight AgentRun without affecting siblings. The Orchestrator implements this interface so an external caller can address one issue at a time even when the batch is still running.
_Avoid_: per-issue abort handle (that's the operation, not the seam).

**Attach**:
Connect a terminal to a running daemon via the control socket to stream its output. Invoked via `sandman attach`.
_Avoid_: Tail, follow.

**Portal**:
A repo-scoped local HTTP dashboard started by `sandman portal` that rescans the current repository's `.sandman/runs/` tree on each poll, shows active and recent Sandman instances, and exposes a typed preset launcher for repo-scoped Sandman commands. It does not manage daemon lifecycle.
_Avoid_: dashboard, monitor, control panel.

**Continue**:
Re-run the latest AgentRun for one or more issues with a new raw prompt while reusing each issue's prior branch, base branch, agent, and review command. Multi-issue `sandman continue <issue>... <prompt>` submits a single Batch with per-issue `Branches`, `BaseBranch`, and `PreviousRunIDs` maps so the orchestrator parallelizes across issues. Continuation keeps branch checkout unchanged, resolves the model from `--model` or `model`, and uses the stored base branch for prompt rendering and event metadata only. Per-issue prompt rendering is built on top of this surface by #443. Invoked via `sandman continue`. When `.sandman/handoff.md` and `.sandman/handoff-prompt.md` are present in the worktree, the resumed run consumes them instead of starting from a blank prompt.
_Avoid_: Retry.

**Continuation**:
An AgentRun or Batch request mode that skips prompt template rendering and writes raw prompt text to `handoff-prompt.md` inside the worktree.
_Avoid_: Replay mode.

**Handoff**:
The persisted state written by `sandman-handoff` to `.sandman/handoff.md`, capturing the current workflow stage, completed work, pending items, blockers, key decisions, the originating rendered prompt (`## Source Prompt: .sandman/rendered-prompt.md`), the last executed sub-skill (`## Last Skill`), and its completion status (`## Last Skill Status`) so that a `Continue` or retry can resume from the right checkpoint. (ADR-0023). Removed by the orchestrator once the PR is merged.
_Avoid_: Continuation context (legacy filename), continuation file.

## Relationships

- A **Daemon Process** is created by `sandman run`, which starts a **Control Socket**
- A **Control Socket** at `.sandman/runs/<run-id>/run.sock` accepts **Attach** connections for the duration of the **Batch**
- A **Daemon Process** also starts a **Command Server** at `.sandman/runs/<run-id>/cmd.sock` for surgical per-issue control requests
- An **Issue Commander** is the orchestrator seam the **Command Server** dispatches to; it maps an external cancel to a single **AgentRun**
- A **Daemon Process** stops the **Control Socket** and **Command Server** when its **Batch** completes
- An **Attach** client connects to the **Control Socket** and reads the daemon's output until EOF
- A **Portal** is repo-scoped and can show multiple **Daemon Process** instances from the same repository at once
- A **Portal** rescans the current repository's `.sandman/runs/` tree on each poll so newly started **Daemon Process** instances appear without restarting the portal
- An **Archive** entry under `.sandman/archive/<run-id>` is the relocated home of a run directory whose **Daemon Process** is no longer live; `sandman archive run <run-id>` performs the move after the liveness check fails
- The run directory under `.sandman/runs/<run-id>` is the **Daemon Process**'s home; once it has been moved to `.sandman/archive/<run-id>` the directory is no longer owned by any **Daemon Process**

- A **Batch** contains zero or more **AgentRuns**
- An **AgentRun** targets exactly one **Issue** and produces exactly one **Branch**
- An **Issue** may have **BlockedBy** relationships to other **Issues**
- A **DependencyResolver** produces a **ResolvedBatch** from a set of **Issues**
- An **Orchestrator** executes a **ResolvedBatch**, respecting **BlockedBy** ordering
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
