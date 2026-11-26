# AgentRun state machine

This is the canonical lifecycle contract. It separates **lifecycle**, **readiness**,
**admission**, and **artifact ownership**. The append-only event log is the only
authority for lifecycle and terminal outcomes. Persisted ownership, schedules,
operation budgets, and leases support execution and recovery; they are not an
alternate mutable status store.

## Lifecycle

```text
queued → running ⇄ waiting → success / failure / aborted
   └──────────────────────→ blocked / aborted / pre-start failure
```

- `queued` is unfinished initial admission marked `initial_admission: true`.
  The row has not started and counts zero active time. Untagged historical
  `run.queued` records and explicit `terminal_placeholder` records retain their
  terminal skipped-placeholder meaning; append-only history needs no migration.
- `running` means the row has execution admission. A retry or continuation is
  still the same lifecycle when it keeps its RunID.
- `waiting` is non-terminal suspension after start: an external operation is
  resolving, or the next executable action is ready but admission is delayed.
  A started run **never returns to queued**.
- `success`, `failure`, `blocked`, and `aborted` are terminal. `blocked` belongs
  exclusively to dependency outcomes. A terminal outcome cannot be replaced by
  stale scheduler/readiness evidence or artifact availability.
  Terminal `run.finished` payloads carry no active `await`, `await_reason`, or
  `gate` markers; retained gate diagnostics use `external_gate`.
- `unknown` means insufficient lifecycle evidence; no artifact or dead socket
  may invent a completion.
- `reviewing` is a Portal aggregate label for a non-terminal implementation with
  an active linked review, independently of whether the implementation is
  running or waiting. Terminal parents remain terminal.

## Required transitions

| Situation | Required transition |
| --- | --- |
| Unstarted row awaiting first admission | Stay **queued**, unfinished and cancellation-aware |
| Started run yields for current-head queued/running CI | **Running → waiting**; release execution capacity |
| Confirmed review request pending, reviewer not yet started | Stay **waiting** |
| Pending review request, but aggregate PR looks clean | Stay **waiting**; no approval-based resume |
| Current-request actionable feedback | Resume implementation when capacity permits |
| Current-request approval with green CI | Resume merge work when capacity permits |
| Current-head blind approval with green CI and a mergeable head | Resume merge work when capacity permits |
| Green current-head PR, no current-head blind approval, no configured review command | Resume one implementor-owned blind round (`BLIND_REVIEW_REQUIRED`) when capacity permits |
| Blind round leaves head and evidence unchanged, or its relaunch allowance is exhausted | Finish **failure** (`REMEDIATION_BUDGET_EXHAUSTED`) naming the `blind-review-required` gate, with a next action of `sandman run --continue`; never treat the missing publication as an expired delegated request |
| Blind reviewer cannot be spawned | Fail closed with `REVIEW_SUBAGENT_UNAVAILABLE`; never self-approve or post a delegated trigger |
| Next action ready, capacity occupied | Stay **waiting**, reason capacity |
| CI failure | Resume bounded repair; on stall or exhaustion, enter `ci-recovery` only with a matching confirmed transient rerun receipt, otherwise finish **failure** (`REMEDIATION_BUDGET_EXHAUSTED`) naming `ci-failure` |
| Diagnosed CI recovery reaches its bound without conclusive changed evidence | Finish **failure** (`CI_RECOVERY_EXHAUSTED`) with retained `ci_recovery` evidence |
| Merge conflict | Resume bounded repair; when the relaunch allowance leaves the head unchanged, stay **waiting** for external recovery (rc2 behaviour) |
| Missing PR, or an undelivered review request when a review command is configured | Perform bounded owned-work remediation (`run.retry` reason `lifecycle-owned-work`), then failure; never invent external waiting |
| Review request delivered by an attempt that exited non-success | Register it and stay **waiting**; spend no ordinary retry |
| Informal-feedback relaunch leaves head, request and responses unchanged | Finish **failure** (`REVIEW_FEEDBACK_STALLED`): the evidence names the feedback (source, locator, body) and the stalled resume signature, and the next action is `sandman run --continue`; never relaunch on the same evidence |
| Merge work (ready to merge) uses up its relaunch allowance, or a relaunch of it stalls | Finish **failure** (`REMEDIATION_BUDGET_EXHAUSTED`) naming the `ready-to-merge` gate, its last reason and the used-up signature; the next action is `sandman run --continue` |
| Review-request delivery stalls with a review command configured | Finish **failure** (`REMEDIATION_BUDGET_EXHAUSTED`) naming the `implementor-work-required` gate; exhausted delivery (the final ordinary attempt) is already terminal as `REVIEW_REQUEST_REQUIRED` |
| Informal feedback uses up its per-head relaunch allowance | Finish **failure** (`REMEDIATION_BUDGET_EXHAUSTED`) naming the `actionable-feedback` gate with the retained feedback; an unchanged relaunch finishes `REVIEW_FEEDBACK_STALLED` as above; genuine requested changes (formal `CHANGES_REQUESTED`, the review daemon's `changes_requested` verdict or an approval override) left unrepaired finish **failure** |
| Relaunch allowance exhausted on an expired CI wait (`CI_WAIT_TIMEOUT`) or merge-conflict repair | Stay **waiting** (`RESUME_STALLED`) |
| Review request expires with no usable response and no active resolver | Finish **failure** (`REVIEW_REQUEST_EXPIRED`) with the retained request identity; do not spend another implementation attempt |
| Review request expires while current-head CI, its matching active review Run with a live daemon, or a live-daemon quota pause is resolving | Stay **waiting**; accept eligible late current-head approval or actionable feedback before terminal expiry |
| An earlier binary parked an expired-request hold | Reobserve it; keep waiting only while one of the same resolvers is active, otherwise finish `REVIEW_REQUEST_EXPIRED` |
| An idle gate after owned work | Stay **waiting** (continuity hold) on an open current-head PR |
| Recognized provider usage limit | Wait and probe; do not spend ordinary retries immediately |
| Quota recovers | Resume limited run and reopen sibling admission |
| Quota recovers, then CI becomes pending | Restore ordinary CI observation without another agent launch |
| PR becomes verified merged while waiting | Finish **success immediately**, without acquiring an agent slot |
| PR closes without merging | Finish **failure** |
| Explicit operator abort | Abort all unfinished owned rows; prevent further preparation, retries, probes and launches |
| Same RunID resumes in another batch | Preserve active duration; update current ownership |
| New RunID for a manual continuation | Start a fresh clock |
| Linked reviewer active | Portal may show **reviewing**; underlying lifecycle remains inspectable |

## Continuation Task ownership

The Task in the preserved worktree is the agent-owned progress checkpoint.
Manual continuation, quota probes, CI/review resumes, and other runtime-owned
re-entry read that latest file immediately before preparing the next launch;
the immutable batch-start Task is never a recovery source. A missing or
unreadable Task stops continuation before agent launch with a non-success
outcome rather than falling back to stale prompt text or the default scaffold.
Ordinary retries and fresh context-recovery sessions continue to build from
the latest readable Task.

Review command, selected review mode, and effective delegated-review timeout
are runtime-owned settings. Continuation composition refreshes those values in
the Task's Runtime Context from current configuration, using the resolved
configured/empty selection rather than stale Task prose, while preserving the
agent-owned plan, checklist, and next action. A changed Review command therefore
cannot leave the next session following obsolete blind/delegated-review
instructions; in blind mode the timeout is marked as informational and unused.
This prompt reconciliation does not change RunID, session reuse, active
duration, abort fencing, or event-sourced lifecycle decisions.

## AFK continuity

### Diagnosed CI recovery

A qualified CI repair stall releases execution capacity into `ci-recovery`.
The runtime stays quiet until its next hourly attempt. Reported Actions outages
skip the attempt; healthy or unreachable status allows live PR and job
revalidation. Sandman records mutation intent before rerunning the failed job
and confirms the replacement job and dependent quality gate by read-back.
Ambiguous intent is reconciled, never blindly replayed.

Green CI, a new head or a different failure returns to ordinary lifecycle
selection. After 24 hours, including skipped hours, at most one read-only
PR/check observation can establish progress; no status check or rerun may
start at the bound. Restart restores the deadline and due time. Manual
`--continue` can attempt immediately in a fresh window using a matching
confirmed receipt from its continuation lineage. Abort prevents further work.
See [ADR-0064](../adr/0064-bound-runtime-ci-recovery.md).

### Continuity outcomes

After v1.0.0-rc.2 a started Run may stop only for a permanent failure it
cannot handle, or wait for external recovery, and only where rc2 would not
have resolved it. If rc2 would have merged or kept waiting, the Run must not
fail. Concretely, a started Run whose pull request is open at its current head
does not fail for missing evidence an external operation can still supply: it
waits or proceeds to merge. Work the implementor owns and cannot resolve ends
the Run after its bounded attempts; an expired review request ends the Run
when no active resolver or usable response remains.

Permanent outcomes remain terminal: a closed or merged-elsewhere pull request,
explicit abort, a merged pull request without verifiable completion, genuine
requested changes (formal `CHANGES_REQUESTED`, the review daemon's
`changes_requested` verdict, or a daemon approval its own verdict sections
contradict) the implementor cannot address within its relaunch
allowance, informal review feedback that an unchanged relaunch could not act
on (`REVIEW_FEEDBACK_STALLED`), owned work (merge, CI repair, review-request
delivery, a blind round, informal feedback) that stalls or uses up its relaunch
allowance (`REMEDIATION_BUDGET_EXHAUSTED`), no review request delivered at all
when a review command is configured (`REVIEW_REQUEST_REQUIRED`), corrupt review state
with nothing
resolving, GitHub unavailable for a full
24-hour recovery window (`GATE_LOOKUP_FAILED`), exhausted diagnosed CI recovery
(`CI_RECOVERY_EXHAUSTED`), an expired delegated-review
request with no active resolver (`REVIEW_REQUEST_EXPIRED`), and agent authentication,
usage-limit or operator stops. With no configured review command, a missing
blind publication is owned work selected as `BLIND_REVIEW_REQUIRED`, not a
terminal or external-review failure. Every lifecycle failure
reason is classified once as permanent or recoverable; the AFK-continuity
contract suite fails on an unclassified reason and replays the real incidents
that motivated the rule. One gate/reason/cause outcome table centralizes
failure, stall, exhausted-allowance, expired-operation, and unreadable-ledger
decisions; the contract enumerates its current rows. ADR-0063 records the
staged policy and the sibling-owned rows that will change.

An expired delegated-review request does not authorize its own continued wait.
The runtime retains the wait only while current-head CI is active, the event log
shows a matching nonterminal review Run and the review daemon is live, or the
daemon is live with a quota pause. It then reobserves the request: current-head
approval or actionable feedback remains usable if seen before terminalization;
unknown verdicts and stale-head evidence do not. When no resolver remains and
there is no usable response, it finishes with `REVIEW_REQUEST_EXPIRED`.

One continuity decision applies these rules to every lifecycle decision;
the attempt loop, entry continuation and runtime observation consume its
action unchanged. A resume signature identifies what a relaunch acts on: gate, reason, live
head, request trigger and the request's response identities, excluding CI
rollup and merge state. Failed-CI repair is scoped to its identified check
execution instead of review trigger/response identities: review delivery or
approval alone does not make unchanged failed-CI work new. A wait entered
because a relaunch stalled records its
signatures in `run.await` (`stalled_resumes`). Observation reads them through
the event fold, so a restart keeps waiting instead of relaunching on the same
evidence; a changed CI execution is new evidence and may resume within the
existing request/head relaunch allowance. A stall of informal review feedback
does not wait: a relaunch that leaves the head, the review request and its
responses unchanged found nothing the implementor could do, so the Run finishes
`failure` with `REVIEW_FEEDBACK_STALLED`. The terminal evidence keeps the
retained `review_request` with its informal feedback (source, locator, body),
the stalled resume signature (`stalled_resume`) and a next action to inspect the
feedback, address or answer it, and run `sandman run --continue`. The recorded
`stalled_resumes` of a Run that an earlier binary parked on such a hold prove the
stall, so it finishes with the same reason on its next observation, without a
relaunch; a recorded hold for used-up informal feedback cannot be told apart
and ends the same way. A new head, a new review request or a new response is
new evidence and still resumes the implementor. Genuine requested changes are
never stalled feedback: they keep their stall row and their per-head
allowance.

The other work the implementor owns ends the Run in the same way when it cannot
be resolved (#3001). Merge work (`ready-to-merge`), unqualified CI repair (`ci-failure`),
review-request delivery (`implementor-work-required` /
`REVIEW_REQUEST_REQUIRED`) and a blind-review round (`blind-review-required`)
finish `failure` with `REMEDIATION_BUDGET_EXHAUSTED` when a relaunch stalls on an
unchanged resume signature or the lifecycle relaunch allowance is used up, and
informal feedback bounded per head does so when its allowance is used up (an
unchanged informal relaunch finishes `REVIEW_FEEDBACK_STALLED`, above). A
relaunch stalls on its own only for a blind round and non-formal informal
feedback; the other gates reach their stall row through a recorded signature or a
used-up allowance. The
terminal evidence names the gate (`external_gate`), its last reason
(`stalled_reason`), the used-up resume signature (`stalled_resume`, which names
the CI execution or the blind publication where there is one) and the retained
pull request, head, `ci_wait`, `review_request` and blind publication, and its
next action tells the operator to fix what blocks the gate and run
`sandman run --continue`. A Run an earlier binary parked in one of these holds
ends on its next observation, without a relaunch, because its recorded
`stalled_resumes` prove the stall; a hold recorded after a transient stalled-ledger
read failure ends the same way once the ledger is readable again. Exhausted
review-request delivery (the final ordinary owned-work attempt) is already
terminal as `REVIEW_REQUEST_REQUIRED`. Merge-conflict repair, the expired CI wait
(`CI_WAIT_TIMEOUT`) and the idle gate without a review command keep their
waits. Newly actionable merge
conflicts must not be hidden behind unchanged failed CI: conflict remediation
takes priority, allowing integration to produce a new head
whose checks and review must be revalidated. A continuity hold
(`continuity_hold`) waits without an operation deadline; it stays abortable,
resumes when the gate changes, and its schedule checkpoint uses the stable
`hold:<RunID>` recovery identity, so restart discovery re-enters it within the
ordinary ownerless grace. Review feedback has three relaunches per head in
addition to entry. This allowance carries across waits in the active Run.
Neither re-requesting review nor returning to an earlier head resets it.
Genuine requested changes (formal `CHANGES_REQUESTED`, a daemon
`changes_requested` verdict, or an approval override) and other informal
feedback end the Run when the allowance is spent
(`REMEDIATION_BUDGET_EXHAUSTED`); informal feedback also finishes
`REVIEW_FEEDBACK_STALLED` as soon as a relaunch leaves its evidence unchanged.
A hold an earlier binary recorded for used-up informal feedback ends
`REVIEW_FEEDBACK_STALLED` on its next observation, as described above. Approval
can resume merge work under its own round.
Other gates retain the in-session request-and-head rule; a changed CI
execution does not reset that allowance. An unreadable event log
fails closed into a wait. Exhausted remediation of unusable CI wait state
(`CI_WAIT_STATE_ERROR`) stays terminal.

A failed pull-request lookup or refresh (`gh` exit status, API 5xx, timeout,
network error) is an external outage, not evidence about the pull request. The
Run waits with `await_reason: github-unavailable` (reason `GITHUB_UNAVAILABLE`),
whether or not a review or CI operation was established; an established
operation is retained with its deadline unchanged and judged only after
GitHub answers again. Recovery is attempted hourly. Before each attempt the
runtime reads GitHub's health from githubstatus.com: a major or critical
incident, or a degraded API Requests, Git Operations, Pull Requests or Actions
component, skips the attempt; a healthy or unreachable status page lets it
run. Observation before the next attempt makes no GitHub call. Each check and
attempt is recorded in the wait's `github_recovery` evidence (window start,
next attempt, last status). Only after 24 hours of continuous failed recovery,
measured from the first failure of the window and including skipped hours,
does the Run finish `failure` with `GATE_LOOKUP_FAILED`. Restart rehydrates the
window and its next attempt from the event fold; the scheduler treats the
24-hour bound as the wait's operation deadline. `sandman run <n> --continue`
is a new RunID: it attempts observation immediately and opens a fresh 24-hour
window. Explicit abort ends the wait at once. The window, its hourly cadence,
the 24-hour bound, the health gate and the restart rehydration are a reusable
recovery window; GitHub recovery is its first user and judges health by the
pull-request lookup components above. A non-success agent attempt with
no established operation also waits when the PR lookup or refresh fails; it
consumes no ordinary retry while GitHub is unavailable. The failed attempt and
its remaining configured retry allowance survive scheduler re-entry; a retry
event is written only when that retry actually starts after recovery. An
approved ready PR follows the normal merge path, and a no-op observation returns
the failed attempt to its bounded retry path. Recognized usage-limit failures
without an established operation remain owned by quota recovery instead.

A closed, unmerged pull request ends a Run (`PULL_REQUEST_CLOSED`) only when it
is proven to belong to the logical Run: its head equals the Run's head, or it
closed (`closedAt`) at or after the Run's original fresh admission. The original
admission is followed through `run.continued` `previous_run_id` links, so a
`--continue` Run with no commits ignores a pull request closed before its first
admission and continues to publish a new one. A closure the runtime cannot
place continues owned publication work while the work item is open; on a
closed work item or an already-resolved task it keeps the closed-PR decision.
Sandman's own review daemon publishes a machine-readable verdict trailer
(`<!-- sandman-verdict:approved|changes_requested -->`) directly before the
publication identity, which binds the decision to one trigger revision and one
reviewed head. The runtime trusts that verdict (or, for older publications,
the closed-set `## Decision` token) and treats a publication bound to another
trigger or head as stale. Only the verdict sections (Findings, Suggested next
steps and Decision) may contradict a published APPROVED verdict: an opposing
verdict, a repair directive, an unresolved assessment, or a merge or approval
condition addressed to the author there ("Merge only after the race in
`worker.go` is fixed."). Summary, Standards, Spec, Quality check, Scope,
Metrics, Tools used and review history describe the change and never veto it;
an explicit `CHANGES_REQUESTED` or `NOT_APPROVED` state still contradicts it
anywhere. Every captured real approving report
(`internal/batch/testdata/approved-*.md`) is a corpus test. The verdict is
additive: human, third-party and paraphrased approvals keep the prose rules.

A contradicted daemon APPROVED is an **approval override**: the runtime treats
the report as requested changes instead of waiting for a request that cannot
resolve. It is never an approval, and the prose rules never overrule it, so a
daemon APPROVED report is exactly one of approved and overridden. The report is
retained as `informal_feedback`, and the retained `review_request` carries
`approval_override` as evidence (the continuity decision and the resume prompt
derive the override from the retained report, never from the flag), so the Run resumes the same RunID with actionable feedback
(gate `actionable-feedback`, reason `REVIEW_INFORMAL_FEEDBACK`) while CI is
pending or green. `run.resumed` (or, when the relaunch happens at scheduler
re-entry, the ready continuation's `run.capacity_queued`), any wait that
carries that retained request, and the terminal failure evidence all record
`approval_override`; a pending wait for a new re-request carries the new request
without it. The resume prompt names the override and tells the implementor not
to merge on the report. An override counts as genuine requested changes under
the per-head allowance: three relaunches on one head, however many review
requests it makes, then `REMEDIATION_BUDGET_EXHAUSTED`. A new head starts a
fresh allowance and goes through the normal review flow. Eligible approval is
evaluated first: when an approval and a contradicted approval answer the same
request, the approval wins and no override is recorded. A labelled Blocking or
Important finding beside an APPROVED decision never reaches the trailer as
APPROVED (the daemon stamps no verdict), and an unknown daemon verdict is never
repair evidence, so those still wait. Blind review keeps its own evidence path.

## Blind review

With no configured review command (absent, empty, or whitespace-only), review is
implementor-owned work. The implementor spawns a fresh, context-free subagent
with the exact daemon review prompt in the same worktree, read-only, and
publishes the report on the pull request. The publication carries the shared
verdict trailer and a fixed `blind-review` identity bound to the reviewed head;
the newest current-head publication is the evidence. A configured review command
ignores blind publications entirely.

An `approved` blind verdict is current-head approval that selects `ready-to-merge`
only when CI is green and the head is mergeable; `changes_requested` is
actionable feedback that resumes the same RunID. With no current-head blind
approval and no pending CI, a green open PR resumes with
`BLIND_REVIEW_REQUIRED` (`blind-review-required`). That reason is recoverable and
classified once as implementor-owned, never `REVIEW_REQUEST_REQUIRED`, an expired
delegated request, or the old idle hold. A relaunch that leaves the head and
blind evidence unchanged, or an exhausted relaunch allowance, finishes the Run
with `REMEDIATION_BUDGET_EXHAUSTED` naming the `blind-review-required` gate; a
Run an earlier binary parked on that hold ends on its next observation, restart
rehydrates the evidence from events, and explicit abort revokes the resume.
A harness that cannot spawn a fresh subagent fails closed with
`REVIEW_SUBAGENT_UNAVAILABLE`; it must never review in the implementor's own
context or post a delegated trigger instead.

The publication author is deliberately not filtered, consistent with informal
approval. The identity binds the verdict to the reviewed head but is not
authentication: any account able to comment can publish a report with a valid
trailer. That tradeoff is accepted in [ADR-0059](../adr/0059-blind-review-as-the-default-pull-request-review.md);
green checks and a mergeable head still gate merge.

## Action and admission rules

The runtime selects one action: await, resume, success, failure, or abort. Every
adapter must respect that action. A gate string is evidence, not a second
decision. In particular, a pending request on a clean PR can still select await;
its `ready-to-merge` aggregate label cannot authorize merge execution.

Terminal decisions do not require execution capacity. Only resume actions acquire
a slot, revalidate the live PR/head/request, then execute. Readiness does not
release dependencies. Scheduler queues, start delay, and opportunity fairness
cannot turn started work back into public queued admission.

`run.capacity_queued` is retained as compatibility/readiness evidence. Started
rows project as waiting; unstarted rows remain queued. Its historical metadata
must not override a newer continuation's BatchID, branch, or artifact ownership.

## Authorized external waits

Admit waiting for current-head CI that is actually queued/running, a confirmed
current-head delegated-review request within its deadline, or a supported
recognized resettable provider limit. Successful review delivery establishes an
external operation even before a reviewer starts. A generic pending/CLEAN label,
absent checks, nonexistent publication, or stale evidence does not establish one.
A failed lookup does not establish a CI/review operation; it authorizes only the
separate GitHub recovery wait described above. A blind review round is not an
external wait: it is implementor-owned work that requires execution admission and
is never satisfied by a pending delegated request or a daemon slot.

Transient observation failures may retry an already-established identity-valid
CI/review operation within its original deadline and bounded recovery budget.
A failed pull-request lookup or refresh is a separate GitHub recovery wait and
does not require prior operation evidence; it uses the hourly, status-gated
attempts and 24-hour bound specified above. No poll, lookup retry, admission
delay, executor reconstruction, or restart renews an operation's authorized
lifetime.

When managed review is enabled with a configured review command, merge work
requires confirmed delegated
request-scoped approval for the current PR head and green CI. Missing delivery
resumes review-request work. Reviewer process exit alone is not approval; stale,
unknown formal `APPROVED`, mismatched, or superseded approval cannot authorize a
merge. Request-scoped informal approval is sufficient without a formal GitHub
approval or a nonempty aggregate `reviewDecision`. A `COMMENTED` review without
commit identity is eligible as an informal response inside the confirmed
current-head request window; an explicitly stale `COMMENTED` review is not.
An approving response's scope paths, metrics, or resolved history are not
actionable feedback. Eligible approval is checked before informal feedback from
the same request, matching the standalone review loop; formal requested changes,
current-head CI failures and merge conflicts remain blocking. Explicitly disabled
managed review retains its configuration contract.

With no configured review command, the current-head blind publication is the
review evidence. Its approval selects merge work only when CI is green and the
head is mergeable; a publication bound to another head, an unknown verdict, or a
configured-command repository cannot authorize merge. Blind requested changes
resume the same RunID for repair. The author is not filtered; the identity binds
the head but is not authentication (ADR-0059).

The complete managed sequence is request confirmation -> waiting -> current-head
feedback resume -> renewed request -> approval resume -> verified merge ->
dependent admission. Re-entry preserves the RunID and active duration; an
explicit abort ends owned intent. Durable publication and canonical evidence
are recovery inputs, not alternate lifecycle authorities.

Verified merged completion wins over retained review or quota signals. A merged
PR without required closing intent is a structured failure. The already-resolved
exception requires the existing live verification path; Task prose is not an
alternate lifecycle authority.

A closed-unmerged PR on a reused branch is historical, not this Run's terminal
outcome, only when its known head differs from the successfully observed local
head and its last update predates the Run's original fresh admission. The
comparison uses second precision: a PR updated within the admission second is
not proven historical. The
append-only admission evidence is required; the latest continuation timestamp
cannot establish this distinction. Missing timestamps, heads, host-path
restoration, or readable admission evidence retain the closed-PR decision.
Historical closures do not bypass the shared provider-quota wait or prevent its
recovery probe from publishing a new PR. A PR closed during the current Run
remains terminal even when local commits have not been pushed.

## Budgets and reset boundaries

| Operation | Budget | Reset boundary |
| --- | --- | --- |
| Managed CI | **30 minutes** per identified current-head check execution, durable hard deadline | New head or verified same-head CI rerun |
| Standalone CI | **60 minutes** and at most **three fixes** per current head within an invocation | New head or fresh invocation |
| Managed observation cadence | **30 seconds from the first poll** between observations of an ordinary external wait, in the batch and foreground paths; excludes API-call duration and execution-capacity admission delay; never renews a deadline or resets retry/relaunch accounting | Not a budget: polling never renews a deadline or resets retry/relaunch accounting |
| Delegated review | Configured `review_timeout`; default 1,800 seconds, minimum 240 seconds; managed daemon-backed requests exclude recorded reviewer quota pauses | New confirmed request |
| Blind review round | Synchronous implementor round; no `review_timeout`, trigger envelope, or polling window | New head or fresh cycle/relaunch round |
| Implementation provider quota | **Five hours of accumulated completed scheduled waits**, at a trustworthy provider reset when available or ten-minute fallback intervals, then final boundary probe and configured ordinary retries | Verified recovery followed by a new quota episode; re-entry/restart preserves consumed polling |
| Implementation lifecycle relaunch | Default **three in-session relaunches**, entry excluded; a feedback relaunch that changes nothing ends the allowance; exhaustion ends the Run for owned work (merge, CI repair, blind round, feedback) and waits for merge-conflict repair and the expired CI wait | Fresh executor session with a new resume signature |
| Ordinary execution retry | Configured retry budget, including owned publication/review delivery recovery | Ordinary retry policy |
| Ownerless recovery | **Five minutes**, capped by an existing operation deadline | Live owner renewal, never repeated discovery |

Implementation quota waits use a validated provider reset time when the selected
agent strategy supplies one; otherwise they retain ten-minute fallback polling.
The review daemon keeps its separate fixed ten-minute quota schedule. Hourly
GitHub recovery attempts with the outage gate and the standalone review cycle's
own plan also keep their schedules; the 30-second cadence changes only the
interval between ordinary managed observations. It applies to default and new
waits: a retained valid explicit plan (a persisted review-request plan or a test
override) keeps its meaning. The row driver's observation never preempts an executing attempt:
review or CI that becomes ready while the agent is still running required local
verification is observed only after the attempt returns, by the same single
lifecycle decision, which selects merge work without an initial wait.

The implementor owns local verification and publication; the runtime owns
external observation. A still-running required command, an unresolved failure,
unpushed work, or an incomplete publication, request delivery or blind review
round is implementor-owned. Once that work is complete the agent checkpoints and
returns immediately, without redundant verification or agent-side polling, and
reports live or timestamped observations rather than stale CI/review statements.
See [ADR-0060](../adr/0060-managed-handoff-boundary-and-thirty-second-observation.md).

CI and review retain independent bounds. CI keeps its hard deadline; a managed
daemon-backed review consumes its response budget only outside recorded reviewer
quota pauses. At expiry, the runtime
revalidates current-head operations: an expired review request cannot override
CI that is still resolving. The request also stays waiting while its matching
review Run is nonterminal and the daemon is live, or while a quota pause is
active with that daemon. A confirmed request alone is not a resolver beyond its
deadline. If those guards clear without usable current-head approval or
actionable feedback, the Run ends with `REVIEW_REQUEST_EXPIRED`; an eligible
late response observed before that terminal decision still resolves the request.
CI expiry remains governed by its own deadline and `CI_WAIT_TIMEOUT` row. The
historical in-session relaunch cap and ordinary retries bound owned work;
persisted same-head remediation counts are diagnostic, not cumulative launch
reservations. Obsolete implementation, reviewer-launch, and standalone CI budget
files cannot veto executable work. Standalone same-head polling preserves its
invocation-local window/count; a changed head resets both.

Quota accounting restores consumed polling into both the executor session and
batch scheduler. A wait is scheduled at a validated provider reset when
available, otherwise for ten minutes, and is capped by the remaining five-hour
allowance. Its scheduled duration counts only after the wait returns
successfully; cancellation or interruption charges zero. Capacity delay and
process downtime do not count. On restart, the persisted `NextPollAt` checkpoint
is honored; if it is missing, the due time is reconstructed from current event
evidence, and only a successfully completed live remainder accrues. Re-entry
cannot create another five-hour allowance. The provider reset is distinct from
the recomputed expected exhaustion timestamp, which is diagnostic only: neither
is an admission or ownership cutoff. Quota schedules use stable
`quota:<RunID>` operation identity with no hard operation deadline.

After the allowance is consumed, the final boundary probe and configured
fresh-session ordinary retries remain available. Missing, malformed, or
unreadable accounting disables further quota waits while permitting bounded
ordinary execution. Quota recovery preserves supported agent session reuse;
generic failure and context rollover remain fresh-session retries. Custom
commands keep their documented eligibility boundaries; arbitrary auth,
configuration, and non-resettable spend/budget errors are not external waits.

## Quota scopes and logical ownership

Implementation admission pauses are **batch-local**. Reviewer recovery is a
separate **daemon-wide** persistent gate. Different configured providers must
not be assumed to share credentials or quota.

Suspension retains logical row ownership and cancellation registration while
releasing execution capacity. New-start waiters must revalidate quota immediately
before launch. Verified recovery clears the relevant gate and automatically
readmits eligible siblings. After recovery, a later CI/review wait returns to
ordinary observation; quota-probe mode must not leak into it.

Failed or terminal quota owners retire their own admission pause. Other active
owners still pause admission; siblings establish their own outcomes rather than
inheriting failure from an exhausted probe. Yielding execution retains unfinished
queued/await intent until admission is permitted.

Reviewer quota recovery retains its daemon-wide fixed ten-minute schedule;
implementation provider reset evidence does not affect reviewer scheduling.
Reviewer launch failures back off from 10 seconds, doubling to a 10-minute cap,
counted per request revision across Runs and daemon restarts, and keep
request/head advisory artifact claims, and durable single-publication recovery,
without a cumulative three-launch gate or a new reviewer request-age cutoff.
The daemon records the start of a quota pause and closes it atomically on a
successful probe, before another review launch. An overlapping managed request
keeps the remaining budget across repeated pauses and restart; it cannot regain
time after expiry. A pause without a trustworthy start supplies no time credit.
Each observation uses the same validated request and quota snapshot for
wait authorization, expiry, and retained evidence.
After a backward clock correction, only time beyond the previous recorded
recovery can be credited; the probe schedule itself does not change.
The canonical request and response identity stay unchanged. The managed
daemon-backed wait uses normal ownerless recovery grace rather than a fixed
review deadline as its ownership cutoff, so a pause between observations cannot
strand the Run. Any independent current-head CI cutoff is unchanged. Standalone
review, blind review, and requests for external reviewers do not use this clock.
Once current-head CI is observed green, the review wait drops that CI cutoff.
This also applies when a live resolver keeps an expired request waiting; its
expired deadline stays diagnostic-only.
If the process stops before checkpointing the change, the observation anchors
the ordinary ownerless grace without renewing it on discovery.
Durable decision-publication recovery does not require a duplicate
reviewer launch. Stopping the reviewer daemon preserves recoverable requests and
publications. Implementation abort ends its own lifecycle without rewriting the
separate review run's outcome. Expired parent requests cannot reopen an exhausted
reviewer quota gate; only a clean probe can clear that gate.

## Cancellation and restart

Explicit batch/per-row abort applies to every unfinished owned phase: initial
admission, preparation, execution, retry, observation, probe, readiness, capacity
delay, and quota deferral. Cancellation consumes no retry budget and must prevent
subsequent retry bookkeeping or launches. Completed siblings retain their outcomes.

Atomic run-owned schedule/lease evidence and an exclusive advisory RunID claim
support recovery. A live owner renews the lease every 30 seconds; a transient
renewal I/O failure is retried while the next renewal still falls within the
last successful renewal's uncapped recovery grace (the held claim, not the
lease, protects the live row), and invalid ownership aborts immediately.
`sandman clean` reclaims claim files of event-terminal Runs while holding the
claim; claimants revalidate the locked inode. Unclean exit releases the claim,
but valid intent remains recoverable for five minutes, capped by its existing
operation deadline. Repeated discovery does not extend grace.
Quota diagnostic estimates do not cap grace. Legacy quota schedules normalize
on reads and live-owner writes without renewing existing ownerless grace.

Normal `sandman run` admission automatically discovers and re-enters valid
ownerless waiting/ready/initial intent without requiring `--continue`. This uses
existing admission and stale-recovery activation, not a new lifecycle daemon or
a guarantee that a process respawns when no Sandman process is running.

Takeover rereads authoritative terminal events under the exclusive claim before
transferring ownership. Explicit abort persists terminal cancellation before
claim release and cannot later be rehydrated. Expired grace aborts; malformed
recovery evidence has a bounded failure outcome. Initial admissions are rebuilt
without requiring a worktree or Task that was never created.

## Duration and dependencies

Duration is the sum of active execution segments for a **RunID**. Queueing,
external waiting, capacity delay and ownerless recovery contribute zero. Resume
adds to the existing total across batches; a new RunID starts fresh. Terminal
duration freezes at the terminal event.

An in-batch prerequisite admits its dependent only after verified success **and**
closed work item. Success with an open item, failure, or blocked produces a
blocked dependent. Abort propagates aborted. Unknown/non-terminal prerequisite
state cannot authorize admission. External prerequisites are rechecked before
launch. Artifact cleanup, archive location, and socket liveness do not revise
these lifecycle outcomes.
