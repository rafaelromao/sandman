package sandman

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reviewWaitResult struct {
	Protocol     string          `json:"protocol"`
	State        string          `json:"state"`
	Lifecycle    string          `json:"lifecycle"`
	ObservedHead string          `json:"observed_head_sha"`
	StartedAt    string          `json:"started_at"`
	DeadlineAt   string          `json:"deadline_at"`
	Reason       string          `json:"reason"`
	SnapshotPath string          `json:"snapshot_path"`
	Evidence     json.RawMessage `json:"evidence"`
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
	return writeReviewObserver(t, `{"state":"pending","observed_head_sha":"abc123","snapshot":{"comments":[],"reviews":[],"inline_comments":[]}}`)
}

func writeReviewObserver(t *testing.T, result string) string {
	t.Helper()
	observer := filepath.Join(t.TempDir(), "observer.sh")
	observerScript := "#!/bin/sh\nprintf '%s\\n' '" + result + "'\n"
	if err := os.WriteFile(observer, []byte(observerScript), 0o700); err != nil {
		t.Fatalf("write observer: %v", err)
	}
	return observer
}

func runReviewWait(t *testing.T, helper, requestFile, observer string, once bool) reviewWaitResult {
	return runReviewWaitWithEnv(t, helper, requestFile, observer, once, nil)
}

func runReviewWaitWithEnv(t *testing.T, helper, requestFile, observer string, once bool, extraEnv map[string]string) reviewWaitResult {
	t.Helper()
	args := []string{helper, "--request-file", requestFile, "--json"}
	if once {
		args = append(args, "--once")
	}
	cmd := exec.Command("sh", args...)
	cmd.Env = environmentWithOverrides(map[string]string{
		"SANDMAN_REVIEW_WAIT_OBSERVER": observer,
	})
	for key, value := range extraEnv {
		cmd.Env = environmentWithOverridesOn(cmd.Env, key, value)
	}
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

func environmentWithOverrides(overrides map[string]string) []string {
	args := make([]string, 0, len(overrides)*2)
	for key, value := range overrides {
		args = append(args, key, value)
	}
	return environmentWithOverridesOn(os.Environ(), args...)
}

func environmentWithOverridesOn(base []string, keyAndValues ...string) []string {
	keys := make(map[string]struct{}, len(keyAndValues)/2)
	for i := 0; i+1 < len(keyAndValues); i += 2 {
		keys[keyAndValues[i]] = struct{}{}
	}
	filtered := make([]string, 0, len(base)+len(keyAndValues)/2)
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, overridden := keys[key]; overridden {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	for i := 0; i+1 < len(keyAndValues); i += 2 {
		filtered = append(filtered, keyAndValues[i]+"="+keyAndValues[i+1])
	}
	return filtered
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

func TestReviewWaitV1ReturnsRespondedEvidenceAndResumesThatRequest(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := writeReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)

	first := runReviewWait(t, helper, requestFile, observer, true)
	second := runReviewWait(t, helper, requestFile, observer, true)

	if first.State != "responded" || second.State != "responded" {
		t.Fatalf("states = %q then %q, want responded for both observations", first.State, second.State)
	}
	if first.Lifecycle != "started" || second.Lifecycle != "resumed" {
		t.Fatalf("lifecycles = %q then %q, want started then resumed", first.Lifecycle, second.Lifecycle)
	}
	if second.Request.TriggerID != "1001" || second.ObservedHead != "abc123" {
		t.Fatalf("resumed response identity = trigger %q/head %q", second.Request.TriggerID, second.ObservedHead)
	}
}

func TestReviewWaitV1RetainsTimedOutRequestOnReentry(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	timedOutObserver := writeReviewObserver(t, `{"state":"timed_out","observed_head_sha":"abc123","reason":"request-deadline-exhausted","snapshot":{"comments":[],"reviews":[],"inline_comments":[]}}`)
	first := runReviewWait(t, helper, requestFile, timedOutObserver, true)

	second := runReviewWait(t, helper, requestFile, filepath.Join(t.TempDir(), "missing-observer"), true)

	if first.State != "timed_out" || second.State != "timed_out" {
		t.Fatalf("states = %q then %q, want timed_out for both observations", first.State, second.State)
	}
	if second.Lifecycle != "resumed" || second.ObservedHead != "abc123" {
		t.Fatalf("timed-out reentry = lifecycle %q/head %q, want resumed/abc123", second.Lifecycle, second.ObservedHead)
	}
}

func TestReviewWaitV1ReturnsTimedOutWhenCallerDeadlineIsReached(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := writePendingReviewObserver(t)
	clock := filepath.Join(t.TempDir(), "clock.sh")
	if err := os.WriteFile(clock, []byte("#!/bin/sh\nprintf '%s\\n' 4102444800\n"), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, false, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK": clock,
	})

	if result.State != "timed_out" {
		t.Fatalf("state = %q, want timed_out", result.State)
	}
	if result.Request.TriggerID != "1001" {
		t.Fatalf("timed-out request trigger = %q, want 1001", result.Request.TriggerID)
	}
}

func TestReviewWaitV1DoesNotReturnLateObserverEvidenceAsResponded(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := writeReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"late"}],"reviews":[],"inline_comments":[]}}`)
	clock := filepath.Join(t.TempDir(), "clock.sh")
	clockState := filepath.Join(t.TempDir(), "clock.state")
	if err := os.WriteFile(clockState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write clock state: %v", err)
	}
	clockScript := `#!/bin/sh
calls=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_CLOCK_STATE")
if [ "$calls" = "0" ]; then
  printf '%s\n' 4102444799
else
  printf '%s\n' 4102444801
fi
printf '%s\n' $((calls + 1)) > "$SANDMAN_REVIEW_WAIT_CLOCK_STATE"
`
	if err := os.WriteFile(clock, []byte(clockScript), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK":       clock,
		"SANDMAN_REVIEW_WAIT_CLOCK_STATE": clockState,
	})

	if result.State != "timed_out" {
		t.Fatalf("late observer state = %q, want timed_out", result.State)
	}
}

func TestReviewWaitV1BoundsObserverAtCallerDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := filepath.Join(t.TempDir(), "slow-observer.sh")
	if err := os.WriteFile(observer, []byte("#!/bin/sh\nsleep 5\nprintf '%s\\n' '{\"state\":\"responded\",\"observed_head_sha\":\"abc123\",\"snapshot\":{}}'\n"), 0o700); err != nil {
		t.Fatalf("write slow observer: %v", err)
	}
	clock := filepath.Join(t.TempDir(), "clock.sh")
	clockState := filepath.Join(t.TempDir(), "clock.state")
	if err := os.WriteFile(clockState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write clock state: %v", err)
	}
	clockScript := `#!/bin/sh
calls=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_CLOCK_STATE")
if [ "$calls" = "0" ]; then
  printf '%s\n' 4102444799
else
  printf '%s\n' 4102444800
fi
printf '%s\n' $((calls + 1)) > "$SANDMAN_REVIEW_WAIT_CLOCK_STATE"
`
	if err := os.WriteFile(clock, []byte(clockScript), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}

	started := time.Now()
	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK":       clock,
		"SANDMAN_REVIEW_WAIT_CLOCK_STATE": clockState,
	})

	if result.State != "timed_out" {
		t.Fatalf("slow observer state = %q, want timed_out", result.State)
	}
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("observer was not bounded by request deadline: elapsed %s", elapsed)
	}
}

func TestReviewWaitV1UnavailableDoesNotApprove(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := writeReviewObserver(t, `{"state":"unavailable","observed_head_sha":"abc123","reason":"api-unavailable","snapshot":null}`)

	result := runReviewWait(t, helper, requestFile, observer, true)

	if result.State != "unavailable" {
		t.Fatalf("state = %q, want unavailable", result.State)
	}
	if result.Reason != "api-unavailable" {
		t.Fatalf("reason = %q, want api-unavailable", result.Reason)
	}
}

func TestReviewWaitV1UnavailableIsTerminalForSameTrigger(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	unavailableObserver := writeReviewObserver(t, `{"state":"unavailable","observed_head_sha":"abc123","reason":"api-unavailable","snapshot":null}`)

	first := runReviewWait(t, helper, requestFile, unavailableObserver, true)
	if first.State != "unavailable" || first.Reason != "api-unavailable" {
		t.Fatalf("first result = %q/%q, want unavailable/api-unavailable", first.State, first.Reason)
	}

	respondedObserver := writeReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)
	second := runReviewWait(t, helper, requestFile, respondedObserver, true)
	if second.State != "unavailable" || second.Reason != "api-unavailable" {
		t.Fatalf("same-trigger re-entry = %q/%q, want unavailable/api-unavailable", second.State, second.Reason)
	}
}

func TestReviewWaitV1CoordinatorFailureIsTerminalForSameTrigger(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	failingObserver := filepath.Join(t.TempDir(), "failing-observer.sh")
	if err := os.WriteFile(failingObserver, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatalf("write failing observer: %v", err)
	}

	first := runReviewWait(t, helper, requestFile, failingObserver, true)
	if first.State != "unavailable" || first.Reason != "observer-failed" {
		t.Fatalf("first result = %q/%q, want unavailable/observer-failed", first.State, first.Reason)
	}

	respondedObserver := writeReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)
	second := runReviewWait(t, helper, requestFile, respondedObserver, true)
	if second.State != "unavailable" || second.Reason != "observer-failed" {
		t.Fatalf("same-trigger re-entry = %q/%q, want unavailable/observer-failed", second.State, second.Reason)
	}
}

func TestReviewWaitV1RejectsChangedSameTriggerWithoutResettingState(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	observer := writePendingReviewObserver(t)
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	runReviewWait(t, helper, requestFile, observer, true)
	before, err := os.ReadFile(requestFile + ".state")
	if err != nil {
		t.Fatalf("read state before mismatch: %v", err)
	}

	changed, err := os.ReadFile(requestFile)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	changed = []byte(strings.Replace(string(changed), `"head_sha": "abc123"`, `"head_sha": "different"`, 1))
	if err := os.WriteFile(requestFile, changed, 0o600); err != nil {
		t.Fatalf("write changed request: %v", err)
	}
	result := runReviewWait(t, helper, requestFile, observer, true)
	after, err := os.ReadFile(requestFile + ".state")
	if err != nil {
		t.Fatalf("read state after mismatch: %v", err)
	}

	if result.State != "unavailable" || result.Reason != "same-trigger-request-changed" {
		t.Fatalf("changed request result = %q/%q, want unavailable/same-trigger-request-changed", result.State, result.Reason)
	}
	if string(after) != string(before) {
		t.Fatalf("same-trigger mismatch changed state file:\nbefore %safter %s", before, after)
	}
}

func TestReviewWaitV1RejectsMalformedState(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	if err := os.WriteFile(requestFile+".state", []byte("not-json\n"), 0o600); err != nil {
		t.Fatalf("write malformed state: %v", err)
	}

	result := runReviewWait(t, helper, requestFile, writePendingReviewObserver(t), true)

	if result.State != "unavailable" || result.Reason != "state-file-invalid" {
		t.Fatalf("malformed state result = %q/%q, want unavailable/state-file-invalid", result.State, result.Reason)
	}
}

func TestReviewWaitV1UsesProductionObserverForAllResponseSurfaces(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	observer := filepath.Join(wd, "pr-review", "review-observe-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "https://github.com/owner/repo/pull/42#issuecomment-1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")

	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
	printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"IC_kwDOSYKm0c8AAAABOXLU5g","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"},{"id":"IC_kwDOSYKm0c8AAAABOXLU5w","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"LGTM","createdAt":"2026-08-11T18:01:00Z"}],"reviewDecision":"","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"COMMENTED","submitted_at":"2026-08-11T18:02:00Z","body":"file.go needs a test"}]' ;;
    */comments) printf '%s\n' '[{"id":3001,"created_at":"2026-08-11T18:03:00Z","body":"inline feedback"}]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})

	if result.State != "responded" {
		t.Fatalf("state = %q, want responded", result.State)
	}
	evidence := string(result.Evidence)
	for _, want := range []string{"LGTM", "file.go needs a test", "inline feedback", "top_level", "formal_reviews", "inline_comments"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("response evidence missing %q: %s", want, evidence)
		}
	}
}
