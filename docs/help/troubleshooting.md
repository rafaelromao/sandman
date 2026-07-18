# Troubleshooting

Operator-facing recovery for the most common failure modes. Each entry names the symptom, the most likely cause, and the first thing to try.

> Read [Monitoring](../usage/monitoring.md) for the canonical `status` / `history` / `events.jsonl` walkthrough. This page is the shortlist.

## A run appears stuck and nothing is happening

If a run has produced no log output for `run_idle_timeout` seconds (default: 1800), the heartbeat watchdog aborts it and the run is emitted as `aborted` with a `run.idle_timeout` event written just before.

- Check `sandman status` for elapsed time.
- Read the row's log (`.sandman/batches/<batch-id>/runs/<run-id>/run.log` or via the portal's Log tab).
- If the agent is genuinely doing real work but is silent for long stretches (for example, waiting on an external webhook), disable the watchdog for that invocation: `sandman run --run-idle-timeout 0 <issue>`.

See [Monitoring > Idle timeout](../usage/monitoring.md#idle-timeout).

## `Error: missing blockers: #N`

A BlockedBy relationship refers to an issue that is not in the current batch. Two options:

- Add the blocker explicitly to the batch: `sandman run <this-issue> <blocker>`.
- Auto-expand with transitive blockers: `sandman run --include-dependencies <this-issue>`.

Cycles are reported with the cycle path. See [Workflows > BlockedBy-aware execution](../usage/workflows.md#blockedby-aware-execution).

## Stranded worktrees

A worktree whose HEAD points to a different branch than its directory name expects — typically caused by a previous run that was interrupted after `git worktree add` but before the agent checked out the right branch.

- Detect: `sandman stranded [--json]`. The text output prints a one-line `git checkout -f ...` remediation per worktree. `--json` returns the structured list.
- Auto-recover: `--reconcile-stranded` (enabled by default on `sandman run --override` and on a fresh issue under `--continue`) prunes and re-registers the stale worktree. Opt out with `--no-reconcile-stranded`.
- Prunable worktrees (the gitlink in `git worktree list` is missing) are also auto-recovered on `--continue`.

See [`sandman stranded`](../usage/commands.md#sandman-stranded).

## Container mode refuses to start

`Sandman run` fails fast with a clear error when the `.sandman/Dockerfile` is missing in container mode. The preflight check is covered by `TestRunBatch_ContainerModeFailsBeforeAgentWhenDockerfileMissing` in the unit suite.

- Confirm `.sandman/Dockerfile` exists. If not, re-run `sandman init` to scaffold it.
- Confirm the chosen runtime (`podman` or `docker`) is on `PATH` and the sandbox user has permission to run containers.
- Confirm `keychain_auth: false` on the active agent preset. **Keychain auth is explicitly rejected in container mode** — see [Agent Compatibility > Container auth model](../usage/agent-compatibility.md#container-auth-model).

## Portal shows unknown rows after upgrading Sandman

Sandman does not migrate on-disk state across version upgrades. Existing `.sandman/` state can contain identifiers the current portal does not understand.

- Clear `.sandman/` and rebuild: `rm -rf .sandman && sandman init`.
- Re-run current Sandman jobs after re-initializing.

## `/sandman review` keeps triggering itself

The review daemon's primary defence is the **daemon-side redactor** in `internal/review/redactor.go`. The agent's `decision.md` body cannot contain a `/sandman` substring after the daemon transforms it. The redactor runs out-of-band of the LLM, so the trigger substring is stripped regardless of what the model writes.

- Confirm `sandman review` is running. The command starts the review daemon and does not accept positional arguments.
- If a bot review body does land with `## Previous review progress` *and* the literal `/sandman review` substring, the structural sniff `LooksLikeBotReviewBody` drops it before `ParseTrigger` runs — defence-in-depth.
- The daemon-side redactor is the primary defence; the structural sniff is defence-in-depth.

## `gh` auth / API failures

Sandman shells out to `gh` for every issue fetch, PR check, and review comment.

- Run `gh auth status`. Confirms scopes (`repo` is required for issue reads and PR writes on private repos).
- For e2e tests, the `gh` shim contract is documented in [Testing > GH shim contract](../development/testing.md#gh-shim-contract); a shim must include the `blocked_by` field on the issue JSON, not just body text, for dependency detection.

## E2E test side effects

Interrupted or failed e2e runs leave worktrees, orphaned batch directories, and temp directories under `/tmp/`. The accumulation is most painful in CI with disk quotas and in worktree-based sandboxes.

- Preview: `sandman clean --dry-run --orphaned`.
- Full cleanup: `sandman clean --all` — runs stale recovery, orphaned removal, archived removal, and the shared temp-dir / `sandman-smoke-*` image sweep in one pass.
- Remove orphaned test batch dirs: `sandman clean --orphaned`.
- Recover stale runs in dead batches: `sandman clean --stale` — emits `run.aborted` events so the event log matches the on-disk state.
- Bare `sandman clean` is a hard error: every invocation needs an explicit mode flag (`--all`, `--archived`, `--stale`, or `--orphaned`).
- Combinations and mutual exclusion rules are documented in [Commands > `sandman clean`](../usage/commands.md#sandman-clean) and [Testing > Side effects and cleanup](../development/testing.md#side-effects-and-cleanup).

## The portal binds but nothing loads

`127.0.0.1` is the default bind host. If you started the portal expecting to reach it from another machine:

- Use `sandman portal --host 0.0.0.0` (or set `SANDMAN_PORTAL_HOST=0.0.0.0`).
- Confirm any firewall allows the chosen port (default 5000).
- See [Portal > Expose the portal on another interface](../usage/portal.md#expose-the-portal-on-another-interface).

## Sandbox container image changes don't take effect

Smoke tests skip the expensive real-agent cases unless `SANDMAN_RUN_SMOKE_E2E=1` is set. When enabled, they build a per-provider / per-buildTools image on demand. Set `SANDMAN_SMOKE_PREFETCH=1` to enable the optional upfront prewarm fan-out; subsequent test invocations reuse the cached image unless the cache is cleared.

- Enable the real-agent smoke path with prewarm: `SANDMAN_RUN_SMOKE_E2E=1 SANDMAN_SMOKE_PREFETCH=1 SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke`.
- See [Testing > Smoke image prewarm](../development/testing.md#smoke-image-prewarm).

## Git identity missing

Sandman resolves `user.name` and `user.email` from `~/.gitconfig`, then the host global/XDG Git config, then repo-local `.git/config`. `sandman run` fails early if either value is missing.

- Set the identity before the first run: `git config --global user.name "..."` and `git config --global user.email "..."`.
- Sandman never stores its own commit identity; the agent commits under your identity.
