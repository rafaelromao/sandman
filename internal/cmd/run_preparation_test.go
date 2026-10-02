package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

type scheduledPlanningGitHub struct {
	*specDiscoveryGitHubClient
	entered  chan int
	finished chan int
	release  map[int]chan struct{}
}

func (c *scheduledPlanningGitHub) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	if release := c.release[number]; release != nil {
		c.entered <- number
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	issue, err := c.specDiscoveryGitHubClient.FetchIssue(ctx, number)
	if c.release[number] != nil {
		c.finished <- number
	}
	return issue, err
}

func TestRun_PreparationTransitiveOrderIgnoresFetchCompletionOrder(t *testing.T) {
	for _, first := range []int{20, 30} {
		t.Run(fmt.Sprintf("first=%d", first), func(t *testing.T) {
			gh := &scheduledPlanningGitHub{
				specDiscoveryGitHubClient: newSpecDiscoveryGitHub(map[int]*github.Issue{
					1:  {Number: 1, State: "open", Title: "Parent", Body: "## Children\n\n- #2\n", BlockedBy: []int{30, 20}},
					2:  {Number: 2, State: "open", Title: "Child", BlockedBy: []int{40}},
					20: {Number: 20, State: "open", Title: "A", BlockedBy: []int{40}},
					30: {Number: 30, State: "open", Title: "B", BlockedBy: []int{40}},
					40: {Number: 40, State: "open", Title: "Shared transitive blocker"},
				}), entered: make(chan int, 2), finished: make(chan int, 2), release: map[int]chan struct{}{20: make(chan struct{}), 30: make(chan struct{})},
			}
			spy := &spyBatchRunner{result: &batch.Result{}}
			deps := newRunDeps(t, spy)
			deps.GitHubClient = gh
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := NewRunCmd(deps)
			cmd.SetContext(ctx)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"1", "--include-dependencies"})
			done := make(chan error, 1)
			exited := make(chan struct{})
			go func() { defer close(exited); done <- cmd.Execute() }()
			t.Cleanup(func() { cancel(); <-exited })
			for range 2 {
				select {
				case <-gh.entered:
				case <-ctx.Done():
					t.Fatal("blocker fetch workers did not overlap")
				}
			}
			close(gh.release[first])
			select {
			case n := <-gh.finished:
				if n != first {
					t.Fatalf("first completed fetch = %d, want %d", n, first)
				}
			case <-ctx.Done():
				t.Fatal("first fetch did not finish")
			}
			close(gh.release[50-first])
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if want := []int{40, 2, 20, 30, 1}; !slices.Equal(spy.req.Issues, want) {
				t.Fatalf("order = %v, want %v", spy.req.Issues, want)
			}
			if !slices.Equal(spy.req.Dependencies[1], []int{2, 20, 30}) || gh.fetchCount[40] != 1 {
				t.Fatalf("gates=%v fetch counts=%v", spy.req.Dependencies, gh.fetchCount)
			}
		})
	}
}

func TestRun_PreparationRejectsMixedCycleBeforeExecution(t *testing.T) {
	gh := newSpecDiscoveryGitHub(map[int]*github.Issue{
		1: {Number: 1, State: "open", Title: "Parent", Body: "## Children\n\n- #2\n"},
		2: {Number: 2, State: "open", Title: "Child", BlockedBy: []int{1}},
	})
	assertPreparationError(t, gh, "dependency cycle detected: #2 -> #1 -> #2", "1")
}

func assertPreparationError(t *testing.T, gh github.Client, want string, args ...string) {
	t.Helper()
	spy := &spyBatchRunner{}
	deps := newRunDeps(t, spy)
	deps.GitHubClient = gh
	cmd := NewRunCmd(deps)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if spy.called {
		t.Fatal("invalid preparation invoked the batch runner")
	}
}

type missingPlanningGitHub struct {
	*specDiscoveryGitHubClient
	err error
}

func (c *missingPlanningGitHub) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	if number == 999 {
		return nil, c.err
	}
	return c.specDiscoveryGitHubClient.FetchIssue(ctx, number)
}

func TestRun_PreparationRequiresEveryDeclaredBlocker(t *testing.T) {
	for _, include := range []bool{false, true} {
		for _, missingErr := range []error{nil, errors.New("not found")} {
			t.Run(fmt.Sprintf("include=%t/error=%v", include, missingErr), func(t *testing.T) {
				gh := &missingPlanningGitHub{specDiscoveryGitHubClient: newSpecDiscoveryGitHub(map[int]*github.Issue{
					1: {Number: 1, State: "open", Title: "Dependent", BlockedBy: []int{999}},
				}), err: missingErr}
				args := []string{"1"}
				want := "missing blockers: #999"
				if include {
					args = append(args, "--include-dependencies")
					if missingErr != nil {
						want = "fetch issue #999: not found"
					}
				}
				assertPreparationError(t, gh, want, args...)
			})
		}
	}
}

func TestRun_PreparationRetainsParentWithAllChildrenClosed(t *testing.T) {
	gh := newSpecDiscoveryGitHub(map[int]*github.Issue{
		1: {Number: 1, State: "open", Title: "Parent", Body: "## Children\n\n- #2\n"},
		2: {Number: 2, State: "closed", Title: "Closed child"},
	})
	spy, _ := executeRunWithGitHub(t, gh, "1")
	if !slices.Equal(spy.req.Issues, []int{1}) || len(spy.req.Dependencies[1]) != 0 {
		t.Fatalf("parent should run alone: %+v", spy.req)
	}
}

// Each call returns a separate snapshot. The first child fetch is discovery
// metadata; its second fetch can only be the live filtering fallback.
type closingPlanningGitHub struct{ *specDiscoveryGitHubClient }

func (c *closingPlanningGitHub) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	issue, err := c.specDiscoveryGitHubClient.FetchIssue(ctx, number)
	if number == 2 && err == nil {
		c.setIssueState(2, "closed")
	}
	return issue, err
}

func TestRun_PreparationClosedFilterUsesLiveFallback(t *testing.T) {
	for _, limit := range []bool{false, true} {
		t.Run(fmt.Sprintf("search-limit=%t", limit), func(t *testing.T) {
			gh := &closingPlanningGitHub{newSpecDiscoveryGitHub(map[int]*github.Issue{
				1: {Number: 1, State: "open", Title: "Parent", Body: "## Children\n\n- #2\n"},
				2: {Number: 2, State: "open", Title: "Closes during preparation"},
			})}
			if limit {
				gh.searchIssuesResult = make([]github.Issue, 1000)
			} else {
				gh.searchIssuesError = errors.New("search unavailable")
			}
			spy, output := executeRunWithGitHub(t, gh, "1")
			if !slices.Equal(spy.req.Issues, []int{1}) || len(spy.req.Dependencies[1]) != 0 {
				t.Fatalf("freshly closed child retained: %+v", spy.req)
			}
			if gh.fetchCount[2] != 2 || !strings.Contains(output, "Issue #2 is closed, skipping") {
				t.Fatalf("live fallback not observed: calls=%v output=%s", gh.fetchCount, output)
			}
		})
	}
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

func TestPreparation_AdmitsReadyContinuationsWithoutRediscoveringChildren(t *testing.T) {
	gh := newSpecDiscoveryGitHub(map[int]*github.Issue{
		1: {Number: 1, State: "open", Title: "Ready parent", Body: "## Children\n\n- #404\n", BlockedBy: []int{8}},
		7: {Number: 7, State: "open", Title: "Selected"},
		8: {Number: 8, State: "open", Title: "Ready blocker"},
	})
	plan, err := batch.NewPreparer(gh, gh, io.Discard).Prepare(context.Background(), batch.Preparation{
		Issues: []int{7}, ReadyIssues: []int{1, 1, 7}, IncludeDependencies: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Issues, []int{7, 8, 1}) || !slices.Equal(plan.Deps[1], []int{8}) || len(plan.ParentChildren) != 0 || gh.fetchCount[404] != 0 {
		t.Fatalf("continuation was rediscovered or lost dependencies: %+v, fetches=%v", plan, gh.fetchCount)
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
