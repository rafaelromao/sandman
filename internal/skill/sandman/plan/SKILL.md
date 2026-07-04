---
name: sandman-plan
description: Create an execution-ready implementation plan from the implementor's open work item, codebase exploration, and subagent review. Use before sandman-tdd when sandman-implement needs a behavior-first plan.
---

# Plan

Create a plan that `sandman-tdd` can execute directly.

## Purpose

Turn issue context into a concise, behavior-first plan:

- explore the codebase and understand the issue context
- identify behaviors to test, not implementation steps
- design interfaces for testability
- get subagent review and reach consensus before finalizing
- write the result into `.sandman/task.md` under `## Plan`

## Workflow

### 1. Gather context

- Read the issue body, parent issue, and linked comments.
- Read relevant repo docs, especially `CONTEXT.md` and nearby ADRs.
- Read sibling Sandman skills that frame the workflow, especially `sandman-implement` and `sandman-tdd`.
- Use the repo's glossary and domain language so the plan matches existing terminology.

### 2. Explore and design

- Inspect code paths closest to the issue.
- Design testable seams: identify where to introduce interfaces, accept dependencies, and separate side effects so that each behavior can be verified through a public boundary.
- Prefer deep modules: small surface area, complex logic hidden behind it.
- If no seam exists today, identify where to create one — a new interface, a dependency injection point, or a pure-function extraction.

### 3. Draft the plan

- List behaviors to test in execution order, starting with the tracer bullet.
- For each behavior, name the observable outcome, not the implementation.
- For each slice, note the interface seam or boundary that makes it testable.
- Call out assumptions, risks, or open questions.
- Keep the plan executable: no file-by-file chores, no internal refactor steps, no speculative extras.

### 4. Ask for subagent review

- Send the draft plan to a general-purpose subagent for review.
- Include the issue context, the draft plan, and the `Search Scope Restriction` verbatim.
- Review against this rubric:
  - behavior-first, not implementation-first
  - clear testability seams
  - execution-ready ordering for `sandman-tdd`
  - full coverage of issue context, ADRs, and sibling skill constraints
  - no hidden implementation steps

### 4a. Subagent liveness cap

- Start a wall-clock timer when the review subagent is spawned.
- Hard-reject that attempt at the **20 minute** mark, whether or not a result has returned.
- Re-spawn up to **2** times after a timeout, for **3 total attempts** maximum.
- If all 3 attempts hit the cap or fail to reach consensus, surface a **subagent stuck** or **review-failed** finding and stop looping.

### 5. Finalize the plan

- Revise until consensus.
- Write the final result into `.sandman/task.md`.
- Preserve the task document scaffold; do not replace it.
- Put the plan in a top-level `## Plan` section inside the body.
- Below the plan, add a `## Next Step` section set to the first `sandman-tdd` action, so the execution phase can start immediately.

## Plan output shape

Use this shape inside `.sandman/task.md`:

```md
## Plan
### Behaviors to test
- ...

### Testable interfaces
- ...

### Assumptions / risks
- ...
```

## Rules

- Behaviors only, not implementation steps.
- Public interfaces and seams only, not private details.
- Ordered slices only, not a bulk test list.
- Write the task doc in-place, not as a replacement for its scaffold.
