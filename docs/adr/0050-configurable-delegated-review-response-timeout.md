# ADR-0050: Configurable delegated review response timeout

## Status

proposed

## Context

The implementor-side delegated review loop needs a bounded response budget, but
reviewer AgentRuns can take different amounts of time across repositories and
providers. A fixed budget cannot be tuned per repository or for an unusually
large change. The effective value also needs to survive retries and be refreshed
for continuations without using the globally installed shared skill as mutable
project configuration.

## Decision

Represent the implementor-side delegated review response budget as the
top-level `review_timeout` configuration value, measured in integer seconds.
Use 1800 seconds when the value is absent and reject values below 240 seconds.
An explicit run or continuation override takes precedence over the current
repository configuration, which takes precedence over the default.

The effective value is an absolute wall-clock deadline for one fresh delegated
review request. The deadline starts immediately after the trigger post is
confirmed and includes GitHub/API calls, command overhead, parsing, and polling
sleep. A new trigger starts a new deadline; retries, continuations, and ordinary
polls of the same trigger reuse the persisted `(head_sha, trigger_id,
started_at, deadline_at)` state.

Render the effective value into the current AgentRun task context and expose it
as the `REVIEW_TIMEOUT` prompt key. Retries of the same AgentRun retain the
effective value; a genuinely new review request replaces its persisted
wall-clock deadline. Continuations resolve current flags and configuration and
supersede stale persisted timeout wording. Run-started and run-continued events
record the effective value for diagnosis.

Keep the shared installed skill repository-independent. The skill reads the
current task's value, falling back to 1800 seconds only for older task context,
so synchronization for one repository cannot alter another repository's active
runs. Preserve the existing 120, 60, 60, then 30-second polling cadence and
allow a command that completes exactly at the configured deadline. A cooperative
direct-child watcher plus a post-command clock check prevents slow API calls
from silently restoring expired review time.

## Consequences

### Positive

- Operators can tune delegated review waiting per repository or run.
- Retries and continuations use an explicit, observable effective policy.
- Shared skill synchronization cannot become hidden global configuration.

### Negative

- Configuration, prompt rendering, event payloads, and skill guidance must stay
  consistent when the policy changes.
- Older task files require the skill's 1800-second compatibility fallback.
- A larger budget can keep an implementor AgentRun active longer.
- Integer-second process watching can observe a scheduler overrun of up to one
  clock second, but the loop never intentionally starts a command or sleep
  after its deadline.
