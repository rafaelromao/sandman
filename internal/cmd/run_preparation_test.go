package cmd

import (
	"context"
	"io"
	"reflect"
	"slices"
	"testing"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/github"
)

func newPlanningGitHub() *specDiscoveryGitHubClient {
	return newSpecDiscoveryGitHub(map[int]*github.Issue{
		1: {Number: 1, State: "open", Title: "Root", Body: "## Children\n\n- #2\n- #4\n- #6\n", BlockedBy: []int{4, 8}},
		2: {Number: 2, State: "open", Title: "Nested", Body: "## Children\n\n- #4\n- #5\n"},
		3: {Number: 3, State: "open", Title: "Shared parent", Body: "## Children\n\n- #4\n"},
		4: {Number: 4, State: "open", Title: "Shared child", BlockedBy: []int{8}},
		5: {Number: 5, State: "open", Title: "Nested child"},
		6: {Number: 6, State: "closed", Title: "Closed child"},
		7: {Number: 7, State: "open", Title: "Independent"},
		8: {Number: 8, State: "open", Title: "Declared blocker"},
	})
}

// Keep the existing command filter regression cases pointed at the relocated
// production implementation.
func filterClosedIssuesAfterExpansion(ctx context.Context, expanded, selected []int, search func(context.Context, string) ([]github.Issue, error), fetch func(context.Context, int) (*github.Issue, error), warnings io.Writer) ([]int, error) {
	return batch.FilterClosedIssuesAfterExpansion(ctx, expanded, selected, search, fetch, warnings)
}

func TestPreparation_RetainsAcceptedChildrenAndSharesMetadata(t *testing.T) {
	gh := newPlanningGitHub()
	plan, err := batch.NewPreparer(gh, gh, io.Discard).Prepare(context.Background(), batch.Preparation{Issues: []int{1, 3, 7, 8, 1}})
	if err != nil {
		t.Fatal(err)
	}
	wantChildren := map[int][]int{1: {2, 4, 6}, 2: {4, 5}, 3: {4}}
	if !reflect.DeepEqual(plan.ParentChildren, wantChildren) {
		t.Fatalf("accepted children = %v, want %v", plan.ParentChildren, wantChildren)
	}
	if slices.Contains(plan.Deps[1], 6) || slices.Contains(plan.Issues, 6) {
		t.Fatalf("closed child must stay out of executable graph: %+v", plan)
	}
	for n, count := range gh.fetchCount {
		if count != 1 {
			t.Errorf("metadata #%d fetched %d times, want once across preparation", n, count)
		}
	}
}

func TestRun_PreparesOneStableSpecificationGraph(t *testing.T) {
	gh := newPlanningGitHub()
	// Search payloads are selection/filter data, not authoritative metadata.
	for _, n := range []int{1, 2, 3, 4, 5, 7, 8} {
		gh.searchIssuesResult = append(gh.searchIssuesResult, github.Issue{Number: n, State: "open", Title: "Old search title"})
	}
	spy, _ := executeRunWithGitHub(t, gh, "1", "3", "7", "8", "1")
	if want := []int{8, 4, 5, 2, 1, 3, 7}; !slices.Equal(spy.req.Issues, want) {
		t.Fatalf("Issues = %v, want %v", spy.req.Issues, want)
	}
	wantDeps := map[int][]int{1: {2, 4, 8}, 2: {4, 5}, 3: {4}, 4: {8}, 5: nil, 7: nil, 8: nil}
	if !reflect.DeepEqual(spy.req.Dependencies, wantDeps) {
		t.Errorf("Dependencies = %v, want %v", spy.req.Dependencies, wantDeps)
	}
	for _, n := range spy.req.Issues {
		if got, want := spy.req.IssueTitles[n], gh.issues[n].Title; got != want {
			t.Errorf("title #%d = %q, want metadata %q", n, got, want)
		}
	}
	for n, calls := range gh.fetchCount {
		if calls != 1 {
			t.Errorf("metadata #%d fetched %d times, want once", n, calls)
		}
	}
}
