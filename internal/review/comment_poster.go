package review

import (
	"context"
)

// CommentPoster publishes a comment body on a pull request.
//
// It is the seam between the review daemon and the `gh pr comment`
// integration. Production wires a *github.GHCommentPoster (issue
// #1846); tests inject a fake that captures the posted body and
// returns a configurable error. The context flows through so the
// daemon's polling loop (or an operator signal) can cancel a hung
// `gh` invocation the same way every other gh-backed call honours
// cancellation (issue #1780).
//
// Implementations MUST honour context cancellation. A returned
// error wraps (or is, by errors.Is) ctx.Err() when cancellation
// is the cause of the failure; the daemon treats that as the
// "ctx cancelled between RunBatch and post" branch.
type CommentPoster interface {
	PostComment(ctx context.Context, prNumber int, body string) error
}

// nopCommentPoster is a stub CommentPoster used by tests that do
// not care about the post step. The daemon's other tests (issue #1480,
// restart, canonical, etc.) wire this so the renamed
// review.New signature stays consistent without each test having
// to add a CommentPoster fixture. Production wiring goes through
// NewGHCommentPoster in internal/github/comment_poster.go.
type nopCommentPoster struct{}

func (nopCommentPoster) PostComment(context.Context, int, string) error {
	return nil
}
