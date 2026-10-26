package batch

import (
	"testing"
	"time"
)

func TestUsageLimitAllowanceRestoresCompletedPolling(t *testing.T) {
	deadline := time.Unix(1_800_000_000, 0)
	allowance, err := restoreUsageLimitAllowance(map[string]any{
		"usage_limit_waited_seconds":        17_400,
		"usage_limit_deadline_unix_seconds": deadline.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := allowance.persistedEvidence()
	if got := evidence["usage_limit_waited_seconds"]; got != 17_400 {
		t.Fatalf("restored polling = %v, want 17400 seconds", got)
	}
	if got := evidence["usage_limit_deadline_unix_seconds"]; got != deadline.Unix() {
		t.Fatalf("restored exhaustion estimate = %v, want %d", got, deadline.Unix())
	}
	if !allowance.canWait() {
		t.Fatal("restored allowance unexpectedly exhausted")
	}
}

func TestUsageLimitAllowanceAccruesCompletedPoll(t *testing.T) {
	allowance, err := restoreUsageLimitAllowance(map[string]any{"usage_limit_waited_seconds": 0})
	if err != nil {
		t.Fatal(err)
	}
	updated := allowance.completedPoll(usageLimitPollInterval)
	if got := updated.persistedEvidence()["usage_limit_waited_seconds"]; got != 600 {
		t.Fatalf("completed poll accrued %v seconds, want 600", got)
	}
	if got := allowance.persistedEvidence()["usage_limit_waited_seconds"]; got != 0 {
		t.Fatalf("accrual mutated the prior allowance value: %v", got)
	}
}

func TestUsageLimitAllowanceDoesNotAccruePartialPoll(t *testing.T) {
	allowance, err := restoreUsageLimitAllowance(map[string]any{"usage_limit_waited_seconds": 0})
	if err != nil {
		t.Fatal(err)
	}
	updated := allowance.completedPoll(usageLimitPollInterval - time.Second)
	if got := updated.persistedEvidence()["usage_limit_waited_seconds"]; got != 0 {
		t.Fatalf("partial poll accrued %v seconds, want 0", got)
	}
}

func TestUsageLimitAllowanceReportsFiveHourExhaustion(t *testing.T) {
	allowance, err := restoreUsageLimitAllowance(map[string]any{
		"usage_limit_waited_seconds": int((usageLimitRetryWindow - usageLimitPollInterval) / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	allowance = allowance.completedPoll(usageLimitPollInterval)
	if !allowance.exhausted() || allowance.canWait() {
		t.Fatalf("five-hour allowance was not exhausted: evidence=%v", allowance.persistedEvidence())
	}
}

func TestUsageLimitAllowanceDegradesAfterInvalidAccounting(t *testing.T) {
	allowance, err := restoreUsageLimitAllowance(map[string]any{"usage_limit_waited_seconds": "untrusted"})
	if err == nil {
		t.Fatal("invalid completed-poll accounting was accepted")
	}
	if allowance.canWait() {
		t.Fatal("invalid accounting authorized another quota wait")
	}
	if allowance.allowSessionReuse(true) {
		t.Fatal("invalid accounting authorized reuse of the prior agent session")
	}
}
