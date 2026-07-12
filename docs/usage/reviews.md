# Reviews

Sandman can run review agents against open pull requests. You can trigger reviews directly from the CLI or keep a daemon running that watches for the configured review command.

## Direct review

Run one review and exit:

```bash
sandman review 42
```

Review several pull requests:

```bash
sandman review 42 43
sandman review 42:45
```

Override the review agent or model for one invocation:

```bash
sandman review 42 --agent opencode --model opencode/big-pickle
```

## Review daemon

Run without positional arguments to start the daemon:

```bash
sandman review
```

The daemon polls open pull requests for the configured review command, which defaults to:

```text
/sandman review
```

When it sees a matching comment, it launches a review AgentRun and posts the result back to the pull request.

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

Or override it for one daemon/direct invocation:

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

## See also

- [Commands Reference](commands.md#sandman-review) — full `sandman review` flags
- [Configuration](configuration.md) — `review_command`, `review_agent`, `review_model`, and `parallel_reviews`
- [Portal](portal.md) — watching review runs in the browser
- [Sandman Skills](skills.md) — how the review command is injected into shared skills
