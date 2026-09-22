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

The implementor-side delegated review loop waits according to the project
`review_timeout` setting (default `1800` seconds, minimum `240`). The value is
carried in the current AgentRun Task and is independent of the review daemon's
reviewer AgentRun lifetime and lifecycle re-evaluation. Use
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
implementor skill writes one request envelope under `.sandman/state/` and calls
the versioned `review-wait-v1.sh` entry point. Re-entering with the same
trigger reuses that request; a later confirmed trigger is a new request, even
on the same pull request. The wait returns structured transport state and raw
response evidence plus a request-scoped `review-classification/v1` envelope.
That envelope preserves the existing top-level, formal-review, and inline
sources while associating them with one trigger and head; existing approval and
feedback rules consume it without adding natural-language inference.

When it sees a matching comment authored by the GitHub user authenticated to the daemon, it launches a review AgentRun and posts the result back to the pull request. Requests from other users are ignored. Use direct review for manual or CI-driven reviews that should not be tied to the daemon's authenticated user.

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

## Review output

The review agent writes its response to the review worktree's `decision.md`. The daemon reads that file, removes self-triggering review-command text, and posts the final body as a pull-request comment.

Review runs appear in:

- `sandman status`
- `sandman history`
- `sandman portal`
- `.sandman/batches/<batch-id>/runs/<run-id>/`

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

The default prompt requires the `sandman-code-review` skill for daemon reviews and explicitly forbids `sandman-pr-review`, which owns the implementor-side review loop (posting `/sandman review` and driving iterative approval). The daemon reviewer is **read-only**: it reports mergeability or CI problems as findings but never fixes code, pushes, merges, posts GitHub comments, or requests another review. Keep that boundary when you edit the template — a review prompt that tells the reviewer to orchestrate the pull request will conflict with the daemon's own posting workflow.

The template must not contain stray `{{...}}` literals beyond the placeholders above: an unknown key fails the render and the review is not launched.

## Complexity analyzers

Every built-in build-tools preset provisions a pinned, non-interactive complexity analyzer for the reviewer's `### Metrics` step. The inventory is recorded in the scaffolded `.sandman/Dockerfile` metadata line `# sandman analyzers: <name>@<version>,...`; presets with no analyzers render `# sandman analyzers: manual`. The reviewer reads that line, verifies each tool with `command -v <tool>` / `<tool> --version`, and runs the analyzer matching the PR's language.

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
