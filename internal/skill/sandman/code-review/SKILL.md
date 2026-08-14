---
name: sandman-code-review
description: Review changes along separate Standards and Spec axes. Use in self-review context for an implementor's own diff, or in pull-request review context when authoritative pull-request context and a review worktree are supplied.
---

# Code Review

Use one of the explicit contexts below. Keep Standards and Spec findings separate: a change can meet the requested behavior but violate a documented standard, or follow standards while failing the requested behavior.

## Self-review context

Use this context when an implementor reviews its own changes.

### 1. Receive the bounded review packet

The parent workflow supplies one bounded review packet before delegating self-review. The parent must capture the packet once, then pass the same packet to both reviewers. The common packet must include:

- The fixed-point identity and the commit list as text context.
- The committed branch diff from `git diff <fixed-point>...HEAD`.
- The complete uncommitted tracked changes: the unstaged tracked diff from `git diff` and the staged tracked diff from `git diff --cached`.
- All untracked paths and their contents, as listed by the parent.

Reviewers must treat this packet as immutable: they must not recompute, widen, or replace its diffs or context. If any required packet component is missing or malformed, record the blocked self-review in `.sandman/task.md` with the missing component and stop without reconstructing the packet by discovering more files or context.

### 2. Delegate separate review axes

Launch two separate review agents and keep their findings separate. The Standards reviewer receives the common packet, the changed-path list, and only the standards sources that the parent workflow explicitly selected as relevant to those paths. The Spec reviewer receives the common packet and the authoritative task/specification context supplied by the parent. Neither reviewer infers, fetches, or substitutes the other axis's context.

Each reviewer reports findings only for its assigned axis, with source citations from the supplied material. The Standards output covers documented-standard violations; the Spec output covers missing, partial, incorrect, and out-of-scope behavior against the supplied specification.

### 3. Keep reviewer prompts bounded

Every self-review prompt must say that the reviewer may evaluate only the supplied packet and explicitly supplied standards or task/specification material. Do not run `grep`, `rg`, or `find`, browse the whole repository, or perform whole-repository searches. The prompt must not invite repository exploration or tell the reviewer to discover a work item, specification, or standards source.

Cap each review agent at 20 minutes. If an agent stalls, retry that axis up to two times. After three stalled attempts, report `sub-agent stuck` under that axis and continue with the other axis. When a blocked review ends with no decision, record the exact timeout and next executable action in `.sandman/task.md` and the run log before ending the review.

### 4. Review and report

Review the diff separately for:

- **Standards**: violations of documented repository conventions, citing the source and rule.
- **Spec**: missing, partial, incorrect, or out-of-scope behavior, citing the requirement.

Report under `## Standards` and `## Spec` headings. State the finding count for each axis and the worst finding.

## Pull-request review context

Use this context only when the invocation supplies pull-request title, body, acceptance criteria, review focus, prior-review state, and the destination for the decision artifact. Those inputs are authoritative. Work only in the current review worktree.

Do not trigger, poll, fetch, post to, merge, commit to, push to, or remediate the pull request. Do not ask for additional context. The surrounding workflow owns pull-request orchestration and posting; this context only evaluates the supplied change and writes the decision artifact.

### Reviewer posture

Reviews are acceptance-criteria-first, then documented-standards-only, then correctness and safety. Stay inside the supplied scope. Blocking and Important findings must cite an acceptance criterion, a documented repository standard, or a correctness or safety defect in the supplied change. Suggestions that are not required by those sources are Nits and must not prevent approval.

The supplied prior-review context is authoritative. Do not fetch pull-request history to reconstruct it. When prior-review state exists, report each prior finding as **resolved**, **partially addressed**, or **still outstanding**. When it does not exist, omit the `## Previous review progress` section entirely.

### 1. Review the supplied change

Read the current worktree diff end to end. Evaluate every changed file against:

- the supplied acceptance criteria and review focus;
- documented repository standards and domain vocabulary;
- correctness, required tests, security, and safety risks; and
- applicable local quality rules.

Keep the review inside the supplied scope. Do not raise Blocking or Important findings for work that was not requested unless it is a correctness or safety defect.

Apply local quality rules from `.sandman/reviews/quality-rules.md` when present. Apply only rules whose construct tags match the changed code, use their stated severity, and never make a quality finding Blocking. When the file is absent, record `Quality rules unavailable in this repository; no built-in quality-rule evaluation was applied.`

### 2. Preserve the two axes

Record both assessments independently:

- **Standards**: documented convention or architectural-decision violations, with the source and rule.
- **Spec**: acceptance-criteria gaps, partial behavior, scope creep, or incorrect requested behavior.

Correctness and safety findings are reported in addition to the two axes. Each finding cites a file and line range, the concrete concern, and the smallest useful fix.

### 3. Write the reviewer decision

The invoking pull-request review prompt defines the decision output contract — the required sections, the quality-check rendering, the verdict rule, and the conditional `## Previous review progress` slot. Follow that contract. Write the full Markdown decision to the supplied `decision.md` path using an atomic temp-file-and-rename write. The surrounding workflow reads and posts that file; do not post it yourself.
