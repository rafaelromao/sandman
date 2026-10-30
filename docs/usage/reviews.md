# Reviews

Sandman runs review agents through a daemon that watches open pull requests for the configured review command.

## Review daemon

Run this command to start the daemon:

```bash
sandman review
```

Set an independent review model variant in project configuration or override
it for one daemon process:

```bash
sandman config set review_variant "provider/foo bar"
sandman review --variant "provider/other"
```

The CLI value wins when supplied. Both configured and CLI values are trimmed
but otherwise treated as opaque provider-specific text. Empty values do not
add a variant flag to the OpenCode command.

The daemon polls open pull requests for the configured review command, which defaults to:

```text
/sandman review
```

The implementor-side delegated review request uses the project `review_timeout`
setting (default `1800` seconds, minimum `240`). The value is carried in the
current AgentRun Task and is independent of the review daemon's reviewer
AgentRun lifetime and lifecycle re-evaluation. Use
`sandman config set review_timeout <seconds>` for repository policy or
`sandman run --review-timeout <seconds>` for one run or continuation.

While this request is active, the implementation AgentRun retains logical row
ownership but releases the execution slot and container lease between
observations. Sandman repeats the final observation interval and holds
dependents rather than starting another attempt. Matching current-request
feedback may resume the AgentRun; stale, mismatched, malformed, or invalid
evidence is diagnostics-only and leaves observation active. Explicit
cancellation produces the normal aborted outcome and does not launch a
dependent.

After a configured trigger is posted and confirmed for the current head, the
managed runtime owns the canonical request record, observation, evidence,
waiting, resume, and terminal decision. The managed agent does not invoke the
standalone cycle. A standalone `sandman-run` session instead invokes
`sandman-review-request` through `sandman-review-cycle`, keeps cycle state in
memory, and starts fresh from live pull-request state after a restart. It
returns structured transport state and raw response evidence plus a
request-scoped `review-classification/v1` envelope. That envelope preserves the
existing top-level, formal-review, and inline sources while associating them
with one trigger and head; existing approval and feedback rules consume it
without adding natural-language inference.

The managed sequence is request, wait, current-head feedback resume, renewed
request, current-head approval, merge verification, and dependent release.
Waiting preserves the RunID and dependency ownership while releasing execution
capacity; it does not consume a generic retry. A restart rehydrates the event
and wait records, while cancellation terminalizes unfinished owned rows and
prevents later probes or launches. Failed publication is recovered from the
durable redacted decision body, and legacy evidence is accepted only at the
validated request/head boundary.

`{{REVIEW_COMMAND}}` is substituted from repository configuration when the
shared skills are synchronized. `{{REVIEW_TIMEOUT}}` is supplied per managed
run or standalone session and is never shared global skill state.

When it sees a matching comment authored by the GitHub user authenticated to the daemon, it launches a review AgentRun and posts the result back to the pull request. Requests from other users are ignored. Use direct review for manual or CI-driven reviews that should not be tied to the daemon's authenticated user.

The terminal running `sandman review` shows two streams:

- **stderr** carries the daemon's own decisions: failed launches with a short
  cause and the time the request is retried, health repairs being launched,
  paused, or out of budget, and other operational errors.
- **stdout** carries the transcripts of the review and repair agents it runs.

While the daemon works on a request, it adds a 👀 reaction to the comment and
the pull request, and removes it when the launch ends. If the reaction
disappears without a decision being posted, the stderr line for that request
says why. `sandman attach` streams the same daemon lines from another terminal.
Errors remain visible on stderr. Locally repairable failures are also kept in
`.sandman/reviews/health-repair.json`.

## Review command guard

By default, `sandman init` writes this config:

```yaml
review_command: /sandman review
```

Runs that use `/sandman review` expect the review daemon to be available. Start `sandman review` before launching implementation runs when you want the full implement-review loop.

To opt out of the daemon guard, set a different review command that does not contain `/sandman`:

```bash
sandman config set review_command "/oc review"
```

Changing `review_command` also regenerates the installed shared Sandman skill tree. If that tree has local edits, Sandman prompts before overwriting in a TTY and fails in non-interactive mode.

## Concurrency and sandboxing

Review runs have their own concurrency setting:

```bash
sandman config set parallel_reviews 2
```

Or override it for the daemon invocation:

```bash
sandman review --parallel 2
```

The review command also accepts the usual sandbox controls:

```bash
sandman review \
  --sandbox podman \
  --container-capacity 1 \
  --max-containers 2
```

## Health repair

When a locally repairable operation fails (for example a broken credential helper,
container image, review worktree, or decision artifact), the daemon records the
failure in `.sandman/reviews/health-repair.json` and launches a diagnostic repair
agent. The agent gets the failed operation, its error output, and the paths to the
event log and run artifacts.

GitHub connection errors, call timeouts, and temporary server failures use bounded
read retries and normal daemon re-observation; they launch no local repair agent.
Each read allows at most three attempts, each capped by the GitHub call timeout
and the caller's unchanged deadline. Read recovery does not renew CI or review
deadlines, spend implementation retries, or authorize a merge from stale facts.
Writes such as posting review decisions are not automatically retried after an
availability failure: their durable publication recovery checks for an existing
post before trying again. GitHub rate limits, agent usage limits, and operator
cancellation retain their separate recovery behavior and launch no health repair.

The repair agent differs from the reviewer:

- **It runs on the host**, at the repository root, with the configured review
  agent and model. It does not run in a container, because the container image
  or sandbox may be what is broken. The agent's binary and its authentication
  must be available on the host; for Claude Code, see
  [Agent Compatibility > Authentication](agent-compatibility.md#authentication).
- **It may change operational files.** The reviewer only reads the pull request
  and writes its decision. The repair agent is told to change only
  operator-owned files under `.sandman/` (the Dockerfile, `config.yaml`, prompt
  templates), never Sandman-owned state such as `events.jsonl` or `batches/`,
  and to report the command for you when a step is not permitted.
  - Claude Code: the launch allows edits under `.sandman/` and keeps every
    other prompting call denied (`--allowedTools 'Edit(/.sandman/**)'
    --permission-mode dontAsk`), with no permission bypass. The repair cannot
    run shell commands that need approval, such as an image rebuild; the daemon
    rebuilds the image on the next review attempt. Managed Claude Code settings
    with `allowManagedPermissionRulesOnly` ignore this rule, so there the repair
    can only diagnose and you apply its reported fix. Do not loosen managed
    policy to work around it.
    - The repair scope replaces a configured `permission_mode`, including a
      read-only mode such as `plan`: the repair always launches with
      `dontAsk` and the `.sandman/` edit rule.
    - A custom `command` is launched as written: Sandman adds no edit scope,
      permission mode, or bypass. Any permission flags the command carries
      itself, a bypass included, apply to repairs unchanged, so a custom
      command must carry its own permission rules.
  - OpenCode: the repair follows your own OpenCode permission configuration. It
    is not narrowed to `.sandman/`, and Sandman cannot detect refused actions.

Each failure episode allows three repairs, with a cooldown that grows after each.
A repair whose actions were refused is recorded as `unresolved: repair actions
denied (<tools>)`, never as a successful repair. The daemon does not relaunch a
repair for the same failures with the same agent and model; it logs `health
repair paused: ...` once and waits for you. A changed failure, a new review
request, or a different review agent or model admits a repair again. The record
resets once every failed operation succeeds.

A repair whose agent cannot start on the host, for example because its binary is
not on the daemon's `PATH` or it is not authenticated there, is recorded as
`unresolved: prompt-only run failed`. The cause is in the repair Run's log under
`.sandman/batches/<repair-run-id>/runs/<repair-run-id>/` (the daemon log names
the repair Run ID), not in the daemon log itself. It counts toward the three
repairs of the episode.

Whatever the repair did, the daemon retries the original operation through its
normal path. If review launches fail with `mise: not found`, see
[Troubleshooting](../help/troubleshooting.md#container-image-build-fails-with-mise-not-found).

## Review output

The review agent writes its response to the review worktree's `decision.md`; that file is only a handoff. The daemon copies it into the review's run folder, removes self-triggering review-command text, and saves the exact text it will post as the run's pending publication (`pending-publication.json`) before posting it as a pull-request comment. If posting fails or the daemon restarts, it posts that saved text later, first checking that it was not already posted, and removes the pending publication only once the post is confirmed. Deleting the worktree's `decision.md` does not cancel a saved publication.

Review runs appear in:

- `sandman status`
- `sandman history`
- `sandman portal`
- `.sandman/batches/<batch-id>/runs/<run-id>/` (including the copied `decision.md` and, until the post is confirmed, `pending-publication.json`)

## Review prompt

`.sandman/reviews/review-prompt.md` is the **live daemon template** for pull-request reviews. `sandman init` writes it into `.sandman/reviews/` alongside `.sandman/reviews/quality-rules.md`; the review daemon reads it back for every review run and renders each launched reviewer request from it. Edit the file in the repository to make repository-specific prompt instructions govern reviewer behavior — later reviews pick up the change without a daemon restart.

The template is PR-agnostic. PR context is injected per run through these placeholders:

| Placeholder | Meaning |
|-------------|---------|
| `{{PR_NUMBER}}` | Pull-request number |
| `{{PR_TITLE}}` | Pull-request title |
| `{{PR_BODY}}` | Pull-request body |
| `{{ACCEPTANCE_CRITERIA}}` | Linked work item's acceptance criteria |
| `{{REVIEW_FOCUS}}` | The review focus after `/sandman review` |
| `{{RUN_DIR}}` | The review worktree directory (where `decision.md` is written) |
| `{{PRIOR_REVIEW_EXISTS}}` | `YES`/`NO` — whether a prior review exists |
| `{{PRIOR_REVIEW_CONTEXT}}` | Prior review entries supplied by the daemon |

The file is preserved across re-runs of `sandman init`. When it is missing, the daemon atomically re-materializes it from the built-in default before rendering the next review, so the shared template always exists on disk. An empty or whitespace-only template is treated as if it were missing and renders the built-in default.

### Reviewer versus implementor roles

The default prompt requires the `sandman-code-review` skill for daemon reviews and explicitly forbids `sandman-pr-review`, `sandman-review-cycle`, and `sandman-run`. `sandman-pr-review` is only a compatibility facade over the standalone cycle; the daemon reviewer is **read-only** and must not enter either standalone workflow. It reports mergeability or CI problems as findings but never fixes code, pushes, merges, posts GitHub comments, or requests another review. Health repairs are different: they are separate modifying prompt-only runs on the host (see [Health repair](#health-repair)). Keep that boundary when you edit the template — a review prompt that tells the reviewer to orchestrate the pull request will conflict with the daemon's own posting workflow.

The template must not contain stray `{{...}}` literals beyond the placeholders above: an unknown key fails the render and the review is not launched.

## Complexity analyzers

Every built-in build-tools preset provisions a pinned, non-interactive complexity analyzer for the reviewer's `### Metrics` step. The inventory is recorded in the scaffolded `.sandman/Dockerfile` metadata line `# sandman analyzers: <name>@<version>,...`; presets with no analyzers render `# sandman analyzers: manual`. The reviewer reads that line, verifies each tool with `command -v <tool>` / `<tool> --version` (for `clippy`, `cargo clippy --version`, since the standalone binary is `cargo-clippy`), and runs the analyzer matching the PR's language.

| Preset | Analyzer | Version | Provisioning | Version floor | Invocation |
|--------|----------|---------|--------------|---------------|------------|
| `go` | gocognit | v1.2.1 | `go install` | go >= 1.24 (omitted below, cognitive manual) | `gocognit -over 0 -avg <dir>` |
| `go` | gocyclo | v0.6.0 | `go install` | none | `gocyclo -over 0 <dir>` |
| `dotnet` | dotnet-crap | 0.1.1 | `dotnet tool install -g` | dotnet >= 8.0 (omitted below, manual) | `dotnet-crap analyze <dir>` |
| `node` | eslint | 10.8.0 | `npm install -g` | node >= 20.19 (omitted below, manual) | `eslint --no-config-lookup --rule 'complexity: [warn, {max: 0}]' <dir>` |
| `python` | radon | 6.0.1 | `pip3 install` | none | `radon cc -s <dir>` |
| `elixir` | credo | 1.7.19 | `mix escript.install` | none | `credo suggest` |
| `ruby` | flog | 4.9.4 | `gem install` | none | `flog <dir>` |
| `rust` | clippy | bundled toolchain | `rustup component add clippy` | none | `cargo clippy -- -W clippy::cognitive_complexity` |
| `java` | pmd | 7.27.0 | release zip under `/opt` | none | `pmd check -d <dir> -R "category/java/design.xml/CyclomaticComplexity,category/java/design.xml/CognitiveComplexity" -f text` |
| `generic` | — | — | none | — | manual |

Ecosystem limitations and fallbacks:

- **go < 1.24** — gocognit requires a go 1.24 toolchain and is omitted; the reviewer uses gocyclo for cyclomatic complexity and assesses cognitive complexity manually.
- **.NET < 8.0** — dotnet-crap targets .NET 8.0+ and is omitted; the whole complexity step falls back to manual assessment.
- **node < 20.19** — eslint 10 requires Node 20.19+ and is omitted; complexity falls back to manual assessment.
- **node / python / ruby / elixir** — the provisioned tools are cyclomatic analyzers; cognitive complexity is assessed manually.
- **rust** — clippy only reports `clippy::cognitive_complexity` in a cargo project; a non-cargo rust repo falls back to manual assessment.
- **generic** — never provisioned; complexity is always manual.

Complexity analyzers are a **reviewer-only** capability. They are not invoked anywhere else in the pipeline — no CI status check, no pre-commit hook, and no build-time validation depends on them.

## See also

- [Commands Reference](commands.md#sandman-review) — full `sandman review` flags
- [Configuration](configuration.md) — `review_command`, `review_agent`, `review_model`, `review_variant`, and `parallel_reviews`
- [Portal](portal.md) — watching review runs in the browser
- [Sandman Skills](skills.md) — how the review command is injected into shared skills
