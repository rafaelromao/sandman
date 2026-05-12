# Sandman

Domain vocabulary for Sandman, a terminal-native CLI tool that orchestrates AFK coding agents in isolated sandboxes.

## Language

**Agent**:
An external AI coding tool (OpenCode, Codex, Cloud Code, Pi) invoked by Sandman via `os/exec`. Sandman does not contain the agent; it renders a command template and executes it.
_Avoid_: AI model, LLM, copilot.

**Agent Provider**:
A configured agent type declared in `.sandman/config.yaml`, with a command template, env map, and known config mount paths.
_Avoid_: Agent type, runner.

**AgentRun**:
One execution of an agent against one issue, producing commits on a branch. The unit of work within a batch.
_Avoid_: Run, job, task.

**Batch**:
The set of AgentRuns triggered by a single `sandman run` invocation. Coordinated for parallel execution with a concurrency limit.
_Avoid_: Batch run, invocation.

**Branch**:
A git branch named `sandman/<issue-number>-<slugified-title>`, created per AgentRun.
_Avoid_: Feature branch, PR branch.

**ContainerSandbox**:
A Docker or Podman container providing filesystem and process isolation for one or more AgentRuns. May contain multiple worktrees.
_Avoid_: Docker container, sandbox container.

**Event**:
A single structured log entry in the append-only JSONL event log (`.sandman/events.jsonl`). Examples: `run.started`, `agent.stdout`, `run.finished`.
_Avoid_: Log line, record.

**Issue**:
A GitHub issue fetched via `gh` CLI. The unit of work delegated to an agent.
_Avoid_: Ticket, story.

**Prompt**:
The generated instruction file passed to an agent, rendered from a template with issue metadata and built-in substitutions.
_Avoid_: Instructions, query.

**Sandbox**:
The abstract isolation mechanism within which an AgentRun executes. Hides whether isolation is provided by a worktree, a shared container, or an isolated container.
_Avoid_: Environment, boundary, boundary context.

**Worktree**:
A git worktree under `.sandman/worktrees/`, created per AgentRun, providing a dedicated checkout for the agent to modify. Not a sandbox on its own; a sandbox may contain one or more worktrees.
_Avoid_: Working directory, checkout, clone.

**WorktreeSandbox**:
A sandbox adapter that uses only a git worktree for isolation, with no container. One worktree per AgentRun.
_Avoid_: Default sandbox, local sandbox.

## Relationships

- A **Batch** contains one or more **AgentRuns**
- An **AgentRun** targets exactly one **Issue** and produces exactly one **Branch**
- A **Sandbox** provides isolation for one or more **AgentRuns**
- In worktree mode, each **AgentRun** gets its own **Sandbox** (a **WorktreeSandbox**)
- In shared-container mode, a single **Sandbox** (a **ContainerSandbox**) contains multiple **Worktrees**, each hosting one **AgentRun**
- An **AgentRun** generates many **Events**
- A **Prompt** is rendered per **AgentRun** from an **Agent Provider** config

## Example dialogue

> **Dev:** "When a **Batch** contains three **AgentRuns**, do they all share one **Sandbox**?"
> **Domain expert:** "By default, yes. Shared container mode is the default — a single **ContainerSandbox** hosts all three **Worktrees**. With `--isolated-containers` each **AgentRun** gets its own **ContainerSandbox**. With `sandbox: worktree` each **AgentRun** gets its own **WorktreeSandbox**."

> **Dev:** "Does the **Sandbox** interface expose the worktree path?"
> **Domain expert:** "Yes — the **Sandbox** contract returns a working directory path, but callers must not assume it is a git worktree. That detail belongs to the adapter."

## Flagged ambiguities

- "run" was used to mean both the CLI command (`sandman run`) and a single agent execution. Resolved: the CLI command triggers a **Batch**; each execution is an **AgentRun**.
- "sandbox" was used interchangeably with "worktree" and "container." Resolved: **Sandbox** is the abstract isolation contract; **WorktreeSandbox** and **ContainerSandbox** are the concrete adapters. A **Worktree** is a git concept — a dedicated checkout that lives inside a sandbox.
