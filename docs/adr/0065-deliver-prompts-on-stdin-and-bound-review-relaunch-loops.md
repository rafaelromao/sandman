# ADR-0065: Deliver prompts on stdin and bound review relaunch loops

## Status

accepted

## Context

Four behaviours chained into a loop of several hundred failed review launches on
one pull request:

- The built-in commands passed the prompt as one argument. Linux limits one
  argument to 128 KiB, and a review prompt carries every prior review entry.
- Health repair worked around that by replacing the review agent's built-in
  command, as [ADR-0056](0056-claude-code-preset-behind-an-agent-strategy-seam.md)
  allows an operator to. A custom command has no output parser.
- Without a parser, OpenCode's usage-limit response is a raw JSON record that
  the line rule did not match, so the daemon never entered its quota pause.
- Every review launch writes its own state, so the attempt count behind the
  launch backoff of [ADR-0029](0029-review-daemon-stateless-on-age-stateful-on-comment.md)
  was always one.

## Decision

- Every built-in command reads the prompt on stdin. No built-in command passes
  prompt content as an argument.
- A configured command equal to a preset's former built-in command still means
  the built-in command. Former commands are pinned per preset; the config file
  is not rewritten.
- A launch of a custom OpenCode command that serves no work item (a review, the
  quota probe or another prompt-only run) also recognises a raw `type: "error"`
  record carrying the usage-limit message. The record counts whatever status the
  command exits with, because a wrapper around the agent can exit zero.
  Implementation launches are unchanged, because a recognised limit changes
  their admission and lifecycle selection.
- The launch attempt count belongs to the request revision and is carried across
  Runs, daemon restarts and cache refreshes: the highest recorded count and its
  next-attempt time. The backoff doubles from ten seconds to a ten-minute cap.
  There is no attempt limit. A quota pause does not advance the count, and an
  edited request starts a new one.
- Health repair is told not to add or change the command of a built-in preset,
  and the review daemon warns at start when its agent runs one.

## Consequences

A prompt of any size the agent itself accepts now launches. A custom command
still has to read the prompt itself and should read it from stdin.

A request that keeps failing costs one launch every ten minutes. After a repair,
the retry can wait that long. Deleting one Run's `review-state.json` no longer
gives a request a fresh count; editing the request comment does.

Usage limits are matched per line. A container's terminal merges stdout and
stderr before Sandman reads them, so output left unterminated immediately before
a usage-limit line hides it, for built-in and custom commands alike; the launch
then follows the ordinary path for its exit status. Recognising it would take
searching inside lines, which quoted output could trigger, or a different
container execution model.

An agent that ends without writing a decision is not a recorded launch failure
and is outside this decision.
