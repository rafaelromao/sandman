package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGHCommentPoster_HappyPath(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{output: ""}}}
	cli := &CLIClient{runner: runner, Timeout: 0}
	poster := NewGHCommentPoster(cli)

	body := "hello /sandman world"
	if err := poster.PostComment(context.Background(), 17, body); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 gh invocation, got %d", len(runner.calls))
	}
	call := runner.calls[0]
	if call.name != "gh" {
		t.Errorf("expected binary gh, got %q", call.name)
	}
	wantPrefix := []string{"pr", "comment", "17", "--body", body}
	if !reflect.DeepEqual(call.args, wantPrefix) {
		t.Errorf("expected args %v, got %v", wantPrefix, call.args)
	}
}

func TestGHCommentPoster_GhFailureSurfacesError(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{err: errors.New("boom")}}}
	cli := &CLIClient{runner: runner, Timeout: 0}
	poster := NewGHCommentPoster(cli)

	err := poster.PostComment(context.Background(), 17, "body")
	if err == nil {
		t.Fatalf("expected error from gh failure, got nil")
	}
	if !strings.Contains(err.Error(), "gh pr comment") {
		t.Errorf("expected wrapped gh pr comment error, got %q", err.Error())
	}
}

func TestGHCommentPoster_HonoursContextCancellation(t *testing.T) {
	runner := &blockingFakeRunner{}
	cli := &CLIClient{runner: runner, Timeout: 0}
	poster := NewGHCommentPoster(cli)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := poster.PostComment(ctx, 17, "body")
	if err == nil {
		t.Fatalf("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected wrapped context.Canceled, got %v", err)
	}
}

func TestGHCommentPoster_RejectsInvalidPRNumber(t *testing.T) {
	cli := &CLIClient{}
	poster := NewGHCommentPoster(cli)
	if err := poster.PostComment(context.Background(), 0, "body"); err == nil {
		t.Fatal("expected error for prNumber=0")
	}
	if err := poster.PostComment(context.Background(), -1, "body"); err == nil {
		t.Fatal("expected error for negative prNumber")
	}
}
