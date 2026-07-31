# ADR-0047: Retain Specification as an in-memory-blocked parent row

## Status

accepted

## Context

`SpecificationResolver` currently replaces each Specification in the input with its accepted children. The Specification itself is dropped from the batch and never executes as an AgentRun. The legacy contract — "Specification is replaced so the orchestrator never sees it" — is the symmetric counterpart of the resolver's "expand then forget" design (ADR-0021) and is documented as the operator-visible behaviour in `CONTEXT.md`.

That contract has two operational gaps that surface once an operator adopts the broader Specification patterns (threeterm-style multi-level specs, body-only `## Children` headings per ADR-0045, `## Part of` per ADR-0046):

1. **Closed children are not a closed parent.** When the spec's children succeed, the parent is left open on GitHub. Operators have to close the parent manually even though every accepted child has reached `success`. There is no in-batch verification path that proves "every leaf of this spec is implemented".
2. **All-closed expansion collapses the spec.** When every discovered child is already closed, the spec disappears from the batch. A user-typed spec on a fully-implemented repo gets silently dropped — no AgentRun, no `## Status: already resolved` path, no close attempt.

The fix must preserve the existing dependency-resolver contract. `ResolvedBatch.Deps` already holds in-memory blocker edges that the orchestrator reads; the topological sort already sequences dependents after their blockers. The change is to add a new category of in-memory edge: a parent → child edge synthesised by the resolver, so the retained parent row is held back from starting until its children succeed.

## Decision

1. **Retain the Specification in the resolved list, placed immediately after its accepted children.** `SpecificationResolver.Resolve` keeps the spec at the end of its own expansion slice, after every leaf of that slice. Nested specs retain at the end of each of their slices, so the order is `[grandchild, middle, root]` for a three-level chain. The legacy "spec is replaced" claim is retracted: the spec now participates in the batch as an ordinary AgentRun row.

2. **The retained parent uses the default AgentRun prompt and verification flow.** No special "parent completion" mode is introduced. The default task prompt renders, the agent can write `## Status: already resolved` (the orchestrator's verification chain still recognises the marker), and the close path is the same close path every other issue uses. The retained parent behaves like any other typed issue at the AgentRun level.

3. **In-memory `ParentChildren` map flows from the resolver to the dependency resolver.** `SpecificationResolver.Resolve` returns `([]int, map[int][]int, error)`. The map keys are retained parent issue numbers; the values are the deduped, sorted, accepted *open* children. The cmd layer (`internal/cmd/run.go`) threads the map into `DependencyResolver.Resolve` as a new `parentChildren map[int][]int` parameter. Callers that don't care about parent-gate edges (unit tests of the dep resolver in isolation) pass `nil`.

4. **Synthetic edges are unioned with declared `BlockedBy`, not replaced.** `DependencyResolver` reads `parentChildren[issueNum]` and unions the synthetic edges with the active blockers it has already computed from `BlockedBy`. The union is deduplicated and sorted so the topological sort can compare the set stably. A parent with declared `[99]` and synthetic `[10, 11]` resolves to `deps[1] == [10, 11, 99]`.

5. **Closed children are dropped from the synthetic edge set.** The dependency resolver only unions a synthetic child when the child is in the `known` set (i.e. it actually made it into the requested batch after the closed-issue filter). A child that the cmd layer filtered out as closed is silently dropped from the gate — no missing-blocker error, no explicit close attempt for a child that is already closed.

6. **The synthetic edge never lands on GitHub.** The parent's `BlockedBy` field is untouched; the spec's children do not gain a `BlockedBy` pointing at the parent; the comment harvest, the body heading, and the native `/dependencies/blocked_by` are all unchanged. The edge lives only in `ResolvedBatch.Deps` for the lifetime of the batch.

7. **A Specification with no accepted children runs as a single regular row, no synthetic blockers.** The "no-children" branch in `expandOne` is unchanged: the spec is added to the output via `addUnique(num)` and the strict-spec log line is suppressed for non-spec bodies (per ADR-0034). No `ParentChildren` entry is recorded, so the dependency resolver does not synthesise a blocker edge.

8. **Two specs sharing a child both retain the child once and both retain a synthetic edge to it.** Global deduplication via `addUnique` already handles the child; the per-spec `recordChildren` callback populates each parent's `ParentChildren` entry independently. The shared child is fetched once; each parent's gate still requires the child to reach `success`.

9. **The orchestrator, portal, and prompt are unchanged.** The retained parent is an ordinary row in `ResolvedBatch.Issues`; the orchestrator's per-row lifecycle (queue → start → finish → dependency release) is what gates the parent on its children. The portal renders the parent like any other issue. The default task prompt is the unchanged Default Task Prompt.

## Consequences

### Positive

- The operator gap (children succeed → parent left open) is closed: the retained parent runs, verification can write `## Status: already resolved`, and the close path runs like any other issue.
- The all-closed-expansion gap (spec silently dropped) is closed: even with every child closed, the retained parent runs so the operator can choose to mark it `## Status: already resolved` or close it explicitly.
- The change is contained to the expansion stage + the dependency resolver. The orchestrator, the verification flow, the portal, and the prompt are untouched.
- The `In-batch blocker` contract from `CONTEXT.md` is preserved: the parent's gate is sourced from the batch's terminal status, not from the GitHub issue state at start time. A child that fails terminal causes the parent to be marked `run.blocked` via the existing cascade path.
- The existing topological sort already sequences dependents after their blockers, so the parent's place in the resolved list is natural — it sits immediately after its open children, regardless of how many siblings or external blockers exist.

### Negative

- The legacy "Specification is replaced, never seen by the orchestrator" claim is retracted. Tests that asserted this contract had to be updated to the new "children + retained spec" shape. The change is a one-time update; the test counts are recorded in the commit.
- Closed-issue filtering now leaves the retained parent in the batch even when every child is closed. The batch still runs (the parent is the only row), so a fully-closed spec produces a single-row batch rather than a no-op. This is the desired operator-visible behaviour but is a small change in how the `--all-closed` no-op signal is generated.
- A retained parent whose children succeed but whose verification is inconclusive still relies on the agent writing `## Status: already resolved` to close. The default prompt's instructions are unchanged; the operator's mental model is unchanged.
- The `SpecificationResolver.Resolve` signature now returns three values. Every internal test had to be updated to ignore the second return via `_, _, err := r.Resolve(...)`. The breaking-change cost is paid once; the new signature is the public API going forward.

### Neutral

- The `ParentChildren` map is built once during expansion and consumed once by the dependency resolver. It is not persisted to the event log, the spec's body, or the spec's comment thread. The in-memory-only contract is the load-bearing design choice.
- The default AgentRun prompt is unchanged. The retained parent is an ordinary row; the agent's contract on `## Status: already resolved` is unchanged.
- The `ResolvedBatch.Deps` map now carries three categories of edges: declared `BlockedBy`, declared `BlockedBy` unioned with synthetic parent-gate edges, and pure synthetic edges. The dep resolver's union logic treats them uniformly via the `mergeSyntheticBlockers` helper — the topological sort does not need to know which is which.
- ADR-0021 step 3 (parent verification) is amended in place by this ADR; the high-level flow described there (spec detection → harvest → verify → flatten) is unchanged. ADR-0042, ADR-0045, ADR-0046, and ADR-0034 §4 are unaffected; the new behavior stacks on top of them.
- The smoke and e2e tiers are unaffected because no CLI flag changes. The cmd layer's only change is the new return value from `expandSpecifications` and the new parameter to `DependencyResolver.Resolve`; the user-visible CLI surface is identical.
