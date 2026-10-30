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
| Next action ready, capacity occupied | Stay **waiting**, reason capacity |
| CI failure or merge conflict | Resume bounded repair; when the relaunch allowance leaves the head unchanged, stay **waiting** for external recovery (rc2 behaviour) |
| Missing PR or undelivered review request | Perform bounded owned-work remediation (`run.retry` reason `lifecycle-owned-work`), then failure; never invent external waiting |
| Review request delivered by an attempt that exited non-success | Register it and stay **waiting**; spend no ordinary retry |
| Feedback relaunch leaves head, request and responses unchanged | Stay **waiting** (`RESUME_STALLED`); do not relaunch on the same evidence |
| Relaunch allowance exhausted on review, CI (including an expired CI wait), request or merge work | Stay **waiting** (`RESUME_STALLED`); genuine requested changes (formal `CHANGES_REQUESTED` or the review daemon's `changes_requested` verdict) left unrepaired finish **failure** |
| Review request expired with nothing else resolving, or (without a delegated review command) an idle gate after owned work | Stay **waiting** (continuity hold) on an open current-head PR |
| Recognized provider usage limit | Wait and probe; do not spend ordinary retries immediately |
| Quota recovers | Resume limited run and reopen sibling admission |
| Quota recovers, then CI becomes pending | Restore ordinary CI observation without another agent launch |
| PR becomes verified merged while waiting | Finish **success immediately**, without acquiring an agent slot |
| PR closes without merging | Finish **failure** |
| Explicit operator abort | Abort all unfinished owned rows; prevent further preparation, retries, probes and launches |
| Same RunID resumes in another batch | Preserve active duration; update current ownership |
| New RunID for a manual continuation | Start a fresh clock |
| Linked reviewer active | Portal may show **reviewing**; underlying lifecycle remains inspectable |

## AFK continuity

After v1.0.0-rc.2 a started Run may stop only for a permanent failure it
cannot handle, or wait for external recovery, and only where rc2 would not
have resolved it. If rc2 would have merged or kept waiting, the Run must not
fail. Concretely, a started Run whose pull request is open at its current head
never becomes terminal-failed when the only missing evidence is review, CI, or
a review request the runtime did not observe: it waits or proceeds to merge.

Permanent outcomes remain terminal: a closed or merged-elsewhere pull request,
explicit abort, a merged pull request without verifiable completion, genuine
requested changes (formal `CHANGES_REQUESTED` or the review daemon's
`changes_requested` verdict) the implementor cannot address within its relaunch
allowance, no review request delivered at all (`REVIEW_REQUEST_REQUIRED`),
corrupt review state with nothing resolving, an unobserved pull request, and
agent authentication, usage-limit or operator stops. Every lifecycle failure
reason is classified once as permanent or recoverable; the AFK-continuity
contract suite fails on an unclassified reason and replays the real incidents
that motivated the rule.

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
existing request/head relaunch allowance. Newly actionable merge
conflicts must not be hidden behind unchanged failed CI: conflict remediation
takes priority, allowing integration to produce a new head
whose checks and review must be revalidated. A continuity hold
(`continuity_hold`) waits without an operation deadline; it stays abortable,
resumes when the gate changes, and its schedule checkpoint uses the stable
`hold:<RunID>` recovery identity, so restart discovery re-enters it within the
ordinary ownerless grace. The relaunch allowance belongs to one repair round
(request and head): a new head or request starts a fresh allowance, and the
entry launch counts as the round's first relaunch; a changed CI execution does
not reset that allowance. An unreadable event log
fails closed into a wait. Exhausted remediation of unusable CI wait state
(`CI_WAIT_STATE_ERROR`) stays terminal.

Sandman's own review daemon publishes a machine-readable verdict trailer
(`<!-- sandman-verdict:approved|changes_requested -->`) directly before the
publication identity, which binds the decision to one trigger revision and one
reviewed head. The runtime trusts that verdict (or, for older publications,
the closed-set `## Decision` token) and treats a publication bound to another
trigger or head as stale. An APPROVED verdict beside author-facing
contradictions (an opposing verdict, a labelled Blocking or Important finding,
or a repair directive addressed to the author) is not an approval; such an
unsure report waits. The verdict is additive: human, third-party and
paraphrased approvals keep the prose rules, and an unknown daemon verdict is
never repair evidence.

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
absent checks, nonexistent publication, stale evidence, or a failed lookup alone
does not establish one.

Transient observation failures may retry an already-established identity-valid
CI/review operation within its original deadline and bounded recovery budget.
Without prior valid evidence, fail explicitly. No poll, lookup retry, admission delay,
executor reconstruction, or restart renews an operation's authorized lifetime.

When managed review is enabled, merge work requires confirmed delegated
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
current-head CI failures and merge conflicts remain blocking. Explicitly disabled managed review retains its
configuration contract.

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
| Delegated review | Configured `review_timeout`; default 1,800 seconds, minimum 240 seconds | New confirmed request |
| Implementation provider quota | **Five hours of accumulated completed polling**, ten-minute intervals, then final boundary probe and configured ordinary retries | Verified recovery followed by a new quota episode; re-entry/restart preserves consumed polling |
| Implementation lifecycle relaunch | Default **three in-session relaunches**, entry excluded; a feedback relaunch that changes nothing ends the allowance; exhaustion waits | Fresh executor session with a new resume signature |
| Ordinary execution retry | Configured retry budget, including owned publication/review delivery recovery | Ordinary retry policy |
| Ownerless recovery | **Five minutes**, capped by an existing operation deadline | Live owner renewal, never repeated discovery |

CI and review retain independent hard deadlines. The earlier expired operation
selects bounded remediation even if another operation remains active. The
historical in-session relaunch cap and ordinary retries bound owned work;
persisted same-head remediation counts are diagnostic, not cumulative launch
reservations. Obsolete implementation, reviewer-launch, and standalone CI budget
files cannot veto executable work. Standalone same-head polling preserves its
invocation-local window/count; a changed head resets both.

Quota accounting restores consumed polling into both the executor session and
batch scheduler. Only completed polling intervals count; cancellation, capacity
delay, and process downtime do not. Re-entry cannot create another five-hour
allowance. The recomputed expected exhaustion timestamp is diagnostic: it is
neither an admission nor ownership cutoff. Quota schedules use stable
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

Reviewer launch failures retain the existing 10/20/40/60-second capped backoff,
request/head advisory artifact claims, and durable single-publication recovery,
without a cumulative three-launch gate or a new reviewer request-age cutoff.
The waiting parent's configured request deadline remains unchanged.
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
