package sandman

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runReviewTimeoutHelper(t *testing.T, command string, args ...string) (string, error) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-timeout.sh")
	parts := append([]string{command}, args...)
	quoted := make([]string, len(parts))
	for i, part := range parts {
		quoted[i] = shellQuote(part)
	}
	script := `. "$1"; ` + strings.Join(quoted, " ")
	cmd := exec.Command("sh", "-c", script, "review-timeout", helper)
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestReviewTimeoutHelperCapsWaitAtDeadline(t *testing.T) {
	got, err := runReviewTimeoutHelper(t, "review_timeout_cap_wait", "17000", "30")
	if err != nil {
		t.Fatalf("review_timeout_cap_wait: %v", err)
	}
	if got != "17.000" {
		t.Fatalf("capped wait = %q, want 17.000", got)
	}
}

func TestReviewTimeoutHelperUsesWallClockRemaining(t *testing.T) {
	deadline, err := runReviewTimeoutHelper(t, "review_timeout_deadline", "1000000", "240")
	if err != nil {
		t.Fatalf("review_timeout_deadline: %v", err)
	}
	if deadline != "1240000" {
		t.Fatalf("deadline = %q, want 1240000", deadline)
	}

	remaining, err := runReviewTimeoutHelper(t, "review_timeout_remaining", deadline, "1125000")
	if err != nil {
		t.Fatalf("review_timeout_remaining: %v", err)
	}
	if remaining != "115000" {
		t.Fatalf("remaining = %q, want 115000", remaining)
	}
	if _, err := runReviewTimeoutHelper(t, "review_timeout_remaining", deadline, deadline); err == nil {
		t.Fatal("expired deadline should reject remaining time")
	}

	if _, err := runReviewTimeoutHelper(t, "review_timeout_cap_wait", "0", "30"); err == nil {
		t.Fatal("expired deadline should reject a wait")
	}
	exact, err := runReviewTimeoutHelper(t, "review_timeout_cap_wait", "240000", "240")
	if err != nil {
		t.Fatalf("exact deadline wait: %v", err)
	}
	if exact != "240.000" {
		t.Fatalf("exact capped wait = %q, want 240.000", exact)
	}
}

func TestReviewTimeoutHelperStopsDirectChildAtDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-timeout.sh")
	cmd := exec.Command("sh", "-c", `. "$1"; now=$(review_timeout_now); deadline=$((now + 1000)); review_timeout_run "$deadline" sleep 2`, "review-timeout", helper)
	started := time.Now()
	err = cmd.Run()
	if err == nil {
		t.Fatal("review_timeout_run should stop a child that crosses its deadline")
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 124 {
		t.Fatalf("review_timeout_run error = %v, want exit 124", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("deadline watcher took %s, expected less than child duration", elapsed)
	}
}

func TestReviewTimeoutHelperRemapsChild124BeforeDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-timeout.sh")
	cmd := exec.Command("sh", "-c", `. "$1"; now=$(review_timeout_now); deadline=$((now + 10000)); review_timeout_run "$deadline" sh -c 'exit 124'`, "review-timeout", helper)
	err = cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 123 {
		t.Fatalf("child status remap error = %v, want exit 123", err)
	}
}

func TestReviewTimeoutHelperWritesAtomicDeadlineState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), ".sandman", "state", "385.review_deadline")
	if _, err := runReviewTimeoutHelper(t, "review_timeout_write_state", statePath, "head-sha", "comment-42", "1000000", "1240000"); err != nil {
		t.Fatalf("review_timeout_write_state: %v", err)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read deadline state: %v", err)
	}
	want := "head_sha=head-sha\ntrigger_id=comment-42\nstarted_at=1000000\ndeadline_at=1240000\n"
	if string(data) != want {
		t.Fatalf("deadline state = %q, want %q", data, want)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat deadline state: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("deadline state mode = %o, want 600", mode)
	}
	if _, err := runReviewTimeoutHelper(t, "review_timeout_state_matches", statePath, "head-sha", "comment-42"); err != nil {
		t.Fatalf("same trigger should reuse deadline state: %v", err)
	}
	if _, err := runReviewTimeoutHelper(t, "review_timeout_state_matches", statePath, "new-head", "comment-43"); err == nil {
		t.Fatal("new head and trigger should not reuse the old deadline state")
	}
	if err := os.WriteFile(statePath, []byte("head_sha=head-sha\ntrigger_id=comment-42\nstarted_at=bad\ndeadline_at=1240000\n"), 0o600); err != nil {
		t.Fatalf("write malformed deadline state: %v", err)
	}
	if _, err := runReviewTimeoutHelper(t, "review_timeout_state_matches", statePath, "head-sha", "comment-42"); err == nil {
		t.Fatal("malformed deadline state should not match")
	}
}
