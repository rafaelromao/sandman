package sandman

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type reviewWaitResult struct {
	Protocol     string `json:"protocol"`
	State        string `json:"state"`
	Lifecycle    string `json:"lifecycle"`
	ObservedHead string `json:"observed_head_sha"`
	StartedAt    string `json:"started_at"`
	DeadlineAt   string `json:"deadline_at"`
	Reason       string `json:"reason"`
	Request      struct {
		PullRequest    int    `json:"pull_request"`
		HeadSHA        string `json:"head_sha"`
		TriggerID      string `json:"trigger_id"`
		TimeoutSeconds int    `json:"effective_timeout_seconds"`
	} `json:"request"`
}

func writeReviewWaitRequest(t *testing.T, path, triggerID, startedAt, deadlineAt string) {
	t.Helper()
	request := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 42,
  "head_sha": "abc123",
  "trigger_id": "` + triggerID + `",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "` + startedAt + `",
  "confirmed_at": "` + startedAt + `",
  "started_at": "` + startedAt + `",
  "deadline_at": "` + deadlineAt + `",
  "deadline_unix_seconds": 4102444800,
  "effective_timeout_seconds": 1800,
  "poll_plan": [120, 60, 60, 30]
}
`
	if err := os.WriteFile(path, []byte(request), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func writePendingReviewObserver(t *testing.T) string {
	t.Helper()
	observer := filepath.Join(t.TempDir(), "observer.sh")
	observerScript := `#!/bin/sh
printf '%s\n' '{"state":"pending","observed_head_sha":"abc123","snapshot":{"comments":[],"reviews":[],"inline_comments":[]}}'
`
	if err := os.WriteFile(observer, []byte(observerScript), 0o700); err != nil {
		t.Fatalf("write observer: %v", err)
	}
	return observer
}

func runReviewWait(t *testing.T, helper, requestFile, observer string, once bool) reviewWaitResult {
	t.Helper()
	args := []string{helper, "--request-file", requestFile, "--json"}
	if once {
		args = append(args, "--once")
	}
	cmd := exec.Command("sh", args...)
	cmd.Env = append(os.Environ(), "SANDMAN_REVIEW_WAIT_OBSERVER="+observer)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("review wait: %v\n%s", err, output)
	}
	var result reviewWaitResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &result); err != nil {
		t.Fatalf("parse result %q: %v", output, err)
	}
	return result
}

func TestReviewWaitV1StartsRequestAndReturnsStructuredPending(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	result := runReviewWait(t, helper, requestFile, writePendingReviewObserver(t), true)
	if result.Protocol != "review-wait/v1" {
		t.Fatalf("protocol = %q, want review-wait/v1", result.Protocol)
	}
	if result.State != "pending" {
		t.Fatalf("state = %q, want pending", result.State)
	}
	if result.Lifecycle != "started" {
		t.Fatalf("lifecycle = %q, want started", result.Lifecycle)
	}
	if result.ObservedHead != "abc123" {
		t.Fatalf("observed head = %q, want abc123", result.ObservedHead)
	}
	if result.Request.PullRequest != 42 || result.Request.HeadSHA != "abc123" || result.Request.TriggerID != "1001" || result.Request.TimeoutSeconds != 1800 {
		t.Fatalf("request identity = %+v, want PR 42/head abc123/trigger 1001/timeout 1800", result.Request)
	}
	if result.StartedAt != "2026-08-11T18:00:01Z" || result.DeadlineAt != "2026-08-11T18:30:01Z" {
		t.Fatalf("request timing changed: started=%q deadline=%q", result.StartedAt, result.DeadlineAt)
	}
	if _, err := os.Stat(requestFile + ".state"); err != nil {
		t.Fatalf("result state sidecar missing: %v", err)
	}
}

func TestReviewWaitV1ResumesSameTriggerWithoutChangingRequestTiming(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := writePendingReviewObserver(t)

	started := runReviewWait(t, helper, requestFile, observer, true)
	resumed := runReviewWait(t, helper, requestFile, observer, true)

	if started.Lifecycle != "started" || resumed.Lifecycle != "resumed" {
		t.Fatalf("lifecycles = %q then %q, want started then resumed", started.Lifecycle, resumed.Lifecycle)
	}
	if resumed.StartedAt != started.StartedAt || resumed.DeadlineAt != started.DeadlineAt {
		t.Fatalf("same-trigger resume changed timing: first (%q, %q), second (%q, %q)", started.StartedAt, started.DeadlineAt, resumed.StartedAt, resumed.DeadlineAt)
	}
	if resumed.Request.TriggerID != "1001" {
		t.Fatalf("resumed trigger = %q, want 1001", resumed.Request.TriggerID)
	}
}

func TestReviewWaitV1StartsFreshRequestForLaterTriggerOnSamePullRequest(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	observer := writePendingReviewObserver(t)
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	first := runReviewWait(t, helper, requestFile, observer, true)

	writeReviewWaitRequest(t, requestFile, "1002", "2026-08-11T19:00:01Z", "2026-08-11T19:30:01Z")
	second := runReviewWait(t, helper, requestFile, observer, true)

	if first.Lifecycle != "started" || second.Lifecycle != "started" {
		t.Fatalf("lifecycles = %q then %q, want both started for distinct requests", first.Lifecycle, second.Lifecycle)
	}
	if second.Request.TriggerID != "1002" {
		t.Fatalf("new trigger = %q, want 1002", second.Request.TriggerID)
	}
	if second.StartedAt == first.StartedAt || second.DeadlineAt == first.DeadlineAt {
		t.Fatalf("new trigger reused timing: first (%q, %q), second (%q, %q)", first.StartedAt, first.DeadlineAt, second.StartedAt, second.DeadlineAt)
	}
}
