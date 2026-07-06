package github

import (
	"context"
	"fmt"
	"strconv"
)

// GHCommentPoster posts a comment on a pull request via the `gh pr
// comment` CLI. It is the production CommentPoster wired into the
// review daemon (issue #1846).
//
// The implementation delegates to the package's existing CLIClient
// so timeout handling, the exec runner seam, and process-group kill
// semantics exactly match every other gh-backed call in this
// package — see CLIClient.command and the issue #1780 rationale
// therein.
type GHCommentPoster struct {
	cli *CLIClient
}

// NewGHCommentPoster returns a GHCommentPoster that uses cli for
// every gh invocation. cli must be non-nil; the caller owns its
// lifecycle (typically the same *CLIClient the rest of the GitHub
// client uses).
func NewGHCommentPoster(cli *CLIClient) *GHCommentPoster {
	if cli == nil {
		panic("github.NewGHCommentPoster: cli is nil")
	}
	return &GHCommentPoster{cli: cli}
}

// PostComment posts body as a comment on the given pull request by
// running `gh pr comment <N> --body <body>`. Context cancellation
// (or cli.Timeout) cancels the underlying exec.Cmd via the same
// process-group kill CLIClient already uses.
func (p *GHCommentPoster) PostComment(ctx context.Context, prNumber int, body string) error {
	if prNumber <= 0 {
		return fmt.Errorf("gh pr comment: invalid prNumber %d", prNumber)
	}
	callCtx, cancel := p.cli.boundContext(ctx)
	defer cancel()
	cmd := p.cli.command(callCtx, "gh", "pr", "comment", strconv.Itoa(prNumber), "--body", body)
	_, err := runCmd(callCtx, cmd, "gh pr comment")
	if err != nil {
		return fmt.Errorf("gh pr comment: %w", err)
	}
	return nil
}
