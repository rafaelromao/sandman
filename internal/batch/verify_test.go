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
		Issue:     &github.Issue{Number: 42, Body: "No ACs."},
		Branch:    "sandman/42",
		WorkDir:   t.TempDir(),
		PreFilter: &fakeOracle{outcome: OracleAbstain},
		CheapGate: &fakeOracle{outcome: OracleAbstain},
		Decision:  &fakeOracle{outcome: OracleAbstain},
	})
	if out != VerifyNoSignal {
		t.Errorf("VerifyOutcome = %v, want VerifyNoSignal", out)
	}
	if checks != nil {
		t.Errorf("checks = %+v, want nil when all abstain", checks)
	}
}

// TestRunVerifyPath_DecisionVerifiedTriggersAutoClose pins the only
// path that auto-closes the orphan PR + issue: Decision returns
// `Verified`. The PreFilter / CheapGate oracles' abstains are ignored.
func TestRunVerifyPath_DecisionVerifiedTriggersAutoClose(t *testing.T) {
	t.Parallel()
	out, checks := RunVerifyPath(VerifyInput{
		Issue:     &github.Issue{Number: 42, Body: "## Acceptance criteria\n\n- [ ] go test -run TestX ./internal/x/...\n"},
		Branch:    "sandman/42",
		WorkDir:   t.TempDir(),
		PreFilter: &fakeOracle{outcome: OracleAbstain},
		CheapGate: &fakeOracle{outcome: OracleAbstain},
		Decision:  &fakeOracle{outcome: OracleVerified, check: OracleCheck{Name: "decision", Details: map[string]any{"ran": 1}}},
	})
	if out != VerifyVerified {
		t.Errorf("VerifyOutcome = %v, want VerifyVerified", out)
	}
	if !reflect.DeepEqual(checks, []OracleCheck{{Name: "decision", Details: map[string]any{"ran": 1}}}) {
		t.Errorf("checks = %+v, want decision-only", checks)
	}
}

// TestRunVerifyPath_DecisionFailedReturnsFailed pins that a Decision
// `Failed` outcome stops the chain with `VerifyFailed` and surfaces
// the Decision check in the payload.
func TestRunVerifyPath_DecisionFailedReturnsFailed(t *testing.T) {
	t.Parallel()
	out, checks := RunVerifyPath(VerifyInput{
		Issue:     &github.Issue{Number: 42},
		Branch:    "sandman/42",
		WorkDir:   t.TempDir(),
		PreFilter: &fakeOracle{outcome: OracleAbstain},
		CheapGate: &fakeOracle{outcome: OracleAbstain},
		Decision:  &fakeOracle{outcome: OracleFailed, check: OracleCheck{Name: "decision", Details: map[string]any{"failed": 1}}},
	})
	if out != VerifyFailed {
		t.Errorf("VerifyOutcome = %v, want VerifyFailed", out)
	}
	if !reflect.DeepEqual(checks, []OracleCheck{{Name: "decision", Details: map[string]any{"failed": 1}}}) {
		t.Errorf("checks = %+v, want decision-only", checks)
	}
}

// TestRunVerifyPath_CheapGateDefersToDecision pins that a CheapGate
// `defer-to-Decision` outcome (APPROVED + CLEAN + green checks) is
// treated as OracleAbstain for the run summary: Decision still runs
// and decides.
func TestRunVerifyPath_CheapGateDefersToDecision(t *testing.T) {
	t.Parallel()
	out, _ := RunVerifyPath(VerifyInput{
		Issue:     &github.Issue{Number: 42},
		Branch:    "sandman/42",
		WorkDir:   t.TempDir(),
		PreFilter: &fakeOracle{outcome: OracleAbstain},
		CheapGate: &fakeOracle{outcome: OracleDeferDecision, check: OracleCheck{Name: "cheap-gate"}},
		Decision:  &fakeOracle{outcome: OracleVerified, check: OracleCheck{Name: "decision"}},
	})
	if out != VerifyVerified {
		t.Errorf("VerifyOutcome = %v, want VerifyVerified (CheapGate deferred to Decision which verified)", out)
	}
}

// TestRunVerifyPath_PreFilterRejectsSkipsRest pins that a PreFilter
// `reject` is treated as OracleAbstain: PreFilter abstains from
// verifying (it cannot prove) but signals the conservative backstop
// should run. The rest of the chain is short-circuited because there
// is no point running expensive oracles on a branch whose diff
// diverges from main.
func TestRunVerifyPath_PreFilterRejectsSkipsRest(t *testing.T) {
	t.Parallel()
	decisionCalled := false
	out, checks := RunVerifyPath(VerifyInput{
		Issue:     &github.Issue{Number: 42},
		Branch:    "sandman/42",
		WorkDir:   t.TempDir(),
		PreFilter: &fakeOracle{outcome: OracleReject, check: OracleCheck{Name: "pre-filter", Details: map[string]any{"reason": "diverged"}}},
		CheapGate: &fakeOracle{outcome: OracleAbstain},
		Decision:  &fakeOracle{outcome: OracleVerified, onCallFlag: &decisionCalled},
	})
	if out != VerifyNoSignal {
		t.Errorf("VerifyOutcome = %v, want VerifyNoSignal (PreFilter reject means we cannot prove)", out)
	}
	if decisionCalled {
		t.Errorf("Decision should not be called when PreFilter rejects (early-exit)")
	}
	if !reflect.DeepEqual(checks, []OracleCheck{{Name: "pre-filter", Details: map[string]any{"reason": "diverged"}}}) {
		t.Errorf("checks = %+v, want pre-filter-only", checks)
	}
}

// TestRunVerifyPath_RunsOraclesInOrder pins the order: PreFilter,
// CheapGate, Decision. A test fake records the call order; the
// function must invoke them in the documented sequence so a slow
// Decision doesn't fire before the cheap CheapGate.
func TestRunVerifyPath_RunsOraclesInOrder(t *testing.T) {
	t.Parallel()
	order := []string{}
	rec := func(name string) *fakeOracle {
		return &fakeOracle{outcome: OracleAbstain, onCall: &order, name: name}
	}
	_, _ = RunVerifyPath(VerifyInput{
		Issue:     &github.Issue{Number: 42},
		Branch:    "sandman/42",
		WorkDir:   t.TempDir(),
		PreFilter: rec("pre-filter"),
		CheapGate: rec("cheap-gate"),
		Decision:  rec("decision"),
	})
	want := []string{"pre-filter", "cheap-gate", "decision"}
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
