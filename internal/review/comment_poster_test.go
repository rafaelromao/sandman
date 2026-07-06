package review

import (
	"context"
	"errors"
	"testing"
)

// compile-time check that a fake implementation satisfies the
// published signature. The test pins the method set so a future
// signature drift surfaces at compile-time, not at runtime.
func TestCommentPoster_CompileTimeShape(t *testing.T) {
	var _ CommentPoster = (*commentPosterCompileFake)(nil)

	var poster CommentPoster = commentPosterCompileFake{}
	if err := poster.PostComment(context.Background(), 1, "x"); err != nil && !errors.Is(err, errSentinel) {
		t.Fatalf("unexpected error from compile-time shape: %v", err)
	}
}

var errSentinel = errors.New("comment-poster compile fake")

type commentPosterCompileFake struct{}

func (commentPosterCompileFake) PostComment(context.Context, int, string) error {
	return errSentinel
}
