package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// FetchReviewHead obtains the base repository's PR ref, including fork PRs,
// without borrowing a mutable branch or the process-shared FETCH_HEAD file.
func FetchReviewHead(ctx context.Context, repo string, pr int, head string) error {
	if pr <= 0 || !commitSHA.MatchString(head) {
		return fmt.Errorf("invalid review snapshot identity")
	}
	ref := "refs/sandman/review-fetch/" + rand.Text()
	defer func() {
		cmd := exec.Command("git", "update-ref", "-d", ref)
		cmd.Dir = repo
		_ = cmd.Run()
	}()
	cmd := exec.CommandContext(ctx, "git", "fetch", "--no-tags", "--no-write-fetch-head", "origin", fmt.Sprintf("refs/pull/%d/head:%s", pr, ref))
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch review head: %w: %s", err, out)
	}
	cmd = exec.CommandContext(ctx, "git", "rev-parse", ref+"^{commit}")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("resolve fetched review head: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(out)), head) {
		return fmt.Errorf("review head changed while fetching PR #%d", pr)
	}
	return nil
}

// VerifyReviewHead checks the actual sandbox view before a reviewer attempt.
// A reused worktree is not proof that the expected snapshot is checked out.
func VerifyReviewHead(ctx context.Context, sb Sandbox, head string) error {
	var out, stderr bytes.Buffer
	if !commitSHA.MatchString(head) {
		return fmt.Errorf("invalid review snapshot head")
	}
	if err := sb.Exec(ctx, "git rev-parse HEAD", &out, &stderr); err != nil {
		return fmt.Errorf("read review checkout: %w: %s", err, stderr.String())
	}
	if !strings.EqualFold(strings.TrimSpace(out.String()), head) {
		return fmt.Errorf("review checkout does not match expected PR head %s", head)
	}
	if err := sb.Exec(ctx, "git diff --quiet HEAD --", nil, &stderr); err != nil {
		return fmt.Errorf("review checkout has tracked changes or cannot be verified: %w: %s", err, stderr.String())
	}
	return nil
}
