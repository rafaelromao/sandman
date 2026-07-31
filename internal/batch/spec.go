package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/rafaelromao/sandman/internal/github"
)

// discoveredChildrenMarker is the hidden HTML marker the
// Specification resolver writes on a spec to persist auto-discovered
// children. The marker lets future runs short-circuit the expensive
// open-issue scan (the existing comment harvest picks the candidates
// up via ExtractIssueReferences) and lets operators identify the
// auto-generated comment. See ADR-0044.
const discoveredChildrenMarker = "<!-- sandman-discovered-children -->"

// SpecificationResolver resolves Specification issues to their child issues during batch preparation.
type SpecificationResolver struct {
	client               github.Client
	warningWriter        io.Writer
	maxConcurrentFetches int
}

type issueFetchCall struct {
	done  chan struct{}
	issue *github.Issue
	err   error
}

type issueFetchGroup struct {
	mu       sync.Mutex
	cache    map[int]*github.Issue
	inFlight map[int]*issueFetchCall
}

func newIssueFetchGroup() *issueFetchGroup {
	return &issueFetchGroup{
		cache:    make(map[int]*github.Issue),
		inFlight: make(map[int]*issueFetchCall),
	}
}

func (g *issueFetchGroup) fetch(ctx context.Context, client github.Client, number int) (*github.Issue, error) {
	g.mu.Lock()
	if issue, ok := g.cache[number]; ok {
		g.mu.Unlock()
		return issue, nil
	}
	if call, ok := g.inFlight[number]; ok {
		g.mu.Unlock()
		select {
		case <-call.done:
			return call.issue, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &issueFetchCall{done: make(chan struct{})}
	g.inFlight[number] = call
	g.mu.Unlock()

	issue, err := client.FetchIssue(ctx, number)
	g.mu.Lock()
	call.issue = issue
	call.err = err
	if err == nil && issue != nil {
		g.cache[number] = issue
	}
	delete(g.inFlight, number)
	close(call.done)
	g.mu.Unlock()
	return issue, err
}

// NewSpecificationResolver returns a resolver that expands any issue
// that declares child issues — in any of the supported forms (body
// heading, body prose, issue comments, native sub-issues, search
// fallback) — into those children. The body alone is no longer
// sufficient or required: a parent with no body content can still
// expand if comments, native sub-issues, or a mention search surface
// its children. The warning writer receives one line per expansion
// or per dropped candidate.
func NewSpecificationResolver(client github.Client, warningWriter io.Writer) *SpecificationResolver {
	if warningWriter == nil {
		warningWriter = os.Stderr
	}
	return &SpecificationResolver{client: client, warningWriter: warningWriter, maxConcurrentFetches: 8}
}

// specSectionPattern matches an H2 heading whose name equals the
// given Specification section name. Case-insensitive.
func specSectionPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?im)^##\s+` + regexp.QuoteMeta(name) + `\s*$`)
}

// IsSpecification reports whether the body looks like a
// Specification. A body is a Specification if it carries a
// children-list declaration (an H2 heading whose title contains the
// word `children` or `child`, case-insensitive substring — `##
// Children`, `## Child Issues`, `## Leaf children`, `## Children in
// this area`, `## Child tasks`, etc.) OR the canonical
// Specification shape (`## Problem Statement` + `## Solution`; `##
// User Stories` is optional and does not contribute to the
// canonical signal). The widened children-heading matcher is
// documented in ADR-0045 and is the symmetric counterpart of the
// broadened parent-section matcher from ADR-0042; it is what lets
// a threeterm-style body that lists leaf children under `## Leaf
// children` (issue #305) be detected as a specification and
// expanded. Prose `#N` and `/issues/N` references outside the `##
// Parent` backlink do NOT by themselves make an issue a
// Specification — they are incidental mentions and would otherwise
// cause every child with a casual reference (e.g. "Tracking #500
// for context") to be flattened as a sub-spec, which is the bug
// that issue #2333 fixes. The `## Parent` backlink is excluded
// from the children-list probe because it points upward, not
// downward.
//
// The recursive-flatten path uses IsSpecification to decide whether
// to recurse into a harvested child. The user-typed bypass (in
// expandOne) covers the carve-out case: a user-typed nested spec
// whose body has no children-list or canonical signal (e.g. a spec
// typed alongside the parent) is still expanded because the user
// explicitly asked for it.
func (r *SpecificationResolver) IsSpecification(body string) bool {
	bodyNoParent := StripParentSection(body)
	if github.ParseChildrenFromBody(bodyNoParent) != nil {
		return true
	}
	// Canonical-shape signal: the body carries both `## Problem
	// Statement` and `## Solution` (case-insensitive on the
	// section text). Either alone is not enough — a lone
	// `## Solution` heading in an ordinary issue must not be
	// mistaken for a Specification, because doing so would let a
	// prose child reference in that body replace the issue with
	// the cited child at expansion time. `## User Stories` is
	// presentation, not structure, so it does not contribute to
	// the canonical-shape signal.
	hasProblem := specSectionPattern("Problem Statement").MatchString(bodyNoParent)
	hasSolution := specSectionPattern("Solution").MatchString(bodyNoParent)
	return hasProblem && hasSolution
}

// HasChildren reports whether the issue identified by `number` has at least one child
// reference discovered in its comment bodies. It is the lazy probe that complements
// IsSpecification for the broadened detector: callers that find IsSpecification(body) false
// can use HasChildren to decide whether to expand the input anyway.
//
// HasChildren only scans comment bodies; body-shape references and native sub-issues
// are discovered later in the expanded path by collectCandidates and by the broadened
// branch in expandOne respectively. The cached GitHub client memoises ListIssueComments
// per (run, number), so a re-entry on the same number within one run pays zero additional
// REST requests.
func (r *SpecificationResolver) HasChildren(ctx context.Context, number int) (bool, error) {
	comments, err := r.client.ListIssueComments(ctx, number)
	if err != nil {
		return false, fmt.Errorf("list comments for #%d: %w", number, err)
	}
	for _, c := range comments {
		for _, n := range ExtractIssueReferences(c.Body) {
			if n != 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// Resolve is the entry point for Specification expansion. It walks the input list
// and replaces each Specification with its accepted child issues, removing the Specification
// itself and deduplicating across Specifications and explicit inputs. Non-Specification
// issues pass through unchanged.
//
// The second return value is the in-memory parent-children map: every
// retained Specification is keyed by its issue number, and the value
// is the list of accepted (open) child issues. The cmd layer threads
// this map into DependencyResolver.Resolve so each retained parent
// is held back from starting until its children succeed in the
// batch. The edges never leave the batch — they are not persisted
// to GitHub. Callers that only care about the expanded list can
// discard the map. See ADR-0047.
//
// The no-other-gate contract: every input is probed for children
// unconditionally. Body heading, body prose, issue comments, native
// sub-issues, and the mention-search fallback all feed a single
// collectCandidates pipeline. The body-shape gate is gone; an issue
// is a Specification iff it has children (in any form) or the user
// typed it (the carve-out).
//
// Nested Specifications are flattened recursively (per the corrected invariant
// documented on ADR-0025 §4 as the destination-aligned reading): any candidate
// that is itself a Specification is expanded in turn, and each recursive
// expansion emits a per-flatten log line.
//
// Cycle behaviour: depth is not capped by an explicit N. Cycles are bound
// instead by the `seen`+`addUnique` pair in the per-run output buffer —
// each number is emitted at most once, so a Specification that recurses
// into itself (e.g. two specs whose bodies reference each other) emits
// both numbers and then short-circuits when `addUnique` returns false on
// re-entry. Depth at worst is `len(uniqueIssues(issues)) + len(accepted
// descendants)` and terminates once every reachable number has been
// emitted exactly once.
//
// Errors:
//   - any FetchIssue error encountered while loading a candidate child
func (r *SpecificationResolver) Resolve(ctx context.Context, issues []int) ([]int, map[int][]int, error) {
	unique := uniqueIssues(issues)
	userInputSet := make(map[int]struct{}, len(unique))
	for _, num := range unique {
		userInputSet[num] = struct{}{}
	}
	out := make([]int, 0, len(unique))
	seen := make(map[int]struct{}, len(unique))
	addUnique := func(n int) bool {
		if _, ok := seen[n]; ok {
			return false
		}
		seen[n] = struct{}{}
		out = append(out, n)
		return true
	}
	parentChildren := make(map[int][]int)
	recordChildren := func(parent int, children []int) {
		if len(children) == 0 {
			return
		}
		// Dedupe and sort so the DependencyResolver can compare
		// the synthetic edge set without an extra pass.
		seen := make(map[int]struct{}, len(children))
		out := make([]int, 0, len(children))
		for _, c := range children {
			if _, ok := seen[c]; ok {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
		sort.Ints(out)
		parentChildren[parent] = out
	}
	fetches := newIssueFetchGroup()
	for _, num := range unique {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := r.expandOne(ctx, num, 0, "-", 0, userInputSet, addUnique, fetches, recordChildren); err != nil {
			return nil, nil, err
		}
	}
	return out, parentChildren, nil
}

// expandOne resolves a single input number into one-or-more child issues, mutating
// the output buffer via addUnique. The recursive case flattens a nested Specification
// in place; the depth parameter selects the top-level "expanded" verb vs the nested
// "flattened" verb. parentLabel is used in the nested-flatten log line (the parent
// specification number that triggered the recursive call); pass "-" at depth 0 to make that
// distinction crisp in operator logs. recursionParent is the issue that triggered this
// recursion (0 at depth 0); the carve-out in collectAcceptedChildren uses it to skip the
// recursion-tree parent so the recursive flatten does not echo it back into the output.
//
// The userInputSet is the original typed input set; candidates drawn from it bypass
// the IsSpecification re-check and the ## Parent verification (the user owns the choice).
// Dedupe runs through the addUnique closure: each number is emitted at most once, so
// recursions that revisit a parent (e.g. two specs whose bodies reference each other)
// short-circuit when addUnique returns false.
func (r *SpecificationResolver) expandOne(
	ctx context.Context,
	num int,
	depth int,
	parentLabel string,
	recursionParent int,
	userInputSet map[int]struct{},
	addUnique func(int) bool,
	fetches *issueFetchGroup,
	recordChildren func(parent int, children []int),
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	issue, err := fetches.fetch(ctx, r.client, num)
	if err != nil {
		return fmt.Errorf("fetch issue #%d: %w", num, err)
	}
	if issue == nil {
		return fmt.Errorf("fetch issue #%d: not found", num)
	}

	// Probe every supported child-discovery source unconditionally
	// for Specification bodies: body heading, body prose, issue
	// comments, native sub-issues, and (via collectCandidates) the
	// mention-search fallback. The IsSpecification body-shape gate
	// is gone; presence of any accepted child is sufficient to
	// expand the parent.
	//
	// For inputs that carry no child signal in any of the cheaper
	// sources (body, comments, native sub-issues), skip the
	// mention-search fallback. This preserves the historical
	// pass-through for label- and range-resolved inputs whose
	// surface has already been filtered upstream — the fallback
	// would otherwise overwrite the operator-visible search query
	// and re-discover the same surface for no benefit.
	nums, subErr := r.client.ListSubIssues(ctx, num)
	if subErr != nil {
		fmt.Fprintf(r.warningWriter, "warning: could not list sub-issues for specification #%d: %v\n", num, subErr)
	}
	subIssues := nums

	if !r.IsSpecification(issue.Body) {
		hasChildren, hcErr := r.HasChildren(ctx, num)
		if hcErr != nil {
			fmt.Fprintf(r.warningWriter, "warning: could not list comments for #%d: %v\n", num, hcErr)
		}
		bodyChildren := github.ParseChildrenFromBody(issue.Body)
		if !hasChildren && len(bodyChildren) == 0 && len(subIssues) == 0 {
			addUnique(num)
			return nil
		}
	}

	accepted, err := r.collectAcceptedChildren(ctx, num, issue.Body, subIssues, userInputSet, fetches, depth, recursionParent)
	if err != nil {
		return err
	}

	if len(accepted) == 0 {
		addUnique(num)
		// Re-check the spec gate so the broadened-detector path stays
		// silent. The strict-spec log line is reserved for bodies that
		// actually look like a Specification (canonical shape or
		// children-content signal); a non-spec body whose candidate
		// harvest is empty is the broadened-detector carve-out and
		// must not log the misleading "as a regular issue" line.
		// Mirrors ADR-0034 §4.
		if r.IsSpecification(issue.Body) {
			fmt.Fprintf(r.warningWriter, "running issue #%d as a regular issue (no children)\n", num)
		}
		return nil
	}

	if depth == 0 {
		fmt.Fprintf(r.warningWriter, "expanded specification #%d to %d accepted children\n", num, len(accepted))
	} else {
		fmt.Fprintf(r.warningWriter, "flattened specification #%d inside %s to %d accepted children\n", num, parentLabel, len(accepted))
	}

	for _, child := range accepted {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !addUnique(child) {
			continue
		}
		// Recursively flatten if the child is itself a Specification
		// (body declares children or carries the canonical
		// Specification shape). The user-typed bypass is not
		// needed here: a user-typed leaf input that happens to
		// share a `## Parent` backlink with another user input
		// must not recurse, otherwise the carve-out in
		// collectAcceptedChildren would echo the parent input
		// back into the output.
		childIssue, err := fetches.fetch(ctx, r.client, child)
		if err != nil {
			return fmt.Errorf("fetch child #%d: %w", child, err)
		}
		if childIssue == nil {
			return fmt.Errorf("fetch child #%d: not found", child)
		}
		if r.IsSpecification(childIssue.Body) {
			if err := r.expandOne(ctx, child, depth+1, fmt.Sprintf("#%d", num), num, userInputSet, addUnique, fetches, recordChildren); err != nil {
				return err
			}
		}
	}
	// Retain the Specification itself as a regular row, placed
	// immediately after its accepted children. The retained parent
	// participates in the batch as an ordinary AgentRun; the
	// DependencyResolver synthesises in-memory blocker edges from
	// each accepted child to this parent so it cannot start until
	// its children finish. See ADR-0047.
	recordChildren(num, accepted)
	addUnique(num)
	return nil
}

// collectAcceptedChildren runs the existing collectCandidates flow (body refs →
// comments → search fallback) merged with the pre-collected subIssue numbers
// gathered by expandOne. It applies the per-candidate ## Parent
// verification. User-typed inputs bypass verification on the immediate
// acceptance step but are still subject to recursive expansion in
// expandOne.
//
// The userInputSet carve-out (accepting ancestors that are user-typed
// inputs without ## Parent verification) only fires when the issue
// being expanded is itself a Specification. The carve-out's purpose
// is the recursive-flatten path: a nested spec typed alongside the
// parent must be expanded even when its ## Parent backlink points at
// the outermost input. A leaf input that happens to share a
// `## Parent` backlink with another user input does not recurse, so
// its carve-out is disabled and the echo is filtered out.
func (r *SpecificationResolver) collectAcceptedChildren(ctx context.Context, parent int, body string, subIssues []int, userInputSet map[int]struct{}, fetches *issueFetchGroup, depth int, recursionParent int) ([]int, error) {
	// ancestorSet is the union of the original typed inputs and the
	// current parent. Candidates drawn from a child body that
	// match an ancestor are parent-backlink noise, not new children,
	// and must be filtered out so the recursive flatten cannot echo
	// the outermost input back into the output.
	ancestorSet := make(map[int]struct{}, len(userInputSet)+1)
	ancestorSet[parent] = struct{}{}
	for n := range userInputSet {
		ancestorSet[n] = struct{}{}
	}
	// The userInputSet carve-out (accepting ancestors that are
	// user-typed inputs without ## Parent verification) only fires
	// when this is the recursive-flatten path — i.e., the parent is
	// a Specification body. For top-level leaf inputs that happen to
	// share a `## Parent` backlink with another user input, the echo
	// would inflate the output with parallel user inputs and is
	// filtered out.
	carveOutEnabled := r.IsSpecification(body)
	candidates := r.collectCandidates(ctx, parent, body, subIssues)
	if len(candidates) == 0 {
		return nil, nil
	}
	childIssues := make([]*github.Issue, len(candidates))
	fetchErrors := make([]error, len(candidates))
	pending := make([]int, 0, len(candidates))
	for idx, child := range candidates {
		if _, ok := ancestorSet[child]; ok {
			// Either the user-typed input itself or the current
			// parent — not a child of the current parent.
			continue
		}
		pending = append(pending, idx)
	}
	workers := r.maxConcurrentFetches
	if workers <= 0 {
		workers = 1
	}
	if workers > len(pending) {
		workers = len(pending)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case idx, ok := <-jobs:
					if !ok {
						return
					}
					child := candidates[idx]
					childIssues[idx], fetchErrors[idx] = fetches.fetch(ctx, r.client, child)
				}
			}
		}()
	}
sendLoop:
	for _, idx := range pending {
		select {
		case <-ctx.Done():
			break sendLoop
		case jobs <- idx:
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	accepted := make([]int, 0, len(candidates))
	for idx, child := range candidates {
		if _, ok := ancestorSet[child]; ok {
			// Ancestor echo (parent or outer user input): accept
			// it for the recursion carve-out only when the
			// carve-out is enabled (see above) AND the candidate
			// is not the recursion-tree parent. The recursion-tree
			// parent is already in the output via the depth-0
			// carve-out of the call that triggered this
			// expansion; pulling it in again at the recursive
			// level would echo it as a child of its own
			// descendant (issue #2333). The userInputSet bypass
			// of the ## Parent check stays active for ancestors
			// that are NOT the recursion-tree parent.
			if carveOutEnabled && child != recursionParent {
				if _, isUserInput := userInputSet[child]; isUserInput {
					accepted = append(accepted, child)
				}
			}
			continue
		}
		if fetchErrors[idx] != nil {
			return nil, fmt.Errorf("fetch child #%d: %w", child, fetchErrors[idx])
		}
		childIssue := childIssues[idx]
		if childIssue == nil {
			return nil, fmt.Errorf("fetch child #%d: not found", child)
		}
		// Verifier widened in ADR-0042: any parent-section H2
		// accepts the candidate when the originating spec is in
		// the unioned refs.
		if !HasParentSectionBacklinkTo(childIssue.Body, parent) {
			continue
		}
		accepted = append(accepted, child)
	}
	if len(accepted) == 0 {
		return nil, nil
	}
	return accepted, nil
}

func (r *SpecificationResolver) collectCandidates(ctx context.Context, parent int, body string, subIssues []int) []int {
	order := make([]int, 0)
	seen := make(map[int]struct{})
	add := func(nums []int) {
		for _, n := range nums {
			if n == parent {
				continue
			}
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = struct{}{}
			order = append(order, n)
		}
	}
	// Per-section body harvest; skips blocker-style headings
	// (vocabulary owned by github.IsBlockedByHeading, see ADR-0042).
	add(bodyReferencesOutsideBlockerSections(body))
	if comments, err := r.client.ListIssueComments(ctx, parent); err == nil {
		for _, c := range comments {
			add(ExtractIssueReferences(c.Body))
		}
	} else {
		fmt.Fprintf(r.warningWriter, "warning: could not list comments for specification #%d: %v\n", parent, err)
	}
	add(subIssues)
	// The mention-search fallback runs only when the cheaper
	// sources (body refs, comment refs, native sub-issues) have
	// not surfaced any candidate. Native sub-issues count as a
	// cheap source: when GitHub already gave us the children,
	// there is no need to re-discover them through search.
	if len(order) == 0 {
		if results, err := r.client.SearchIssues(ctx, specSearchToken(parent)); err == nil {
			for _, issue := range results {
				add([]int{issue.Number})
			}
		} else {
			fmt.Fprintf(r.warningWriter, "warning: mention search for specification #%d failed: %v\n", parent, err)
		}
	}
	// Open-issue scan: last-resort harvest source. Fires only when
	// every cheaper source (body refs, comment refs, native
	// sub-issues, mention-search fallback) returned zero
	// candidates. The scan walks every open issue in the repo and
	// keeps the ones whose body carries a `## Parent`-style H2
	// section (broadened per ADR-0042) that cites the spec —
	// those candidates pass the verifier by construction. Discovered
	// candidates are persisted as a marker comment on the spec so
	// future runs see them via the existing comment harvest and
	// operators can review the auto-discovery. The two new
	// operations are exposed as optional interfaces; clients that
	// do not implement them silently skip the new step, which is
	// what keeps existing test fakes unchanged. See ADR-0044.
	if len(order) == 0 {
		discovered := r.discoverChildrenViaOpenIssueScan(ctx, parent)
		if len(discovered) > 0 {
			add(discovered)
			r.postDiscoveredChildrenComment(ctx, parent, discovered)
		}
	}
	return order
}

// discoverChildrenViaOpenIssueScan returns the issue numbers of
// every open issue in the repo whose body has at least one parent
// H2 section (heading text contains the word "parent",
// case-insensitive) that cites `parent`. Returns nil when the
// client does not implement OpenIssueLister (existing test fakes)
// or when the scan fails; a scan failure logs a warning via
// r.warningWriter so the operator can diagnose. The spec issue
// itself is excluded. The filter reuses HasParentSectionBacklinkTo
// from spec_parse.go so every candidate the scan returns passes
// the resolver's verifier.
func (r *SpecificationResolver) discoverChildrenViaOpenIssueScan(ctx context.Context, parent int) []int {
	lister, ok := r.client.(github.OpenIssueLister)
	if !ok {
		return nil
	}
	issues, err := lister.ListOpenIssues(ctx)
	if err != nil {
		fmt.Fprintf(r.warningWriter, "warning: open-issue scan for specification #%d failed: %v\n", parent, err)
		return nil
	}
	var discovered []int
	for _, issue := range issues {
		if issue.Number == parent {
			continue
		}
		if HasParentSectionBacklinkTo(issue.Body, parent) {
			discovered = append(discovered, issue.Number)
		}
	}
	return discovered
}

// postDiscoveredChildrenComment persists the discovered children
// as a marker comment on the spec so future runs see them via the
// existing comment harvest and the operator can review or curate
// the auto-discovery. Idempotent: if a marker comment already
// exists on the spec, posting is skipped (operators force a
// re-scan by deleting the marker). A post failure logs a warning
// and does not abort the resolver — the candidates harvested in
// memory are still accepted this run. No-op when the client does
// not implement IssueCommentPoster.
func (r *SpecificationResolver) postDiscoveredChildrenComment(ctx context.Context, parent int, discovered []int) {
	poster, ok := r.client.(github.IssueCommentPoster)
	if !ok {
		return
	}
	if r.markerCommentExists(ctx, parent) {
		return
	}
	body := buildDiscoveredChildrenComment(discovered)
	if err := poster.PostIssueComment(ctx, parent, body); err != nil {
		fmt.Fprintf(r.warningWriter, "warning: could not post discovered-children comment for specification #%d: %v\n", parent, err)
	}
}

// markerCommentExists reports whether the spec already carries a
// comment whose body contains the discovered-children marker. A
// comment-list failure is treated as "no marker" so the post
// proceeds; the marker absence is the conservative default.
func (r *SpecificationResolver) markerCommentExists(ctx context.Context, parent int) bool {
	comments, err := r.client.ListIssueComments(ctx, parent)
	if err != nil {
		return false
	}
	for _, c := range comments {
		if strings.Contains(c.Body, discoveredChildrenMarker) {
			return true
		}
	}
	return false
}

// buildDiscoveredChildrenComment renders the discovered-children
// marker comment body. The hidden HTML marker identifies the
// comment as auto-generated; the `## Discovered children` H2
// section plus `- #N` bullets is the format the existing comment
// harvest (ExtractIssueReferences) recognises on subsequent runs,
// so the expensive open-issue scan is short-circuited for any
// spec whose marker comment is intact. Bullet items are sorted by
// issue number ascending for stable rendering.
func buildDiscoveredChildrenComment(children []int) string {
	sorted := append([]int(nil), children...)
	sort.Ints(sorted)
	var b strings.Builder
	b.WriteString(discoveredChildrenMarker)
	b.WriteString("\n\n## Discovered children\n\n")
	for _, n := range sorted {
		fmt.Fprintf(&b, "- #%d\n", n)
	}
	return b.String()
}
