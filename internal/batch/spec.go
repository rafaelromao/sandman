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
	sectionREs           []*regexp.Regexp
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

// NewSpecificationResolver returns a resolver that expands any Specification issues in the input
// into their child issues. The warning writer receives one line per expansion
// or per dropped candidate.
func NewSpecificationResolver(client github.Client, warningWriter io.Writer) *SpecificationResolver {
	if warningWriter == nil {
		warningWriter = os.Stderr
	}
	required := []string{"Problem Statement", "Solution"}
	sectionREs := make([]*regexp.Regexp, len(required))
	for i, name := range required {
		sectionREs[i] = regexp.MustCompile(`(?im)^##\s+` + regexp.QuoteMeta(name) + `\s*$`)
	}
	return &SpecificationResolver{client: client, warningWriter: warningWriter, sectionREs: sectionREs, maxConcurrentFetches: 8}
}

// IsSpecification reports whether the body carries the two structural
// H2 sections — Problem Statement and Solution — that distinguish a
// Specification from a regular issue. The previous three-section rule
// (adding User Stories) was dropped because User Stories is
// presentation, not structure, and the broadened child-discovery
// path now handles child discovery from any heading or comment
// signal, so the User Stories gate only blocked valid parents
// that happened to author the work without that section.
func (r *SpecificationResolver) IsSpecification(body string) bool {
	for _, re := range r.sectionREs {
		if !re.MatchString(body) {
			return false
		}
	}
	return true
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
// Two detector branches feed a single recursive expansion path:
//   - section-shape: IsSpecification(body) is true.
//   - broadened: IsSpecification(body) is false, but HasChildren(ctx, n) is true.
//   - neither: pass-through.
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

	broadened := !r.IsSpecification(issue.Body)
	var subIssues []int
	if broadened {
		bodyChildren := parseChildrenFromBodyForExpansion(issue.Body)
		hasChildren, hcErr := r.HasChildren(ctx, num)
		if hcErr != nil {
			fmt.Fprintf(r.warningWriter, "warning: could not list comments for #%d: %v\n", num, hcErr)
		}
		if !hasChildren && len(bodyChildren) == 0 {
			nums, subErr := r.client.ListSubIssues(ctx, num)
			if subErr != nil {
				fmt.Fprintf(r.warningWriter, "warning: could not list sub-issues for specification #%d: %v\n", num, subErr)
				addUnique(num)
				return nil
			}
			if len(nums) == 0 {
				addUnique(num)
				return nil
			}
			subIssues = nums
		}
	} else {
		nums, subErr := r.client.ListSubIssues(ctx, num)
		if subErr != nil {
			fmt.Fprintf(r.warningWriter, "warning: could not list sub-issues for specification #%d: %v\n", num, subErr)
		} else {
			subIssues = nums
		}
	}

	accepted, err := r.collectAcceptedChildren(ctx, num, issue.Body, subIssues, userInputSet, fetches)
	if err != nil {
		return err
	}

	if r.IsSpecification(issue.Body) && len(accepted) == 0 {
		addUnique(num)
		fmt.Fprintf(r.warningWriter, "running specification #%d as a regular issue (no children)\n", num)
		return nil
	}

	if !r.IsSpecification(issue.Body) && len(accepted) == 0 {
		addUnique(num)
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
		// Recursively flatten if the child is itself a Specification.
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
// gathered by expandOne for broadened-detector candidates. It applies the
// per-candidate ## Parent verification. User-typed inputs bypass verification on
// the immediate acceptance step but are still subject to recursive expansion in
// expandOne.
func (r *SpecificationResolver) collectAcceptedChildren(ctx context.Context, parent int, body string, subIssues []int, userInputSet map[int]struct{}, fetches *issueFetchGroup) ([]int, error) {
	candidates := r.collectCandidates(ctx, parent, body, subIssues)
	if len(candidates) == 0 {
		return nil, nil
	}
	childIssues := make([]*github.Issue, len(candidates))
	fetchErrors := make([]error, len(candidates))
	pending := make([]int, 0, len(candidates))
	for idx, child := range candidates {
		if _, ok := userInputSet[child]; ok {
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
		if _, ok := userInputSet[child]; ok {
			accepted = append(accepted, child)
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
	if len(order) == 0 {
		if results, err := r.client.SearchIssues(ctx, specSearchToken(parent)); err == nil {
			for _, issue := range results {
				add([]int{issue.Number})
			}
		} else {
			fmt.Fprintf(r.warningWriter, "warning: mention search for specification #%d failed: %v\n", parent, err)
		}
	}
	add(subIssues)
	return order
}

// parseChildrenFromBodyForExpansion is a thin alias that lets the
// broadened-detector path consult the same heading-only child parser
// without widening the parser's exported contract.
func parseChildrenFromBodyForExpansion(body string) []int {
	return github.ParseChildrenFromBody(body)
}
