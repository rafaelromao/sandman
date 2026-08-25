# ADR-0054: Retain GitHub closure gate for in-batch blockers

## Status

accepted

## Context

ADR-0003 defined the success-dependency semantics as a two-part gate: a dependent AgentRun for issue A cannot start until every blocker B in A's `BlockedBy` has both reached batch status `success` and is `closed` on GitHub. ADR-0016 superseded that clause for the in-batch population, replacing it with a single-source rule: in-batch blockers need only reach `success` in the batch; GitHub `state` is not consulted. External blockers remained gated on `closed`.

In practice the single-source rule made the failure mode described in #2655 hard to diagnose. A batch could complete a blocker as `success` while its GitHub issue remained `open`. Its dependent would then run against a work item that was not formally complete, and a later `--override` could erase the blocker history, leaving Portal fallback with no durable dependency cause. The operator could not distinguish a correct block from an agent failure, and a replaced branch could keep its pull request misleadingly open.

The product decision is to restore the two-part gate for in-batch blockers and keep it aligned with external blockers, at the cost of re-introducing sensitivity to GitHub closure latency. Dependency outcomes must remain terminal and diagnosable without a wait state.

## Decision

Restore the two-part in-batch gate. A dependent AgentRun may start only after every in-batch `BlockedBy` issue both reaches terminal batch status `success` and is reported as `closed` on GitHub at the dependent start decision.

If a successful in-batch blocker remains `open` at that instant, the dependent finishes as terminal `run.blocked` with the open blocker(s) named in `blocked_by`; no agent is launched and no wait or retry state (`run.await`) is added. This is the same terminal dependency outcome used for `failure` and `blocked` blockers. A blocker that finished as `aborted` cascades as `run.aborted` with `aborted_by`.

Implementation is the in-batch closure check already present in `internal/batch/orchestrator.go`'s per-dependent goroutine: after the channel wait, a `success` blocker is re-fetched via `FetchIssueState` and an `open` result moves the blocker into `stillBlockedBy`, which emits `run.blocked` without entering the launch path. The external blocker check in `runSession.execute` (`recheckBlockedBy` on `ExternalBlockers`) is retained unchanged; the two checks compose into a uniform `success AND closed` policy. `CONTEXT.md` BlockedBy / In-batch blocker / External blocker entries, `docs/usage/monitoring.md` Blocked runs and `run.blocked` payload, and this ADR establish the policy as canonical. ADR-0016 is marked `superseded by ADR-0054` per immutability; ADR-0003's two-part wording is restored as the intended policy.

## Consequences

### Positive

- Dependents cannot advance before the blocker work item is formally closed on GitHub, matching the operator's definition of complete.
- The dependency failure mode is diagnosable: a blocked row always carries `blocked_by` (or `aborted_by`) and `batch_id`; Portal and event-log fallback retain the same cause without a separate log file.
- Semantics are uniform across in-batch and external blockers, simplifying the `BlockedBy` definition in `CONTEXT.md`.

### Negative

- A batch that co-schedules a dependent with its blocker is now sensitive to GitHub closure latency again. If the blocker's issue does not auto-close promptly, the dependent is correctly blocked and must be re-run after closure (or not co-scheduled).
- Topological ordering alone no longer implies startability for dependents; both status predicates must be checked before launch.

### Neutral

- No new event types or payload fields; `run.blocked` (and `run.aborted` for the abort cascade) remain the terminal dependency outcomes.
- Regression coverage lives in `TestRunBatch_InBatchBlockerSuccessButIssueOpen_EmitsRunBlocked`, `TestRunBatch_InBatchBlockerSuccessKeepsDependentBlockedWhenIssueOpen`, `TestRunBatch_InBatchBlockerStaysQueuedUntilBlockerTerminal`, and `TestRunBatch_InBatchBlockerAborted_EmitsRunAborted` in `internal/batch/orchestrator_issue2186_test.go`, plus the orchestrator-level blocked/aborted cascade suites.
