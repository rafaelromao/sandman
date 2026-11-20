## Summary

This PR reverts #2976 (`da2068bc`), so neither Sandman's runtime nor the `sandman-pr-merge` skill closes the implementor's work item after a merge. It removes:
- the runtime fallback `closeIssueAfterMerge` and its plumbing: `verifiedMergedPR` on `runSession`, `mergedPR` on `lifecycleVerdict`, and the method form of `checkPRMergedForIssue`;
- `baseRefName` from the `FindPRByBranch` lookup;
- step 6 of the merge skill, with its contract test, and the runtime tests;
- the **Base branch** glossary entry and the ADR-0061 sentences in `AGENTS.md`, `CONTEXT.md`, `run-state-machine.md`, `monitoring.md` and the `sandman-implement` skill.

ADR-0061 is kept with status `rejected` and a note pointing to Specification #2977, and its index row reads `rejected`. Dependency gating returns to the ADR-0054 behaviour: a blocker counts only when it succeeded **and** its issue is closed.

I checked every acceptance criterion of #2978:

| Acceptance criterion | Result |
| --- | --- |
| Every `da2068bc` change reverted except ADR-0061 | Met. I excluded the two ADR files and compared every changed line of `git diff origin/main HEAD` with `git diff da2068bc da2068bc^`. They match exactly, even though four later commits landed on `main`. No other code or docs still use `verifiedMergedPR`, `closeIssueAfterMerge`, `mergedIssueCloseComment`, the **Base branch** glossary term or merge-skill step numbers. The remaining `baseRefName` uses predate #2976: the PR-number `gh pr view` lookup and the skills' `gh pr view --json baseRefName`. |
| ADR-0061 kept as `rejected` with a note, index row `rejected` | Met. It follows the existing `rejected` convention used by ADR-0048. |
| `go run scripts/check_adr_index.go` passes | Met: `PASS: ADR index is structurally sound`. |
| fmt/vet/staticcheck and targeted tests pass | Met. Locally, `gofmt -l` is clean, `go build ./...` succeeds, and `go vet` passes on the touched packages. `go test ./internal/github/... ./internal/skill/... ./internal/batch/...` passes every package, including `internal/skillsandman/blind-review`. I did not run staticcheck locally; CI `static-analysis` passed. |
| PR title is `revert: close implemented issues after merge into the configured base branch` | Met. CI `semantic-pull-request` passed. |

Pre-flight: the PR is `MERGEABLE`, and every CI check on head `b98546a7` passes, including `quality-gate`. `regression` is skipped, as expected for ordinary PRs.

## Quality check

### Scope

17 files modified. 31 lines added and 549 removed: Go 470 lines changed, Markdown 110. Modules touched: `internal/batch`, `internal/github`, `internal/skillsandman`, `docs/` and the root agent docs.

`cross-cutting — runtime lifecycle, GitHub client, a skill contract and the project docs together, all removing one rejected decision.`

### Metrics

Analyzers: `gocognit` v1.2.0 and `gocyclo` v0.6.0, run on the changed files at HEAD and at `origin/main`.

- Restored `checkPRMergedForIssue` (`internal/batch/completion.go:69`): cognitive 9 (threshold 15), cyclomatic 10 (threshold 20). No flag.
- Worst cognitive in a changed location: 302 (threshold 15). Flag: `(*runSession).runOnce`, `internal/batch/orchestrator.go:3798`. The score predates this PR and is unchanged by it; the PR only swaps call-site syntax there.
- Worst cyclomatic in a changed location: 129 (threshold 20). Flag: `(*lifecycleObserver).decideLifecycleForAttempt`, `internal/batch/orchestrator_lifecycle.go:562`. The score predates this PR, which lowers it (cyclomatic 132→129, cognitive 154→151).
- `finishDecidedTerminal` also drops from cognitive 23 to 21.

None of these breaches comes with a readability or change-risk concern from this diff, so they are reported here only.

Prior coverage of the blast radius was not measured in this review. The repository's advisory CI coverage profiles were not inspected.

### Findings

None. The diff only deletes code or restores pre-#2976 code, and no rule tagged `[control-flow]` or `[public-api]` is newly violated.

### Tools used

`gocognit` v1.2.0 and `gocyclo` v0.6.0, plus manual review of the diff.

## Findings

No findings.

## Suggested next steps

- None needed for this PR. As the PR body says, dependents of issues merged into a non-default base finish `blocked` again until those issues are closed by hand. The later #2977 slices are meant to fix that.

## Decision

**APPROVED**

<!-- sandman-verdict:approved -->
<!-- sandman-publication:93b1c4da1630b955d806fb2d97af64f7aae25d98b7e9439dae15f0511f4d5390 -->
