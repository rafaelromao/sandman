# FAQ

Questions people ask before installing Sandman. Short answers, linked to the longer page that covers each in depth.

## Why does Sandman live in my repo's `.sandman/` instead of being a SaaS?

The delivery loop is local by design. It reads your `gh` auth, your host git identity, your worktrees, your container images, and your existing `.git` history. It also writes durable state into `.sandman/` so the work survives daemon restart. A SaaS would be a different product.

If you do want a centralized view across machines, the portal is the closest equivalent: run `sandman portal` on each machine, or expose it with `sandman portal --host 0.0.0.0`.

## Why does Sandman need a `gh` auth token?

Sandman shells out to `gh` for every issue fetch, PR check, and review comment. The `gh` CLI's auth flow handles the credential surface (PAT, GitHub App, `gh auth login`) so Sandman doesn't have to. Confirm scopes with `gh auth status`; `repo` is required for private repos.

## Can Sandman use private repos?

Yes. As long as the host `gh` CLI is authenticated and has the `repo` scope on the private repo, Sandman reads and writes against it like any other repo. There is no separate "private mode" — the auth model is the same.

## Why is the portal loopback by default?

The default bind host is `127.0.0.1` so the portal is not reachable from other machines. If you want to expose it (for example, on a Mac Mini or a remote VM), pass `sandman portal --host 0.0.0.0` or set `SANDMAN_PORTAL_HOST=0.0.0.0` before launching. See [Portal > Expose the portal on another interface](../usage/portal.md#expose-the-portal-on-another-interface).

## Does Sandman work in monorepos?

Yes. Each Batch gets its own `.sandman/batches/<batch-id>/` tree under your repo root, and each AgentRun gets its own worktree inside `.sandman/worktrees/`. The portal is repo-scoped, so a monorepo with multiple top-level projects would scope to whichever project is checked out at the portal's working directory.

## Why isn't there a Sandman Docker image I can just run?

By design. Sandman scaffolds `.sandman/Dockerfile` from your selected BuildToolsPreset, so the image matches your stack (Go, Node, Python, .NET, Rust, Elixir, Ruby, Java, or generic). The Dockerfile is the source of truth for the container runtime Sandman uses.

## Why does Sandman need OpenCode's `opencode-shell-strategy` plugin?

Sandman runs OpenCode headlessly (no TTY/PTY). The plugin teaches OpenCode to avoid interactive shell commands that would hang indefinitely in that environment — package manager prompts, `git commit` without `-m`, editors, pagers. OpenCode subagents inherit the same instructions, so the plugin is a per-installation prerequisite, not a per-run flag. See [Installation > OpenCode setup](../get-started/install.md#opencode-setup).

## Does Sandman commit on my behalf?

Yes. Sandman resolves `user.name` and `user.email` from your git config (host global/XDG first, then repo-local) and uses that identity for every agent commit. There is no Sandman-side commit author; the commit attribution is yours, with a `Co-authored-by:` trailer where appropriate (for example, on badge sidecars).

## What stays on disk when a run finishes?

Everything. Worktrees stay under `.sandman/worktrees/` until you remove them with `sandman clean`. Run logs persist as `.sandman/batches/<batch-id>/runs/<run-id>/run.log`. The append-only event log at `.sandman/events.jsonl` grows on every event. Use `sandman archive` to move terminal batches to `.sandman/archive/` if you want to keep the active `batches/` tree small.

## Can I stop a run mid-flight?

Yes — via the portal's Stop button (`POST /api/runs/abort`) or by sending SIGINT/SIGTERM to the `sandman run` process. The AgentRun is emitted as `run.aborted`, the control socket closes, and partial results and events are preserved. See [Monitoring > Graceful shutdown](../usage/monitoring.md#graceful-shutdown).

## What happens to a run if my laptop sleeps?

The daemon process and any in-flight AgentRun are paused by the OS. They resume when the host wakes. There is no daemon-side idle detection for sleep (the heartbeat watchdog fires only on agent inactivity, not host sleep).

## Can Sandman run without an AI agent?

No. Sandman is the *delivery loop*; the agent does the implementation work. The agent is one of the things Sandman shells out to, configured via `.sandman/config.yaml` (`agent: opencode` by default).

## What's the relationship to Spec-Driven Development and Loop Engineering?

SDD describes the work. Sandman delivers it. The clearest reference for SDD is GitHub's [Spec-driven development with AI](https://github.blog/ai-and-ml/generative-ai/spec-driven-development-with-ai-get-started-with-a-new-open-source-toolkit/) article. Loop Engineering describes the broader operating model; Addy Osmani's [Loop Engineering](https://addyosmani.com/blog/loop-engineering/) article frames agent systems as prompt, state, verify, judge loops. Sandman is one concrete loop inside that discipline — CLI-owned AFK delivery from GitHub issue to reviewed, merged PR. See [Positioning](positioning.md) for the canonical framing.

## Is Sandman production-ready?

It depends on what you mean by "production." Sandman is local-only, requires `gh` auth and either a container runtime or worktree mode, and treats agent output the way you'd treat fast human output — as input to validation, not a release confidence signal in itself. The recommended pattern is: run Sandman to a reference branch, then run your regular validation suite (smoke, e2e, QA) on the merged result. See the landing page section on validation.
