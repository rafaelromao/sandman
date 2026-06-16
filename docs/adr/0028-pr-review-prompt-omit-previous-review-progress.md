# ADR-0028: PR review prompt — omit `## Previous review progress` when there are no prior reviews

## Status

accepted

## Context

Parent PRD #907 hardened `internal/prompt/default_pr_review_prompt.md` with four targeted improvements. The first of these — the **conditional review history section** — instructs the review agent to omit the `## Previous review progress` section from the posted comment when the PR has no prior reviews, instead of writing a placeholder like "No previous reviews found."

The rule appeared in the prompt from PR #922 onwards, but on PR #1018 the model still rendered a `## Previous review progress\n\nNo prior review comments or reviews on this PR.` placeholder despite the rule. Two follow-up slices hardened the prompt against this regression:

- **Slice 1** (issue #1025, PR #1059) — `internal/prompt/engine_test.go` gained a static-prompt guard test (`TestDefaultPRReviewPrompt_ContainsOmitPreviousReviewProgressRule`) that reads the prompt file from disk and asserts the canonical omit phrases are present. The test fails on any branch that deletes or rewords the rule.
- **Slice 2** (issue #1026, PR #1060) — the `## Previous review progress` format-spec bullet was moved to the end of the format-spec list and re-framed in negation ("Do not render this section if there are no prior reviews. Do not write a placeholder…"), breaking the "always render this slot" visual pattern the other four slots establish.

## Decision

Verification close-out for the Slice 1 + Slice 2 fix lands as a no-op diff against `main`: the prompt file, the guard test, and the negative-framed format-spec bullet are all already present in `origin/main` (verification-time snapshot was commit `8e05c9e`; `origin/main` has since moved on with commits orthogonal to the prompt content — the cited SHA is a verification-time snapshot, not a merge target). The close-out is the act of pinning the regression guard and recording the verification result.

The static-prompt guard test (`engine_test.go:653`) pins the following three phrases:

1. `**omit** the \`## Previous review progress\` section from the posted comment` — procedural instruction in the review procedure (prompt line 46).
2. `Do not render this section if there are no prior reviews` — format-spec, negative-framed (prompt line 89).
3. `Do not write a placeholder such as "No previous reviews found."` — format-spec, negative-framed (prompt line 89).

Any future regression on the PR #1018-style behaviour must first delete or reword one of these three phrases — at which point `go test -race ./internal/prompt/...` fails locally and in CI before the change can land.

## Consequences

### Positive

- The PR #1018 regression cannot recur silently: a wording edit that drops the omit rule fails the static guard test.
- The format-spec bullet is visually distinct from the other four "always render" slots (Summary, Findings, Suggested next steps, Decision), reducing the chance a model treats it as a required section.
- The verification log is reproducible: `go test -race ./internal/prompt/...` is sufficient to confirm the fix is in place.

### Negative

- The static-prompt guard is a string-equality check, not a behaviour check. It pins the wording but does not guarantee the model obeys it. A more robust end-to-end check would run the review agent against a fixture PR with empty prior reviews and assert the comment omits the section — that remains an operational task, not a CI gate.
- A line-edit PR that legitimately wants to reword the omit rule must update the test in the same change. That is the intended cost.

### Neutral

- The diff for this verification close-out is intentionally empty against `main`; the value is in pinning the existing test and recording the result.

## Blocked by

None. Both blocked-by issues (#1025 and #1026) are closed and merged.

## Runtime Context

- You are running inside a Sandman-created worktree.
- Current branch: `sandman/1027-verification-close-out-omit-previous-review-progress-fix`
- Source branch: `sandman/1027-verification-close-out-omit-previous-review-progress-fix`
- Base branch: `main`
- Review command: `/sandman review`
