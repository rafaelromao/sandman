# ADR-0025: Specification expansion to child issues

## Status

accepted

## Context

Sandman accepts issue numbers, label filters, and `gh`-query strings as
batch inputs. Some users want to hand Sandman a single issue that
describes a set of vertical slices (a Specification) and
have Sandman implement the slices, not the umbrella Specification itself.

Today, the only way to use a Specification as an input is to expand it by hand:
copy the child issue numbers from the Specification body, comments, or
cross-references, and pass them to `sandman run`. There is no first-class
support for Specifications in the batch preparation flow.

This change introduces Specification detection and child expansion so that
`sandman run <spec>` resolves to a batch of the Specification's child issues
before dependency resolution and execution.

Specifications and their children are identified by body structure, not labels:

- A Specification is an issue whose body declares children — in any of
  the supported forms (body heading, body prose, issue comments,
  native sub-issues, or the mention-search fallback) — or whose body
  carries the canonical Specification shape (`## Problem Statement` +
  `## Solution` + optional `## User Stories`). The no-other-gate
  contract drops the body-shape as the identification gate: child
  existence alone is sufficient. The canonical sections remain one
  valid spec signal so historical authoring keeps working without the
  user having to add `## Children` bullets. Inline phrases like
  `Children: #10`, `Child Issues: #10`, `Blocked by #10`, and
  `Depends on #10` outside a heading are deprecated — prose mentions
  of the phrase were sensitive noise (see ADR follow-up).
- A child of Specification `<n>` is an issue whose body contains a `## Parent`
  section citing `<n>` (via `#N` shorthand or a full GitHub issue URL).
- Specification expansion is the act of replacing a Specification with its accepted
  children, removing the Specification itself from the batch, and deduping
  across Specifications and explicit inputs.

Alternatives considered:

- **Label-based detection**: Mark Specifications with a `specification` label. Easier to
  parse but requires label discipline and diverges from the project's
  body-as-source-of-truth convention (see ADR-0003 for the parallel
  decision on dependency detection).
- **Single canonical section (e.g. `## Child Issues`)**: A more
  constrained schema would simplify the parser but rejects the
  long-tail of partial / evolving Specification bodies that still list
  children inline or in comments.
- **No mention-search fallback**: A simpler implementation, but real
  Specifications in this repo (#1, #895) and likely elsewhere do not always list
  children in a machine-parseable section, so the fallback is
  load-bearing.

## Decision

We will resolve Specifications to their child issues during batch preparation,
using the same `DependencyResolver`-style seam that already lives in
`internal/batch`. The flow is:

1. **Detection** — `SpecificationResolver.IsSpecification(body)` returns true iff the
   body declares children (heading or prose refs to other issues,
   outside the `## Parent` backlink) OR carries the canonical
   Specification shape (any of `## Problem Statement`, `## Solution`,
   or `## User Stories`). Case-insensitive on the section text. H3 or
   deeper sections do not count as the canonical-shape signal. The
   `## Parent` backlink is excluded from the children-content probe
   because it points upward, not downward.
2. **Child discovery** — In order: (a) `#N` references and full issue URLs in the
   Specification body, (b) `#N` references and full issue URLs in each Specification
   comment in chronological order,
   (c) a single `gh issue list --search` against the path token
   `issues/<n>` (the only form GitHub search reliably matches;
   verified empirically, see test
   `TestSpecificationResolver_FallsBackToSearch`). Within each source,
   references are deduped; across sources the first occurrence wins.
   The Specification itself is excluded from the candidate set.
3. **Parent verification** — For each candidate, the resolver fetches the
   candidate and parses its `## Parent` section. Candidates
   whose parent reference does not resolve to the originating Specification
   are dropped from the accepted set. URLs and `#N` shorthand are
   both accepted. The per-candidate stderr warning that previously
   announced each drop was removed in #1039: with the user-typed-wins
   relaxation in step 3a, the warning became operator noise
   (prose cross-references in Specification bodies are routinely not
   authored children, and the per-Specification `expanded specification #N to M accepted
   children` summary already accounts for the drop). The filtering
   contract is unchanged; only the operator-facing log line is gone.
   This `## Parent` filter applies only to candidates harvested from
   a Specification's body, comments, or search fallback; user-typed input
   numbers in a mixed batch bypass the verification and are
   accepted unconditionally, except when the number is itself a
   Specification, in which case step 6 expands it on its own pass (see
   step 3a).
3a. **User-typed input is authoritative** (#1036) — `Resolve` builds
   a `userInputSet` from the deduped input slice and threads it into
   `resolveSpecificationChildren`. Candidates whose number is in `userInputSet`
   skip `FetchIssue`, the `IsSpecification` re-check, and the `## Parent`
   verification. The relaxation exists so that a mixed batch (e.g. a
   Specification alongside
   prose-mentioned sibling issues, or a user-typed Specification nested in
   another Specification's body) is not aborted by harvest-time filters. The
   authored `## Parent` body convention remains the contract for
   *authored* parent-child relationships; the relaxation applies only
   to numbers the user typed into the input.
4. **Nested-Specification flatten** — For each harvested or user-typed
   candidate, the resolver re-applies `IsSpecification`. A candidate that is
   itself a Specification is expanded recursively: the resolver collects its
   own children via the same body → comments → search-fallback path and
   applies the `## Parent` verification on its own candidates. Each recursive
   expansion emits a `flattened specification #<child> inside #<parent> to <N>
   accepted children` log line on stderr (the top-level expansion uses
   `expanded specification #<n> to <N> accepted children`). Depth is bounded
   by the per-run unique-set seen+addUnique pair: each issue number is
   emitted at most once, so a Specification whose body references itself
   (directly or transitively) recurses once and then short-circuits when
   the already-seen number is re-encountered. The carve-out in step 3a
   continues to apply at the immediate acceptance step on every pass.
5. **No-children rejection** — An input whose accepted-child set is empty
   (no body children, no comment children, no native sub-issues) is
   passed through as a regular issue with a `running issue #<n> as a
   regular issue (no children)` log line. The mention-search fallback
   only fires when one of the cheaper sources surfaced a candidate;
   for inputs whose surface is already filtered upstream (label
   search, range selection) the fallback is skipped so the operator
   search query is preserved and re-discovery is avoided.
6. **Replacement and dedup** — The resolver walks the input issue
   list and replaces each Specification with its accepted children in the
   original order. The Specification itself is not in the output. Duplicates
   across Specifications and explicit inputs are removed (first occurrence
   wins).
7. **Run-flow integration** — `sandman run` invokes `SpecificationResolver`
   immediately after issue selection and before
   `DependencyResolver.Resolve`. The cached GitHub client adds a
   `comments` cache so repeated `ListIssueComments` calls within one
   run are cheap.

The Specification resolver is implemented in `internal/batch/spec.go` and
`internal/batch/spec_parse.go`. The `github.Client` interface gains
`ListIssueComments(number) ([]IssueComment, error)`, and the cached
client mirrors the existing `ListPRComments` pattern.

## Consequences

### Positive

- A single Specification becomes a first-class Sandman input; users no longer
  need to copy child numbers by hand.
- Specification detection is structural and does not require a label
  convention, so existing and historical Specifications are recognized
  automatically.
- The fallback to comment and mention search covers partial / evolving
  Specification bodies, not just perfectly-marked-up ones.
- Reuses the existing `DependencyResolver` style seam, so the
  resolution step composes naturally with the rest of the batch
  pipeline.

### Negative

- `FetchIssue` is called once for every input plus once for every
  accepted candidate, increasing API call volume. The `cachedGitHubClient`
  already caches `FetchIssue`, but the first invocation per issue is
  uncached.
- `ListIssueComments` and `SearchIssues` errors are logged as warnings
  rather than aborting. This avoids blocking resolution on transient
  gh failures but can mask persistent issues. A subsequent failure
  mode (e.g. a child `FetchIssue` error) still aborts the resolution.
- The fallback search query (`issues/<n>`) is empirically the only form
  `gh` search reliably matches. A future change to GitHub search
  semantics could silently break the fallback.

### Neutral

- `IssueComment` becomes a new public type in `internal/github`,
  parallel to `PRComment`.
- `CONTEXT.md` gains entries for `Specification`, `SpecificationResolver`, and the
  `## Parent` body convention.
