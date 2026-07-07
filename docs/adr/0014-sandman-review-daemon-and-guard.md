# ADR-0014: Sandman Review - Daemon-Monitored PR Reviews

## Status

accepted

## Context

Sandman needs a PR review workflow: when someone posts `/sandman review [focus]` on a GitHub PR, a review agent should read the diff and post findings as a comment. This introduces a new command and a coordination problem.

### Three design questions

1. **One-shot vs daemon**: Should `sandman review` scan PRs once and exit, or run as a background daemon that polls for new commands?
2. **Who posts the review**: Should the sandman daemon run the agent and post its output, or should the agent itself post the comment?
3. **How to ensure the daemon is running**: The default `review_command` config value drives the pr-review skill's PR comment. When it contains `/sandman`, the agent posts a comment that the daemon must intercept. If the daemon isn't running, that comment hangs. How do we prevent this?

## Decision

### Two modes: daemon (default) and one-shot

`sandman review` defaults to **daemon mode** - it polls the repo every 60 seconds for new PRs with `/sandman review` comments. PR numbers passed as positional arguments run in **one-shot mode** for manual or CI-driven review invocation. One-shot mode accepts bare numbers, closed ranges (`N:M`), and unbounded ranges (`N:`, `:M`).

Rationale: the daemon enables the full automated workflow (AFK agent finishes work, posts `/sandman review`, daemon picks it up). The one-shot mode covers ad-hoc cases without running a background process.

### Daemon posts the review comment

The daemon posts the review comment. The reviewer agent writes its body to `<worktree>/decision.md` (its CWD, the per-row review worktree, issue #1953); the daemon reads it, applies a regex redactor that strips every `/sandman` substring (case-insensitive), and posts the redacted body via `gh pr comment`. This moves the no-self-loop invariant from LLM-side prompt compliance to a daemon-side transform that runs out-of-band of the LLM.

Rationale: the daemon-side redaction is the load-bearing safety net for the no-self-loop invariant. The agent can be instructed, prompted, and audited; the daemon transform cannot forget, drift, or be subverted by an LLM refusing to follow the prompt's hard rule. The redaction runs out-of-band of the LLM, so a self-loop is structurally impossible regardless of what the bot writes.

### Control-socket guard

When the configured `review_command` contains `/sandman`, the `sandman run`, `sandman run --continue`, and `--auto` selection commands check for a control socket at `.sandman/review.sock` **before** executing. If the socket is missing, the command fails with a clear message.

This prevents the following failure mode:
1. User runs `sandman run` with default config
2. Agent finishes, posts `/sandman review` on the PR per the pr-review skill
3. Nobody is watching - review never happens

The socket check adds a forward dependency (review daemon must be running before you can run batches with the default review command), but it is a **fail-fast** dependency: the user sees the error immediately, not after a batch completes.

### Why a socket over alternatives

- **PID file**: Stale PID files survive crashes, leading to false positives (run thinks daemon is alive when it's dead).
- **Event log**: Requires reading and parsing the JSONL log, which is slower and less reliable than a single `os.Stat`.
- **Port check**: Requires binding a TCP port - overkill for a local workflow tool.

The socket (`net.Listen("unix", ...)`) is already an established pattern in the codebase (see `daemon.ControlSocket` and `attach`) and gives us a natural attach endpoint for future `sandman review --attach` functionality.

## Consequences

### Positive

- Fail-fast guard prevents the "orphaned review comment" failure mode.
- Two modes cover both automated and ad-hoc workflows.
- One-shot mode shares all prompt and agent infrastructure with daemon mode - no duplication.
- Socket-based guard reuses existing, tested infrastructure.
- Users can opt out of the guard entirely by setting `review_command` to something that does not contain `/sandman` (e.g. `/oc review` for backward compat).

### Negative

- Forward dependency: `sandman run` fails if `review_command` contains `/sandman` and no daemon is running. Users must either override `review_command` or start the daemon.
- Daemon consumes a terminal or must be started as a background job.
- The socket check adds a filesystem operation to the startup path of `run`/`continue`.

### Neutral

- The default `review_command` changes from `/oc review` to `/sandman review`.
- The one-shot mode is a thin wrapper around `BatchRunner.RunBatch` - no new execution infrastructure.

## Daemon-side redaction (issues #1845, #1846, #1847)

The review workflow is a closed loop: the implementor agent posts `/sandman review ...` on a PR, the daemon picks it up, launches a reviewer agent, the reviewer agent produces a review. If that review body ever contains the trigger substring (a self-review summary quoting the request, a future skill variant, an LLM that ignores the prompt's hard rule), the daemon would re-launch the review and the bot would loop on itself. The mitigation is a daemon-side redaction layer that runs **out-of-band of the LLM**: the agent can be prompted, audited, and disciplined, but the daemon transform cannot forget, drift, or be subverted by the model refusing the prompt rule.

### Canonical body file: `decision.md`

The reviewer agent writes its review body to `<worktree>/decision.md` (issue #1953) — the per-row review worktree, which is the agent's CWD, not a shared daemon path. The worktree path is `<WorktreeDir>/<reviewBranchName(pr, commentID)>`, where `<WorktreeDir>` is resolved by `paths.NewLayout(cfg, repoRoot)` against the repo root in production. The run folder (`<batchDir>/runs/<rowID>/`) keeps `run.json`, `run.log`, `run.sock`, and the per-row `review-state.json`; the review body belongs to the worktree, not the run folder. The agent writes the file via `os.WriteFile` and the daemon reads it back at post time. The file is the canonical hand-off: the daemon never sees the agent's stdout, never parses the agent's run log, and never asks the LLM to call `gh pr comment` itself. The agent's only daemon-visible side effect is the file; the daemon owns everything after.

### Redactor: `RedactBody`

The daemon reads `<worktree>/decision.md`, runs the body through `RedactBody` (S1, `internal/review/redactor.go:21`), and posts the redacted body via `gh pr comment`. `RedactBody` applies the regex `(?i)/sandman` → `sandman` — every `/sandman` substring, case-insensitive, is replaced with the bare word `sandman`. The regex is deliberately narrow: it only strips the leading-slash form (`/sandman`), and only the bot's own review body. Quote markers, paths, and prose that mentions the word `sandman` without the leading slash are untouched (`sandman/review-1234` stays as-is). The cross-reference to S1's `RedactBody` is the implementation source of truth.

Issue #1953 also removes the `SANDMAN_RUN_DIR` env-var fallback that previously carried the artifact path on the agent's environment. The prompt's `{{RUN_DIR}}` is the single source of truth for the write path; the env-var contract is intentionally gone (the prompt's fallback-discovery section was deleted alongside it) so the two surfaces cannot drift.

### Why the redactor is the load-bearing safety net

The no-self-loop invariant used to live in the LLM's prompt compliance: the review prompt carried a hard rule forbidding the literal `{{REVIEW_COMMAND}}` substring, and the LLM was trusted to obey. Prompt compliance is a soft constraint — it depends on the model reading its instructions correctly, on no future skill variant changing the prompt, and on the model not being subverted. The daemon-side redaction moves the invariant out of the LLM's control: the bot can write whatever it likes into `decision.md`, and the daemon transform still strips the trigger substring before the post lands. The invariant becomes a property of the daemon code, not the model.

### Structural self-defence sniff (issue #1821) — belt-and-braces

The redactor is the primary defence. The structural sniff `LooksLikeBotReviewBody` survives as a belt-and-braces backstop, not the primary gate: it flags bodies that match the *structural shape* the bot's review body always carries — a `## Previous review progress` markdown heading AND the literal `/sandman review` substring somewhere in the body — and drops them before `ParseTrigger` runs. The sniff covers the corner case where the redaction layer has not yet been wired through (an older daemon still on the path) or where a bot body legitimately quotes the trigger substring in a way the redaction is allowed to let pass (the redactor strips the leading-slash form, leaving bare `sandman review` substrings in the body — the sniff catches those). The sniff is asymmetric: bare implementor triggers (`/sandman review` standalone, with focus, with a leading bot mention) never carry the previous-review-progress heading, so they are not flagged.

Tests:

- `TestLooksLikeBotReviewBody_HitsBotShapedBodies` and `TestLooksLikeBotReviewBody_MissesImplementorTriggers` pin the asymmetric contract.
- `TestDaemon_BotShapedBody_DoesNotReactEvenWithEmptySelfPostStore` is the AC-level pin: a bot-shaped body alone, with no other fixture, must not trigger a batch run or add an eyes reaction. Pre-fix this fails (got 1 batch run, 1 reaction). Post-fix it passes.
- `TestRedactBody` pins the case-insensitive `/sandman` → `sandman` substitution and the leave-alone behaviour for non-leading-slash forms.

## Rehydrate-on-startup (issue #1847, S4)

A review that wrote `decision.md` but failed to post (daemon crash, transient `gh` failure, kill -9 between the agent exit and the post) leaves the file on disk for the next daemon restart to discover. The in-memory `pendingPost` map (`internal/review/daemon.go:138`) is the rehydrate-on-startup index: on `Daemon.New`, the daemon walks every review-kind batch's `runs/<rowID>/run.log` and the per-row worktree's `decision.md` (issue #1953), registers a `pendingPostEntry` for each unreviewed (PR, comment) pair, and queues the post step on the first tick after restart. The rehydrate path is best-effort: a missing `run.json`, a non-review kind, an unreadable `decision.md`, or a partial write is logged and skipped, never fatal — a transient read error on one batch does not block startup. The atomic-rename property of `decision.md` (the agent writes the file via temp-file + `os.Rename`) is what makes the rehydrate safe: the daemon only ever sees a fully-formed body, never a half-written one.

## Bounded-retry grace (issues #1759, #1845)

Pre-#1759, the bounded-retry escape (3 cycles × 30s = 90s) marked a pending review as `failure` when no post-`since` PR comment had arrived. The bot's typical post latency is sometimes longer than 90s, so a successful review was wrongly labelled as a failure. The new bounded-retry grace: when the cycle counter reaches `pendingMaxCycles`, the daemon performs one final `decision.md` read. If the file is present and redacts cleanly, the daemon attempts the post; only a missing or unredactable file (the bot truly failed) triggers the failure escape. The grace path closes the "bot was slower than 90s" race without giving up the bounded-retry escape that prevents silent retry forever. The grace is now a daemon-post failure recovery, not an agent-post race: it tolerates transient `gh` failures (rate limit, network blip) and retry-then-succeed.

## Retry the post step on transient gh failures (issue #1891)

A transient `gh pr comment` failure (network blip, rate limit, auth refresh) during the post step used to be treated as terminal: postDecision recorded `MarkSeen("failure")` on disk and called `MarkTerminalSeen` to drop the trigger from the seen cache. All subsequent ticks then short-circuited the trigger via `IsTerminalSeen`, the bot's review body never reached GitHub, and the trigger was permanently ignored until a daemon restart. PR #1887 hit this exact path on 2026-07-06 at 15:38:46 — the agent approved the PR but the bot never posted the review because the daemon treated a transient `gh` error as terminal.

**Chosen option:** retry the post step in-process up to `PostStepMaxAttempts = 5` times with exponential backoff (1s, 2s, 4s, 8s, 16s; total worst case ~31s). On final failure, fall back to the S4 rehydrate path: write `pending` to `review-state.json` AND register a `pendingPost` entry so the same-process next tick (or the S4 walker after a daemon restart) re-attempts the post. The trigger is NOT marked terminal-seen — a post failure means "post did not land", not "review is permanently lost".

**Carve-out:** the `missing decision.md` and `decision.md is a directory` branches of `postDecision` still call `MarkTerminalSeen`. Those represent "the agent did not produce a review", not "the post could not land"; the retry-then-pending escape applies only to the post-failure branch.

**Critical-path timing:** the launch goroutine holds the per-PR slot until `launchReview` returns, which now includes the full retry budget worst case (~31s). The busy semaphore (`busy=1`) has already been released when the goroutine launched, so the next `tick` runs unaffected and processes other PRs. The per-PR slot table (issue #1481 slice C) keeps the trigger from being re-launched while the post retries are in flight — `acquirePRSlot` returns false on the next tick for the same PR until the goroutine completes.

**Operator escape:** a stuck pending entry (e.g. `gh` outage longer than 31s) can be cleared by removing `decision.md` from the run folder; the next tick observes the missing-decision.md condition via the S4 walker's stale-entry branch and falls through to the launch path. See `internal/review/daemon.go::tryRehydratePost` for the existing operator escape.

**Why not longer backoff or more attempts?** 5 attempts × 16s = 31s, which already approaches the `PollingInterval` (30s). A larger budget would risk tripping the per-PR slot from being held past a tick boundary, complicating the launch path's invariants. The S4 rehydrate walker is the correct escape for sustained `gh` outages; the inline retry budget is sized for the typical transient case (single-digit second blips).

**Tests:** `TestDaemon_PostRetriesOnTransientFailure`, `TestDaemon_PostFinalFailure_RegistersPendingPost`, `TestDaemon_PostFinalFailure_NextTickRehydrates`, `TestDaemon_PostRetry_CtxCancelStopsRetrying` (in `internal/review/daemon_s6_test.go`); the seam-3 `TestDaemon_S3_FailedPost_FallsBackToPending` was renamed from `TestDaemon_S3_FailedPost_FailsClosed` and updated to assert the new pending contract.

## Blocked by

None - can start immediately

## Runtime Context

- You are running inside a Sandman-created worktree.
- Current branch: `sandman/380-adr-0013-sandman-review-daemon-and-guard`
- Source branch: `sandman/380-adr-0013-sandman-review-daemon-and-guard`
- Base branch: `main`
- Review command: `/oc review`

