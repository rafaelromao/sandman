# Sandman

Domain vocabulary for Sandman, a terminal-native CLI tool that orchestrates AFK coding agents in isolated sandboxes.

## Language

**BlockedBy**:
The set of issue numbers that must complete successfully before an AgentRun for this issue can start. Derived from the union of body references and GitHub native dependency fields.
_Avoid_: dependencies, prerequisites.

**Agent**:
An external AI coding tool (OpenCode or Pi) invoked by Sandman via `os/exec`. Sandman does not contain the agent; it renders a command template and executes it.
_Avoid_: AI model, LLM, copilot.

**AgentPreset**:
A built-in command, config source, and auth profile for a known AI coding tool keyed by name (opencode, pi). Declared in `config.BuiltInAgentPresets` and resolved by `config.ResolveAgentProvider`.
_Avoid_: Provider template, agent type.

**Agent Provider**:
Deprecated language for repo-local provider configuration. Sandman now supports built-in agents only.
_Avoid_: Agent type, runner.

**AgentModel**:
A built-in agent model identifier overridden via `sandman run --model`.
_Avoid_: agent model, default model.

**AgentRun**:
One execution of an agent against one issue, producing commits on a branch. The unit of work within a batch.
_Avoid_: Run, job, task.

**DependencyResolver**:
The component that fetches issues, extracts their BlockedBy relationships, validates the dependency graph (detecting cycles and missing blockers), and produces a topologically sorted ResolvedBatch.
_Avoid_: scheduler, planner.

**Batch**:
The set of AgentRuns triggered by a single `sandman run` invocation. Coordinated for parallel execution with a concurrency limit.
_Avoid_: Batch run, invocation.

**Branch**:
A git branch named `sandman/<issue-number>-<slugified-title>`, created per AgentRun.
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

**ContainerSandbox**:
A Docker or Podman container providing filesystem and process isolation for one or more AgentRuns. A Batch may scale a pool of ContainerSandboxes up or down within configured limits.
_Avoid_: Docker container, sandbox container.

**ContainerCapacity**:
The maximum number of AgentRuns that may execute concurrently inside one ContainerSandbox. `0` means auto/default mode: use the default container capacity behavior instead of an explicit per-container limit. When capacity is full, additional AgentRuns wait for another container or for capacity to free up.
_Avoid_: shared mode, isolated mode.

**StartDelay**:
A batch pacing delay in seconds. After any AgentRun finishes, the Batch waits this long before starting the next AgentRun. `0` disables pacing.
_Avoid_: cooldown, throttle.

**MaxContainers**:
The maximum number of ContainerSandboxes Sandman may create for one Batch. `max_containers=0` means auto mode: create up to the minimum number of containers needed for the currently active AgentRuns, based on ContainerCapacity.
_Avoid_: isolated container toggle, fixed pool size.

**Event**:
A single structured log entry in the append-only JSONL event log (`.sandman/events.jsonl`). Examples: `run.started`, `agent.stdout`, `run.finished`.
_Avoid_: Log line, record.

**Issue**:
A GitHub issue fetched via `gh` CLI. The unit of work delegated to an agent.
_Avoid_: Ticket, story.

**Prompt**:
The generated instruction file passed to an Agent, rendered from a template with issue metadata and built-in substitutions.
_Avoid_: Instructions, query.

**Default Prompt**:
The canonical built-in prompt template embedded in Sandman at `internal/prompt/default_prompt.md`.
_Avoid_: Base prompt, stock prompt.

**Project Prompt Template**:
The repo-local `.sandman/prompt.md` template created from the Default Prompt by `sandman init` and materialized on run when missing.
_Avoid_: User prompt, custom prompt.

**Prompt keys**:
The built-in substitution keys available in prompt templates: `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, `{{ISSUE_BODY}}`, `{{SOURCE_BRANCH}}`, `{{TARGET_BRANCH}}`, `{{BRANCH}}`, `{{DEFAULT_BRANCH}}`, `{{REVIEW_COMMAND}}`. Custom keys are supported via `promptArgs` in config.

**Command template key**:
The `{{.PromptFile}}` key available in agent command templates, resolved to the relative path of the rendered prompt file.

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

**Daemon Process**:
A long-lived sandman process executing a Batch in the background. Writes `.sandman/run.pid` and listens on the control socket.
_Avoid_: Background job, server.

**Control Socket**:
A Unix domain socket at `.sandman/run.sock` that accepts attach client connections. Created when a daemon starts, removed when it stops.
_Avoid_: IPC socket, management socket.

**PID Lock**:
A file at `.sandman/run.pid` containing the PID of the running daemon process. Prevents concurrent `sandman run` invocations in the same project directory.
_Avoid_: Lock file, process file.

**Attach**:
Connect a terminal to a running daemon via the control socket to stream its output. Invoked via `sandman attach`.
_Avoid_: Tail, follow.

## Relationships

- A **Daemon Process** is created by `sandman run`, which acquires a **PID Lock** and starts a **Control Socket**
- A **PID Lock** at `.sandman/run.pid` prevents a second `sandman run` while the first is alive
- A stale **PID Lock** (daemon crashed) is cleaned up automatically on the next `sandman run`
- A **Control Socket** at `.sandman/run.sock` accepts **Attach** connections for the duration of the **Batch**
- A **Daemon Process** stops the **Control Socket** and releases the **PID Lock** when its **Batch** completes
- An **Attach** client connects to the **Control Socket** and reads the daemon's output until EOF

- A **Batch** contains one or more **AgentRuns**
- An **AgentRun** targets exactly one **Issue** and produces exactly one **Branch**
- An **Issue** may have **BlockedBy** relationships to other **Issues**
- A **DependencyResolver** produces a **ResolvedBatch** from a set of **Issues**
- An **Orchestrator** executes a **ResolvedBatch**, respecting **BlockedBy** ordering
- An **AgentRun** may be **blocked** if any of its **BlockedBy** issues failed
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
