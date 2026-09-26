# Troubleshooting

Operator-facing recovery for the most common failure modes. Each entry names the symptom, the most likely cause, and the first thing to try.

> Read [Monitoring](../usage/monitoring.md) for the canonical `status` / `history` / `events.jsonl` walkthrough. This page is the shortlist.

## A run appears stuck and nothing is happening

If a run has produced no log output for `run_idle_timeout` seconds (default: 3600), the heartbeat watchdog aborts it and the run is emitted as `aborted` with a `run.idle_timeout` event written just before.

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
- `scaffold metadata drift: Dockerfile installed-agents [...] does not include agent "claude"` means the run's agent is not installed in the image. Re-run `sandman init --agent claude`, or add the agent's install line and list it in the `# sandman installed-agents:` header (see [Scaffolding > Agents in the image](../usage/scaffolding.md#agents-in-the-image)).

## Claude Code runs fail in containers

- `--dangerously-skip-permissions cannot be used with root/sudo privileges`: podman runs the agent as root. The `claude` preset exports `IS_SANDBOX=1` for this check; if your Claude Code version still refuses, switch to the pre-approved-tools command in [Agent Compatibility > Permissions](../usage/agent-compatibility.md#permissions).
- `Not logged in · Please run /login` on macOS: the run log shows a `system/init` record with `"apiKeySource":"none"`, then a `result` record with `"is_error":true` and that message, and every retry fails within seconds. The host login lives in the macOS Keychain, which never reaches a container. Fix it once per machine:

  1. Create a one-year subscription token (Pro, Max, Team, or Enterprise). It opens a browser to authorize and prints a value that starts with `sk-ant-oat01-`:

     ```bash
     claude setup-token
     ```

     If `claude` is not on your `PATH` but the Claude desktop app is installed, run the copy the app manages instead, adjusting the version folder to the one present: `"$HOME/Library/Application Support/Claude/claude-code/<version>/claude.app/Contents/MacOS/claude" setup-token`.

  2. Add the token to `.sandman/config.yaml`, keeping `preset: claude` so the entry still inherits the preset:

     ```yaml
     agents:
       claude:
         preset: claude
         env:
           CLAUDE_CODE_OAUTH_TOKEN: sk-ant-oat01-...
     ```

  `.sandman/` is gitignored and Sandman writes the config with owner-only permissions, but treat the file as a secret. The token is exported on the agent's command line and is visible to `ps` on the host during a run. When it expires the same `Not logged in` or `Login expired` message returns; create a new token. Worktree runs do not need the token: they use the host login.
- Every edit or command is denied in a worktree run: print mode cannot answer permission prompts. Pass `--dangerously-skip-permissions` or allow the tools in `~/.claude/settings.json`.
- A run waits with `await_reason: usage-limit`: your subscription hit its session, weekly, or model limit. Sandman probes every ten minutes for up to five hours and resumes the same conversation; see [Agent Compatibility > Usage limits](../usage/agent-compatibility.md#usage-limits).

## Container image build fails with `mise: not found`

Symptom: a container run stops with `build image from .sandman/Dockerfile: exit status 127`, and the build log ends with `RUN mise use -g --pin ...` and `/bin/sh: 1: mise: not found`. Scroll up to the `RUN curl https://mise.run | ... sh` step: it printed `curl: (60) SSL certificate problem: unable to get local issuer certificate`.

Cause: HTTPS traffic from the container build is re-signed by a certificate authority the Debian base image does not trust, usually a corporate TLS-inspection proxy. The host trusts that authority, so the same URLs work outside the container, and `apt-get` still works because Debian mirrors use plain HTTP. The mise step only looks successful: `curl` fails, `sh` reads empty input and exits 0, and the missing `mise` surfaces at the first step that uses it.

Fix: trust the proxy's root certificate inside the image.

1. Confirm the issuer from the host. A company name instead of a public certificate authority confirms the cause:

   ```bash
   openssl s_client -connect mise.run:443 -servername mise.run </dev/null 2>/dev/null | grep '^issuer='
   ```

2. On macOS, export that root certificate from the system keychain into the repo:

   ```bash
   security find-certificate -c "<issuer common name>" -p /Library/Keychains/System.keychain > .sandman/corp-ca.crt
   ```

   If the issuer is an intermediate certificate, export the root of its chain instead.

3. Add these lines to `.sandman/Dockerfile` directly after the `apt-get install` line:

   ```dockerfile
   COPY .sandman/corp-ca.crt /usr/local/share/ca-certificates/corp-ca.crt
   RUN update-ca-certificates
   ENV NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt
   ```

   `update-ca-certificates` fixes `curl` (mise, rtk), and `NODE_EXTRA_CA_CERTS` makes npm and the agent CLIs trust the same authority during the build and at run time.

- `sandman init` regenerates `.sandman/Dockerfile`, so re-apply these lines after every re-init.
- To run without the image while you fix it, use a worktree run, which uses the agent CLI installed on the host: `sandman run <issue> --sandbox worktree`. The `claude` preset also needs `--dangerously-skip-permissions` there; see [Agent Compatibility > Permissions](../usage/agent-compatibility.md#permissions).

## Portal shows unknown rows after upgrading Sandman

Sandman does not migrate on-disk state across version upgrades. Existing `.sandman/` state can contain identifiers the current portal does not understand.

- Clear `.sandman/` and rebuild: `rm -rf .sandman && sandman init`.
- Re-run current Sandman jobs after re-initializing.

## `/sandman review` keeps triggering itself

The review daemon's primary defence is the **daemon-side redactor**. The agent's `decision.md` body cannot contain a `/sandman` substring after the daemon transforms it. The redactor runs out-of-band of the LLM, so the trigger substring is stripped regardless of what the model writes.

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

- Enable the real-agent smoke path with prewarm: `SANDMAN_RUN_SMOKE_E2E=1 SANDMAN_SMOKE_PREFETCH=1 SANDMAN_TEST_PROVIDERS=opencode go test -tags smoke ./internal/cmd -run Smoke` (use `SANDMAN_TEST_PROVIDERS=claude` or `all` to prewarm the Claude Code image too).
- See [Testing > Smoke image prewarm](../development/testing.md#smoke-image-prewarm).

## Git identity missing

Sandman resolves `user.name` and `user.email` from `~/.gitconfig`, then the host global/XDG Git config, then repo-local `.git/config`. `sandman run` fails early if either value is missing.

- Set the identity before the first run: `git config --global user.name "..."` and `git config --global user.email "..."`.
- Sandman never stores its own commit identity; the agent commits under your identity.
