package sandman

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	Elapsed      int             `json:"elapsed_seconds"`
	Evidence     json.RawMessage `json:"evidence"`
	Request      struct {
		PullRequest    int    `json:"pull_request"`
		HeadSHA        string `json:"head_sha"`
		TriggerID      string `json:"trigger_id"`
		StartedUnix    int    `json:"started_unix_seconds"`
		DeadlineUnix   int    `json:"deadline_unix_seconds"`
		TimeoutSeconds int    `json:"effective_timeout_seconds"`
	} `json:"request"`
}

type reviewClassificationEvidence struct {
	ID                json.RawMessage `json:"id"`
	Body              string          `json:"body"`
	ResponseTimestamp string          `json:"response_timestamp"`
	HeadStatus        string          `json:"head_status"`
	State             string          `json:"state"`
}

type reviewClassification struct {
	Protocol     string `json:"protocol"`
	RequestState string `json:"request_state"`
	Decision     string `json:"decision"`
	Request      struct {
		TriggerID        string `json:"trigger_id"`
		HeadSHA          string `json:"head_sha"`
		TriggerCreatedAt string `json:"trigger_created_at"`
		DeadlineUnix     int    `json:"deadline_unix_seconds"`
	} `json:"request"`
	ResponseCounts struct {
		TopLevel      int `json:"top_level"`
		FormalReviews int `json:"formal_reviews"`
		Inline        int `json:"inline_comments"`
	} `json:"response_counts"`
	Sources struct {
		TopLevel      []reviewClassificationEvidence `json:"top_level"`
		FormalReviews []reviewClassificationEvidence `json:"formal_reviews"`
		Inline        []reviewClassificationEvidence `json:"inline_comments"`
	} `json:"sources"`
	Formal struct {
		Decision                  string                         `json:"decision"`
		ApprovalEvidence          []reviewClassificationEvidence `json:"approval_evidence"`
		AmbiguousApprovalEvidence []reviewClassificationEvidence `json:"ambiguous_approval_evidence"`
		RequestedChanges          []reviewClassificationEvidence `json:"requested_changes"`
	} `json:"formal"`
	BoundaryEvidence struct {
		Request struct {
			HeadSHA      string `json:"head_sha"`
			DeadlineAt   string `json:"deadline_at"`
			DeadlineUnix int    `json:"deadline_unix_seconds"`
		} `json:"request"`
		Sources struct {
			TopLevel      []reviewClassificationEvidence `json:"top_level"`
			FormalReviews []reviewClassificationEvidence `json:"formal_reviews"`
			Inline        []reviewClassificationEvidence `json:"inline_comments"`
		} `json:"sources"`
	} `json:"boundary_evidence"`
}

func decodeReviewClassification(t *testing.T, result reviewWaitResult) reviewClassification {
	t.Helper()
	var evidence struct {
		Classification reviewClassification `json:"classification"`
	}
	if err := json.Unmarshal(result.Evidence, &evidence); err != nil {
		t.Fatalf("decode classification evidence: %v\n%s", err, result.Evidence)
	}
	return evidence.Classification
}

func writeReviewWaitRequest(t *testing.T, path, triggerID, startedAt, deadlineAt string) {
	writeReviewWaitRequestValues(t, path, triggerID, startedAt, deadlineAt, 4102443000, 4102444800, 1800)
}

func writeReviewWaitRequestValues(t *testing.T, path, triggerID, startedAt, deadlineAt string, startedUnix, deadlineUnix, timeout int) {
	writeReviewWaitRequestValuesWithPrefix(t, path, triggerID, startedAt, deadlineAt, startedUnix, deadlineUnix, timeout, "/sandman review")
}

func writeReviewWaitRequestValuesWithPrefix(t *testing.T, path, triggerID, startedAt, deadlineAt string, startedUnix, deadlineUnix, timeout int, triggerPrefix string) {
	t.Helper()
	request := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 42,
  "head_sha": "abc123",
  "trigger_id": "` + triggerID + `",
  "trigger_prefix": "` + triggerPrefix + `",
  "trigger_created_at": "` + startedAt + `",
  "confirmed_at": "` + startedAt + `",
  "started_at": "` + startedAt + `",
  "deadline_at": "` + deadlineAt + `",
  "started_unix_seconds": ` + strconv.Itoa(startedUnix) + `,
  "deadline_unix_seconds": ` + strconv.Itoa(deadlineUnix) + `,
  "effective_timeout_seconds": ` + strconv.Itoa(timeout) + `,
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

func writeStructuredReviewObserver(t *testing.T, result string) string {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		t.Fatalf("decode observer result: %v", err)
	}
	snapshot, ok := envelope["snapshot"].(map[string]any)
	if !ok {
		t.Fatalf("observer result snapshot is not an object: %s", result)
	}
	snapshot["classification"] = map[string]any{
		"protocol":          "review-classification/v1",
		"request":           map[string]any{"repository": "owner/repo", "pull_request": 42, "head_sha": "abc123", "trigger_id": "1001", "trigger_prefix": "/sandman review", "trigger_created_at": "2026-08-11T18:00:01Z", "deadline_at": "2026-08-11T18:30:01Z", "deadline_unix_seconds": 4102444800},
		"observed_head_sha": "abc123",
		"request_state":     "active",
		"decision":          "responded",
		"window":            map[string]any{"start": "2026-08-11T18:00:01Z", "end": nil, "deadline_at": "2026-08-11T18:30:01Z", "deadline_unix_seconds": 4102444800, "next_trigger": nil},
		"response_counts":   map[string]any{"top_level": 1, "formal_reviews": 0, "inline_comments": 0},
		"sources":           map[string]any{"top_level": []any{map[string]any{"id": "1002", "source": "top_level", "response_timestamp": "2026-08-11T18:01:00Z", "head_status": "current", "body": "LGTM"}}, "formal_reviews": []any{}, "inline_comments": []any{}},
		"formal":            map[string]any{"decision": "none", "approval_evidence": []any{}, "ambiguous_approval_evidence": []any{}, "requested_changes": []any{}},
		"boundary_evidence": map[string]any{"request": map[string]any{"repository": "owner/repo", "pull_request": 42, "head_sha": "abc123", "trigger_id": "1001", "trigger_prefix": "/sandman review", "trigger_created_at": "2026-08-11T18:00:01Z", "deadline_at": "2026-08-11T18:30:01Z", "deadline_unix_seconds": 4102444800}, "sources": map[string]any{"top_level": []any{map[string]any{"id": "1002", "source": "top_level", "response_timestamp": "2026-08-11T18:01:00Z", "head_status": "current", "body": "LGTM"}}, "formal_reviews": []any{}, "inline_comments": []any{}}},
	}
	updated, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode observer result: %v", err)
	}
	return writeReviewObserver(t, string(updated))
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

func TestReviewWaitV1ChargesObserverOverheadAndExpiresAtExactDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:100", 80, 100, 20)
	clock := filepath.Join(t.TempDir(), "clock.sh")
	clockState := filepath.Join(t.TempDir(), "clock.state")
	if err := os.WriteFile(clockState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write clock state: %v", err)
	}
	clockScript := `#!/bin/sh
calls=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_CLOCK_STATE")
if [ "$calls" = "0" ]; then
  value=90
else
  value=100
fi
printf '%s\n' "$value"
printf '%s\n' $((calls + 1)) > "$SANDMAN_REVIEW_WAIT_CLOCK_STATE"
`
	if err := os.WriteFile(clock, []byte(clockScript), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, writePendingReviewObserver(t), true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK":       clock,
		"SANDMAN_REVIEW_WAIT_CLOCK_STATE": clockState,
	})
	replayed := runReviewWait(t, helper, requestFile, filepath.Join(t.TempDir(), "missing-observer"), true)

	if result.State != "timed_out" {
		t.Fatalf("state = %q, want timed_out after observer overhead reaches deadline", result.State)
	}
	if result.Elapsed != 20 {
		t.Fatalf("elapsed_seconds = %d, want 20", result.Elapsed)
	}
	if !strings.Contains(string(result.Evidence), `"response_counts":{"top_level":0,"formal_reviews":0,"inline_comments":0}`) {
		t.Fatalf("timeout evidence missing response counters: %s", result.Evidence)
	}
	if replayed.State != "timed_out" || replayed.Elapsed != result.Elapsed {
		t.Fatalf("terminal replay = %q/%d, want timed_out/%d", replayed.State, replayed.Elapsed, result.Elapsed)
	}
	state, err := os.ReadFile(requestFile + ".state")
	if err != nil {
		t.Fatalf("read timeout state: %v", err)
	}
	if !strings.Contains(string(state), `"elapsed_seconds":20`) {
		t.Fatalf("timeout state missing elapsed diagnostics: %s", state)
	}
	if !strings.Contains(string(state), `"response_counts":{"top_level":0,"formal_reviews":0,"inline_comments":0}`) {
		t.Fatalf("timeout state missing response counters: %s", state)
	}
}

func TestReviewWaitV1PreservesRespondedEvidenceAtExactDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	clock := filepath.Join(t.TempDir(), "clock.sh")
	clockState := filepath.Join(t.TempDir(), "clock.state")
	if err := os.WriteFile(clockState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write clock state: %v", err)
	}
	clockScript := `#!/bin/sh
calls=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_CLOCK_STATE")
if [ "$calls" -lt 4 ]; then
  value=4102444799
else
  value=4102444800
fi
printf '%s\n' "$value"
printf '%s\n' $((calls + 1)) > "$SANDMAN_REVIEW_WAIT_CLOCK_STATE"
`
	if err := os.WriteFile(clock, []byte(clockScript), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}
	sleeper := filepath.Join(t.TempDir(), "sleeper.sh")
	if err := os.WriteFile(sleeper, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write sleeper: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, writeStructuredReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`), true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK":       clock,
		"SANDMAN_REVIEW_WAIT_CLOCK_STATE": clockState,
		"SANDMAN_REVIEW_WAIT_SLEEP":       sleeper,
	})

	if result.State != "responded" {
		t.Fatalf("state = %q, want responded at inclusive deadline", result.State)
	}
	if !strings.Contains(string(result.Evidence), "LGTM") {
		t.Fatalf("responded evidence lost at deadline: %s", result.Evidence)
	}
}

func TestReviewWaitV1DoesNotStartObserverAfterDeadlineRace(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:100", 0, 100, 100)
	observerStarted := filepath.Join(t.TempDir(), "observer.started")
	observer := filepath.Join(t.TempDir(), "observer.sh")
	observerScript := "#!/bin/sh\nprintf '%s\\n' started >> \"" + observerStarted + "\"\nprintf '%s\\n' '{\"state\":\"pending\",\"observed_head_sha\":\"abc123\",\"snapshot\":{}}'\n"
	if err := os.WriteFile(observer, []byte(observerScript), 0o700); err != nil {
		t.Fatalf("write observer: %v", err)
	}
	clock := filepath.Join(t.TempDir(), "clock.sh")
	clockState := filepath.Join(t.TempDir(), "clock.state")
	if err := os.WriteFile(clockState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write clock state: %v", err)
	}
	clockScript := `#!/bin/sh
calls=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_CLOCK_STATE")
if [ "$calls" = "0" ]; then
  value=99
else
  value=100
fi
printf '%s\n' "$value"
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
		t.Fatalf("state = %q, want timed_out at observer launch boundary", result.State)
	}
	if _, err := os.Stat(observerStarted); !os.IsNotExist(err) {
		t.Fatalf("observer started after deadline race, stat error = %v", err)
	}
}

func TestReviewWaitV1DoesNotStartSleepAfterDeadlineRace(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:100", 0, 100, 100)
	clock := filepath.Join(t.TempDir(), "clock.sh")
	clockState := filepath.Join(t.TempDir(), "clock.state")
	if err := os.WriteFile(clockState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write clock state: %v", err)
	}
	clockScript := `#!/bin/sh
calls=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_CLOCK_STATE")
if [ "$calls" -lt 4 ]; then
  value=99
else
  value=100
fi
printf '%s\n' "$value"
printf '%s\n' $((calls + 1)) > "$SANDMAN_REVIEW_WAIT_CLOCK_STATE"
`
	if err := os.WriteFile(clock, []byte(clockScript), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}
	sleepCalls := filepath.Join(t.TempDir(), "sleep.calls")
	sleeper := filepath.Join(t.TempDir(), "sleeper.sh")
	sleeperScript := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"" + sleepCalls + "\"\nexit 0\n"
	if err := os.WriteFile(sleeper, []byte(sleeperScript), 0o700); err != nil {
		t.Fatalf("write sleeper: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, writePendingReviewObserver(t), false, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK":       clock,
		"SANDMAN_REVIEW_WAIT_CLOCK_STATE": clockState,
		"SANDMAN_REVIEW_WAIT_SLEEP":       sleeper,
	})

	if result.State != "timed_out" {
		t.Fatalf("state = %q, want timed_out at sleep launch boundary", result.State)
	}
	if calls, err := os.ReadFile(sleepCalls); err == nil {
		entries := strings.Fields(string(calls))
		if len(entries) > 1 {
			t.Fatalf("wait sleeper started after deadline race: %q", calls)
		}
	}
}

func TestReviewObserveV1DoesNotStartNextAPICallAfterDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	observer := filepath.Join(wd, "pr-review", "review-observe-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:100", 0, 100, 100)

	bin := t.TempDir()
	callsFile := filepath.Join(t.TempDir(), "gh.calls")
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
printf '%s %s\n' "$1" "$2" >> "$SANDMAN_TEST_GH_CALLS"
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}],"reviewDecision":"","mergeStateStatus":"CLEAN"}'
  printf '%s\n' 100 > "$SANDMAN_REVIEW_WAIT_CLOCK_STATE"
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
	clock := filepath.Join(t.TempDir(), "clock.sh")
	clockState := filepath.Join(t.TempDir(), "clock.state")
	if err := os.WriteFile(clockState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write clock state: %v", err)
	}
	clockScript := `#!/bin/sh
calls=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_CLOCK_STATE")
if [ "$calls" -ge 100 ]; then value=100; else value=99; fi
printf '%s\n' "$value"
printf '%s\n' $((calls + 1)) > "$SANDMAN_REVIEW_WAIT_CLOCK_STATE"
`
	if err := os.WriteFile(clock, []byte(clockScript), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH":                            bin + ":" + os.Getenv("PATH"),
		"SANDMAN_REVIEW_WAIT_CLOCK":       clock,
		"SANDMAN_REVIEW_WAIT_CLOCK_STATE": clockState,
		"SANDMAN_TEST_GH_CALLS":           callsFile,
	})
	if result.State != "timed_out" {
		t.Fatalf("state = %q, want timed_out when deadline blocks next API call", result.State)
	}
	calls, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatalf("read gh calls: %v", err)
	}
	if got, want := strings.TrimSpace(string(calls)), "pr view"; got != want {
		t.Fatalf("GitHub calls = %q, want only %q before deadline", got, want)
	}
}

func TestReviewWaitV1PreservesCadenceAndShortensFinalInterval(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:270", 0, 270, 270)
	clock := filepath.Join(t.TempDir(), "clock.sh")
	sleeper := filepath.Join(t.TempDir(), "sleeper.sh")
	timeState := filepath.Join(t.TempDir(), "time.state")
	sleepCalls := filepath.Join(t.TempDir(), "sleep.calls")
	if err := os.WriteFile(timeState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write time state: %v", err)
	}
	clockScript := `#!/bin/sh
tr -d '\n' < "$SANDMAN_REVIEW_WAIT_TIME_STATE"
printf '\n'
`
	if err := os.WriteFile(clock, []byte(clockScript), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}
	sleeperScript := `#!/bin/sh
if [ "$1" = "1" ]; then
  exit 0
fi
current=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_TIME_STATE")
printf '%s\n' $((current + $1)) > "$SANDMAN_REVIEW_WAIT_TIME_STATE"
printf '%s\n' "$1" >> "$SANDMAN_REVIEW_WAIT_SLEEP_CALLS"
`
	if err := os.WriteFile(sleeper, []byte(sleeperScript), 0o700); err != nil {
		t.Fatalf("write sleeper: %v", err)
	}

	first := runReviewWaitWithEnv(t, helper, requestFile, writePendingReviewObserver(t), false, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK":       clock,
		"SANDMAN_REVIEW_WAIT_SLEEP":       sleeper,
		"SANDMAN_REVIEW_WAIT_TIME_STATE":  timeState,
		"SANDMAN_REVIEW_WAIT_SLEEP_CALLS": sleepCalls,
	})

	if first.State != "timed_out" || first.Elapsed != 270 {
		t.Fatalf("full cadence result = %q/%d, want timed_out/270", first.State, first.Elapsed)
	}
	calls, err := os.ReadFile(sleepCalls)
	if err != nil {
		t.Fatalf("read sleep calls: %v", err)
	}
	if got, want := strings.TrimSpace(string(calls)), "120\n60\n60\n30"; got != want {
		t.Fatalf("full sleep cadence = %q, want %q", got, want)
	}

	writeReviewWaitRequestValues(t, requestFile, "1002", "2026-08-11T19:00:01Z", "unix:250", 0, 250, 250)
	if err := os.WriteFile(timeState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("reset time state: %v", err)
	}
	if err := os.WriteFile(sleepCalls, nil, 0o600); err != nil {
		t.Fatalf("reset sleep calls: %v", err)
	}
	second := runReviewWaitWithEnv(t, helper, requestFile, writePendingReviewObserver(t), false, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK":       clock,
		"SANDMAN_REVIEW_WAIT_SLEEP":       sleeper,
		"SANDMAN_REVIEW_WAIT_TIME_STATE":  timeState,
		"SANDMAN_REVIEW_WAIT_SLEEP_CALLS": sleepCalls,
	})
	if second.State != "timed_out" || second.Elapsed != 250 {
		t.Fatalf("shortened cadence result = %q/%d, want timed_out/250", second.State, second.Elapsed)
	}
	calls, err = os.ReadFile(sleepCalls)
	if err != nil {
		t.Fatalf("read shortened sleep calls: %v", err)
	}
	if got, want := strings.TrimSpace(string(calls)), "120\n60\n60\n10"; got != want {
		t.Fatalf("shortened sleep cadence = %q, want %q", got, want)
	}
}

func TestReviewWaitV1ChargesSnapshotPersistenceBeforePendingResult(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:100", 0, 100, 100)
	timeState := filepath.Join(t.TempDir(), "time.state")
	if err := os.WriteFile(timeState, []byte("90\n"), 0o600); err != nil {
		t.Fatalf("write time state: %v", err)
	}
	clock := filepath.Join(t.TempDir(), "clock.sh")
	if err := os.WriteFile(clock, []byte("#!/bin/sh\ntr -d '\\n' < \"$SANDMAN_REVIEW_WAIT_TIME_STATE\"\nprintf '\\n'\n"), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatalf("find mv: %v", err)
	}
	bin := t.TempDir()
	mv := filepath.Join(bin, "mv")
	mvScript := "#!/bin/sh\ncase \"$2\" in\n  *.snapshot.json.tmp.*) printf '%s\\n' 100 > \"$SANDMAN_REVIEW_WAIT_TIME_STATE\" ;;\nesac\nexec \"" + realMV + "\" \"$@\"\n"
	if err := os.WriteFile(mv, []byte(mvScript), 0o700); err != nil {
		t.Fatalf("write mv wrapper: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, writePendingReviewObserver(t), true, map[string]string{
		"PATH":                           bin + ":" + os.Getenv("PATH"),
		"SANDMAN_REVIEW_WAIT_CLOCK":      clock,
		"SANDMAN_REVIEW_WAIT_TIME_STATE": timeState,
	})

	if result.State != "timed_out" || result.Elapsed != 100 {
		t.Fatalf("post-persistence result = %q/%d, want timed_out/100", result.State, result.Elapsed)
	}
}

func TestReviewWaitV1ChargesStatePersistenceBeforePendingResult(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:100", 0, 100, 100)
	timeState := filepath.Join(t.TempDir(), "time.state")
	if err := os.WriteFile(timeState, []byte("90\n"), 0o600); err != nil {
		t.Fatalf("write time state: %v", err)
	}
	clock := filepath.Join(t.TempDir(), "clock.sh")
	if err := os.WriteFile(clock, []byte("#!/bin/sh\ntr -d '\\n' < \"$SANDMAN_REVIEW_WAIT_TIME_STATE\"\nprintf '\\n'\n"), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatalf("find mv: %v", err)
	}
	bin := t.TempDir()
	mv := filepath.Join(bin, "mv")
	mvScript := "#!/bin/sh\ncase \"$2\" in\n  *.state.tmp.*) printf '%s\\n' 100 > \"$SANDMAN_REVIEW_WAIT_TIME_STATE\" ;;\nesac\nexec \"" + realMV + "\" \"$@\"\n"
	if err := os.WriteFile(mv, []byte(mvScript), 0o700); err != nil {
		t.Fatalf("write mv wrapper: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, writePendingReviewObserver(t), true, map[string]string{
		"PATH":                           bin + ":" + os.Getenv("PATH"),
		"SANDMAN_REVIEW_WAIT_CLOCK":      clock,
		"SANDMAN_REVIEW_WAIT_TIME_STATE": timeState,
	})

	if result.State != "timed_out" || result.Elapsed != 100 {
		t.Fatalf("post-state-persistence result = %q/%d, want timed_out/100", result.State, result.Elapsed)
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

func TestReviewWaitV1SameTriggerReentryKeepsOriginalDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:300", 100, 300, 200)
	observer := writePendingReviewObserver(t)
	clock := filepath.Join(t.TempDir(), "clock.sh")
	if err := os.WriteFile(clock, []byte("#!/bin/sh\nprintf '%s\\n' 150\n"), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}

	first := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK": clock,
	})
	second := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK": clock,
	})

	if first.Lifecycle != "started" || second.Lifecycle != "resumed" {
		t.Fatalf("lifecycles = %q then %q, want started then resumed", first.Lifecycle, second.Lifecycle)
	}
	if first.StartedAt != second.StartedAt || first.DeadlineAt != second.DeadlineAt {
		t.Fatalf("same-trigger re-entry changed timestamps: first (%q, %q), second (%q, %q)", first.StartedAt, first.DeadlineAt, second.StartedAt, second.DeadlineAt)
	}
	if first.Request.TimeoutSeconds != 200 || second.Request.TimeoutSeconds != 200 {
		t.Fatalf("same-trigger budget changed: first %d, second %d", first.Request.TimeoutSeconds, second.Request.TimeoutSeconds)
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

func TestReviewWaitV1LaterTriggerReceivesFullFreshBudget(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	observer := writePendingReviewObserver(t)
	clock := filepath.Join(t.TempDir(), "clock.sh")
	if err := os.WriteFile(clock, []byte("#!/bin/sh\nprintf '%s\\n' 500\n"), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}

	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:300", 100, 300, 200)
	first := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK": clock,
	})

	writeReviewWaitRequestValues(t, requestFile, "1002", "2026-08-11T19:00:01Z", "unix:700", 500, 700, 200)
	second := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK": clock,
	})

	if first.Request.TriggerID != "1001" || second.Request.TriggerID != "1002" {
		t.Fatalf("triggers = %q then %q, want 1001 then 1002", first.Request.TriggerID, second.Request.TriggerID)
	}
	if second.Lifecycle != "started" || second.Request.TimeoutSeconds != 200 {
		t.Fatalf("later trigger = lifecycle %q/budget %d, want started/200", second.Lifecycle, second.Request.TimeoutSeconds)
	}
	if second.Request.DeadlineUnix-second.Request.StartedUnix != second.Request.TimeoutSeconds {
		t.Fatalf("later trigger deadline = %d-%d, want full budget %d", second.Request.DeadlineUnix, second.Request.StartedUnix, second.Request.TimeoutSeconds)
	}
	if second.Elapsed != 0 {
		t.Fatalf("later trigger elapsed_seconds = %d, want fresh zero elapsed", second.Elapsed)
	}
}

func TestReviewWaitV1RejectsDeadlineArithmeticMismatch(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:300", 100, 300, 200)
	request, err := os.ReadFile(requestFile)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	request = []byte(strings.Replace(string(request), `"deadline_unix_seconds": 300`, `"deadline_unix_seconds": 301`, 1))
	if err := os.WriteFile(requestFile, request, 0o600); err != nil {
		t.Fatalf("write malformed request: %v", err)
	}

	result := runReviewWait(t, helper, requestFile, writePendingReviewObserver(t), true)
	if result.State != "unavailable" || result.Reason != "request-envelope-invalid" {
		t.Fatalf("invalid request result = %q/%q, want unavailable/request-envelope-invalid", result.State, result.Reason)
	}
	if _, err := os.Stat(requestFile + ".state"); !os.IsNotExist(err) {
		t.Fatalf("invalid request must not create state sidecar, stat error = %v", err)
	}
}

func TestReviewWaitV1AlwaysEmitsStructuredUnavailableForMalformedRequestObject(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	if err := os.WriteFile(requestFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write malformed request: %v", err)
	}

	result := runReviewWait(t, helper, requestFile, filepath.Join(t.TempDir(), "missing-observer"), true)
	if result.Protocol != "review-wait/v1" || result.State != "unavailable" || result.Reason != "request-envelope-invalid" {
		t.Fatalf("malformed request result = protocol %q/state %q/reason %q, want review-wait/v1/unavailable/request-envelope-invalid", result.Protocol, result.State, result.Reason)
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
	observer := writeStructuredReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)

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

func TestReviewWaitV1RejectsRespondedEvidenceWithoutClassification(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := writeReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)

	result := runReviewWait(t, helper, requestFile, observer, true)
	if result.State != "unavailable" || result.Reason != "observer-classification-invalid" {
		t.Fatalf("missing classification result = %q/%q, want unavailable/observer-classification-invalid", result.State, result.Reason)
	}
}

func TestReviewWaitV1RejectsInconsistentClassification(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := writeStructuredReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)
	data, err := os.ReadFile(observer)
	if err != nil {
		t.Fatalf("read observer: %v", err)
	}
	updated := strings.Replace(string(data), `"decision":"responded"`, `"decision":"approved"`, 1)
	if updated == string(data) {
		t.Fatal("structured observer fixture did not contain its decision")
	}
	if err := os.WriteFile(observer, []byte(updated), 0o700); err != nil {
		t.Fatalf("write malformed observer: %v", err)
	}

	result := runReviewWait(t, helper, requestFile, observer, true)
	if result.State != "unavailable" || result.Reason != "observer-classification-invalid" {
		t.Fatalf("inconsistent classification result = %q/%q, want unavailable/observer-classification-invalid", result.State, result.Reason)
	}
}

func TestReviewWaitV1RejectsSyntheticFormalApprovalEvidence(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := writeStructuredReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)
	data, err := os.ReadFile(observer)
	if err != nil {
		t.Fatalf("read observer: %v", err)
	}
	syntheticSource := `{"id":2001,"state":"COMMENTED","response_timestamp":"2026-08-11T18:02:00Z","head_status":"unknown","source":"formal_review"}`
	syntheticApproval := `{"id":999,"state":"APPROVED","response_timestamp":"2026-08-11T18:02:00Z","head_status":"current","source":"formal_review"}`
	updated := string(data)
	updated = strings.Replace(updated, `"formal_reviews":[]`, `"formal_reviews":[`+syntheticSource+`]`, 1)
	updated = strings.Replace(updated, `"formal_reviews":0`, `"formal_reviews":1`, 1)
	updated = strings.Replace(updated, `"approval_evidence":[]`, `"approval_evidence":[`+syntheticApproval+`]`, 1)
	updated = strings.Replace(updated, `"decision":"none"`, `"decision":"approved"`, 1)
	updated = strings.Replace(updated, `"decision":"responded"`, `"decision":"approved"`, 1)
	if updated == string(data) {
		t.Fatal("synthetic classification fixture was not changed")
	}
	if err := os.WriteFile(observer, []byte(updated), 0o700); err != nil {
		t.Fatalf("write synthetic observer: %v", err)
	}

	result := runReviewWait(t, helper, requestFile, observer, true)
	if result.State != "unavailable" || result.Reason != "observer-classification-invalid" {
		t.Fatalf("synthetic approval result = %q/%q, want unavailable/observer-classification-invalid", result.State, result.Reason)
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
	state, err := os.ReadFile(requestFile + ".state")
	if err != nil {
		t.Fatalf("read timed-out state: %v", err)
	}
	if !strings.Contains(string(state), `"response_counts":{"top_level":0,"formal_reviews":0,"inline_comments":0}`) {
		t.Fatalf("timed-out state missing explicit zero response counters: %s", state)
	}
	if second.Lifecycle != "resumed" || second.ObservedHead != "abc123" {
		t.Fatalf("timed-out reentry = lifecycle %q/head %q, want resumed/abc123", second.Lifecycle, second.ObservedHead)
	}
}

func TestExternalGate_SameTriggerReentryRetainsTimedOutRequest(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:4102443200", 4102443000, 4102443200, 200)
	observer := writePendingReviewObserver(t)
	clock := filepath.Join(t.TempDir(), "clock.sh")
	if err := os.WriteFile(clock, []byte("#!/bin/sh\nprintf '%s\\n' 4102443200\n"), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}

	first := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK": clock,
	})
	second := runReviewWaitWithEnv(t, helper, requestFile, filepath.Join(t.TempDir(), "missing-observer"), true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK": clock,
	})
	if first.State != "timed_out" || second.State != "timed_out" || second.Lifecycle != "resumed" {
		t.Fatalf("same-trigger timeout replay = %q/%q/%q, want timed_out/timed_out/resumed", first.State, second.State, second.Lifecycle)
	}
	if second.Request.TriggerID != "1001" || second.Request.DeadlineUnix != 4102443200 || second.Request.TimeoutSeconds != 200 {
		t.Fatalf("same-trigger replay request = %+v, want trigger 1001/deadline 4102443200/budget 200", second.Request)
	}
}

func TestExternalGate_LaterTriggerReceivesFreshFullBudget(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	observer := writePendingReviewObserver(t)
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:4102443200", 4102443000, 4102443200, 200)
	first := runReviewWait(t, helper, requestFile, observer, true)

	writeReviewWaitRequestValues(t, requestFile, "1002", "2026-08-11T19:00:01Z", "unix:4102444200", 4102444000, 4102444200, 200)
	second := runReviewWait(t, helper, requestFile, observer, true)
	if first.Lifecycle != "started" || second.Lifecycle != "started" {
		t.Fatalf("later-trigger lifecycles = %q/%q, want started/started", first.Lifecycle, second.Lifecycle)
	}
	if second.Request.TriggerID != "1002" || second.Request.DeadlineUnix != 4102444200 || second.Request.TimeoutSeconds != 200 {
		t.Fatalf("later-trigger request = %+v, want trigger 1002/deadline 4102444200/budget 200", second.Request)
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

func TestReviewWaitV1RejectsResponseCompletingAfterDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	responseObserver := writeStructuredReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"late"}],"reviews":[],"inline_comments":[]}}`)
	observerStarted := filepath.Join(t.TempDir(), "observer.started")
	observer := filepath.Join(t.TempDir(), "observer.sh")
	observerScript := "#!/bin/sh\nprintf '%s\\n' started > \"" + observerStarted + "\"\nexec sh \"" + responseObserver + "\"\n"
	if err := os.WriteFile(observer, []byte(observerScript), 0o700); err != nil {
		t.Fatalf("write observer wrapper: %v", err)
	}
	clock := filepath.Join(t.TempDir(), "clock.sh")
	clockState := filepath.Join(t.TempDir(), "clock.state")
	if err := os.WriteFile(clockState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write clock state: %v", err)
	}
	clockScript := `#!/bin/sh
calls=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_CLOCK_STATE")
if [ "$calls" -lt 4 ]; then value=4102444799; else value=4102444801; fi
printf '%s\n' "$value"
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
	if !strings.Contains(string(result.Evidence), `"response_counts"`) {
		t.Fatalf("late observer evidence = %s, want response counters", result.Evidence)
	}
	state, err := os.ReadFile(requestFile + ".state")
	if err != nil {
		t.Fatalf("read late observer state: %v", err)
	}
	if !strings.Contains(string(state), `"response_counts":{"top_level":0,"formal_reviews":0,"inline_comments":0}`) {
		t.Fatalf("late observer state missing response counters: %s", state)
	}
	if _, err := os.Stat(observerStarted); err != nil {
		t.Fatalf("late observer did not start before completion crossed deadline: %v", err)
	}
}

func TestReviewWaitV1PreservesRespondedEvidenceWhenPersistenceCrossesDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	timeState := filepath.Join(t.TempDir(), "time.state")
	if err := os.WriteFile(timeState, []byte("4102444799\n"), 0o600); err != nil {
		t.Fatalf("write time state: %v", err)
	}
	clock := filepath.Join(t.TempDir(), "clock.sh")
	if err := os.WriteFile(clock, []byte("#!/bin/sh\ntr -d '\\n' < \"$SANDMAN_REVIEW_WAIT_TIME_STATE\"\nprintf '\\n'\n"), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatalf("find mv: %v", err)
	}
	bin := t.TempDir()
	mv := filepath.Join(bin, "mv")
	mvScript := "#!/bin/sh\ncase \"$2\" in\n  *.snapshot.json.tmp.*) printf '%s\\n' 4102444800 > \"$SANDMAN_REVIEW_WAIT_TIME_STATE\" ;;\nesac\nexec \"" + realMV + "\" \"$@\"\n"
	if err := os.WriteFile(mv, []byte(mvScript), 0o700); err != nil {
		t.Fatalf("write mv wrapper: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, writeStructuredReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`), true, map[string]string{
		"PATH":                           bin + ":" + os.Getenv("PATH"),
		"SANDMAN_REVIEW_WAIT_CLOCK":      clock,
		"SANDMAN_REVIEW_WAIT_TIME_STATE": timeState,
	})

	if result.State != "responded" || !strings.Contains(string(result.Evidence), "LGTM") {
		state, _ := os.ReadFile(requestFile + ".state")
		t.Fatalf("persistence-crossing result = %q/%q with evidence %s, state %s, want responded with original evidence", result.State, result.Reason, result.Evidence, state)
	}
	if result.Request.DeadlineUnix != 4102444800 {
		t.Fatalf("deadline = %d, want immutable request deadline", result.Request.DeadlineUnix)
	}
}

func TestReviewWaitV1PinsRequestDeadlineAcrossObserverMutation(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2026-08-11T18:00:01Z", "unix:100", 0, 100, 100)
	observer := filepath.Join(t.TempDir(), "observer.sh")
	observerScript := "#!/bin/sh\nprintf '%s\\n' '{\"protocol\":\"review-wait/v1\",\"repository\":\"owner/repo\",\"pull_request\":42,\"head_sha\":\"abc123\",\"trigger_id\":\"1001\",\"trigger_prefix\":\"/sandman review\",\"trigger_created_at\":\"2026-08-11T18:00:01Z\",\"confirmed_at\":\"2026-08-11T18:00:01Z\",\"started_at\":\"2026-08-11T18:00:01Z\",\"deadline_at\":\"unix:1000\",\"started_unix_seconds\":0,\"deadline_unix_seconds\":1000,\"effective_timeout_seconds\":1000,\"poll_plan\":[120]}' > '" + requestFile + "'\nprintf '%s\\n' '{\"state\":\"pending\",\"observed_head_sha\":\"abc123\",\"snapshot\":{}}'\n"
	if err := os.WriteFile(observer, []byte(observerScript), 0o700); err != nil {
		t.Fatalf("write observer: %v", err)
	}
	clock := filepath.Join(t.TempDir(), "clock.sh")
	clockState := filepath.Join(t.TempDir(), "clock.state")
	if err := os.WriteFile(clockState, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write clock state: %v", err)
	}
	clockScript := `#!/bin/sh
calls=$(tr -d '\n' < "$SANDMAN_REVIEW_WAIT_CLOCK_STATE")
if [ "$calls" = "0" ]; then value=99; else value=100; fi
printf '%s\n' "$value"
printf '%s\n' $((calls + 1)) > "$SANDMAN_REVIEW_WAIT_CLOCK_STATE"
`
	if err := os.WriteFile(clock, []byte(clockScript), 0o700); err != nil {
		t.Fatalf("write clock: %v", err)
	}

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK":       clock,
		"SANDMAN_REVIEW_WAIT_CLOCK_STATE": clockState,
	})
	if result.State != "timed_out" || result.Request.DeadlineUnix != 100 {
		t.Fatalf("mutated request result = %q/deadline %d, want timed_out/100", result.State, result.Request.DeadlineUnix)
	}
}

func TestReviewWaitV1ReplaysRespondedEvidenceAfterDeadline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")
	observer := writeStructuredReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)
	beforeDeadline := filepath.Join(t.TempDir(), "before-deadline.sh")
	if err := os.WriteFile(beforeDeadline, []byte("#!/bin/sh\nprintf '%s\\n' 4102444799\n"), 0o700); err != nil {
		t.Fatalf("write before-deadline clock: %v", err)
	}
	first := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK": beforeDeadline,
	})
	if first.State != "responded" {
		t.Fatalf("initial state = %q, want responded", first.State)
	}

	afterDeadline := filepath.Join(t.TempDir(), "after-deadline.sh")
	if err := os.WriteFile(afterDeadline, []byte("#!/bin/sh\nprintf '%s\\n' 4102444800\n"), 0o700); err != nil {
		t.Fatalf("write after-deadline clock: %v", err)
	}
	second := runReviewWaitWithEnv(t, helper, requestFile, filepath.Join(t.TempDir(), "missing-observer"), true, map[string]string{
		"SANDMAN_REVIEW_WAIT_CLOCK": afterDeadline,
	})
	if second.State != "responded" || second.Lifecycle != "resumed" || !strings.Contains(string(second.Evidence), "LGTM") {
		t.Fatalf("replay = %q/%q/%s, want resumed responded evidence", second.State, second.Lifecycle, second.Evidence)
	}
	if second.Request.DeadlineUnix != first.Request.DeadlineUnix {
		t.Fatalf("replay deadline = %d, initial deadline = %d", second.Request.DeadlineUnix, first.Request.DeadlineUnix)
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

	respondedObserver := writeStructuredReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)
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

	respondedObserver := writeStructuredReviewObserver(t, `{"state":"responded","observed_head_sha":"abc123","snapshot":{"comments":[{"body":"LGTM"}],"reviews":[],"inline_comments":[]}}`)
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

func TestReviewWaitV1PreservesCommitlessCommentedApprovalEvidence(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	observer := filepath.Join(wd, "pr-review", "review-observe-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2026-08-11T18:00:01Z", "2026-08-11T18:30:01Z")

	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}],"reviewDecision":"","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"COMMENTED","submitted_at":"2026-08-11T18:02:00Z","body":"LGTM"}]' ;;
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	classification := decodeReviewClassification(t, result)
	if result.State != "responded" || classification.Decision != "responded" {
		t.Fatalf("result = state %q/decision %q, want responded/responded", result.State, classification.Decision)
	}
	if len(classification.Sources.FormalReviews) != 1 {
		t.Fatalf("formal sources = %d, want one commitless COMMENTED response", len(classification.Sources.FormalReviews))
	}
	formal := classification.Sources.FormalReviews[0]
	if formal.State != "COMMENTED" || formal.HeadStatus != "unknown" || formal.Body != "LGTM" {
		t.Fatalf("commitless COMMENTED evidence = %+v, want COMMENTED/LGTM/unknown", formal)
	}
	if classification.Formal.Decision != "none" || len(classification.Formal.AmbiguousApprovalEvidence) != 0 {
		t.Fatalf("formal mechanical classification = decision %q/ambiguous %d, want none/0", classification.Formal.Decision, len(classification.Formal.AmbiguousApprovalEvidence))
	}
}

func TestReviewWaitV1ReturnsRequestScopedClassificationForAllResponseSurfaces(t *testing.T) {
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
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"IC_kwDOSYKm0c8AAAABOXLU5g","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z","author":{"login":"owner"}},{"id":"IC_kwDOSYKm0c8AAAABOXLU5w","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"/sandman review follow-up","createdAt":"2026-08-11T18:05:00Z","author":{"login":"owner"}},{"id":"IC_kwDOSYKm0c8AAAABOXLU6A","url":"https://github.com/owner/repo/pull/42#issuecomment-1003","body":"LGTM","createdAt":"2026-08-11T18:02:00Z","author":{"login":"owner"}}],"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"COMMENTED","submitted_at":"2026-08-11T18:03:00Z","body":"file.go needs a test","commit_id":"abc123","user":{"login":"owner"}}]' ;;
    */comments) printf '%s\n' '[{"id":3001,"created_at":"2026-08-11T18:04:00Z","body":"inline feedback","commit_id":"abc123","user":{"login":"owner"}}]' ;;
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

	classification := decodeReviewClassification(t, result)
	if classification.Protocol != "review-classification/v1" {
		t.Fatalf("classification protocol = %q, want review-classification/v1", classification.Protocol)
	}
	if classification.RequestState != "superseded" || classification.Decision != "pending" {
		t.Fatalf("classification state = %q/%q, want superseded/pending", classification.RequestState, classification.Decision)
	}
	if classification.Request.TriggerID != "https://github.com/owner/repo/pull/42#issuecomment-1001" || classification.Request.HeadSHA != "abc123" || classification.Request.TriggerCreatedAt != "2026-08-11T18:00:01Z" || classification.Request.DeadlineUnix != 4102444800 {
		t.Fatalf("classification request = %+v", classification.Request)
	}
	if classification.ResponseCounts.TopLevel != 1 || classification.ResponseCounts.FormalReviews != 1 || classification.ResponseCounts.Inline != 1 {
		t.Fatalf("classification counts = %+v, want one response per source", classification.ResponseCounts)
	}
	if len(classification.Sources.TopLevel) != 1 || len(classification.Sources.FormalReviews) != 1 || len(classification.Sources.Inline) != 1 {
		t.Fatalf("classification sources = top %d/formal %d/inline %d, want one each", len(classification.Sources.TopLevel), len(classification.Sources.FormalReviews), len(classification.Sources.Inline))
	}
	if classification.BoundaryEvidence.Request.HeadSHA != "abc123" || classification.BoundaryEvidence.Request.DeadlineAt != "2026-08-11T18:30:01Z" || classification.BoundaryEvidence.Request.DeadlineUnix != 4102444800 {
		t.Fatalf("boundary request = %+v", classification.BoundaryEvidence.Request)
	}
	if classification.BoundaryEvidence.Sources.TopLevel[0].ResponseTimestamp != "2026-08-11T18:02:00.000000000Z" || classification.BoundaryEvidence.Sources.FormalReviews[0].ResponseTimestamp != "2026-08-11T18:03:00.000000000Z" || classification.BoundaryEvidence.Sources.Inline[0].ResponseTimestamp != "2026-08-11T18:04:00.000000000Z" {
		t.Fatalf("boundary timestamps = top %q/formal %q/inline %q", classification.BoundaryEvidence.Sources.TopLevel[0].ResponseTimestamp, classification.BoundaryEvidence.Sources.FormalReviews[0].ResponseTimestamp, classification.BoundaryEvidence.Sources.Inline[0].ResponseTimestamp)
	}
	for _, source := range classification.Sources.TopLevel {
		if strings.Contains(source.Body, "/sandman review follow-up") {
			t.Fatalf("trigger-prefixed top-level comment was classified as a response: %+v", source)
		}
	}
}

func TestReviewWaitV1AssociatesEvidenceWithOneConfirmedRequest(t *testing.T) {
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
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"first request response","createdAt":"2026-08-11T18:01:00Z"},{"id":"1003","url":"https://github.com/owner/repo/pull/42#issuecomment-1003","body":"/sandman review","createdAt":"2026-08-11T18:02:00Z"},{"id":"1004","url":"https://github.com/owner/repo/pull/42#issuecomment-1004","body":"second request response","createdAt":"2026-08-11T18:03:00Z"}],"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
	    */reviews) printf '%s\n' '[{"id":2001,"state":"APPROVED","submitted_at":"2026-08-11T18:01:30Z","commit_id":"abc123","body":"first request approval"}]' ;;
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

	first := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	firstClassification := decodeReviewClassification(t, first)
	if first.State != "responded" || firstClassification.RequestState != "superseded" || firstClassification.Decision != "pending" || firstClassification.Formal.Decision != "approved" {
		t.Fatalf("first request = state %q/classification %q/%q, want responded/superseded/pending", first.State, firstClassification.RequestState, firstClassification.Decision)
	}
	if len(firstClassification.Sources.TopLevel) != 1 || firstClassification.Sources.TopLevel[0].Body != "first request response" {
		t.Fatalf("first request sources = %+v, want only first response", firstClassification.Sources.TopLevel)
	}

	writeReviewWaitRequest(t, requestFile, "https://github.com/owner/repo/pull/42#issuecomment-1003", "2026-08-11T18:02:00Z", "2026-08-11T18:30:01Z")
	second := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	secondClassification := decodeReviewClassification(t, second)
	if second.State != "responded" || secondClassification.RequestState != "active" || secondClassification.Decision != "responded" {
		t.Fatalf("second request = state %q/classification %q/%q, want responded/active/responded", second.State, secondClassification.RequestState, secondClassification.Decision)
	}
	if len(secondClassification.Sources.TopLevel) != 1 || secondClassification.Sources.TopLevel[0].Body != "second request response" {
		t.Fatalf("second request sources = %+v, want only second response", secondClassification.Sources.TopLevel)
	}
}

func TestReviewWaitV1ReturnsSupersededBoundaryWithoutResponseAsResponded(t *testing.T) {
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
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"/sandman review","createdAt":"2026-08-11T18:01:00Z"}],"reviewDecision":"","mergeStateStatus":"CLEAN"}'
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	classification := decodeReviewClassification(t, result)
	if result.State != "responded" || classification.RequestState != "superseded" || classification.Decision != "pending" {
		t.Fatalf("superseded empty request = state %q/request %q/decision %q, want responded/superseded/pending", result.State, classification.RequestState, classification.Decision)
	}
	if classification.ResponseCounts.TopLevel != 0 || classification.ResponseCounts.FormalReviews != 0 || classification.ResponseCounts.Inline != 0 {
		t.Fatalf("superseded empty counts = %+v, want zero response counts", classification.ResponseCounts)
	}
}

func TestReviewWaitV1PreservesRequestedChangesPrecedenceOverStaleApproval(t *testing.T) {
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
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}],"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"APPROVED","submitted_at":"2026-08-11T18:01:00Z","body":"old approval","commit_id":"oldsha"},{"id":2002,"state":"CHANGES_REQUESTED","submitted_at":"2026-08-11T18:02:00Z","body":"please fix","commit_id":"oldsha"},{"id":2003,"state":"APPROVED","submitted_at":"2026-08-11T18:03:00Z","body":"current approval","commit_id":"abc123"}]' ;;
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	classification := decodeReviewClassification(t, result)
	if result.State != "responded" || classification.Decision != "changes_requested" || classification.Formal.Decision != "changes_requested" {
		t.Fatalf("result = state %q/classification %q/formal %q, want responded/changes_requested/changes_requested", result.State, classification.Decision, classification.Formal.Decision)
	}
	if len(classification.Formal.RequestedChanges) != 1 || len(classification.Formal.ApprovalEvidence) != 1 || len(classification.Formal.AmbiguousApprovalEvidence) != 1 {
		t.Fatalf("formal evidence = changes %d/current approvals %d/ambiguous approvals %d, want 1/1/1", len(classification.Formal.RequestedChanges), len(classification.Formal.ApprovalEvidence), len(classification.Formal.AmbiguousApprovalEvidence))
	}
	if classification.Formal.ApprovalEvidence[0].HeadStatus != "current" || classification.Formal.AmbiguousApprovalEvidence[0].HeadStatus != "stale" {
		t.Fatalf("approval head statuses = current %q/stale %q", classification.Formal.ApprovalEvidence[0].HeadStatus, classification.Formal.AmbiguousApprovalEvidence[0].HeadStatus)
	}
}

func TestReviewWaitV1FailsClosedOnStaleOrUnknownApprovalEvidence(t *testing.T) {
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
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}],"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"APPROVED","submitted_at":"2026-08-11T18:01:00Z","body":"old approval","commit_id":"oldsha"},{"id":2002,"state":"APPROVED","submitted_at":"2026-08-11T18:02:00Z","body":"unattributed approval"}]' ;;
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	classification := decodeReviewClassification(t, result)
	if result.State != "responded" || classification.Decision != "pending" || classification.Formal.Decision != "ambiguous" {
		t.Fatalf("result = state %q/classification %q/formal %q, want responded/pending/ambiguous", result.State, classification.Decision, classification.Formal.Decision)
	}
	if len(classification.Formal.ApprovalEvidence) != 0 || len(classification.Formal.AmbiguousApprovalEvidence) != 2 {
		t.Fatalf("approval evidence = current %d/ambiguous %d, want 0/2", len(classification.Formal.ApprovalEvidence), len(classification.Formal.AmbiguousApprovalEvidence))
	}
}

func TestReviewWaitV1IgnoresInvalidResponseTimestampsAsPending(t *testing.T) {
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
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"not timestamped","createdAt":"not-a-timestamp"}],"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[]' ;;
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	classification := decodeReviewClassification(t, result)
	if result.State != "pending" || classification.Decision != "pending" || classification.ResponseCounts.TopLevel != 0 {
		t.Fatalf("result = state %q/decision %q/top %d, want pending/pending/0", result.State, classification.Decision, classification.ResponseCounts.TopLevel)
	}
}

func TestReviewWaitV1RejectsResponseFromAStaleHead(t *testing.T) {
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
  printf '%s\n' '{"headRefOid":"new-head","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}],"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"APPROVED","submitted_at":"2026-08-11T18:01:00Z","commit_id":"abc123"}]' ;;
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	if result.State != "unavailable" || result.Reason != "head-mismatch" {
		t.Fatalf("stale-head result = %q/%q, want unavailable/head-mismatch", result.State, result.Reason)
	}
}

func TestReviewWaitV1IncludesExactDeadlineResponsesAndUsesConfiguredTriggerPrefix(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	observer := filepath.Join(wd, "pr-review", "review-observe-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValuesWithPrefix(t, requestFile, "1001", "2099-12-31T23:30:00Z", "2100-01-01T00:00:00Z", 4102443000, 4102444800, 1800, "/custom review")

	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/custom review","createdAt":"2099-12-31T23:30:00Z","author":{"login":"reviewer"}},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"LGTM","createdAt":"2100-01-01T00:00:00.000Z","author":{"login":"reviewer"}},{"id":"1003","url":"https://github.com/owner/repo/pull/42#issuecomment-1003","body":"/sandman review is not configured here","createdAt":"2099-12-31T23:59:59Z","author":{"login":"reviewer"}},{"id":"1004","url":"https://github.com/owner/repo/pull/42#issuecomment-1004","body":"late top-level response","createdAt":"2100-01-01T00:00:01Z","author":{"login":"reviewer"}}],"reviewDecision":"CHANGES_REQUESTED","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"APPROVED","submitted_at":"2100-01-01T00:00:00Z","commit_id":"abc123","body":"approved at the deadline"},{"id":2002,"state":"CHANGES_REQUESTED","submitted_at":"2100-01-01T00:00:01Z","commit_id":"abc123","body":"late changes"}]' ;;
    */comments) printf '%s\n' '[{"id":3001,"created_at":"2100-01-01T00:00:00.000Z","body":"inline response at the deadline","commit_id":"abc123"},{"id":3002,"created_at":"2100-01-01T00:00:01Z","body":"late inline response","commit_id":"abc123"}]' ;;
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
	classification := decodeReviewClassification(t, result)
	if result.State != "responded" || classification.Decision != "approved" {
		t.Fatalf("result = state %q/decision %q, want responded/approved", result.State, classification.Decision)
	}
	if classification.ResponseCounts.TopLevel != 2 || classification.ResponseCounts.FormalReviews != 1 || classification.ResponseCounts.Inline != 1 {
		t.Fatalf("response counts = %+v, want top-level 2/formal 1/inline 1", classification.ResponseCounts)
	}
	if len(classification.Formal.ApprovalEvidence) != 1 || classification.Formal.ApprovalEvidence[0].ResponseTimestamp != "2100-01-01T00:00:00.000000000Z" {
		t.Fatalf("deadline approval evidence = %+v", classification.Formal.ApprovalEvidence)
	}
	if !strings.Contains(string(result.Evidence), "late changes") || !strings.Contains(string(result.Evidence), "late inline response") {
		t.Fatalf("raw snapshot lost post-deadline audit records: %s", result.Evidence)
	}
}

func TestReviewWaitV1PreservesFractionalDeadlineOrdering(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	observer := filepath.Join(wd, "pr-review", "review-observe-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "https://github.com/owner/repo/pull/42#issuecomment-1001", "2099-12-31T23:59:59.999Z", "2100-01-01T00:00:00Z", 4102443000, 4102444800, 1800)

	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2099-12-31T23:59:59.999Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"response exactly at deadline","createdAt":"2100-01-01T00:00:00Z"}],"reviewDecision":"","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"APPROVED","submitted_at":"2100-01-01T00:00:00.001Z","commit_id":"abc123"}]' ;;
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	classification := decodeReviewClassification(t, result)
	if result.State != "responded" || classification.Decision != "responded" {
		t.Fatalf("fractional boundary result = state %q/decision %q, want responded/responded", result.State, classification.Decision)
	}
	if classification.ResponseCounts.TopLevel != 1 || classification.ResponseCounts.FormalReviews != 0 {
		t.Fatalf("fractional boundary counts = %+v, want one top-level and no formal response", classification.ResponseCounts)
	}
}

func TestReviewWaitV1FailsClosedOnMalformedResponsePayload(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	observer := filepath.Join(wd, "pr-review", "review-observe-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2099-12-31T23:30:00Z", "2100-01-01T00:00:00Z")

	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2099-12-31T23:30:00Z"}],"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
		case "$2" in
			*/reviews) printf '%s\n' '[{"id":2001,"state":"APPROVED","submitted_at":"2099-12-31T23:31:00Z","commit_id":"abc123"},null]' ;;
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	if result.State != "unavailable" || result.Reason != "formal-reviews-invalid" {
		t.Fatalf("malformed response result = %q/%q, want unavailable/formal-reviews-invalid", result.State, result.Reason)
	}
}

func TestReviewWaitV1FailsClosedOnMalformedLaterTriggerBoundary(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	observer := filepath.Join(wd, "pr-review", "review-observe-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2099-12-31T23:30:00Z", "2100-01-01T00:00:00Z")

	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2099-12-31T23:30:00Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"/sandman review","createdAt":"not-a-timestamp"},{"id":"1003","url":"https://github.com/owner/repo/pull/42#issuecomment-1003","body":"looks good","createdAt":"2099-12-31T23:31:00Z"}],"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[]' ;;
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	if result.State != "unavailable" || result.Reason != "classification-failed" {
		t.Fatalf("malformed later trigger result = %q/%q, want unavailable/classification-failed", result.State, result.Reason)
	}
}

func TestReviewWaitV1PreservesSourceTimestampPrecedenceAndCanonicalEvidence(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	observer := filepath.Join(wd, "pr-review", "review-observe-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequestValues(t, requestFile, "1001", "2099-12-31T23:30:00Z", "2100-01-01T00:01:00Z", 4102443000, 4102444860, 1860)

	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2099-12-31T23:30:00Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"top-level response","createdAt":"2100-01-01T00:00:00.1Z"}],"reviewDecision":"","mergeStateStatus":"CLEAN"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */reviews) printf '%s\n' '[{"id":2001,"state":"COMMENTED","createdAt":"2099-12-31T23:29:59Z","submitted_at":"2100-01-01T00:00:00.1234Z","body":"formal response","commit_id":"abc123"}]' ;;
    */comments) printf '%s\n' '[{"id":3001,"createdAt":"2099-12-31T23:29:59Z","created_at":"2100-01-01T00:00:00.234567891Z","body":"inline response","commit_id":"abc123"}]' ;;
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
	classification := decodeReviewClassification(t, result)
	if result.State != "responded" || classification.Decision != "responded" {
		t.Fatalf("result = state %q/decision %q, want responded/responded", result.State, classification.Decision)
	}
	if classification.ResponseCounts.TopLevel != 1 || classification.ResponseCounts.FormalReviews != 1 || classification.ResponseCounts.Inline != 1 {
		t.Fatalf("response counts = %+v, want one response per source", classification.ResponseCounts)
	}
	if got := classification.Sources.TopLevel[0].ResponseTimestamp; got != "2100-01-01T00:00:00.100000000Z" {
		t.Fatalf("top-level canonical timestamp = %q, want fractional nanoseconds", got)
	}
	if got := classification.Sources.FormalReviews[0].ResponseTimestamp; got != "2100-01-01T00:00:00.123400000Z" {
		t.Fatalf("formal canonical timestamp = %q, want submitted_at normalized to nanoseconds", got)
	}
	if got := classification.Sources.Inline[0].ResponseTimestamp; got != "2100-01-01T00:00:00.234567891Z" {
		t.Fatalf("inline canonical timestamp = %q, want created_at normalized to nanoseconds", got)
	}
}

func TestReviewWaitV1FailsClosedOnAmbiguousSameTimestampTriggerBoundary(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	helper := filepath.Join(wd, "pr-review", "review-wait-v1.sh")
	observer := filepath.Join(wd, "pr-review", "review-observe-v1.sh")
	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	writeReviewWaitRequest(t, requestFile, "1001", "2099-12-31T23:30:00Z", "2100-01-01T00:00:00Z")

	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2099-12-31T23:30:00Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"/sandman review","createdAt":"2099-12-31T23:30:00Z"},{"id":"1003","url":"https://github.com/owner/repo/pull/42#issuecomment-1003","body":"LGTM","createdAt":"2099-12-31T23:31:00Z"}],"reviewDecision":"APPROVED","mergeStateStatus":"CLEAN"}'
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

	result := runReviewWaitWithEnv(t, helper, requestFile, observer, true, map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"),
	})
	if result.State != "unavailable" || result.Reason != "classification-failed" {
		t.Fatalf("ambiguous trigger result = %q/%q, want unavailable/classification-failed", result.State, result.Reason)
	}
}
