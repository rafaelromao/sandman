## Summary

This PR fixes the informal-approval incident from #2817: a current-request/current-head approving review (no-findings body with scope paths, metrics, and resolved history) was double-counted as actionable feedback, causing feedback relaunches and `REMEDIATION_BUDGET_EXHAUSTED` while CI was pending. The fix is evidence-layer only — `informal_feedback.go` now skips bodies that classify as approved, and `review_decision_approval.go` accepts ordinary (`LGTM`, `looks good`, `+1`) and eligible `COMMENTED` approvals while fail-closing on conditional/negated/quoted/repair-bearing bodies. The pre-existing lifecycle (`REVIEW_APPROVED` → `gateReadyToMerge`, verified at `internal/batch/orchestrator_gate.go:191`, `orchestrator_lifecycle.go:316`, `orchestrator_resume.go:51`) then awaits pending CI and resumes merge work unchanged, and the continuation prompt now states informal approval is sufficient. All seven acceptance criteria in #2817 map to new hermetic regressions; CI is green (`mergeable: MERGEABLE`, all required checks pass).

## Quality check

### Scope

10 files changed, ~670 diff additions / ~100 diff deletions (incl. patch headers): Go (`internal/batch/review_decision_approval.go`, `internal/batch/informal_feedback.go`, `internal/prompt/task.go`, plus two new test files) and Markdown contract docs (`AGENTS.md`, `CONTEXT.md`, `docs/architecture/run-state-machine.md`, `docs/development/testing.md`).

mixed scope — behaviour change concentrated in approval interpretation across two code modules (`internal/batch`, `internal/prompt`) plus the required lifecycle-vocabulary doc updates.

### Metrics

Worst cognitive complexity (manual assessment, threshold per `.sandman/reviews/quality-rules.md`): `reviewDecisionApprovedBody` estimated ~18–22 (threshold 15). Flag in metrics summary only: the branching follows the existing section-parser shape, each branch is a narrow section test, and the PR's own regression matrix (ordinary/conditional/negated/quoted/metadata cases) demonstrates the control flow is verifiable — no readability or change-risk concern demonstrated, so no finding per the severity table. Worst cyclomatic complexity (manual count of decision points): ~12–14 in the same function (threshold 20). No flag. Prior coverage of the blast radius not measured: repository has no configured coverage tool.

### Findings

None. No construct-tagged readability concern with a concrete small fix was found: `[control-flow]` nesting stays ≤3 levels, no function mixes parsing with I/O, regexes are package-level precompiled matchers over untrusted-but-inert review prose with no injection or filesystem surface.

### Tools used

Manual review of diff, no static analyzer configured for this PR (`gocognit`/`gocyclo` not installed in this environment; CI-side `go vet` + pinned Staticcheck reported green in the PR evidence).

## Findings

No `Blocking`, `Important`, or `Nit` findings. Verification against each acceptance criterion in #2817:

- Ordinary/structured/`COMMENTED` approval acceptance, empty `reviewDecision` tolerance — covered by `TestInformalApprovalLifecycle_OrdinaryAndCommentedApprovalsSuffice` and the `formal_reviews`/`FormalDecision != none` handling.
- No-findings metadata vs. concrete feedback separation with fail-closed ambiguity — covered by the `informal_feedback.go` approved-body skip, `noReviewFindings` metadata/repair distinction (`fix.go` accepted, `` `fix` ``-verb rejected), and the ~25-case ambiguous-decision matrix; independent feedback still repairs (`SeparateFeedbackStillResumesRepair`).
- Pending-CI await without relaunch, green-CI merge resume — covered by the 5-observation no-launch lifecycle test and the executor test (same `RunID`, zero `run.retry` events, verified merge, dependent release).
- Continuation prompt sufficiency with revalidation — covered by `TestTaskWithReviewEvidence_AcceptedInformalApprovalDoesNotRequireFormalReview` (present for `REVIEW_APPROVED`, absent for feedback-only).
- Request/head/window boundaries and delayed-reentry survival — covered by `RequestBoundariesAndFormalPrecedence` and `OnTimeResponseSurvivesDelayedReentry`.
- Production-seam fidelity — tests drive the real registration → `lifecycleDecisionForTest` → `RunExecutor` path with `WithRunnableFactory`/`WithSandboxFactory` injection, per `AGENTS.md` seams; no new globals, no mutable status shortcuts, atomic writes preserved.
- Negation-word broadening (`not`, `but`, `if`, …) errs fail-closed (await rather than merge), which is the safe direction per the issue's fail-closed criterion.

## Suggested next steps

None. Merge when ready.

## Decision

**APPROVED**

<!-- sandman-publication:ce3a45c7c3e3664e27d4e6d569371aa17c477d089b35e642bed72b9c3e0c6bfc -->
