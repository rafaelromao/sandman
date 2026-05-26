package batch

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestDependencyResolverResolve_SortsIssuesTopologically(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42, 7}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "Groundwork"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{100}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{7, 42, 100}) {
		t.Fatalf("expected topological order [7 42 100], got %v", resolved.Issues)
	}

	wantDeps := map[int][]int{
		7:   nil,
		42:  {7},
		100: {7, 42},
	}
	if !reflect.DeepEqual(resolved.Deps, wantDeps) {
		t.Fatalf("expected deps %v, got %v", wantDeps, resolved.Deps)
	}
}

func TestDependencyResolverResolve_PreservesRequestedOrderForIndependentIssues(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			3: {Number: 3, Title: "Third"},
			1: {Number: 1, Title: "First"},
			2: {Number: 2, Title: "Second"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{3, 1, 2}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{3, 1, 2}) {
		t.Fatalf("expected requested order [3 1 2], got %v", resolved.Issues)
	}
}

func TestDependencyResolverResolve_ErrorsOnMissingBlockersWithoutExpansion(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{7}},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	_, err := resolver.Resolve(context.Background(), []int{100, 42}, false)
	if err == nil {
		t.Fatal("expected missing blocker error")
	}
	if err.Error() != "missing blockers: #7" {
		t.Fatalf("expected missing blocker error for #7, got %q", err)
	}
}

func TestDependencyResolverResolve_ExpandsTransitiveBlockers(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{7}},
			7:   {Number: 7, Title: "Groundwork"},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	resolved, err := resolver.Resolve(context.Background(), []int{100}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolved.Issues, []int{7, 42, 100}) {
		t.Fatalf("expected expanded topological order [7 42 100], got %v", resolved.Issues)
	}
}

func TestDependencyResolverResolve_DetectsCycles(t *testing.T) {
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			100: {Number: 100, Title: "Feature", BlockedBy: []int{42}},
			42:  {Number: 42, Title: "Refactor", BlockedBy: []int{100}},
		},
	}

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &bytes.Buffer{}

	_, err := resolver.Resolve(context.Background(), []int{100}, true)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if err.Error() != "dependency cycle detected: #100 -> #42 -> #100" {
		t.Fatalf("expected cycle path in error, got %q", err)
	}
}

func TestDependencyResolverResolve_WarnsWhenExpansionGetsLarge(t *testing.T) {
	issues := make(map[int]*github.Issue, 51)
	for issue := 1; issue <= 51; issue++ {
		issues[issue] = &github.Issue{Number: issue, Title: "Issue"}
	}
	for issue := 2; issue <= 51; issue++ {
		issues[issue].BlockedBy = []int{issue - 1}
	}

	client := &fakeGitHubClient{issues: issues}
	var warnings bytes.Buffer

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &warnings

	resolved, err := resolver.Resolve(context.Background(), []int{51}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resolved.Issues) != 51 {
		t.Fatalf("expected 51 resolved issues, got %d", len(resolved.Issues))
	}
	if !strings.Contains(warnings.String(), "warning: resolved batch expanded to 51 issues") {
		t.Fatalf("expected expansion warning, got %q", warnings.String())
	}
}

func TestDependencyResolverResolve_DoesNotWarnForLargeExplicitBatch(t *testing.T) {
	issues := make(map[int]*github.Issue, 51)
	requested := make([]int, 0, 51)
	for issue := 1; issue <= 51; issue++ {
		issues[issue] = &github.Issue{Number: issue, Title: "Issue"}
		requested = append(requested, issue)
	}

	client := &fakeGitHubClient{issues: issues}
	var warnings bytes.Buffer

	resolver := NewDependencyResolver(client)
	resolver.warningWriter = &warnings

	resolved, err := resolver.Resolve(context.Background(), requested, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resolved.Issues) != 51 {
		t.Fatalf("expected 51 resolved issues, got %d", len(resolved.Issues))
	}
	if warnings.Len() != 0 {
		t.Fatalf("expected no expansion warning, got %q", warnings.String())
	}
}
