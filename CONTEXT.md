# Sandman

Domain vocabulary for Sandman, a terminal-native CLI tool that orchestrates AFK coding agents in isolated sandboxes.

## Language

**BlockedBy**:
The set of issue numbers that must complete successfully before an AgentRun for this issue can start. Derived from the union of body references and GitHub native dependency fields.
_Avoid_: dependencies, prerequisites.

**Agent**:
An external AI coding tool (OpenCode, Codex, Claude Code, Pi) invoked by Sandman via `os/exec`. Sandman does not contain the agent; it renders a command template and executes it.
_Avoid_: AI model, LLM, copilot.

**Agent Provider**:
A configured agent type declared in `.sandman/config.yaml`, with a command template, env map, and known config mount paths.
_Avoid_: Agent type, runner.

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
A scaffold-time recipe chosen during `sandman init` that seeds a pinned container image definition with shared baseline tools, stack tooling, and built-in agent installation defaults.
_Avoid_: language, stack, base image.

**ContainerSandbox**:
A Docker or Podman container providing filesystem and process isolation for one or more AgentRuns. A Batch may scale a pool of ContainerSandboxes up or down within configured limits.
_Avoid_: Docker container, sandbox container.

**ContainerCapacity**:
The maximum number of AgentRuns that may execute concurrently inside one ContainerSandbox. When capacity is full, additional AgentRuns wait for another container or for capacity to free up.
_Avoid_: shared mode, isolated mode.

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
The generated instruction file passed to an agent, rendered from a template with issue metadata and built-in substitutions.
_Avoid_: Instructions, query.

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

## Relationships

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
- An **AgentRun** generates many **Events**
- A **Prompt** is rendered per **AgentRun** from an **Agent Provider** config

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
