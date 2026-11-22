# ADR-0064: Runtime-owned CI reruns in a recovery window

## Status

accepted

## Context

ADR-0060 draws the managed handoff boundary: the implementor owns local
verification and publication, and the runtime only observes external CI and
review. The runtime never changed external state on its own account; the
implementor's single, helper-mediated rerun of a diagnosed transient CI failure
was the only rerun in the system.

A confirmed incident showed what that boundary leaves unowned. Current-head CI
failed because the hosted provider's free usage limit was reached. The
implementor diagnosed the failure as transient and spent its one authorized
rerun, which failed the same way. After that:

- the implementor had no source to repair;
- it could not request review while CI was red;
- it had used its rerun, and a usage limit cannot clear within one immediate
  rerun;
- the runtime never reruns CI.

The next relaunches found nothing to do and the Run entered a stalled
continuity hold that only a new head, a new review request or response, or a
new CI execution could lift. Nothing produced any of them. The Run sat there
for about 17 hours until an operator aborted it. With the exhausted-work rule
that ends such a hold with `REMEDIATION_BUDGET_EXHAUSTED`, the same stall would
end the Run instead. That is clearer, but it still gives up on a failure that
would recover on its own.

## Decision

**A runtime-owned rerun is an explicit, narrow exception to ADR-0060.** The
runtime reruns one thing: the failed job the implementor diagnosed as transient,
and its dependent quality gate, on the workflow run the implementor's typed rerun
receipt identifies. It does not classify CI logs, rerun a failure it was not
handed, dispatch a replacement workflow, or weaken a required check. Everything
else about ADR-0060 stands: the implementor still owns diagnosis, local
verification and publication, and the implementor's allowance is still one
diagnosed rerun per failed execution. After that rerun fails again the
implementor returns and leaves later reruns to the runtime.

**A CI recovery window.** The rerun runs in a recovery window, the bounded
external-recovery wait the GitHub recovery window (ADR-0053's bounded waits,
issue #2938) introduced and issue #3009 made reusable. A CI repair that can no
longer be relaunched (its lifecycle relaunch allowance is used up, or a relaunch
would repeat a stalled signature) enters the window instead of a hold, but only
when both of these hold:

- a typed transient rerun receipt recorded for the logical Run (the Run and the
  Runs it continued) matches the live settled failure: repository, pull request,
  head, workflow run and diagnosed job, with every failed check covered by that
  job and its quality gate; and
- the GitHub client can rerun a workflow job.

Every other stalled CI repair keeps the outcome the continuity table gives it.
The qualifying stall has its own table row (`ci-failure` / `CI_FAILURE` /
transient stall, outcome wait) that is consulted in front of the generic stall and
allowance rows, so it behaves the same whether or not those rows have flipped from
a hold to the exhausted-work terminal; a table that does not wait there renders
the ordinary stall.

**Window rules.** The wait is `run.await` with `await_reason: ci-recovery` and
`ci_recovery` evidence (window start, next attempt, bound, last status, attempts,
last rerun outcome, the diagnosed target). A fresh window attempts at once, then
hourly. Before each attempt the runtime reads githubstatus.com scoped to GitHub
Actions: a degraded Actions component or a major or critical incident skips the
attempt; a healthy, unreachable or absent status page lets it run. An attempt
looks the pull request up and then judges it:

- the diagnosed job failing again (a new execution of the same head, run and
  job) reruns it through the GitHub client seam and keeps the window open;
- CI of the same head still running keeps the window open and reruns nothing;
- anything else ends the window and the normal lifecycle decides: green CI
  proceeds to review-request delivery, review and merge, while a new head, a
  different failing check, a merge conflict or any other outcome returns to
  implementor repair under the ordinary rules;
- a failed lookup is a failed attempt of the window itself, so no other wait
  supersedes it and its bound survives.

Observation between attempts makes no GitHub call and does not re-record the
wait, so a green rerun is noticed at the next attempt. Twenty-four hours from the
window's start, skipped hours included, end the Run `failure` with the permanent
`CI_RECOVERY_EXHAUSTED`, carrying the `ci_recovery` evidence and no wait marker;
no rerun is submitted at or after the bound, but a green result at the bound
still counts. The bound is the wait's scheduler operation deadline. The managed
30-minute per-execution CI bound is not enforced for a runtime rerun inside the
window; the 24-hour bound supersedes it, as the GitHub window's bound supersedes
retained review and CI deadlines.

**Safety of the mutation.** A rerun attempt performs at most one mutation and
reads GitHub back. The client verifies the pull request, workflow run, tested
commit and job against live state before it submits, addresses the job by name
in the latest attempt (job database IDs change with every attempt), answers
`waiting` instead of submitting while the workflow or job is still running, and
never submits a second time within an attempt, even when the outcome is
uncertain. A restart that re-attempts after a crash between the submission and
its record therefore finds the new attempt running and waits. An unreadable
event log proves neither a window nor a receipt, so it opens no window and
submits no rerun: the Run fails closed into the stalled-ledger hold.

**A superseded window resumes.** Another wait of the same Run can become its latest
event (a hold written while the event log could not be read, a GitHub outage). A
later qualifying stall of the same failure resumes the window with its original
start, attempts and schedule, or ends the Run when its bound has passed; it never
restarts the 24 hours.

**Restart, continue, abort.** Restart rehydrates the window and its next attempt
from the event fold. `sandman run <n> --continue` is a new RunID: it finds the
rerun receipt through the `run.continued` chain, opens a fresh window with a new
start and bound, and attempts at once. Explicit abort ends the wait at once with
no lookup and no rerun.

## Alternatives considered

- **Leave the Run in the stalled hold.** It parked for 17 hours on a failure that
  would have recovered.
- **End the Run with `REMEDIATION_BUDGET_EXHAUSTED`.** Clearer, but it gives up on
  a transient failure and asks an operator to rerun it by hand.
- **Let the implementor rerun repeatedly.** Each rerun spends an agent session on
  a failure with nothing to repair, and a usage limit cannot clear on an
  immediate rerun.
- **Classify CI logs in the runtime.** Diagnosis stays with the implementor and
  its typed receipt; the runtime only repeats what it was handed.
- **Poll the rerun every 30 seconds.** It contradicts "observation between
  attempts makes no GitHub call" and re-records the wait on every poll. A green
  rerun is instead noticed at the next hourly attempt.

## Consequences

### Positive

- A transient CI failure that outlasts the implementor's one rerun recovers on
  its own, or ends the Run with a classified permanent reason after 24 hours.
- The runtime's one mutation is narrow, typed and fail-closed.
- The window reuses the GitHub recovery window's mechanics, scheduler identity
  and restart behavior; only its verdicts, reasons and continuity rows are CI's.

### Negative

- The runtime now changes external state. The rerun is bounded to one job of one
  workflow run per attempt and to 24 hours per window.
- A green rerun can wait up to an hour before the Run resumes the normal
  lifecycle.
- `ci-recovery` is a second bounded recovery `await_reason`. Only the batch
  scheduler and persistence special-case a recovery wait, and they now do so
  through one registry of recovery windows; the event fold and the Portal,
  status and history projections treat it as an ordinary waiting Run, which a
  projection test pins.
