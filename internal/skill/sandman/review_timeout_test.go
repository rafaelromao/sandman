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
	got, err := runReviewTimeoutHelper(t, "review_timeout_cap_wait", "17", "30")
	if err != nil {
		t.Fatalf("review_timeout_cap_wait: %v", err)
	}
	if got != "17" {
		t.Fatalf("capped wait = %q, want 17", got)
	}
}

func TestReviewTimeoutHelperUsesWallClockRemaining(t *testing.T) {
	deadline, err := runReviewTimeoutHelper(t, "review_timeout_deadline", "1000", "240")
	if err != nil {
		t.Fatalf("review_timeout_deadline: %v", err)
	}
	if deadline != "1240" {
		t.Fatalf("deadline = %q, want 1240", deadline)
	}

	remaining, err := runReviewTimeoutHelper(t, "review_timeout_remaining", deadline, "1125")
	if err != nil {
		t.Fatalf("review_timeout_remaining: %v", err)
	}
	if remaining != "115" {
		t.Fatalf("remaining = %q, want 115", remaining)
	}

	if _, err := runReviewTimeoutHelper(t, "review_timeout_cap_wait", "0", "30"); err == nil {
		t.Fatal("expired deadline should reject a wait")
	}
}

func TestReviewTimeoutHelperStopsDirectChildAtDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-timeout.sh")
	cmd := exec.Command("sh", "-c", `. "$1"; now=$(review_timeout_now); deadline=$((now + 1)); review_timeout_run "$deadline" sleep 2`, "review-timeout", helper)
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

func TestReviewTimeoutHelperWritesAtomicDeadlineState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), ".sandman", "state", "385.review_deadline")
	if _, err := runReviewTimeoutHelper(t, "review_timeout_write_state", statePath, "head-sha", "comment-42", "1000", "1240"); err != nil {
		t.Fatalf("review_timeout_write_state: %v", err)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read deadline state: %v", err)
	}
	want := "head_sha=head-sha\ntrigger_id=comment-42\nstarted_at=1000\ndeadline_at=1240\n"
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
}
