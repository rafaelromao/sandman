package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/rafaelromao/sandman/internal/github"
)

const (
	dependencyWarningThreshold     = 50
	dependencyMaxConcurrentFetches = 8
)

// ResolvedBatch contains a dependency-validated batch ready for execution.
type ResolvedBatch struct {
	Issues  []int
	Deps    map[int][]int
	Blocked map[int][]int
}

// DependencyResolver fetches BlockedBy relationships and resolves execution order.
type DependencyResolver struct {
	githubClient         github.Client
	warningWriter        io.Writer
	maxConcurrentFetches int
}

func NewDependencyResolver(githubClient github.Client) *DependencyResolver {
	return &DependencyResolver{
		githubClient:         githubClient,
		warningWriter:        os.Stderr,
		maxConcurrentFetches: dependencyMaxConcurrentFetches,
	}
}

// dependencyIssueFetchGroup de-duplicates FetchIssue calls across the
// resolver's workers: the first caller for a given number fetches,
// subsequent callers wait on the same in-flight call.
type dependencyIssueFetchGroup struct {
	mu       sync.Mutex
	cache    map[int]*github.Issue
	inFlight map[int]*dependencyIssueFetchCall
}

type dependencyIssueFetchCall struct {
	done  chan struct{}
	issue *github.Issue
	err   error
}

func newDependencyIssueFetchGroup() *dependencyIssueFetchGroup {
	return &dependencyIssueFetchGroup{
		cache:    make(map[int]*github.Issue),
		inFlight: make(map[int]*dependencyIssueFetchCall),
	}
}

func (g *dependencyIssueFetchGroup) fetch(ctx context.Context, client github.Client, number int) (*github.Issue, error) {
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
	call := &dependencyIssueFetchCall{done: make(chan struct{})}
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

func (r *DependencyResolver) Resolve(ctx context.Context, issues []int, includeDeps bool) (*ResolvedBatch, error) {
	requested := uniqueIssues(issues)
	if len(requested) == 0 {
		return &ResolvedBatch{Deps: map[int][]int{}, Blocked: map[int][]int{}}, nil
	}
	if r.githubClient == nil {
		return nil, fmt.Errorf("github client is required")
	}

	deps := make(map[int][]int, len(requested))
	blocked := make(map[int][]int)
	known := make(map[int]struct{}, len(requested))
	order := make([]int, 0, len(requested))
	queue := append([]int(nil), requested...)
	for _, issue := range requested {
		known[issue] = struct{}{}
		order = append(order, issue)
	}

	fetches := newDependencyIssueFetchGroup()
	missing := map[int]struct{}{}

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		issueNum := queue[0]
		queue = queue[1:]
		if _, done := deps[issueNum]; done {
			continue
		}

		issue, err := fetches.fetch(ctx, r.githubClient, issueNum)
		if err != nil {
			return nil, fmt.Errorf("fetch issue #%d: %w", issueNum, err)
		}

		blockers := uniqueSortedIssues(issue.BlockedBy)
		blockerEntries := fetchBlockersParallel(ctx, fetches, r.githubClient, blockers, issueNum, r.maxConcurrentFetches)
		activeBlockers := make([]int, 0, len(blockers))

		for _, blocker := range blockers {
			if blocker == issueNum {
				continue
			}
			entry := blockerEntries[blocker]
			if entry.err != nil {
				if includeDeps {
					return nil, fmt.Errorf("fetch issue #%d: %w", blocker, entry.err)
				}
				missing[blocker] = struct{}{}
				continue
			}
			blockerIssue := entry.issue
			if blockerIssue == nil {
				missing[blocker] = struct{}{}
				continue
			}

			if _, ok := known[blocker]; ok {
				activeBlockers = append(activeBlockers, blocker)
				continue
			}
			if github.IsIssueClosed(blockerIssue) {
				continue
			}
			if !includeDeps {
				blocked[issueNum] = append(blocked[issueNum], blocker)
				continue
			}

			activeBlockers = append(activeBlockers, blocker)
			known[blocker] = struct{}{}
			queue = append(queue, blocker)
			order = append(order, blocker)
		}

		if len(activeBlockers) == 0 {
			deps[issueNum] = nil
		} else {
			deps[issueNum] = activeBlockers
		}
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing blockers: %s", formatIssueList(issueNumbersFromSet(missing)))
	}

	if includeDeps && len(known) > dependencyWarningThreshold && len(known) > len(requested) {
		r.warnExpansion(len(known))
	}

	ordered, err := topologicalIssues(deps, order)
	if err != nil {
		return nil, err
	}

	return &ResolvedBatch{Issues: ordered, Deps: deps, Blocked: blocked}, nil
}

// fetchBlockersParallel fans out FetchIssue calls across a bounded
// pool sized by maxConcurrentFetches (capped at the work item count).
// It returns a map keyed by blocker number holding either the fetched
// issue, a per-blocker error, or both nil when the blocker was not
// found at all. Callers are responsible for translating a per-key error
// into a missing-blocker entry.
func fetchBlockersParallel(ctx context.Context, fetches *dependencyIssueFetchGroup, client github.Client, blockers []int, exclude int, maxWorkers int) map[int]fetchedBlocker {
	results := make(map[int]fetchedBlocker, len(blockers))
	if len(blockers) == 0 {
		return results
	}
	jobs := make(chan int, len(blockers))
	for _, b := range blockers {
		if b == exclude {
			continue
		}
		results[b] = fetchedBlocker{}
		jobs <- b
	}
	close(jobs)
	workerCount := maxWorkers
	if workerCount <= 0 {
		workerCount = dependencyMaxConcurrentFetches
	}
	if workerCount > len(results) && len(results) > 0 {
		workerCount = len(results)
	}
	var wg sync.WaitGroup
	for n := 0; n < workerCount; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case blocker, ok := <-jobs:
					if !ok {
						return
					}
					issue, err := fetches.fetch(ctx, client, blocker)
					results[blocker] = fetchedBlocker{issue: issue, err: err}
				}
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		for blocker := range results {
			entry := results[blocker]
			if entry.issue == nil && entry.err == nil {
				entry.err = err
				results[blocker] = entry
			}
		}
	}
	return results
}

type fetchedBlocker struct {
	issue *github.Issue
	err   error
}

func (r *DependencyResolver) warnExpansion(issueCount int) {
	if r.warningWriter == nil {
		return
	}
	_, _ = fmt.Fprintf(r.warningWriter, "warning: resolved batch expanded to %d issues\n", issueCount)
}

func topologicalIssues(deps map[int][]int, order []int) ([]int, error) {
	issues := uniqueIssues(order)

	const (
		unvisited = iota
		visiting
		visited
	)

	state := make(map[int]int, len(deps))
	stack := make([]int, 0, len(deps))
	stackIndex := make(map[int]int, len(deps))
	ordered := make([]int, 0, len(deps))

	var visit func(int) error
	visit = func(issue int) error {
		switch state[issue] {
		case visiting:
			cycle := append([]int(nil), stack[stackIndex[issue]:]...)
			cycle = append(cycle, issue)
			return fmt.Errorf("dependency cycle detected: %s", formatIssuePath(cycle))
		case visited:
			return nil
		}

		state[issue] = visiting
		stackIndex[issue] = len(stack)
		stack = append(stack, issue)

		for _, blocker := range deps[issue] {
			if _, ok := deps[blocker]; !ok {
				return fmt.Errorf("missing blockers: %s", formatIssueList([]int{blocker}))
			}
			if err := visit(blocker); err != nil {
				return err
			}
		}

		stack = stack[:len(stack)-1]
		delete(stackIndex, issue)
		state[issue] = visited
		ordered = append(ordered, issue)
		return nil
	}

	for _, issue := range issues {
		if state[issue] != unvisited {
			continue
		}
		if err := visit(issue); err != nil {
			return nil, err
		}
	}

	return ordered, nil
}

func uniqueIssues(issues []int) []int {
	if len(issues) == 0 {
		return nil
	}

	seen := make(map[int]struct{}, len(issues))
	unique := make([]int, 0, len(issues))
	for _, issue := range issues {
		if _, ok := seen[issue]; ok {
			continue
		}
		seen[issue] = struct{}{}
		unique = append(unique, issue)
	}

	return unique
}

func uniqueSortedIssues(issues []int) []int {
	unique := uniqueIssues(issues)
	sort.Ints(unique)
	return unique
}

func issueNumbersFromSet(issues map[int]struct{}) []int {
	if len(issues) == 0 {
		return nil
	}

	numbers := make([]int, 0, len(issues))
	for issue := range issues {
		numbers = append(numbers, issue)
	}
	sort.Ints(numbers)
	return numbers
}

func formatIssueList(issues []int) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, fmt.Sprintf("#%d", issue))
	}
	return strings.Join(parts, ", ")
}

func formatIssuePath(issues []int) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, fmt.Sprintf("#%d", issue))
	}
	return strings.Join(parts, " -> ")
}
