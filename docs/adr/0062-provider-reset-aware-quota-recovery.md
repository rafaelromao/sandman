# ADR-0062: Schedule implementation quota recovery from provider reset evidence

## Status

accepted

## Context

Implementation agents can report a reset time when their provider rejects work
for a resettable quota limit. A fixed ten-minute probe cadence can resume a run
well after that reset. Treating arbitrary output as a reset time would be unsafe:
assistant prose, incomplete timestamps, timezone ambiguity, or stale evidence
must not extend a wait or cause a tight retry loop.

The existing five-hour implementation recovery allowance measures completed
polling, not wall-clock time. Re-entry must preserve that allowance, the final
boundary probe, ordinary retries, session reuse, and the separation between
batch-local implementation admission and the review daemon's provider-wide gate.

## Decision

- Reset extraction belongs to the selected agent strategy and uses only
  supported authoritative provider output. Claude Code supports
  `rate_limit_event.rate_limit_info.resetsAt` only on a rejected rate-limit
  event, and a strict reset clock in the final top-level error `result` record
  containing a recognized quota error (12-hour clock with optional minutes and
  an explicit IANA timezone or UTC). Assistant/tool prose, stderr, warning-only
  events, and unrelated result records are not evidence. Clock-only text uses
  the provider-local current date; past clocks are not rolled forward. The
  strategy rejects malformed, conflicting, stale,
  ambiguous/nonexistent daylight-saving times, and more-than-seven-day-future
  evidence. Unsupported strategies and untrusted evidence provide no reset.
- A trusted reset is distinct from Sandman's
  `usage_limit_deadline_unix_seconds`, which remains a diagnostic estimate of
  five-hour allowance exhaustion. Await and ready event evidence persists the
  trusted `usage_limit_provider_reset_unix_seconds` separately.
- Implementation recovery schedules the next probe at the trusted reset, or
  uses the existing ten-minute fallback when reset evidence is absent or
  untrusted. A reset already passed when a repeated quota failure is evaluated
  is discarded in favor of the fallback, preventing a tight loop. Every wait is
  capped by the remaining five-hour allowance.
- Only the scheduled duration of a wait that returns successfully is added to
  `usage_limit_waited_seconds`, capped at five hours. Cancellation or
  interruption adds zero. Capacity delay and process downtime add zero. After a
  restart, a valid durable `RunWait.NextPollAt` is honored; if the checkpoint is
  absent, recovery reconstructs the due time from the current event evidence.
  Only a successfully completed live remainder accrues.
- The same RunID and supported agent session continue through quota recovery.
  The final boundary probe and configured ordinary fresh-session retries remain
  available after the allowance is consumed. Quota schedules keep stable
  `quota:<RunID>` identity and no hard operation deadline.
- Implementation quota admission remains batch-local. Reviewer quota recovery
  remains daemon-wide and keeps its fixed ten-minute schedule; implementation
  reset evidence does not alter reviewer policy.

## Consequences

### Positive

- Implementation probes can run near a trustworthy provider reset without
  spending most of the delay on an unnecessary fixed-cadence interval.
- Event evidence and the durable wait checkpoint preserve scheduling and
  completed-wait accounting across re-entry while keeping the exhaustion
  estimate, provider reset, and operation ownership distinct.
- Missing or rejected reset evidence preserves the established fallback, and
  interrupted waits cannot consume allowance that was not fully waited.

### Negative

- Provider-specific output parsing and timezone validation add strategy and
  regression-test obligations.
- A long provider reset can leave the run waiting for up to the remaining
  five-hour allowance before the final boundary probe; this preserves the
  existing bound rather than treating the provider reset as an operation
  deadline.

### Neutral

- No mutable lifecycle status or new event type is introduced. Event history
  remains authoritative; `wait.json` remains an atomic ownership/schedule
  checkpoint.
- The review daemon's quota gate and cadence are unchanged.
