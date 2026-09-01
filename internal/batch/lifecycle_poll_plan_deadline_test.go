package batch

import (
	"reflect"
	"testing"
	"time"
)

func TestLifecyclePollIntervals_PersistedFloatPlanDecoding(t *testing.T) {
	s := &runSession{opts: runSessionOptions{}}
	extras := map[string]any{
		"review_request": map[string]any{
			"poll_plan": []any{float64(120), float64(60), float64(30)},
		},
	}
	got := s.lifecyclePollIntervals(extras)
	want := []time.Duration{120 * time.Second, 60 * time.Second, 30 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("poll intervals = %v, want %v", got, want)
	}
}

func TestLifecyclePollIntervals_PersistedIntPlan(t *testing.T) {
	s := &runSession{opts: runSessionOptions{}}
	extras := map[string]any{
		"review_request": map[string]any{
			"poll_plan": []int{120, 60},
		},
	}
	got := s.lifecyclePollIntervals(extras)
	want := []time.Duration{120 * time.Second, 60 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("poll intervals = %v, want %v", got, want)
	}
}

func TestLifecyclePollIntervals_MalformedFallbackUsesDefault(t *testing.T) {
	s := &runSession{opts: runSessionOptions{}}
	want := []time.Duration{120 * time.Second, 60 * time.Second, 60 * time.Second, 30 * time.Second}
	cases := []map[string]any{
		// non-integer float
		{"review_request": map[string]any{"poll_plan": []any{float64(30.5), float64(60)}}},
		// negative value
		{"review_request": map[string]any{"poll_plan": []any{float64(-10), float64(60)}}},
		// wrong element type
		{"review_request": map[string]any{"poll_plan": []any{"30", float64(60)}}},
		// empty slice
		{"review_request": map[string]any{"poll_plan": []any{}}},
		// completely wrong type
		{"review_request": map[string]any{"poll_plan": "not-a-slice"}},
	}
	for i, extras := range cases {
		got := s.lifecyclePollIntervals(extras)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d: expected fallback to default plan %v, got %v", i, want, got)
		}
	}
}

func TestLifecyclePollIntervals_NegativeIntFallback(t *testing.T) {
	s := &runSession{opts: runSessionOptions{}}
	extras := map[string]any{
		"review_request": map[string]any{"poll_plan": []int{120, -1}},
	}
	got := s.lifecyclePollIntervals(extras)
	want := []time.Duration{120 * time.Second, 60 * time.Second, 60 * time.Second, 30 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected fallback to default plan %v on negative int, got %v", want, got)
	}
}

func TestLifecyclePollIntervals_OptsOverridePersists(t *testing.T) {
	s := &runSession{opts: runSessionOptions{lifecyclePollPlan: []time.Duration{7 * time.Second}}}
	extras := map[string]any{
		"review_request": map[string]any{"poll_plan": []any{float64(999)}},
	}
	got := s.lifecyclePollIntervals(extras)
	if len(got) != 1 || got[0] != 7*time.Second {
		t.Fatalf("opts poll plan should win, got %v", got)
	}
}

func TestLifecycleDeadlineSeconds_FloatAndInt(t *testing.T) {
	if v, ok := lifecycleDeadlineSeconds(int64(1234567890)); !ok || v != 1234567890 {
		t.Fatalf("int64 deadline parsing failed: %v %v", v, ok)
	}
	if v, ok := lifecycleDeadlineSeconds(float64(1234567890)); !ok || v != 1234567890 {
		t.Fatalf("float64 integer deadline parsing failed: %v %v", v, ok)
	}
	if _, ok := lifecycleDeadlineSeconds(float64(1234567890.5)); ok {
		t.Fatalf("non-integer float should be rejected")
	}
	if _, ok := lifecycleDeadlineSeconds(float64(-5)); ok {
		t.Fatalf("negative float should be rejected")
	}
	if _, ok := lifecycleDeadlineSeconds("123"); ok {
		t.Fatalf("string deadline should be rejected")
	}
}

func TestLifecycleDeadline_ExpiredReviewTimeoutReasonAndAction(t *testing.T) {
	past := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	extras := map[string]any{
		"review_request": map[string]any{"deadline_unix_seconds": past.Unix()},
	}
	deadline, gate, ok := lifecycleDeadline(extras)
	if !ok {
		t.Fatal("expected deadline to be found")
	}
	if !deadline.Equal(past) {
		t.Fatalf("deadline = %v, want %v", deadline, past)
	}
	if gate != gateReviewTimeout {
		t.Fatalf("gate = %q, want %q", gate, gateReviewTimeout)
	}
	if g := lifecycleDeadlineReason(gate); g != reviewTimeoutReason {
		t.Fatalf("reason = %q, want %q", g, reviewTimeoutReason)
	}
	if a := lifecycleDeadlineNextAction(gate); a != reviewTimeoutNextAction {
		t.Fatalf("next_action = %q, want %q", a, reviewTimeoutNextAction)
	}
	// expired: Now is after deadline
	if time.Now().Before(deadline) {
		t.Fatal("deadline should be expired")
	}
}

func TestLifecycleDeadline_ExpiredCIWaitTimeoutReasonAndAction(t *testing.T) {
	past := time.Now().Add(-1 * time.Minute).Truncate(time.Second)
	extras := map[string]any{
		"ci_wait": map[string]any{"deadline_unix_seconds": float64(past.Unix())},
	}
	deadline, gate, ok := lifecycleDeadline(extras)
	if !ok {
		t.Fatal("expected ci deadline")
	}
	if gate != gateCIWaitTimeout {
		t.Fatalf("gate = %q, want %q", gate, gateCIWaitTimeout)
	}
	if lifecycleDeadlineReason(gate) != "CI_WAIT_TIMEOUT" {
		t.Fatalf("ci reason mismatch: %q", lifecycleDeadlineReason(gate))
	}
	expectedAction := "inspect current-head CI, repair any failing checks, and push a new pull-request head"
	if lifecycleDeadlineNextAction(gate) != expectedAction {
		t.Fatalf("next_action = %q, want %q", lifecycleDeadlineNextAction(gate), expectedAction)
	}
	if time.Now().Before(deadline) {
		t.Fatal("deadline should be expired")
	}
}

func TestLifecycleDeadline_FloatDecoding(t *testing.T) {
	past := time.Now().Add(-1 * time.Minute).Truncate(time.Second)
	// JSON decodes numbers as float64; ensure that path is exercised.
	extras := map[string]any{
		"review_request": map[string]any{"deadline_unix_seconds": float64(past.Unix())},
	}
	if _, _, ok := lifecycleDeadline(extras); !ok {
		t.Fatal("float64 deadline should be accepted")
	}
	// non-integer float should be rejected -> no deadline
	bad := map[string]any{
		"review_request": map[string]any{"deadline_unix_seconds": float64(123.5)},
	}
	if _, _, ok := lifecycleDeadline(bad); ok {
		t.Fatal("non-integer float deadline should be rejected")
	}
}
