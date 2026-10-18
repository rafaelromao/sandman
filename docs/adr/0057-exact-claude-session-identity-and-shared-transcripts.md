# ADR-0057: Exact Claude Code session identity and host-shared transcripts

## Status

proposed

## Context

Operators need to open an agent's conversation from the host, for example to
inspect or continue a Run by hand. Host OpenCode already sees Sandman's OpenCode
sessions, because the preset live-mounts `opencode.db`. Container Claude Code
writes its transcripts into the per-batch config snapshot instead, where host
Claude Code never looks. Run details also never named the agent session.

On Claude Code 2.1.302, `claude --resume <session-id>` finds a transcript in any
directory directly under `~/.claude/projects/`, from any working directory. It
follows no symlinks and searches no nested directories. So a host-visible
transcript has to be a real file there, which means a bind mount of the
directory itself.

ADR-0056 reused Claude conversations with the working-directory-scoped
`--continue`. Every container worktree has the same
`/workspace/.sandman/worktrees/<branch>` path, so in a shared transcript
directory the most recent conversation for a branch name may belong to another
repository or an older batch.

## Decision

The `claude` preset live-mounts `~/.claude/projects`, creating it on the host if
it is missing (`LiveMountDirs`). Container transcripts land in the host's
transcript directory.

The Claude strategy records the session from Claude Code's `system/init` record
in the Run's `session.json` as
`{protocol: "claude-session/v1", provider: "claude", session_id}`. It uses the
same atomic writer as the OpenCode identity (ADR-0055) and writes as soon as the
session is observed. The post-exit write still runs. Each strategy validates
only its own protocol and provider.

On the reuse paths ADR-0055 defines (runtime-owned re-entry and
`--continue --reuse-session`), Claude reuse renders `--resume <uuid>` for the
recorded session. Claude Code's `--continue` is no longer rendered. When no
valid UUID is recorded, the Run starts a fresh session from the preserved Task.
When Claude Code reports `No conversation found with session ID`, the Run
relaunches once with a fresh session.

The Portal shows the recorded identity of either agent as one agent-neutral Run
detail, `agentSessionId`.

This supersedes ADR-0056's session reuse paragraph and its listing of exact-ID
session identity as a limitation.

## Consequences

- An operator can resume any Run from the host with `claude --resume <id>` or
  `opencode --session <id>`, using the ID shown in Run details.
- Claude reuse is exact and cannot select another repository's same-branch
  conversation. Reuse across batches now works in container mode.
- The host `~/.claude/projects` is no longer isolated from containers. It
  accumulates `-workspace--sandman-worktrees-*` directories, and every host
  transcript is visible inside the container.
- Claude Runs that were waiting for re-entry when this change shipped have no
  recorded identity. They resume with a fresh session from their preserved Task.
- Session identity stays a per-Run artifact read for reuse and display. It is
  not lifecycle evidence, and no status is derived from it.
- Resume-by-ID across project directories depends on Claude Code's own lookup.
  The verified version is documented.
