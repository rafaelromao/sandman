package batch

import (
	"reflect"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
)

// TestRunVerifyPath_AllAbstainReturnsNoSignal pins the contract that
// when every oracle abstains, the run falls through to the conservative
// backstop with no `verification` payload. The conservative-backstop
// step itself lives outside RunVerifyPath; the function reports
// `VerifyNoSignal` and a nil checks slice so the orchestrator knows
// it must consult `hasBlockingOpenPR`.
func TestRunVerifyPath_AllAbstainReturnsNoSignal(t *testing.T) {
	t.Parallel()
	out, checks := RunVerifyPath(VerifyInput{
		Issue:   &github.Issue{Number: 42, Body: "No ACs, no evidence."},
		Branch:  "sandman/42",
		WorkDir: t.TempDir(),
		T2:      &fakeOracle{outcome: OracleAbstain},
		T4:      &fakeOracle{outcome: OracleAbstain},
		T1:      &fakeOracle{outcome: OracleAbstain},
		T3:      &fakeOracle{outcome: OracleAbstain},
	})
	if out != VerifyNoSignal {
		t.Errorf("VerifyOutcome = %v, want VerifyNoSignal", out)
	}
	if checks != nil {
		t.Errorf("checks = %+v, want nil when all abstain", checks)
	}
}

// TestRunVerifyPath_T1VerifiedTriggersAutoClose pins the only path
// that auto-closes the orphan PR + issue: T1 returns `Verified`. The
// T2 / T4 / T3 oracles' abstains are ignored.
func TestRunVerifyPath_T1VerifiedTriggersAutoClose(t *testing.T) {
	t.Parallel()
	out, checks := RunVerifyPath(VerifyInput{
		Issue:   &github.Issue{Number: 42, Body: "## Acceptance criteria\n\n- [ ] go test -run TestX ./internal/x/...\n"},
		Branch:  "sandman/42",
		WorkDir: t.TempDir(),
		T2:      &fakeOracle{outcome: OracleAbstain},
		T4:      &fakeOracle{outcome: OracleAbstain},
		T1:      &fakeOracle{outcome: OracleVerified, check: OracleCheck{Name: "T1", Details: map[string]any{"ran": 1}}},
		T3:      &fakeOracle{outcome: OracleAbstain},
	})
	if out != VerifyVerified {
		t.Errorf("VerifyOutcome = %v, want VerifyVerified", out)
	}
	if !reflect.DeepEqual(checks, []OracleCheck{{Name: "T1", Details: map[string]any{"ran": 1}}}) {
		t.Errorf("checks = %+v, want T1-only", checks)
	}
}

// TestRunVerifyPath_T1FailedReturnsFailed pins that a T1 `Failed`
// outcome stops the chain with `VerifyFailed` and surfaces the T1
// check in the payload. T3 never runs because T1 already gave a
// signal.
func TestRunVerifyPath_T1FailedReturnsFailed(t *testing.T) {
	t.Parallel()
	out, checks := RunVerifyPath(VerifyInput{
		Issue:   &github.Issue{Number: 42},
		Branch:  "sandman/42",
		WorkDir: t.TempDir(),
		T2:      &fakeOracle{outcome: OracleAbstain},
		T4:      &fakeOracle{outcome: OracleAbstain},
		T1:      &fakeOracle{outcome: OracleFailed, check: OracleCheck{Name: "T1", Details: map[string]any{"failed": 1}}},
		T3:      &fakeOracle{outcome: OracleAbstain},
	})
	if out != VerifyFailed {
		t.Errorf("VerifyOutcome = %v, want VerifyFailed", out)
	}
	if !reflect.DeepEqual(checks, []OracleCheck{{Name: "T1", Details: map[string]any{"failed": 1}}}) {
		t.Errorf("checks = %+v, want T1-only", checks)
	}
}

// TestRunVerifyPath_T4DefersToT1 pins that a T4 `defer-to-T1` outcome
// (APPROVED + CLEAN + green checks) is treated as OracleAbstain for
// the run summary: T1 still runs and decides.
func TestRunVerifyPath_T4DefersToT1(t *testing.T) {
	t.Parallel()
	out, _ := RunVerifyPath(VerifyInput{
		Issue:   &github.Issue{Number: 42},
		Branch:  "sandman/42",
		WorkDir: t.TempDir(),
		T2:      &fakeOracle{outcome: OracleAbstain},
		T4:      &fakeOracle{outcome: OracleDeferT1, check: OracleCheck{Name: "T4"}},
		T1:      &fakeOracle{outcome: OracleVerified, check: OracleCheck{Name: "T1"}},
		T3:      &fakeOracle{outcome: OracleAbstain},
	})
	if out != VerifyVerified {
		t.Errorf("VerifyOutcome = %v, want VerifyVerified (T4 deferred to T1 which verified)", out)
	}
}

// TestRunVerifyPath_T2RejectsSkipsRest pins that a T2 `reject` is
// treated as OracleAbstain: T2 abstains from verifying (it cannot
// prove) but signals the conservative backstop should run. The rest
// of the chain is short-circuited because there is no point running
// expensive oracles on a branch whose diff diverges from main.
func TestRunVerifyPath_T2RejectsSkipsRest(t *testing.T) {
	t.Parallel()
	t1Called := false
	out, checks := RunVerifyPath(VerifyInput{
		Issue:   &github.Issue{Number: 42},
		Branch:  "sandman/42",
		WorkDir: t.TempDir(),
		T2:      &fakeOracle{outcome: OracleReject, check: OracleCheck{Name: "T2", Details: map[string]any{"reason": "diverged"}}},
		T4:      &fakeOracle{outcome: OracleAbstain},
		T1:      &fakeOracle{outcome: OracleVerified, onCallFlag: &t1Called},
		T3:      &fakeOracle{outcome: OracleAbstain},
	})
	if out != VerifyNoSignal {
		t.Errorf("VerifyOutcome = %v, want VerifyNoSignal (T2 reject means we cannot prove)", out)
	}
	if t1Called {
		t.Errorf("T1 should not be called when T2 rejects (early-exit)")
	}
	if !reflect.DeepEqual(checks, []OracleCheck{{Name: "T2", Details: map[string]any{"reason": "diverged"}}}) {
		t.Errorf("checks = %+v, want T2-only", checks)
	}
}

// TestRunVerifyPath_T3FallsThroughWhenT1Abstains pins that T3 runs
// only when T1 abstains. When T1 produces a signal (verified or
// failed), T3 is not consulted.
func TestRunVerifyPath_T3FallsThroughWhenT1Abstains(t *testing.T) {
	t.Parallel()
	t3Called := false
	out, checks := RunVerifyPath(VerifyInput{
		Issue:   &github.Issue{Number: 42},
		Branch:  "sandman/42",
		WorkDir: t.TempDir(),
		T2:      &fakeOracle{outcome: OracleAbstain},
		T4:      &fakeOracle{outcome: OracleAbstain},
		T1:      &fakeOracle{outcome: OracleAbstain},
		T3:      &fakeOracle{outcome: OracleVerified, onCallFlag: &t3Called, check: OracleCheck{Name: "T3"}},
	})
	if out != VerifyVerified {
		t.Errorf("VerifyOutcome = %v, want VerifyVerified (T3 verified after T1 abstained)", out)
	}
	if !t3Called {
		t.Errorf("T3 should have been called when T1 abstained")
	}
	if !reflect.DeepEqual(checks, []OracleCheck{{Name: "T3"}}) {
		t.Errorf("checks = %+v, want T3-only", checks)
	}
}

// TestRunVerifyPath_RunsOraclesInOrder pins the order: T2, T4, T1,
// T3. A test fake records the call order; the function must invoke
// them in the documented sequence so a slow T1 doesn't fire before
// the cheap T4 gate.
func TestRunVerifyPath_RunsOraclesInOrder(t *testing.T) {
	t.Parallel()
	order := []string{}
	rec := func(name string) *fakeOracle {
		return &fakeOracle{outcome: OracleAbstain, onCall: &order, name: name}
	}
	_, _ = RunVerifyPath(VerifyInput{
		Issue:   &github.Issue{Number: 42},
		Branch:  "sandman/42",
		WorkDir: t.TempDir(),
		T2:      rec("T2"),
		T4:      rec("T4"),
		T1:      rec("T1"),
		T3:      rec("T3"),
	})
	want := []string{"T2", "T4", "T1", "T3"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("oracle order = %v, want %v", order, want)
	}
}

type fakeOracle struct {
	name       string
	outcome    OracleResult
	check      OracleCheck
	onCall     *[]string
	onCallFlag *bool
}

func (f *fakeOracle) Run(VerifyInput) (OracleResult, OracleCheck, error) {
	if f.onCall != nil {
		*f.onCall = append(*f.onCall, f.name)
	}
	if f.onCallFlag != nil {
		*f.onCallFlag = true
	}
	return f.outcome, f.check, nil
}
