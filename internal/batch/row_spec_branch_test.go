package batch

import (
	"context"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestResolvedBranchIsFrozenIntoRunSpec(t *testing.T) {
	client := &fakeGitHubClient{issues: map[int]*github.Issue{
		42: {Number: 42, Title: "Pending lifecycle"},
	}}
	orchestrator := &Orchestrator{githubClient: client}
	req := Request{Issues: []int{42}}

	resolved, err := orchestrator.resolveRunBranches(context.Background(), req, "main")
	if err != nil {
		t.Fatalf("resolve run branches: %v", err)
	}
	if got, want := resolved.Branches[42], "42-pending-lifecycle"; got != want {
		t.Fatalf("resolved branch = %q, want %q", got, want)
	}

	run, _ := runSpecFromRow(RowSpec{
		IssueNumber: 42,
		Branches:    resolved.Branches,
	})
	if got, want := run.Branch, "42-pending-lifecycle"; got != want {
		t.Fatalf("immutable RunSpec branch = %q, want %q", got, want)
	}
	if got := resolved.IssueTitles[42]; got != "Pending lifecycle" {
		t.Fatalf("resolved issue title = %q, want fetched title", got)
	}
}
