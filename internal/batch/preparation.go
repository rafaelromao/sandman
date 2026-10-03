package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/rafaelromao/sandman/internal/github"
)

// Preparation selects the inputs to one validated graph. ReadyIssues are
// runtime-owned continuations: they join after discovery and closed filtering,
// but still participate in dependency validation.
type Preparation struct {
	Issues              []int
	ReadyIssues         []int
	IncludeDependencies bool
}

// Preparer owns Specification discovery, closed-child filtering, and declared
// and parent completion gates. Its metadata snapshot is confined to Prepare;
// the live client is used for fresh filtering fallback, never that snapshot.
type Preparer struct {
	metadata github.Client
	live     github.Client
	warnings io.Writer
}

func NewPreparer(metadata, live github.Client, warnings io.Writer) *Preparer {
	if warnings == nil {
		warnings = os.Stderr
	}
	return &Preparer{metadata: metadata, live: live, warnings: warnings}
}

// Prepare finishes the whole graph before execution can start. Discovery
// provenance stays in ParentChildren; only children admitted to the executable
// batch contribute parent gates in Deps. Neither is persisted to GitHub.
func (p *Preparer) Prepare(ctx context.Context, input Preparation) (*ResolvedBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if (len(input.Issues) > 0 || len(input.ReadyIssues) > 0) && (p.metadata == nil || p.live == nil) {
		return nil, fmt.Errorf("github client is required")
	}
	fetches := newIssueFetchGroup()
	specs := NewSpecificationResolver(p.metadata, p.warnings)
	issues, parents, err := specs.resolve(ctx, input.Issues, fetches)
	if err != nil {
		return nil, fmt.Errorf("resolve specifications: %w", err)
	}
	if len(issues) > 0 {
		issues, err = FilterClosedIssuesAfterExpansion(ctx, issues, input.Issues, p.metadata.SearchIssues, p.live.FetchIssue, p.warnings)
		if err != nil {
			return nil, err
		}
	}
	issues = uniqueIssues(append(issues, input.ReadyIssues...))
	dependencies := NewDependencyResolver(p.metadata)
	dependencies.warningWriter = p.warnings
	resolved, err := dependencies.resolve(ctx, issues, input.IncludeDependencies, parents, fetches)
	if err != nil {
		return nil, fmt.Errorf("resolve dependencies: %w", err)
	}
	resolved.ParentChildren = parents
	resolved.IssueTitles = make(map[int]string, len(resolved.Issues))
	for _, number := range resolved.Issues {
		resolved.IssueTitles[number] = fetches.cache[number].Title
	}
	return resolved, nil
}

// ErrAllExplicitClosed reports a fully closed selection, distinct from an
// expansion that produces no runnable children (which is a successful no-op).
var ErrAllExplicitClosed = errors.New("all explicit issues are closed")

// FilterClosedIssues prefers one open-issue search. A failed or truncated
// search falls back to the caller's live fetch, with per-issue warnings.
func FilterClosedIssues(ctx context.Context, numbers []int, search func(context.Context, string) ([]github.Issue, error), fetch func(context.Context, int) (*github.Issue, error), warnings io.Writer) ([]int, error) {
	results, searchErr := search(ctx, "is:open")
	filtered := make([]int, 0, len(numbers))
	closedCount := 0
	if searchErr == nil && len(results) < 1000 {
		open := make(map[int]struct{}, len(results))
		for _, issue := range results {
			open[issue.Number] = struct{}{}
		}
		for _, n := range numbers {
			if _, ok := open[n]; !ok {
				fmt.Fprintf(warnings, "Issue #%d is closed, skipping\n", n)
				closedCount++
				continue
			}
			filtered = append(filtered, n)
		}
	} else {
		for _, n := range numbers {
			issue, err := fetch(ctx, n)
			if err != nil {
				fmt.Fprintf(warnings, "Warning: could not fetch issue #%d: %v\n", n, err)
				continue
			}
			if github.IsIssueClosed(issue) {
				fmt.Fprintf(warnings, "Issue #%d is closed, skipping\n", n)
				closedCount++
				continue
			}
			filtered = append(filtered, n)
		}
	}
	if len(filtered) == 0 && closedCount > 0 {
		return nil, ErrAllExplicitClosed
	}
	return filtered, nil
}

// FilterClosedIssuesAfterExpansion filters only when discovery introduced new
// numbers. An entirely closed expansion is a no-op rather than a selection error.
func FilterClosedIssuesAfterExpansion(ctx context.Context, expanded, selected []int, search func(context.Context, string) ([]github.Issue, error), fetch func(context.Context, int) (*github.Issue, error), warnings io.Writer) ([]int, error) {
	selectedSet := make(map[int]struct{}, len(selected))
	for _, n := range selected {
		selectedSet[n] = struct{}{}
	}
	for _, n := range expanded {
		if _, ok := selectedSet[n]; ok {
			continue
		}
		filtered, err := FilterClosedIssues(ctx, expanded, search, fetch, warnings)
		if errors.Is(err, ErrAllExplicitClosed) {
			return nil, nil
		}
		return filtered, err
	}
	return expanded, nil
}
