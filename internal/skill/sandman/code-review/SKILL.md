---
name: sandman-code-review
description: Review changes along separate Standards and Spec axes. Use in self-review context for an implementor's own diff, or in daemon-review context when authoritative pull-request context and a review worktree are supplied.
---

# Code Review

Use one of the explicit contexts below. Keep Standards and Spec findings separate: a change can meet the requested behavior but violate a documented standard, or follow standards while failing the requested behavior.

## Self-review context

Use this context when an implementor reviews its own changes.

### 1. Pin the fixed point

Use the fixed point supplied in the invocation. If none was supplied, use `origin/main` when available, then the current branch's upstream, then `HEAD~1` when it exists. If no historical baseline exists, use `HEAD` as the committed baseline, inspect `git diff HEAD` and every untracked path listed by `git status --short`, and record that no historical baseline was available.

Confirm the fixed point resolves and the diff is non-empty before proceeding. Capture `git diff <fixed-point>...HEAD` and `git log <fixed-point>..HEAD --oneline` once.

### 2. Identify review sources

Find the originating work item, a supplied specification path, or a matching specification under `docs/`, `specs/`, or `.scratch/`. If no specification is available, use the work item's body and change-request body; if neither supplies a specification, the Spec assessment records `no spec available` while Standards continues.

Collect documented standards from contributor guidance, domain vocabulary, architectural decisions, and machine-enforced configuration. Do not manually re-check rules already enforced by tooling.

### 3. Run the two axes in parallel

Use separate review agents so the axes do not pollute each other's context. Give the Standards reviewer the diff, commit list, and standards sources; it reports documented-standard violations with source citations. Give the Spec reviewer the diff, commit list, and specification; it reports missing, partial, incorrect, and out-of-scope behavior with requirement citations. Skip the Spec reviewer only when no specification is available.

Cap each review agent at 20 minutes. If an agent stalls, retry that axis up to two times. After three stalled attempts, report `sub-agent stuck` under that axis and continue with the other axis. Record the timeout and next executable action in the run log before ending a blocked review.

### 4. Review and report

Review the diff separately for:

- **Standards**: violations of documented repository conventions, citing the source and rule.
- **Spec**: missing, partial, incorrect, or out-of-scope behavior, citing the requirement.

Report under `## Standards` and `## Spec` headings. State the finding count for each axis and the worst finding.

## Daemon-review context

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

Write the full Markdown decision to the supplied `decision.md` path using an atomic temp-file-and-rename write. The surrounding workflow reads and posts that file; do not post it yourself.

Use this structure:

- `## Summary`
- `## Standards`
- `## Spec`
- `## Quality check`
- `## Findings`, grouped as `Blocking`, `Important`, and `Nit`; omit empty groups
- `## Suggested next steps`
- `## Decision` with `**APPROVED**` only when there are no Blocking or Important findings, otherwise `**CHANGES_REQUESTED**`
- `## Previous review progress` only when the supplied prior-review state says it exists

Keep the decision terse and actionable. When no quality rules are available, say `Quality rules unavailable in this repository; no built-in quality-rule evaluation was applied.`

When quality rules are available, `## Quality check` always includes:

- `### Scope`: changed files, language mix, modules, blast radius, and one of `focused`, `mixed scope`, or `cross-cutting` with a one-line justification.
- `### Metrics`: worst cognitive and cyclomatic values from an available analyzer, their configured thresholds, and prior blast-radius coverage when a coverage tool exists. Otherwise state `Prior coverage of the blast radius not measured: repository has no configured coverage tool.`
- `### Findings`: quality findings with their applicable construct tags.
- `### Tools used`: the analyzer used, or `Manual review of diff, no static analyzer configured for this PR.`

For every Blocking or Important finding, cite the file and line range, quote the relevant code or behavior, explain the concrete impact, and name the smallest useful fix. Do not invent complexity or coverage measurements. Do not recommend splitting the change. An empty `## Findings` section means approval when no Blocking or Important findings exist.

The pull-request review decision must preserve this complete output contract:

```markdown
## Summary

<one paragraph describing the supplied change>

## Standards

<documented-standard assessment and finding count>

## Spec

<acceptance-criteria assessment and finding count>

## Quality check

<the quality-rules verdict and, when available, Scope, Metrics, Findings, and Tools used>

## Findings

### Blocking

<only if present: citation, quoted behavior, impact, and smallest fix>

### Important

<only if present: citation, quoted behavior, impact, and smallest fix>

### Nit

<only if present: actionable optional improvement with its source>

## Suggested next steps

<minimum follow-ups>

## Decision

**APPROVED**

## Previous review progress

<only when authoritative prior-review state exists; status each prior finding>
```

Use `**CHANGES_REQUESTED**` instead of `**APPROVED**` whenever a Blocking or Important finding remains. The surrounding workflow atomically reads `decision.md` and posts it; the reviewer never posts the body or performs any pull-request action.
