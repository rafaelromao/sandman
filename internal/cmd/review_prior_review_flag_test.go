package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

// fakePRCommentsClient is a focused test double for computePriorReviewExists
// (issue #1892). It only implements ListPRComments + the minimum surface
// required by review.GitHubClient.
type fakePRCommentsClient struct {
	comments []github.PRComment
	err      error
}

func (f *fakePRCommentsClient) ListPRComments(ctx context.Context, number int) ([]github.PRComment, error) {
	return f.comments, f.err
}
func (f *fakePRCommentsClient) AuthenticatedLogin(ctx context.Context) (string, error) {
	return "sandman", nil
}

// Stubs to satisfy review.GitHubClient — not exercised by these tests.
func (f *fakePRCommentsClient) FetchPR(ctx context.Context, number int) (*github.PR, error) {
	return nil, errors.New("not implemented")
}
func (f *fakePRCommentsClient) ListOpenPRs(ctx context.Context) ([]github.PR, error) {
	return nil, errors.New("not implemented")
}
func (f *fakePRCommentsClient) RepoName(ctx context.Context) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakePRCommentsClient) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	return nil, errors.New("not implemented")
}
func (f *fakePRCommentsClient) FetchIssue(ctx context.Context, number int) (*github.Issue, error) {
	return nil, errors.New("not implemented")
}
func (f *fakePRCommentsClient) FetchIssueDependencies(ctx context.Context, number int) ([]int, error) {
	return nil, errors.New("not implemented")
}
func (f *fakePRCommentsClient) SearchIssues(ctx context.Context, query string) ([]github.Issue, error) {
	return nil, errors.New("not implemented")
}
func (f *fakePRCommentsClient) ListIssueComments(ctx context.Context, number int) ([]github.IssueComment, error) {
	return nil, errors.New("not implemented")
}
func (f *fakePRCommentsClient) ListSubIssues(ctx context.Context, parent int) ([]int, error) {
	return nil, errors.New("not implemented")
}
func (f *fakePRCommentsClient) EditComment(ctx context.Context, commentID, body string) error {
	return errors.New("not implemented")
}
func (f *fakePRCommentsClient) EditPRBody(ctx context.Context, prNumber int, body string) error {
	return errors.New("not implemented")
}
func (f *fakePRCommentsClient) AddCommentReaction(ctx context.Context, commentID, content string) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakePRCommentsClient) AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakePRCommentsClient) RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error {
	return errors.New("not implemented")
}
func (f *fakePRCommentsClient) RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error {
	return errors.New("not implemented")
}

func TestComputePriorReviewExists_OnlyTriggerReturnsFalse(t *testing.T) {
	cli := &fakePRCommentsClient{comments: []github.PRComment{
		{ID: "t1", Body: "/sandman review", CreatedAt: time.Now()},
	}}
	got, err := computePriorReviewExists(context.Background(), cli, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Errorf("expected false when only an implementor trigger exists, got true")
	}
}

func TestComputePriorReviewExists_HumanReviewReturnsTrue(t *testing.T) {
	cli := &fakePRCommentsClient{comments: []github.PRComment{
		{ID: "h1", Body: "LGTM, no blockers.", CreatedAt: time.Now()},
		{ID: "t1", Body: "/sandman review focus on tests", CreatedAt: time.Now()},
	}}
	got, err := computePriorReviewExists(context.Background(), cli, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Errorf("expected true when a human review exists, got false")
	}
}

func TestComputePriorReviewExists_BotSelfPostReturnsTrue(t *testing.T) {
	cli := &fakePRCommentsClient{comments: []github.PRComment{
		{
			ID:        "bot-1",
			Body:      "## Previous review progress\nFirst pass.\n\n## Summary\nLGTM.",
			CreatedAt: time.Now(),
		},
		{ID: "t1", Body: "/sandman review", CreatedAt: time.Now()},
	}}
	got, err := computePriorReviewExists(context.Background(), cli, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Errorf("expected true when a bot self-post exists (counts as a prior review, issue #1892), got false")
	}
}

func TestComputePriorReviewExists_EmptyCommentsReturnsFalse(t *testing.T) {
	cli := &fakePRCommentsClient{comments: nil}
	got, err := computePriorReviewExists(context.Background(), cli, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Errorf("expected false when no comments exist, got true")
	}
}

func TestComputePriorReviewExists_ListErrorReturnsError(t *testing.T) {
	cli := &fakePRCommentsClient{err: errors.New("boom")}
	_, err := computePriorReviewExists(context.Background(), cli, 42)
	if err == nil {
		t.Fatal("expected error to be propagated")
	}
}
