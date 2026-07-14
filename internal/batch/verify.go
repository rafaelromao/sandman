package batch

import (
	"context"

	"github.com/rafaelromao/sandman/internal/github"
)

// OracleResult is the per-oracle verdict. The three-oracle chain in
// RunVerifyPath reduces these into a single VerifyOutcome plus the
// collected OracleCheck slice.
type OracleResult int

const (
	// OracleAbstain: the oracle cannot prove either way; the next
	// oracle in the chain runs. When all oracles abstain, the
	// conservative backstop is the only remaining signal.
	OracleAbstain OracleResult = iota
	// OracleDeferT1: T4's cheap gate sees APPROVED + CLEAN + green
	// checks and wants the decision oracle to make the final call.
	// The chain treats this like OracleAbstain for ordering: T1
	// runs after T4.
	OracleDeferT1
	// OracleVerified: the oracle produced a positive signal — the
	// change is already on main. RunVerifyPath short-circuits with
	// VerifyVerified and surfaces this oracle's check.
	OracleVerified
	// OracleFailed: the oracle produced a negative signal — the
	// change is not on main. RunVerifyPath short-circuits with
	// VerifyFailed.
	OracleFailed
	// OracleNoSignal: the oracle did not find a reason to verify
	// or fail; equivalent to OracleAbstain for the chain but kept
	// distinct so callers can tell the difference between "tried,
	// no signal" and "didn't try".
	OracleNoSignal
	// OracleReject: T2's pre-filter says the branch's diff is not
	// a subset of origin/main. T2 abstains from verifying but the
	// chain short-circuits because T1 cannot prove either.
	OracleReject
)

// OracleCheck is the per-oracle diagnostic carried in the run.finished
// payload when an oracle produces a non-abstain outcome. Details is
// a free-form map keyed by the oracle (e.g. T1: {"ran": 7}, T2:
// {"reason": "diverged"}).
type OracleCheck struct {
	Name    string
	Details map[string]any
}

// VerifyOutcome is the aggregate RunVerifyPath verdict. It maps onto
// the orchestrator's decision: VerifyVerified triggers auto-close;
// VerifyFailed maps to `failure`; VerifyNoSignal falls through to the
// conservative backstop.
type VerifyOutcome int

const (
	VerifyNoSignal VerifyOutcome = iota
	VerifyVerified
	VerifyFailed
)

// Oracle is the contract every decision oracle implements. The
// three oracles (T2 / T4 / T1) share the same shape; only their
// implementation differs. Run is invoked at most once per RunVerifyPath
// call.
type Oracle interface {
	Run(VerifyInput) (OracleResult, OracleCheck, error)
}

// VerifyInput is the per-run context every oracle reads. WorkDir is
// the working tree to use for shell-out oracles (T1); the rest is
// metadata fetched by the orchestrator before the chain runs. T1 /
// T2 / T4 are the three oracles; a nil oracle is treated as
// OracleAbstain so a test or partial deployment can elide
// individual oracles.
type VerifyInput struct {
	Context context.Context
	Issue   *github.Issue
	Branch  string
	WorkDir string
	PR      *github.PR
	T2      Oracle
	T4      Oracle
	T1      Oracle
}

// VerifyPathFunc is the seam the orchestrator uses to invoke the
// three-oracle chain. Production wiring goes through RunVerifyPath
// with the default T2 / T4 / T1 oracles; tests inject a
// VerifyPathFunc literal to drive the outcome without touching real
// git or GitHub. The signature mirrors RunVerifyPath so the seam is
// 1:1.
type VerifyPathFunc func(VerifyInput) (VerifyOutcome, []OracleCheck)

// DefaultVerifyPath returns a VerifyPathFunc that wires the three
// oracles with their default constructors. T1 needs a working tree;
// the default runner uses the default T1 shell runner. Tests
// that want to drive T1 in isolation build their own
// VerifyPathFunc instead.
func DefaultVerifyPath() VerifyPathFunc {
	return func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
		if in.T2 == nil {
			in.T2 = &T2PreFilter{RepoDir: in.WorkDir, BaseRef: "origin/main", HeadRef: "HEAD"}
		}
		if in.T4 == nil {
			in.T4 = &T4CheapGate{}
		}
		if in.T1 == nil {
			in.T1 = &T1DecisionOracle{}
		}
		return RunVerifyPath(in)
	}
}

// RunVerifyPath runs the three-oracle chain in order: T2 (pre-filter),
// T4 (cheap gate), T1 (decision oracle).
// The chain short-circuits on the first non-abstain outcome; otherwise
// the conservative backstop is invoked by the orchestrator after this
// function returns. The returned checks slice is nil when every oracle
// abstained, so the orchestrator can attach a `verification` payload
// only when it has something to report.
func RunVerifyPath(in VerifyInput) (VerifyOutcome, []OracleCheck) {
	chain := []struct {
		name string
		orc  Oracle
	}{
		{"T2", in.T2},
		{"T4", in.T4},
		{"T1", in.T1},
	}
	for _, step := range chain {
		if step.orc == nil {
			continue
		}
		result, check, err := step.orc.Run(in)
		if err != nil {
			// An oracle that errored is treated as abstain so the
			// conservative backstop still runs. The orchestrator
			// surfaces the error in the run log; we don't put it
			// in the verification payload because errors are not
			// signals.
			continue
		}
		switch result {
		case OracleVerified:
			return VerifyVerified, []OracleCheck{check}
		case OracleFailed:
			return VerifyFailed, []OracleCheck{check}
		case OracleReject:
			// T2's reject path: short-circuit with NoSignal so
			// the conservative backstop runs. Surface the T2
			// check so the operator can see why.
			return VerifyNoSignal, []OracleCheck{check}
		case OracleDeferT1, OracleAbstain, OracleNoSignal:
			continue
		}
	}
	return VerifyNoSignal, nil
}
