//go:build e2e

package cmd

import (
	"context"
	"testing"
	"time"
)

type presetMatrixTestDeadline struct {
	deadline time.Time
}

func (d presetMatrixTestDeadline) Deadline() (time.Time, bool) {
	return d.deadline, !d.deadline.IsZero()
}

func TestPresetMatrixRunContext_AllowsSlowProviderWithoutUsingEntireSuiteBudget(t *testing.T) {
	for _, deadline := range []time.Time{{}, time.Now().Add(90 * time.Minute)} {
		ctx, cancel := presetMatrixRunContext(presetMatrixTestDeadline{deadline: deadline})
		got, ok := ctx.Deadline()
		remaining := time.Until(got)
		cancel()
		if !ok || remaining <= 5*time.Minute || remaining > 15*time.Minute {
			t.Fatalf("real-agent budget = %v (has deadline: %v); must exceed the failing five-minute cap and remain bounded at fifteen minutes", remaining, ok)
		}
	}
}

func TestPresetMatrixRunContext_ReservesSuiteCleanupTime(t *testing.T) {
	suiteDeadline := time.Now().Add(time.Minute)
	ctx, cancel := presetMatrixRunContext(presetMatrixTestDeadline{deadline: suiteDeadline})
	defer cancel()
	got, ok := ctx.Deadline()
	if !ok || !got.Equal(suiteDeadline.Add(-30*time.Second)) {
		t.Fatalf("run deadline = %v; want suite deadline minus thirty seconds: %v", got, suiteDeadline)
	}
}

func TestPresetMatrixRunContext_ExpiredSuiteBudgetCancelsRun(t *testing.T) {
	ctx, cancel := presetMatrixRunContext(presetMatrixTestDeadline{deadline: time.Now().Add(10 * time.Second)})
	defer cancel()
	select {
	case <-ctx.Done():
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("expired run context error = %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("run must not start when the suite has no remaining execution budget")
	}
}
