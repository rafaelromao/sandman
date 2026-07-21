package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"

	"github.com/rafaelromao/sandman/internal/github"
)

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
// Specification. A body is a Specification if it declares children
// (heading or prose refs outside the `## Parent` backlink) OR if
// it carries the canonical Specification shape (`## Problem
// Statement` + `## Solution` + `## User Stories`). The body alone
// is no longer sufficient or required — comments, native
// sub-issues, and the search fallback can also surface children —
// but the body-shape check is preserved as one valid spec signal
// so historical canonical-spec authoring keeps working without the
// user having to add `## Children` bullets. The `## Parent`
// backlink is excluded from the children-content probe because it
// points upward, not downward.
//
// The recursive-flatten path uses IsSpecification to decide whether
// to recurse into a harvested child. The user-typed bypass (in
// expandOne) covers the carve-out case: a user-typed nested spec
// whose body has no children-content (e.g. a spec typed alongside
// the parent) is still expanded because the user explicitly asked
// for it.
func (r *SpecificationResolver) IsSpecification(body string) bool {
	bodyNoParent := StripParentSection(body)
	if github.ParseChildrenFromBody(bodyNoParent) != nil {
		return true
	}
	if len(ExtractIssueReferences(bodyNoParent)) > 0 {
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
func (r *SpecificationResolver) Resolve(ctx context.Context, issues []int) ([]int, error) {
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
	fetches := newIssueFetchGroup()
	for _, num := range unique {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := r.expandOne(ctx, num, 0, "-", userInputSet, addUnique, fetches); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// expandOne resolves a single input number into one-or-more child issues, mutating
// the output buffer via addUnique. The recursive case flattens a nested Specification
// in place; the depth parameter selects the top-level "expanded" verb vs the nested
// "flattened" verb. parentLabel is used in the nested-flatten log line (the parent
// specification number that triggered the recursive call); pass "-" at depth 0 to make that
// distinction crisp in operator logs.
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
	userInputSet map[int]struct{},
	addUnique func(int) bool,
	fetches *issueFetchGroup,
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
			fmt.Fprintf(r.warningWriter, "running issue #%d as a regular issue (no children)\n", num)
			return nil
		}
	}

	accepted, err := r.collectAcceptedChildren(ctx, num, issue.Body, subIssues, userInputSet, fetches, depth)
	if err != nil {
		return err
	}

	if len(accepted) == 0 {
		addUnique(num)
		fmt.Fprintf(r.warningWriter, "running issue #%d as a regular issue (no children)\n", num)
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
			if err := r.expandOne(ctx, child, depth+1, fmt.Sprintf("#%d", num), userInputSet, addUnique, fetches); err != nil {
				return err
			}
		}
	}
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
func (r *SpecificationResolver) collectAcceptedChildren(ctx context.Context, parent int, body string, subIssues []int, userInputSet map[int]struct{}, fetches *issueFetchGroup, depth int) ([]int, error) {
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
			// carve-out is enabled (see above). The carve-out is
			// the nested-spec recursive-flatten escape; for leaf
			// top-level inputs the echo would inflate the output
			// with parallel user inputs that share a `## Parent`
			// backlink. The userInputSet bypass of the ## Parent
			// check stays active here for inputs that are NOT
			// ancestors of the current parent.
			if carveOutEnabled {
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
		ref, ok := ExtractParentReference(childIssue.Body)
		if !ok || ref != parent {
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
	add(ExtractIssueReferences(body))
	add(github.ParseChildrenFromBody(body))
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
	return order
}
