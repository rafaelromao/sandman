# Waiting state-machine audit — 2026-10-03

> **Superseded baseline report.** This audit describes behavior before the
> unified waiting-state changes. Its queue classifications, 60-minute standalone
> CI window, and non-durable quota counters are historical findings, not current
> rules. The authoritative replacement is
> [the run-state-machine contract](../architecture/run-state-machine.md).

## Purpose and baseline

This is a diagnostic report for operator confirmation, not an approved replacement
contract. Its transition IDs and decision IDs are intended to make corrections
unambiguous before implementation resumes.

Baseline: `main` at `d7f3799f` (PR #2758). The local `sandman` binary's build
metadata also identifies this revision. The earlier incident ran before #2758;
its events retain the older immediate usage-limit retry behavior.

The audit covers:

- Initial admission and dependency gating.
- Agent execution, ordinary retries, context rollover, and usage limits.
- CI, delegated review, head reconciliation, and implementation-PR completion.
- Scheduler slot release, readiness, admission pauses, and automatic re-entry.
- Operator cancellation, stale recovery, and restart admission.
- Event projection, duration, terminal summaries, and Portal review aggregation.
- The reviewer daemon's separate quota gate and managed/standalone skill handoff.

There are uncommitted changes from the interrupted fix: started capacity
continuations project as waiting, associated expectations were updated, and
duration regression assertions were added. The duration implementation and the
other defects below remain unchanged. No new PR has been published for this
audit. Findings distinguish the shipped behavior from that partial local change.

## Executive conclusion

The failures span both the lifecycle decision and its adapters. They are not
solely presentation bugs.

Three distinct facts have become entangled:

1. **Run lifecycle:** has this RunID started, is it suspended, or has it finished?
2. **Readiness:** does external work remain, or is an executable next action ready?
3. **Admission:** may the scheduler give that next action a slot now?

A started run can be ready and still waiting for capacity. It does not become an
unstarted queued run. Similarly, a quota-paused row can remain unfinished even
when its scheduling goroutine has returned. Cancellation must still reach it.

There are also conflicting written requirements. Some hardening changes did
exactly what their issue requested, but the combined rules contradict the
operator's intended lifecycle. A single confirmed transition contract is needed
before changing more paths.

## 1. How the rules changed

| Change | Requirement/change | Consequence |
| --- | --- | --- |
| #2696 / PR #2697, September 6 | Initially requested idle-timeout cooldown; implementation evolved into usage-limit waiting with ten-minute polls and a five-hour window. | Provider exhaustion became a distinct suspended path. |
| #2705 / PR #2706, September 9 | Freeze duration on `run.await`, resume on `run.resumed`; explicitly treat `run.continued` as a fresh clock. | Continuation reset was intentional in that contract. |
| #2735 / PR #2739, September 26 | Preserve duration for same RunID **and** BatchID; explicitly allow a different batch to start fresh. | Same-run cross-batch rehydration still resets duration. |
| #2745 / PR #2746, September 29 | Reconcile changed PR heads and use current-head approval; deduplicate warnings. | Live-head reconciliation was added around lifecycle observation. |
| #2743 / PR #2744, September 29 | Require an active external resolver; explicitly distinguish external waiting from ready-but-capacity-queued work; explicitly reject generic quota waits. | Usage-limit waiting was removed; ready continuations gained a new projected queued phase. |
| Commit `89e38c7d`, September 29, 13:17 UTC−03 | “Persist ready continuations across restart,” merged in #2744. | Introduced `run.capacity_queued` and durable ready-continuation discovery. |
| #2754 / PR #2755, October 2 | Pause new starts while quota is exhausted; resume normal admission when quota recovers. | Added a one-way batch-local pause latch and reused capacity events for unstarted rows. Recovery/readmission was not implemented. |
| #2516 / PR #2756, October 3 | Separate event terminality from artifact availability. | Correctly protects terminal outcomes, but preserves capacity-marked rows indefinitely during stale recovery. |
| PR #2758, October 3 | Restore ten-minute/five-hour implementation usage-limit waiting and session reuse. | Fixes the rapid-retry symptom but interacts incorrectly with the pause latch and later CI/review waits. |

### Explicit requirement conflicts

- #2743 says: “No generic quota/authentication/permission/configuration/human-intervention
  wait exception.” The operator now explicitly requires recognized provider limits
  to wait. Recognized resettable quota needs an explicit exception; arbitrary
  authentication/configuration failures still must not masquerade as progress.
- #2743 says to distinguish external waiting from capacity-queued work. This is a
  useful **readiness distinction**, but it must not turn a started run's public
  lifecycle back into `queued`.
- #2705 and #2735 intentionally permitted continuation/batch clock resets. The
  operator now requires the same RunID to retain its accumulated duration.
- #2754 requires automatic readmission after recovery. The implemented gate only
  stops launches; it does not reopen admission or retain those rows in a live
  cancellation-aware scheduling loop.

Related unfinished initiative: #2628 and #2630 explicitly propose coordinator
leases and grace-based recovery. Those guarantees are not implemented on this
baseline and must not be assumed when reasoning about a dead socket today.

## 2. Proposed public state model

The following incorporates the operator's three explicit corrections:

- A started run never returns to `queued`.
- Returning from waiting does not reset that run's duration.
- Aborting a batch aborts its unfinished rows, including deferred rows.

```text
selected -> queued -> running <-> waiting
                |       |           |
                |       +-----------+----> success / failure / aborted
                |
                +----> blocked / aborted / pre-start failure
```

`waiting` can have different reasons without changing the lifecycle label:

| Waiting reason | External operation still pending? | Agent may execute now? |
| --- | --- | --- |
| Current-head CI | Yes | No, unless a separate repair action is required. |
| Confirmed review request | Yes | No, unless a separate repair action is required. |
| Recognized quota reset | Yes | Only a scheduled recovery attempt/probe. |
| Capacity/start-delay | No; next action is ready | Once admission allows it. |

Scheduler priority queues are internal admission structures. Entering one does
not imply a public `queued` lifecycle transition for a started run.

`reviewing` remains an aggregate Portal label when a linked review is active;
the implementation can internally be running or waiting. Terminal parents must
not be promoted by later review activity.

`unknown` means insufficient lifecycle evidence. Artifact presence or a dead
socket alone must not invent an outcome.

**Needs confirmation:** whether an initial `run.queued` is itself a terminal
placeholder. Today the fold treats it as terminal while admission later reopens
the same RunID. The proposed model treats an actual in-batch admission queue as
unfinished; an intentionally skipped placeholder would need separate semantics.

## 3. Transition inventory

“Proposed” is the behavior recommended for confirmation. “Current” describes the
shipped baseline, not a promise that every policy below is already correct.

### A. Initial admission and dependencies

| ID | Condition | Current | Proposed |
| --- | --- | --- | --- |
| A1 | Selected row waiting for its first slot | `run.queued`; event fold marks placeholder terminal, Portal may show active queue using batch membership. | Initial `queued`, zero active duration, unfinished until admitted or stopped. |
| A2 | In-batch prerequisite still running/waiting | Dependent waits on completion channels; releasing a slot does not release its dependent. | Keep dependent queued. |
| A3 | Prerequisite succeeds and GitHub issue is closed | Dependent is eligible for normal admission. | Keep. |
| A4 | Prerequisite succeeds but GitHub issue remains open | Dependent becomes terminal `blocked`. | Confirm this two-part success gate. |
| A5 | Prerequisite fails or is blocked | Dependent becomes terminal `blocked`. | Keep if this is the agreed dependency outcome. |
| A6 | Prerequisite is aborted | Dependent becomes `aborted`, with `aborted_by`. | Confirm cascade rather than `blocked`. |
| A7 | External prerequisite | Rechecked against issue closure before launch. | Keep closure-based gating. |
| A8 | Fetch/identity/sandbox startup fails | Emits pre-start `run.finished/failure` with diagnostics. | Keep explicit failure; cancellation should take precedence over an unfinished startup. |
| A9 | Prerequisite scheduling goroutine returns `queued`/unknown | Those status values fall through dependency checking as nonterminal; quota gate usually stops the dependent later. | Never make the dependent eligible solely because its prerequisite stopped scheduling. |

### B. Execution and clean exits

| ID | Condition | Current | Proposed |
| --- | --- | --- | --- |
| B1 | Initial admitted execution | Writes `run.started`/`run.continued` before entry PR evaluation. | Define this as execution admission; start/resume the active-time segment. |
| B2 | Generic agent failure | Ordinary bounded retry, fresh conversation, preserved Task/branch rules. | Keep, with a cancellation check before retry preparation/event/launch. |
| B3 | Repeated OpenCode context limit | Stops the active attempt; ordinary retry with `context-exhausted`, clean conversation, preserved worktree/Task. | Keep; this is not external waiting. |
| B4 | Agent idle timeout | Kills attempt, emits `run.idle_timeout`, may take bounded retry with `agent-stalled`. | Confirm retry policy; do not classify as provider waiting. |
| B5 | Clean exit with no PR on an open issue | Post-agent failure `PULL_REQUEST_MISSING`; at entry this reason permits the agent to do publication work. | Keep “do the work, never wait for nonexistent publication”; confirm whether a clean incomplete exit should receive bounded remediation before final failure. |
| B6 | Clean exit with generic idle PR gate | Post-agent `EXTERNAL_GATE_IDLE`; at entry launch for owned work. | Keep rejection of unsupported waits; confirm remediation-vs-immediate-failure policy. |
| B7 | PR merged with verified closing intent | Lifecycle decision selects success. | Finish immediately, independently of execution-slot availability and retained quota/review signals. |
| B8 | Merged PR lacks closing intent | Structured completion failure. | Keep failure rather than false success. |
| B9 | PR closed without merge | Lifecycle failure. | Keep. |
| B10 | Explicit already-resolved Task status | Runs the verification path/conservative completion checks; not a generic authority to await. | Preserve verified-resolution exception; prose alone must not replace live verification. |

### C. CI, review, and head transitions

| ID | Condition | Current | Proposed |
| --- | --- | --- | --- |
| C1 | Current-head CI queued/running | Admit `run.await`, release execution capacity, observe automatically. | Keep `waiting`. |
| C2 | CI passes; review must be requested or merge work is ready | Observer obtains a lifecycle action, then scheduler acquires a slot and executor revalidates. | Resume the specific owned action when a slot is available; remain waiting while admission is pending. |
| C3 | CI fails | `resume/ci-failure`; bounded in-session relaunch. | Resume repair, not an external CI wait. |
| C4 | Merge conflict | `resume/merge-conflict`; bounded in-session relaunch. | Resume reconciliation/repair. |
| C5 | Confirmed current-head review request; reviewer not started | Request counts as active until its deadline. | Keep `waiting`; delivery is enough to establish an external operation. |
| C6 | Same pending request, PR aggregate is clean/ready | Pure decision says await, but adapters can relaunch because gate is `ready-to-merge`. | Remain waiting until request-scoped feedback/approval or a separately justified action. **Reproduced defect F7.** |
| C7 | Current-request actionable feedback/changes | Resume implementation, preserving request evidence. | Keep, subject to shared repair budget. |
| C8 | Current-request approval, green CI, mergeable PR | Resume implementor to merge. | Keep; approval is not terminal implementation success. |
| C9 | Stale/mismatched/unknown approval | Cannot authorize current-head approval; stale generations are evidence only. | Keep rejection; reconcile head and perform owned work where possible. |
| C10 | PR head changes | Safe exact-head fast-forward when possible; otherwise resume with reconciliation evidence. | Keep preservation of local changes; no indefinite stale-head wait. |
| C11 | PR ready with no retained review request | Can resume as `PULL_REQUEST_READY`, including absent checks if merge state is clean. | Confirm whether aggregate readiness is enough, or current-head delegated approval must always exist. |
| C12 | Gate lookup fails | Structured `GATE_LOOKUP_FAILED`, no unsupported wait. | Confirm bounded transient observation retry for a previously established operation, versus immediate failure. |
| C13 | Review state unreadable/corrupt | Alone cannot authorize await; independently active CI/review may still justify it. | Keep fail-closed identity handling; an error cannot create a new operation or deadline. |
| C14 | CI deadline expires | Selects CI remediation; intended durable per-head deadline is 30 minutes. | Resume bounded diagnosis/repair; terminal failure when shared budget is exhausted. |
| C15 | Review deadline expires | Depending on evidence/path, selects review timeout failure, generic idle failure, or deadline remediation. Foreground and batch observers differ. | One explicit policy: bounded request recovery or terminal `REVIEW_TIMEOUT`; no silent deadline renewal. |
| C16 | Both CI and review active | Carries separate deadlines; deadline helper chooses the earlier one when evaluated. | Confirm independent operation lifetimes and what happens when one expires while the other is still active. |

### D. Provider quota and admission

| ID | Condition | Current | Proposed |
| --- | --- | --- | --- |
| D1 | Built-in OpenCode recognized usage limit | Ten-minute same-session re-entry; up to five hours of accumulated polling intervals. | Keep waiting; confirm absolute window and timeout policy. |
| D2 | Built-in Claude recognized session/weekly/model limit | Same waiting path; directory-scoped `--continue`. | Keep recognized resettable quota waiting. |
| D3 | Custom command / unknown failure / spend-budget limit | No built-in await eligibility; ordinary failure/retry. | Confirm supported detection boundary; no generic auth/configuration wait. |
| D4 | One row reports quota exhaustion | Sets batch-wide boolean latch; unstarted rows get durable ready events and return `queued`. | Pause eligible new admissions while retaining unfinished row ownership. |
| D5 | Limited row recovers | Its own probe may proceed; gate never clears and siblings do not return to admission. | Reopen admission and automatically continue siblings. **F4.** |
| D6 | Row was already blocked in start-gate acquisition when quota pause began | No quota recheck before its eventual `Execute`. | Recheck admission at launch boundary. **F5.** |
| D7 | Recovered quota run later waits on CI/review | `UsageLimitProbe` remains true, bypassing normal observation/entry evaluation. | Clear quota-probe mode and follow CI/review waiting. **F6.** |
| D8 | Quota flag remains after merged completion is verified | Predicate can still emit quota await. | Verified completion wins. **F8.** |
| D9 | Five-hour limit remains exhausted | Ordinary retries resume after accumulated poll intervals; waited duration is per-row in-memory input. | Confirm fail immediately at deadline versus spending remaining ordinary retries; preserve deadline through restart. |
| D10 | Another batch or reviewer shares the same account/provider quota | Implementation latch is local to one batch; reviewer gate is separate and daemon-wide. | Confirm pause scope: batch, configured agent/account, or broader shared quota identity. |

### E. Capacity, cancellation, recovery, and projections

| ID | Condition | Current | Proposed |
| --- | --- | --- | --- |
| E1 | Started run becomes ready while capacity is full | `run.capacity_queued` clears awaiting and displays queued. | Remain waiting, with readiness/capacity as its reason. **F1.** |
| E2 | Initial unstarted row lacks capacity/quota | Queued admission. | Keep queued; it has not started. |
| E3 | Ready continuation acquires a slot | Revalidates live PR/head/request before launch; same RunID in runtime re-entry. | Keep identity and revalidation; transition waiting to running only when executing/entering its next action. |
| E4 | Await observer reports success/failure/unhandled | Batch adapter treats every non-await result as capacity readiness; defers applying the decision until execution admission/re-evaluation. | Apply terminal decisions immediately; only resume actions need capacity. **F10.** |
| E5 | Start delay/fairness delays a continuation | Internal priority/fairness queue; ten-minute opportunity cooldown. | Remain waiting; preserve limits and fair progress for ordinary work. |
| E6 | Operator abort during executing/polling/slot acquisition | Live row contexts generally append `run.aborted`. | Abort all unfinished owned rows and prevent further preparation/retry/probe/launch. |
| E7 | Operator abort after quota-deferred row has returned | Row cancel registration has been removed; returned result and events stay queued. | Terminalize deferred row as aborted. **F3.** |
| E8 | Operator abort with already-finished siblings | Existing terminal outcomes remain. | Keep; preserve success/failure and abort only unfinished rows. |
| E9 | Dead owner while externally awaiting | Current stale recovery can append recovered abort for eligible started work; no persisted coordinator lease/grace. | Confirm grace-based recovery and automatic restart; #2628/#2630 propose it but it is not shipped. |
| E10 | Dead owner with capacity marker | Exempt from stale/orphan abort; normal run admission rehydrates latest ready rows. | Preserve genuine recoverable intent only; explicit cancellation must end it, invalid evidence needs a bounded failure path. |
| E11 | Unstarted quota-deferred row is marked ready but has no branch/Task | Discovery may accept it; application rejects incomplete identity or missing Task. | Keep it as initial admission intent, not a worktree continuation. **F13.** |
| E12 | Rehydrated RunID moves to another BatchID | Duration can reset; historical capacity metadata can still own `BatchID()`. | Keep duration and use the current owner/artifact location. **F2/F9.** |
| E13 | New RunID for ordinary manual continuation | Fresh duration; session reuse only when opted in. | Confirm that new RunID remains a new clock, even for the same issue/branch. |
| E14 | Linked review starts/stops | Portal promotes nonterminal parent to reviewing, then returns to its lifecycle label. | Keep; underlying waiting/capacity is independently inspectable. |
| E15 | Missing artifacts / archived location / dead socket | Artifact facts are distinct from event outcome; unknown without lifecycle evidence. | Keep event-sourced authority and terminal-parent protection. |

### F. Reviewer daemon and skill handoff

Reviewer runs have their own identities and owners. An implementation's waiting
phase is not the reviewer run's execution status.

| ID | Condition | Current | Proposed |
| --- | --- | --- | --- |
| R1 | Managed implementor delivers a confirmed request | Embedded skill explicitly checkpoints and yields before standalone polling; runtime registers/observes the request. | Keep one managed owner and request-scoped identity. |
| R2 | Standalone PR-review session | Runs the versioned shell coordinator through its deadline; classifies top-level/formal/inline evidence. | Keep standalone behavior separate from managed resource release. |
| R3 | Reviewer starts/finishes | Its AgentRun has separate events; finishing its process does not itself approve the implementation. | Only current request-scoped response evidence resumes/approves implementation work. |
| R4 | Reviewer hits quota | Reviewer is prompt-only, so implementation await does not apply. Daemon persists a quota gate and stops new reviewer launches. | Keep supported provider detection and durable pause; confirm coordination with implementation quota scope. |
| R5 | Reviewer quota probe succeeds | Clears persistent pause; reviewer admission resumes on a later tick. Errors retain pause. | This is the recovery/readmission transition missing from implementation batches. |
| R6 | Review launch fails | Persists retry timing with 10/20/40/60-second capped backoff; no maximum count is enforced by that backoff function. | Confirm request-level bounded outcome and how failure reaches the waiting parent. |
| R7 | Durable review decision exists but publication fails | Tries up to five posts, then retains pending publication for later tick/restart; does not need a second reviewer execution. | Keep recoverable publication; it needs operation identity and a bounded parent outcome. |
| R8 | Reviewer/publication owner is cancelled | Launch failure recording leaves cancelled work pending; durable decisions survive as pending publication. | Confirm whether stopping the daemon means suspend/recover, and whether aborting the implementation cancels its separate review request. |

### Superseded baseline clocks and limits

| Mechanism | Current limit/reset boundary |
| --- | --- |
| Runtime CI wait | 30 minutes per PR head, persisted in `ci_wait.json`; a changed head creates a new window. |
| Standalone skill CI wait | 60 minutes per head and up to three fix attempts in its stated contract. |
| Review request | Configured `review_timeout`, default 1,800 seconds, minimum 240 seconds; confirmed request retains its deadline. |
| Managed CI/review observation | 120, 60, 60, then repeating 30-second intervals. |
| Implementation quota | Ten-minute polls; five hours of accumulated interval values; counter is not durable across restart. |
| Reviewer daemon quota | Ten-minute probes; persisted pause, with no five-hour overall deadline in this gate. |
| In-session lifecycle resume | Default three relaunches, reset with each new `runSession`. |
| Ordinary agent retry | Configured `retries + 1` attempts for each executor invocation. |
| Start-opportunity fairness | Ten-minute priority cooldown; still subject to capacity and `start_delay`. |

These are different budgets. For example, a parent review request can expire
after 30 minutes while its reviewer daemon remains quota-paused for longer.
Neither a polling interval nor a new executor call should silently extend the
operation's authorized lifetime.

## 4. Confirmed defects and evidence

### F1 — A started run returns to queued

Shipped `run.capacity_queued` sets `awaiting=false`; `Status()` returns queued.
The exact started → await → capacity path was reproduced in the event fold and
Portal projection. The local partial fix changes this label while retaining
durable capacity evidence for restart admission.

Sources: `internal/events/run_state.go` event fold and `Status()`;
`internal/cmd/portal_runs_view.go` `runFromState`.

### F2 — Same-run duration resets across batches

`run.continued` preserves the total only when the previous and continued BatchID
match and the projection is nonterminal. Thus rehydrating the same RunID into a
different batch discards earlier active segments. A deterministic test expecting
5 + 7 = 12 minutes gets 7 minutes. Repeated rehydration can reduce a 15-minute
total to the latest 3-minute segment.

Sources: `internal/events/run_state.go` continuation fold; #2735 explicitly
allowed the different-batch reset. The correction now requested changes that rule.

### F3 — Aborted batch retains quota-deferred queued rows

The quota pause branch writes a ready event, sets result status queued, then
returns from the row's goroutine. Its cancel registration and command server are
removed. The final batch result does not reconcile these deferred rows when the
batch context is cancelled.

Diagnostic outcome: `failure / queued / aborted`, with batch-aborted error.
The deferred middle row should also be aborted.

Incident `261003100534-7f03-2516+11`, UTC−03:

- 11:58:40: usage-limit pause records deferred rows 2626, 2627, 2628, 2629,
  2630, 2654, 2631, 2632, 2625.
- 11:59:23: row 2517 receives `run.aborted` after the operator stops the batch.
- Those nine deferred rows have no matching abort in that batch's trace.
- Later admission reuses some of their identities, demonstrating that durable
  continuation intent outlived the old batch's cancellation.

Sources: `internal/batch/orchestrator.go` quota pause branch, goroutine defers,
and result aggregation; `.sandman/events.jsonl` lines 1147–1160.

### F4 — Quota recovery never readmits siblings

The batch latch only transitions false → true. There is no clearing transition.
Moreover, deferred sibling goroutines already returned, so clearing the latch
alone would still not reschedule them.

Diagnostic outcome after recovery: starts `[42, 42]`, results `success / queued`.
Required sibling admission never happens. This violates #2754 acceptance item 3.

Source: `internal/batch/orchestrator.go` `usageLimitPaused` and pause branch.

### F5 — Quota pause has an admission race

The pause check occurs before blocking start-gate acquisition. A row already
queued inside that gate can acquire a newly freed slot and launch after another
row has reported quota exhaustion; there is no final quota recheck.

Reproduced with two occupied slots and a third waiter. The third row launches
after the limited row exits and sets the pause.

Source: `internal/batch/orchestrator.go` pause check → `Acquire*` → `Execute`.

### F6 — Quota probe mode leaks into subsequent CI/review waits

Quota re-entry sets `row.UsageLimitProbe=true`. The ordinary lifecycle-await
branch does not clear it. Both batch observation and session entry guards then
remain bypassed even after the recovered agent yields for current-head CI.

Diagnostic outcome: 3 launches instead of 2; the third launch happens while CI
is still pending rather than observing it without an agent.

Sources: `internal/batch/orchestrator.go` re-entry branches and
`execute` entry guard; `internal/batch/row_spec.go`.

### F7 — Pending delegated review can cause premature resume

The pure lifecycle decision correctly selects `await` when an active review
request remains pending even if aggregate PR state looks ready. Its gate is
`ready-to-merge`. Adapters key resume eligibility on that gate instead of the
selected action, so an await decision can become a merge-work relaunch.

Diagnostic outcome: 2 launches and 1 `run.resumed` before any reviewer response.
The incident trace also shows approval-labelled resumes at 10:08, 10:10, and
10:11 while the review request is pending; the reviewer run finishes at 10:16.

Sources: `decideRecoverableLifecycle` in
`internal/batch/orchestrator_lifecycle.go`; `runOnce` in
`internal/batch/orchestrator.go`; `tryEntryResume`, `isResumeGate`, and
`resumePromptFromGate` in `internal/batch/orchestrator_resume.go`.

### F8 — Quota can override verified merged completion

`shouldAwaitUsageLimit` checks the usage flag/window but not whether lifecycle
arbitration already selected success. `execute` uses it after the run loop, so a
verified merged result can still emit quota await.

Diagnostic outcome: verified merged PR incurs one ten-minute wait before success.
The existing quota fixture already contains a merged PR at initial failure,
which helps this regression escape the restored tests.

Sources: `internal/batch/orchestrator.go` `shouldAwaitUsageLimit` and `execute`;
`internal/batch/usage_limit_retry_test.go` helper fixture.

### F9 — Historical capacity metadata can retain obsolete ownership

`RunState.BatchID()` prefers `CapacityQueuedEvent` even after a later continuation
has cleared the capacity phase. A continuation into batch `new` can still return
batch `old`. This was reproduced independently of duration.

This affects artifact/log lookup and recovery ownership checks, as well as the
duration comparison that consults `BatchID()`.

Source: `internal/events/run_state.go` `BatchID()`.

### F10 — Batch scheduler drops the observer's action

Code-confirmed: when the batch observer returns anything other than handled
await, the scheduler writes capacity readiness and proceeds toward a slot.
Success, failure, and unhandled lookup results are not distinguished there.
Later execution reevaluates them, but completion/failure can be delayed by
unrelated capacity, and the second evaluation can replace the first outcome.

The incident contains ready-capacity events with `reason: EXTERNAL_GATE_IDLE`,
showing a lifecycle failure being used as readiness evidence.

Sources: `internal/batch/orchestrator.go` batch observation loop;
`internal/batch/await_observer.go`.

### F11 — Remediation budgets are not consistently durable

Code-confirmed: `resumeCount` belongs to a short-lived session, reset for each
`Execute`. `ci_wait.json` contains `remediation_attempts`, but production code in
this baseline neither increments it nor uses it to enforce admission. A test
seeds an exhausted persisted count, then still verifies an in-session cap.

Repeated wait/re-entry therefore does not enforce the same durable per-head
repair budget promised by ADR-0053. Ordinary retry indices also restart per
executor call, making cumulative retry reporting a separate decision.

Sources: `internal/batch/ci_wait.go`, `orchestrator_resume.go`, `row_spec.go`,
and `orchestrator_resume_test.go`.

### F12 — Cancellation still emits retry bookkeeping

Observed in both reported and later batches: after operator cancellation,
`run.retry` events for attempts 2, 3, and 4 are emitted within milliseconds,
then `run.aborted` claims three retries done. `AgentRun` may avoid actual process
execution because its context is cancelled, but the run loop still prepares and
records retry iterations. Cancellation guards currently focus on context rollover.

Sources: `internal/batch/orchestrator.go` retry loop;
`.sandman/events.jsonl` lines 1157–1160 and 1193–1196.

### F13 — Initial quota admission is recorded as a worktree continuation

Observed deferred rows in the first batch have `ready_continuation:true` and
`branch:""`. `ApplyReadyContinuations` requires Run/previous-batch/branch/base
identity and an existing Task. These are appropriate for started work, but not
for a row that never created a worktree. Discovery and application disagree about
the required evidence.

Sources: `internal/batch/await_observer.go`, `ready_continuation.go`,
and `.sandman/events.jsonl` lines 1149–1155.

### F14 — Recovery guarantees differ by scheduling substate

Code-confirmed: true external awaits can be stale-recovered as aborted after
owner death; capacity-marked rows are exempt, even when no valid continuation
can be reconstructed. There is no common durable cancellation intent, lease,
or bounded recovery decision distinguishing a user abort from unclean exit.

Source: `internal/daemon/runfs.go` recovery exemptions and
`internal/batch/ready_continuation.go`; compare open #2628 and #2630.

## 5. Reproduction and existing test gaps

Temporary audit tests are loaded with a Go overlay; they do not modify production
files. They use the documented executor/orchestrator factory seams.

```bash
go test -overlay /tmp/opencode/waiting-state-machine-overlay.json \
  ./internal/batch ./internal/events -run '^TestAudit_' -count=2 -v
```

All seven diagnostic assertions fail consistently in both repetitions:

| Diagnostic | Actual outcome |
| --- | --- |
| Pending review on clean PR | `launches=2 resumes=1 status=await` |
| Quota recovery | `starts=[42 42] statuses=success/queued` |
| Abort quota-deferred row | `statuses=failure/queued/aborted`, batch aborted |
| Verified merged completion + quota flag | `waits=[10m0s]` |
| Quota pause after start-gate queueing | Third row launches |
| Quota recovery followed by CI wait | `launches=3 waits=2` |
| Continuation into new batch | `BatchID()=old`, expected new |

Separate duration regressions reproduce the accumulated-time loss. The local
started-capacity projection change passes focused event/Portal/scheduler/recovery
tests, but that is only one part of the audit.

Why existing green suites missed these combinations:

- The new-start quota gate test uses `parallel:1`; it tests pausing, not recovery,
  concurrent launch revalidation, or cancellation of already-deferred rows.
- Usage-limit tests mostly exercise a single row, not a batch reopening admission.
- Pending-review regression uses a blocked PR, not an apparently clean/ready PR.
- Duration tests intentionally required the now-unwanted cross-batch reset.
- Recovery tests intentionally require capacity rows to survive; they do not
  couple that rule to a durable explicit batch abort.
- CI remediation tests conflate an exhausted durable budget with a fresh
  session-local cap.

## 6. Policy decisions for confirmation

The three explicit operator rules are already known. These remaining choices
need confirmation so future tickets and tests cannot encode contradictory paths.

| ID | Decision | Recommended contract |
| --- | --- | --- |
| P1 | Initial queued rows | They are unfinished batch members until started, blocked, failed, or aborted. If terminal skipped placeholders are needed, distinguish them explicitly. |
| P2 | Run identity and duration | Same RunID preserves accumulated active time across batches; new RunID starts fresh. Waiting never contributes active duration. |
| P3 | Capacity-ready representation | Keep ready/capacity as scheduling metadata; public status remains waiting for started runs. Confirm whether to retain `run.capacity_queued` as a compatibility event or replace new writes with waiting/readiness evidence. |
| P4 | Quota timeouts and scope | Keep ten-minute polling with one durable five-hour episode deadline. Pause the same quota identity, reopen admission on verified recovery. Confirm whether timeout fails directly or permits remaining ordinary retries, and whether scope spans batches/reviewers. |
| P5 | Review approval requirement | A pending confirmed request always prevents approval-based merge resume. Confirm whether every implementation merge requires current-head delegated approval, including PRs with no retained request and otherwise-clean aggregate gates. |
| P6 | Timeout/remediation budgets | Persist per-operation/per-head budgets. New head/new confirmed request is the reset boundary; scheduler re-entry is not. Confirm CI timeout, review timeout, and maximum autonomous recovery counts. |
| P7 | Unclean restart versus explicit abort | Explicit abort ends all unfinished owned rows. Unclean exit preserves valid waiting/ready intent for a bounded grace and automatic rehydration. Confirm grace length and which process owns recovery. |
| P8 | Transient observation errors | For a previously validated operation, permit bounded retry within its existing deadline; never renew it or invent approval. Confirm immediate failure if no prior valid operation exists. |
| P9 | Incomplete clean exits | Missing PR/review delivery gets bounded owned-work remediation, then failure. Confirm whether to use ordinary retries or a separate durable remediation budget. |
| P10 | Dependency propagation | Success requires verified completion and closed work item; failure/blocked → blocked; abort → aborted. Confirm these outcomes. |
| P11 | Non-resettable limits/custom commands | Do not automatically await arbitrary budget/auth/config errors. Confirm which custom commands may opt into recognized provider recovery. |

## 7. Implementation plan after confirmation

1. Record the confirmed table as one lifecycle contract and supersede conflicting
   issue/doc statements explicitly.
2. Separate lifecycle phase, readiness reason/action, admission eligibility,
   operation identity/deadline, and owner/cancellation intent.
3. Make adapters consume the selected **action**, not infer it again from a gate
   string. Terminal decisions do not require an execution slot.
4. Retain unfinished row ownership across every suspension, including quota
   pauses; implement pause → recovery → readmission and abort as real transitions.
5. Fold the same RunID's active-time segments cumulatively, with current owner
   metadata taking precedence over historical capacity evidence.
6. Make deadlines, remediation counts, and recovery/cancellation intent durable
   across executor calls and restart.
7. Turn the diagnostic overlays into permanent production-path regressions;
   cover cross-products rather than each feature in isolation.
8. Verify event fold, terminal summary, Portal/API, dependency gating, resource
   capacity, session reuse, and restart on the same emitted traces. Then publish
   the change and complete delegated `sandman-pr-review`.

Essential end-to-end sequences: CI → review → approval → occupied slot → merge;
quota → recovery → pending CI; concurrent quota → blocked admission → recovery;
quota-deferred row → operator abort; wait → crash → rehydration in new batch;
repeated same-head repair → durable budget exhaustion; and terminal completion
arriving concurrently with quota/cancellation.

The intended endpoint is a transition table that every writer, observer,
scheduler adapter, and reader obeys. A passing test for one isolated gate is
insufficient evidence that the composed state machine is correct.
