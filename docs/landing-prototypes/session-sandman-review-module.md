# Sandman Review

Sandman Review is the review module in the AFK delivery loop. It exists so PR review is a gate, not cleanup after code generation.

It works like OpenCode's GitHub `/oc` flow, but local: the trigger is `/sandman review`, and Sandman runs the configured reviewer on the developer's machine instead of handing the work to a GitHub Actions runner.

The module can run `sandman review` as a daemon that responds to review requests. It listens for review activity, runs the configured review workflow, and keeps pushing the PR through feedback, repair, checks, and merge readiness.

Teams can still bring another reviewer: OpenCode, Codex, Copilot, CodeRabbit, or a human reviewer. The important part is not the reviewer brand. The important part is that the delivery loop waits for review instead of treating generated code as done.

In the Sandman story, the order is:

1. The implementation agent builds and self-reviews.
2. The PR opens with logs, task context, and checks attached.
3. Sandman Review watches for review requests and feedback.
4. The loop repairs issues, waits for green checks, and only then reaches merge readiness.
