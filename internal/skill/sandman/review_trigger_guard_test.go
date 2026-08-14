package sandman

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type triggerGuardResult struct {
	Protocol        string               `json:"protocol"`
	Decision        string               `json:"decision"`
	Reason          string               `json:"reason"`
	ObservedHeadSHA string               `json:"observed_head_sha"`
	LatestTrigger   *triggerGuardTrigger `json:"latest_trigger"`
}

type triggerGuardTrigger struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

func TestReviewTriggerGuard_BlocksUnansweredNewestTriggerWithoutLocalRecord(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	marker := filepath.Join(t.TempDir(), "comment-posted")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}]}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews|*/comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "comment" ]; then
  touch "$GUARD_COMMENT_MARKER"
  printf '%s\n' 'https://github.com/owner/repo/pull/42#issuecomment-1002'
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runTriggerGuard(t, helper, bin, marker, "owner/repo", "42", "abc123", "/sandman review", "")
	if result.Protocol != "review-trigger/v1" {
		t.Fatalf("protocol = %q, want review-trigger/v1", result.Protocol)
	}
	if result.Decision != "block" || result.Reason != "unanswered-trigger" {
		t.Fatalf("guard result = %q/%q, want block/unanswered-trigger", result.Decision, result.Reason)
	}
	if result.ObservedHeadSHA != "abc123" {
		t.Fatalf("observed head = %q, want abc123", result.ObservedHeadSHA)
	}
	if result.LatestTrigger == nil || result.LatestTrigger.ID != "https://github.com/owner/repo/pull/42#issuecomment-1001" {
		t.Fatalf("latest trigger = %+v, want the confirmed comment URL", result.LatestTrigger)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("guard must prevent comment delivery, marker stat error = %v", err)
	}
}

func TestReviewTriggerGuard_FailsClosedWhenGitHubQueryFails(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write failing gh shim: %v", err)
	}

	result := runTriggerGuardCommand(t, helper, bin, "", "owner/repo", "42", "abc123", "/sandman review", "")
	if result.Decision != "uncertain" || result.Reason != "pull-request-view-failed" {
		t.Fatalf("guard result = %q/%q, want uncertain/pull-request-view-failed", result.Decision, result.Reason)
	}
}

func TestReviewTriggerGuard_FailsClosedWhenNewestTriggerOrderingIsAmbiguous(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}]}'
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runTriggerGuardCommand(t, helper, bin, "", "owner/repo", "42", "abc123", "/sandman review", "")
	if result.Decision != "uncertain" || result.Reason != "trigger-order-ambiguous" {
		t.Fatalf("guard result = %q/%q, want uncertain/trigger-order-ambiguous", result.Decision, result.Reason)
	}
}

func TestReviewTriggerGuard_FailsClosedOnMalformedTriggerTimestamp(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"not-a-timestamp"}]}'
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runTriggerGuardCommand(t, helper, bin, "", "owner/repo", "42", "abc123", "/sandman review", "")
	if result.Decision != "uncertain" || result.Reason != "comment-history-invalid" {
		t.Fatalf("guard result = %q/%q, want uncertain/comment-history-invalid", result.Decision, result.Reason)
	}
}

func TestReviewTriggerGuard_AllowsAnsweredNewestTrigger(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"review response","createdAt":"2026-08-11T18:01:01Z"}]}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews|*/comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runTriggerGuardCommand(t, helper, bin, "", "owner/repo", "42", "abc123", "/sandman review", "")
	if result.Decision != "allow" || result.Reason != "answered-trigger" {
		t.Fatalf("guard result = %q/%q, want allow/answered-trigger", result.Decision, result.Reason)
	}
}

func TestReviewTriggerGuard_AllowsTrustedRequestAfterHeadChange(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	request := `{"protocol":"review-wait/v1","repository":"owner/repo","pull_request":42,"head_sha":"oldsha","trigger_id":"https://github.com/owner/repo/pull/42#issuecomment-1001","trigger_prefix":"/sandman review","trigger_created_at":"2026-08-11T18:00:01Z"}`
	if err := os.WriteFile(requestFile, []byte(request), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}]}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews|*/comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runTriggerGuardCommand(t, helper, bin, "", "owner/repo", "42", "abc123", "/sandman review", requestFile)
	if result.Decision != "allow" || result.Reason != "head-changed" {
		t.Fatalf("guard result = %q/%q, want allow/head-changed", result.Decision, result.Reason)
	}
}

func TestReviewTriggerGuard_BlocksUnknownNewestTriggerEvenWhenPriorHeadChanged(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	request := `{"protocol":"review-wait/v1","repository":"owner/repo","pull_request":42,"head_sha":"oldsha","trigger_id":"https://github.com/owner/repo/pull/42#issuecomment-1001","trigger_prefix":"/sandman review","trigger_created_at":"2026-08-11T18:00:01Z"}`
	if err := os.WriteFile(requestFile, []byte(request), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"/sandman review","createdAt":"2026-08-11T18:02:01Z"}]}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews|*/comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runTriggerGuardCommand(t, helper, bin, "", "owner/repo", "42", "abc123", "/sandman review", requestFile)
	if result.Decision != "block" || result.Reason != "unanswered-trigger" {
		t.Fatalf("guard result = %q/%q, want block/unanswered-trigger", result.Decision, result.Reason)
	}
}

func TestReviewTriggerGuard_FailsClosedOnMalformedResponsePayload(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}]}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '{"not":"an array"}' ;;
    */comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runTriggerGuardCommand(t, helper, bin, "", "owner/repo", "42", "abc123", "/sandman review", "")
	if result.Decision != "uncertain" || result.Reason != "formal-reviews-invalid" {
		t.Fatalf("guard result = %q/%q, want uncertain/formal-reviews-invalid", result.Decision, result.Reason)
	}
}

func TestReviewTriggerGuard_TreatsFormalReviewAsResponse(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}]}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"COMMENTED","submitted_at":"2026-08-11T18:01:01Z","body":"review response"}]' ;;
    */comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runTriggerGuardCommand(t, helper, bin, "", "owner/repo", "42", "abc123", "/sandman review", "")
	if result.Decision != "allow" || result.Reason != "answered-trigger" {
		t.Fatalf("guard result = %q/%q, want allow/answered-trigger", result.Decision, result.Reason)
	}
}

func TestReviewTriggerGuard_TreatsInlineCommentAsResponse(t *testing.T) {
	helper := filepath.Join(mustWorkingDir(t), "pr-review", "review-trigger-guard-v1.sh")
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}]}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[]' ;;
    */comments) printf '%s\n' '[{"id":3001,"created_at":"2026-08-11T18:01:01Z","body":"inline response"}]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runTriggerGuardCommand(t, helper, bin, "", "owner/repo", "42", "abc123", "/sandman review", "")
	if result.Decision != "allow" || result.Reason != "answered-trigger" {
		t.Fatalf("guard result = %q/%q, want allow/answered-trigger", result.Decision, result.Reason)
	}
}

func mustWorkingDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

func runTriggerGuard(t *testing.T, helper, bin, marker, repository, pullRequest, headSHA, prefix, requestFile string) triggerGuardResult {
	t.Helper()
	return runTriggerGuardCommand(t, helper, bin, marker, repository, pullRequest, headSHA, prefix, requestFile)
}

func runTriggerGuardCommand(t *testing.T, helper, bin, marker, repository, pullRequest, headSHA, prefix, requestFile string) triggerGuardResult {
	t.Helper()
	args := []string{helper, "--repository", repository, "--pull-request", pullRequest, "--head-sha", headSHA, "--trigger-prefix", prefix}
	if requestFile != "" {
		args = append(args, "--request-file", requestFile)
	}
	cmd := exec.Command("sh", args...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GUARD_COMMENT_MARKER="+marker)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run trigger guard: %v\n%s", err, output)
	}
	var result triggerGuardResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &result); err != nil {
		t.Fatalf("decode trigger guard output %q: %v", output, err)
	}
	return result
}
