package batch

import (
	"testing"
	"time"
)

func TestUsageLimitAllowanceRestoresCompletedPolling(t *testing.T) {
	deadline := time.Unix(1_800_000_000, 0)
	providerReset := time.Unix(1_800_001_000, 0)
	allowance, err := restoreUsageLimitAllowance(map[string]any{
		"usage_limit_waited_seconds":              17_400,
		"usage_limit_deadline_unix_seconds":       deadline.Unix(),
		"usage_limit_provider_reset_unix_seconds": providerReset.Unix(),
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
	if got := evidence["usage_limit_provider_reset_unix_seconds"]; got != providerReset.Unix() {
		t.Fatalf("restored provider reset = %v, want %d", got, providerReset.Unix())
	}
	if !allowance.canWait() {
		t.Fatal("restored allowance unexpectedly exhausted")
	}
}

func TestUsageLimitAllowanceAccruesVariableCompletedWait(t *testing.T) {
	allowance, err := restoreUsageLimitAllowance(map[string]any{"usage_limit_waited_seconds": 0})
	if err != nil {
		t.Fatal(err)
	}
	updated := allowance.completedWait(37 * time.Minute)
	if got := updated.persistedEvidence()["usage_limit_waited_seconds"]; got != 2220 {
		t.Fatalf("completed wait accrued %v seconds, want 2220", got)
	}
	if got := allowance.persistedEvidence()["usage_limit_waited_seconds"]; got != 0 {
		t.Fatalf("accrual mutated the prior allowance value: %v", got)
	}
	updated = allowance.completedWait(usageLimitPollInterval - time.Second)
	if got := updated.persistedEvidence()["usage_limit_waited_seconds"]; got != 599 {
		t.Fatalf("completed resumed remainder accrued %v seconds, want 599", got)
	}
}

func TestUsageLimitAllowanceSchedulesResetAndFallbackWaits(t *testing.T) {
	now := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		waited time.Duration
		reset  time.Time
		want   time.Duration
	}{
		{name: "provider reset", reset: now.Add(37 * time.Minute), want: 37 * time.Minute},
		{name: "absent reset uses fallback", want: usageLimitPollInterval},
		{name: "past reset uses fallback", reset: now.Add(-time.Second), want: usageLimitPollInterval},
		{name: "reset capped by remaining allowance", waited: 4 * time.Hour, reset: now.Add(2 * time.Hour), want: time.Hour},
		{name: "fallback capped by remaining allowance", waited: usageLimitRetryWindow - 3*time.Minute, want: 3 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evidence := map[string]any{"usage_limit_waited_seconds": int(tt.waited / time.Second)}
			if !tt.reset.IsZero() {
				evidence["usage_limit_provider_reset_unix_seconds"] = tt.reset.Unix()
			}
			allowance, err := restoreUsageLimitAllowance(evidence)
			if err != nil {
				t.Fatal(err)
			}
			allowance = allowance.withProbe(true)
			if got := allowance.nextWait(now); got != tt.want {
				t.Fatalf("next wait = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUsageLimitAllowanceReportsFiveHourExhaustion(t *testing.T) {
	allowance, err := restoreUsageLimitAllowance(map[string]any{
		"usage_limit_waited_seconds": int((usageLimitRetryWindow - usageLimitPollInterval) / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	allowance = allowance.completedWait(usageLimitPollInterval)
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
