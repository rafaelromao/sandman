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
	Issues  []int
	Deps    map[int][]int
	Blocked map[int][]int
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
		return &ResolvedBatch{Deps: map[int][]int{}, Blocked: map[int][]int{}}, nil
	}
	if r.githubClient == nil {
		return nil, fmt.Errorf("github client is required")
	}

	deps := make(map[int][]int, len(requested))
	blocked := make(map[int][]int)
	issueCache := make(map[int]*github.Issue, len(requested))
	known := make(map[int]struct{}, len(requested))
	order := make([]int, 0, len(requested))
	queue := append([]int(nil), requested...)
	for _, issue := range requested {
		known[issue] = struct{}{}
		order = append(order, issue)
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

		blockers := uniqueSortedIssues(issue.BlockedBy)
		activeBlockers := make([]int, 0, len(blockers))

		for _, blocker := range blockers {
			if blocker == issueNum {
				continue
			}
			blockerIssue, err := fetchDependencyIssue(ctx, r.githubClient, issueCache, blocker)
			if err != nil {
				if includeDeps {
					return nil, fmt.Errorf("fetch issue #%d: %w", blocker, err)
				}
				missing[blocker] = struct{}{}
				continue
			}

			if github.IsIssueClosed(blockerIssue) {
				continue
			}

			if _, ok := known[blocker]; ok {
				activeBlockers = append(activeBlockers, blocker)
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

func fetchDependencyIssue(ctx context.Context, client github.Client, cache map[int]*github.Issue, issueNum int) (*github.Issue, error) {
	if issue, ok := cache[issueNum]; ok {
		return issue, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	issue, err := client.FetchIssue(ctx, issueNum)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("not found")
	}

	cache[issueNum] = issue
	return issue, nil
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
