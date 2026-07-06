package review

import (
	"context"
	"errors"
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
// not care about the post step. The daemon's other tests (slice
// A, slice B, restart, canonical, etc.) wire this so the renamed
// review.New signature stays consistent without each test having
// to add a CommentPoster fixture. Production wiring goes through
// NewGHCommentPoster in internal/github/comment_poster.go.
type nopCommentPoster struct{}

func (nopCommentPoster) PostComment(context.Context, int, string) error {
	return nil
}

// postStepError signals that launchReview's post step recorded
// MarkSeen("failure") for the trigger — either decision.md was
// missing or PostComment returned an error. The launch goroutine
// detects this via errors.As and registers a pending entry so the
// bounded-retry escape engages. Pre-batch errors (RunBatch,
// FetchPR, ...) bypass this wrap so the launch goroutine falls
// through to its release-claim path and the trigger can be
// retried by the next processPR tick.
type postStepError struct{ cause error }

func (e *postStepError) Error() string { return e.cause.Error() }
func (e *postStepError) Unwrap() error { return e.cause }

// asPostStepError is the canonical constructor used by postDecision.
func asPostStepError(cause error) error { return &postStepError{cause: cause} }

// isPostStepError reports whether err originated inside
// postDecision. The launch goroutine uses this to decide
// between registerPendingReview (post-step failure, bounded-
// retry path) and release-claim (pre-batch failure, immediate
// retry path).
func isPostStepError(err error) bool {
	if err == nil {
		return false
	}
	var p *postStepError
	return errors.As(err, &p)
}
