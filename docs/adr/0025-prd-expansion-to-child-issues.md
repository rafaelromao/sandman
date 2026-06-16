# ADR-0025: PRD expansion to child issues

## Status

accepted

## Context

Sandman accepts issue numbers, label filters, and `gh`-query strings as
batch inputs. Some users want to hand Sandman a single issue that
describes a set of vertical slices (a Product Requirements Document) and
have Sandman implement the slices, not the umbrella PRD itself.

Today, the only way to use a PRD as an input is to expand it by hand:
copy the child issue numbers from the PRD body, comments, or
cross-references, and pass them to `sandman run`. There is no first-class
support for PRDs in the batch preparation flow.

This change introduces PRD detection and child expansion so that
`sandman run <prd>` resolves to a batch of the PRD's child issues
before dependency resolution and execution.

PRDs and their children are identified by body structure, not labels:

- A PRD is an issue whose body contains the H2 sections
  `## Problem Statement`, `## Solution`, and `## User Stories`.
- A child of PRD `<n>` is an issue whose body contains a `## Parent`
  section citing `<n>` (via `#N` shorthand or a full GitHub issue URL).
- PRD expansion is the act of replacing a PRD with its accepted
  children, removing the PRD itself from the batch, and deduping
  across PRDs and explicit inputs.

Alternatives considered:

- **Label-based detection**: Mark PRDs with a `prd` label. Easier to
  parse but requires label discipline and diverges from the project's
  body-as-source-of-truth convention (see ADR-0003 for the parallel
  decision on dependency detection).
- **Single canonical section (e.g. `## Child Issues`)**: A more
  constrained schema would simplify the parser but rejects the
  long-tail of partial / evolving PRD bodies that still list
  children inline or in comments.
- **No mention-search fallback**: A simpler implementation, but real
  PRDs in this repo (#1, #895) and likely elsewhere do not always list
  children in a machine-parseable section, so the fallback is
  load-bearing.

## Decision

We will resolve PRDs to their child issues during batch preparation,
using the same `DependencyResolver`-style seam that already lives in
`internal/batch`. The flow is:

1. **Detection** — `PRDResolver.IsPRD(body)` returns true iff the body
   contains the three required H2 sections, case-insensitive on the
   section text. H3 or deeper sections do not count. The section text
   is anchored to the line start with optional whitespace.
2. **Child discovery** — In order: (a) `#N` references in the PRD body,
   (b) `#N` references in each PRD comment in chronological order,
   (c) a single `gh issue list --search` against the path token
   `issues/<n>` (the only form GitHub search reliably matches;
   verified empirically, see test
   `TestPRDResolver_FallsBackToSearch`). Within each source,
   references are deduped; across sources the first occurrence wins.
   The PRD itself is excluded from the candidate set.
3. **Parent verification** — For each candidate, the resolver fetches the
   candidate and parses its `## Parent` section. Candidates
   whose parent reference does not resolve to the originating PRD
   are dropped with a stderr warning. URLs and `#N` shorthand are
   both accepted.
3a. **User-typed input is authoritative** (#1036) — `Resolve` builds
   a `userInputSet` from the deduped input slice and threads it into
   `resolvePRDChildren`. Candidates whose number is in `userInputSet`
   skip `FetchIssue`, the `IsPRD` re-check, and the `## Parent`
   verification, and skip the parent-mismatch stderr warning. The
   relaxation exists so that a mixed batch (e.g. a PRD alongside
   prose-mentioned sibling issues, or a user-typed PRD nested in
   another PRD's body) is not aborted by harvest-time filters. The
   authored `## Parent` body convention remains the contract for
   *authored* parent-child relationships; the relaxation applies only
   to numbers the user typed into the input.
4. **Nested-PRD rejection** — For each harvested (non-user-typed)
   candidate, the resolver re-applies `IsPRD`. A candidate that is
   itself a PRD fails the resolution with
   `nested PRD detected: #<child>`.
5. **No-children rejection** — A PRD whose accepted-child set is
   empty (no candidates, or every candidate dropped at step 3) fails
   the resolution with `no child issues for PRD #<n>`.
6. **Replacement and dedup** — The resolver walks the input issue
   list and replaces each PRD with its accepted children in the
   original order. The PRD itself is not in the output. Duplicates
   across PRDs and explicit inputs are removed (first occurrence
   wins).
7. **Run-flow integration** — `sandman run` invokes `PRDResolver`
   immediately after issue selection and before
   `DependencyResolver.Resolve`. The cached GitHub client adds a
   `comments` cache so repeated `ListIssueComments` calls within one
   run are cheap.

The PRD resolver is implemented in `internal/batch/prd.go` and
`internal/batch/prd_parse.go`. The `github.Client` interface gains
`ListIssueComments(number) ([]IssueComment, error)`, and the cached
client mirrors the existing `ListPRComments` pattern.

## Consequences

### Positive

- A single PRD becomes a first-class Sandman input; users no longer
  need to copy child numbers by hand.
- PRD detection is structural and does not require a label
  convention, so existing and historical PRDs are recognized
  automatically.
- The fallback to comment and mention search covers partial / evolving
  PRD bodies, not just perfectly-marked-up ones.
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
- `CONTEXT.md` gains entries for `PRD`, `PRDResolver`, and the
  `## Parent` body convention.
- A subsequent change (#898) will run Auto Mode on the
  PRD-expanded candidate pool; this ADR does not introduce Auto Mode.
  As of #898, the auto branch (`--auto`) is the one exception to
  "PRD expansion runs after issue selection" (§7 above): the auto
  branch expands PRDs in the candidate pool *before* the auto
  selection phase so the agent (or numeric-sort fallback) chooses
  among real child issues rather than the umbrella PRD. The
  non-auto branches keep the post-selection expansion described
  in §7.
