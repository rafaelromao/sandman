# ADR-0018: Unblock dependents from same-batch success

## Status

proposed

## Context

ADR-0003 established success-dependency semantics: an AgentRun for dependent A cannot start until every blocker B in A's `BlockedBy` set has an AgentRun that completed with status `"success"` **and** the corresponding GitHub issue is closed immediately before start time. The orchestrator enforces both halves in `runSingle`: it first waits on the per-issue completion channel (in-batch success gate), then re-fetches every blocker via the GitHub API and only proceeds if every blocker issue's `state == "closed"`.

The two-gate design assumed that the closure of a blocker's GitHub issue happens before its dependent is ready to start. In practice, closure lags merge for two reasons:

1. The agent merges the blocker's PR (which moves Sandman to status `"success"`) before the issue auto-closes, since GitHub's auto-close happens on PR merge but is observed asynchronously by `gh issue view`.
2. Some workflows do not auto-close the issue at all; the human (or a follow-up agent step) closes it manually after verifying the merge.

In both cases, the GitHub-state gate kept reporting "still open" at the moment the dependent was ready to start, so the dependent was emitted with status `"blocked"` and never ran. The 567-after-566 case made this concrete: 566 finished successfully in the batch, its PR was merged, but its issue was still open at the instant 567's dependent run reached `recheckBlockedBy`; 567 was emitted as `"blocked"` even though the batch had everything it needed to start it.

The GitHub-state gate exists for a different scenario: an **external** blocker — an issue named in A's `BlockedBy` but not in the current batch. Sandman has no in-batch signal for that blocker, so it must consult GitHub state. ADR-0003's wording conflated the two cases.

Alternatives considered:

- **Drop the GitHub-state gate entirely**: simplest, but it removes the only mechanism that prevents Sandman from starting a dependent against an unfinished external blocker. Rejected on the grounds that external dependency gating is the original motivation for the gate.
- **Wait longer / retry the GitHub-state check**: hides the bug behind a timeout that is wrong for both directions — too short for slow GitHub propagation, too long when the issue will not auto-close at all.
- **Split the gate by source**: keep the in-batch success gate (the channel wait) for in-batch blockers, and keep the GitHub-state gate only for external blockers. Each gate runs on the population it is correct for. Chosen.

## Decision

`runSingle` rechecks GitHub `state` only for **external** blockers — issues listed in `req.Blocked[issueNum]`, i.e., issues named in the dependent's `BlockedBy` that are not part of the current batch. In-batch blockers (issues in `req.Issues` that the dependent listed in its `BlockedBy`) are gated solely by the per-issue completion channel and their batch status.

When the channel wait at the top of the dependent's goroutine returns, the orchestrator already knows the in-batch blocker's terminal batch status (`"success"`, `"aborted"`, `"failure"`, or `"blocked"`) and handles the non-success cases there. By the time `runSingle` is invoked, every in-batch blocker has succeeded; re-asking GitHub is redundant and harmful.

The change is one line in `internal/batch/orchestrator.go`:

```go
// Before
blockedBy, err := o.recheckBlockedBy(ctx, append(blockers, externalBlockers...))

// After
blockedBy, err := o.recheckBlockedBy(ctx, externalBlockers)
```

The now-unused `blockers` parameter is dropped from `runSingle` and its single caller, so the dead-code smell does not survive the fix. `CONTEXT.md` adds **In-batch blocker** and **External blocker** as glossary terms and updates the **BlockedBy** definition to reflect the two-population rule. ADR-0003 is not edited per the immutability convention; this ADR supersedes the "and the corresponding GitHub issue is closed immediately before start time" clause in ADR-0003's success-dependency rule, but only for the in-batch population. External-blocker gating is unchanged.

## Consequences

### Positive

- A dependent run starts as soon as every in-batch blocker has succeeded, regardless of how fast (or whether) GitHub propagates the issue closure. The 567-after-566 case now works.
- The GitHub-state gate is left intact for the population it was designed to gate — external blockers — preserving the original ADR-0003 guarantee that a dependent does not run against an unfinished out-of-batch prerequisite.
- The semantics no longer depend on GitHub auto-close latency or on whether the workflow auto-closes issues at all.

### Negative

- A dependent in the same batch will now start even if a human deliberately wanted to keep the blocker issue open (for example, to flag rework after the PR merged). That intent must now be expressed by not co-scheduling the dependent in the same batch, rather than by leaving the blocker issue open.
- Reasoning about a batch's start gate is slightly more nuanced: in-batch and external blockers are gated by different checks. The `BlockedBy` definition in `CONTEXT.md` is the canonical place readers should consult.

### Neutral

- No new public API, no new event types, no new payload fields. The `run.blocked` event is still emitted when an external blocker fails the GitHub-state check, with the same payload shape.
- Regression coverage lives in `TestRunBatch_InBatchBlockerSuccessUnblocksDependentDespiteOpenIssue` (in-batch unblocks) and `TestRunBatch_RechecksExternalBlockerStateBeforeDependentStart` (external still gates) in `internal/batch/orchestrator_test.go`.
