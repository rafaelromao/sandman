package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/rafaelromao/sandman/internal/github"
)

const dependencyWarningThreshold = 50

// ResolvedBatch contains a dependency-validated batch ready for execution.
type ResolvedBatch struct {
	Issues []int
	Deps   map[int][]int
}

// DependencyResolver fetches BlockedBy relationships and resolves execution order.
type DependencyResolver struct {
	githubClient  github.Client
	warningWriter io.Writer
}

func NewDependencyResolver(githubClient github.Client) *DependencyResolver {
	return &DependencyResolver{
		githubClient:  githubClient,
		warningWriter: os.Stderr,
	}
}

func (r *DependencyResolver) Resolve(ctx context.Context, issues []int, includeDeps bool) (*ResolvedBatch, error) {
	requested := uniqueIssues(issues)
	if len(requested) == 0 {
		return &ResolvedBatch{Deps: map[int][]int{}}, nil
	}
	if r.githubClient == nil {
		return nil, fmt.Errorf("github client is required")
	}

	deps := make(map[int][]int, len(requested))
	issueCache := make(map[int]*github.Issue, len(requested))
	known := make(map[int]struct{}, len(requested))
	ordered := append([]int(nil), requested...)
	queue := append([]int(nil), requested...)
	for _, issue := range requested {
		known[issue] = struct{}{}
	}

	missing := map[int]struct{}{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		issueNum := queue[0]
		queue = queue[1:]
		if _, fetched := deps[issueNum]; fetched {
			continue
		}

		issue, err := fetchDependencyIssue(ctx, r.githubClient, issueCache, issueNum)
		if err != nil {
			return nil, fmt.Errorf("fetch issue #%d: %w", issueNum, err)
		}

		blockers := uniqueIssues(issue.BlockedBy)
		activeBlockers := make([]int, 0, len(blockers))

		for _, blocker := range blockers {
			blockerIssue, err := fetchDependencyIssue(ctx, r.githubClient, issueCache, blocker)
			if err != nil {
				if includeDeps {
					return nil, fmt.Errorf("fetch issue #%d: %w", blocker, err)
				}
				missing[blocker] = struct{}{}
				continue
			}

			if isClosedIssue(blockerIssue) {
				continue
			}

			activeBlockers = append(activeBlockers, blocker)
			if _, ok := known[blocker]; ok {
				continue
			}
			if !includeDeps {
				missing[blocker] = struct{}{}
				continue
			}

			known[blocker] = struct{}{}
			ordered = append(ordered, blocker)
			queue = append(queue, blocker)
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

	orderedIssues, err := topologicalIssues(deps, ordered)
	if err != nil {
		return nil, err
	}

	return &ResolvedBatch{Issues: orderedIssues, Deps: deps}, nil
}

func fetchDependencyIssue(ctx context.Context, client github.Client, cache map[int]*github.Issue, issueNum int) (*github.Issue, error) {
	if issue, ok := cache[issueNum]; ok {
		return issue, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	issue, err := client.FetchIssue(issueNum)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("not found")
	}

	cache[issueNum] = issue
	return issue, nil
}

func isClosedIssue(issue *github.Issue) bool {
	return issue != nil && strings.EqualFold(strings.TrimSpace(issue.State), "closed")
}

func (r *DependencyResolver) warnExpansion(issueCount int) {
	if r.warningWriter == nil {
		return
	}
	_, _ = fmt.Fprintf(r.warningWriter, "warning: resolved batch expanded to %d issues\n", issueCount)
}

func topologicalIssues(deps map[int][]int, order []int) ([]int, error) {
	issues := uniqueIssues(order)
	if len(issues) == 0 {
		issues = make([]int, 0, len(deps))
		for issue := range deps {
			issues = append(issues, issue)
		}
	}
	issues = filterKnownIssues(issues, deps)

	position := make(map[int]int, len(issues))
	for i, issue := range issues {
		position[issue] = i
	}
	indegree := make(map[int]int, len(deps))
	dependents := make(map[int][]int, len(deps))
	for issue, blockers := range deps {
		indegree[issue] = 0
		for _, blocker := range blockers {
			if _, ok := deps[blocker]; !ok {
				return nil, fmt.Errorf("missing blockers: %s", formatIssueList([]int{blocker}))
			}
			indegree[issue]++
			dependents[blocker] = append(dependents[blocker], issue)
		}
	}

	ready := make([]int, 0, len(deps))
	for _, issue := range issues {
		if indegree[issue] == 0 {
			ready = append(ready, issue)
		}
	}

	ordered := make([]int, 0, len(deps))
	for len(ready) > 0 {
		issue := ready[0]
		ready = ready[1:]
		ordered = append(ordered, issue)

		for _, dependent := range dependents[issue] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = insertIssueByOrder(ready, dependent, position)
			}
		}
	}

	if len(ordered) != len(deps) {
		if err := findDependencyCycle(deps, issues); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("dependency cycle detected")
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

func filterKnownIssues(issues []int, deps map[int][]int) []int {
	if len(issues) == 0 {
		return nil
	}

	filtered := make([]int, 0, len(issues))
	for _, issue := range issues {
		if _, ok := deps[issue]; ok {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

func insertIssueByOrder(issues []int, issue int, order map[int]int) []int {
	idx := order[issue]
	pos := sort.Search(len(issues), func(i int) bool {
		return order[issues[i]] > idx
	})
	issues = append(issues, 0)
	copy(issues[pos+1:], issues[pos:])
	issues[pos] = issue
	return issues
}

func findDependencyCycle(deps map[int][]int, order []int) error {
	const (
		unvisited = iota
		visiting
		visited
	)

	state := make(map[int]int, len(deps))
	stack := make([]int, 0, len(deps))
	stackIndex := make(map[int]int, len(deps))

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
				continue
			}
			if err := visit(blocker); err != nil {
				return err
			}
		}

		stack = stack[:len(stack)-1]
		delete(stackIndex, issue)
		state[issue] = visited
		return nil
	}

	for _, issue := range order {
		if state[issue] != unvisited {
			continue
		}
		if err := visit(issue); err != nil {
			return err
		}
	}
	for issue := range deps {
		if state[issue] != unvisited {
			continue
		}
		if err := visit(issue); err != nil {
			return err
		}
	}

	return nil
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
