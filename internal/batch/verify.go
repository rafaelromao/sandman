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
	// OracleDeferDecision: CheapGate sees APPROVED + CLEAN + green
	// checks and wants the Decision oracle to make the final call.
	// The chain treats this like OracleAbstain for ordering: the
	// Decision oracle runs after CheapGate.
	OracleDeferDecision
	// OracleVerified: the oracle produced a positive signal — the
	// change is already on main. RunVerifyPath short-circuits with
	// VerifyVerified and surfaces this oracle's check.
	OracleVerified
	// OracleFailed: the oracle produced a negative signal — the
	// change is not on main. RunVerifyPath short-circuits with
	// VerifyFailed.
	OracleFailed
	// OracleNoSignal: the oracle did not find a reason to verify or
	// fail; equivalent to OracleAbstain for the chain but kept
	// distinct so callers can tell the difference between "tried, no
	// signal" and "didn't try".
	OracleNoSignal
	// OracleReject: PreFilter's diff-subset check says the branch's
	// diff is not a subset of origin/main. PreFilter abstains from
	// verifying but the chain short-circuits because the Decision
	// oracle cannot prove either.
	OracleReject
)

// OracleCheck is the per-oracle diagnostic carried in the run.finished
// payload when an oracle produces a non-abstain outcome. Details is a
// free-form map keyed by the oracle (e.g. decision: {"ran": 7},
// pre-filter: {"reason": "diverged"}).
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

// Oracle is the contract every decision oracle implements. The three
// oracles (PreFilter / CheapGate / Decision) share the same shape;
// only their implementation differs. Run is invoked at most once per
// RunVerifyPath call.
type Oracle interface {
	Run(VerifyInput) (OracleResult, OracleCheck, error)
}

// VerifyInput is the per-run context every oracle reads. WorkDir is
// the working tree to use for shell-out oracles (Decision); the rest
// is metadata fetched by the orchestrator before the chain runs.
// PreFilter / CheapGate / Decision are the three oracles; eliding a
// slot requires passing `&fakeOracle{outcome: OracleAbstain}` rather
// than nil, since the RunVerifyPath chain slice hard-codes every
// position.
type VerifyInput struct {
	Context   context.Context
	Issue     *github.Issue
	Branch    string
	WorkDir   string
	PR        *github.PR
	PreFilter Oracle
	CheapGate Oracle
	Decision  Oracle
}

// VerifyPathFunc is the seam the orchestrator uses to invoke the
// three-oracle chain. Production wiring goes through RunVerifyPath
// with the default PreFilter / CheapGate / Decision oracles; tests
// inject a VerifyPathFunc literal to drive the outcome without
// touching real git or GitHub. The signature mirrors RunVerifyPath so
// the seam is 1:1.
type VerifyPathFunc func(VerifyInput) (VerifyOutcome, []OracleCheck)

// DefaultVerifyPath returns a VerifyPathFunc that wires the three
// oracles with their default constructors in chain order (PreFilter →
// CheapGate → Decision). Decision needs a working tree; the default
// runner uses the default Decision shell runner. Tests that want to
// drive the chain in isolation build their own VerifyPathFunc instead.
func DefaultVerifyPath() VerifyPathFunc {
	return func(in VerifyInput) (VerifyOutcome, []OracleCheck) {
		if in.PreFilter == nil {
			in.PreFilter = &PreFilterOracle{RepoDir: in.WorkDir, BaseRef: "origin/main", HeadRef: "HEAD"}
		}
		if in.CheapGate == nil {
			in.CheapGate = &CheapGateOracle{}
		}
		if in.Decision == nil {
			in.Decision = &DecisionOracle{}
		}
		return RunVerifyPath(in)
	}
}

// RunVerifyPath runs the three-oracle chain in order: PreFilter
// (pre-filter), CheapGate (cheap gate), Decision (decision oracle).
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
		{"pre-filter", in.PreFilter},
		{"cheap-gate", in.CheapGate},
		{"decision", in.Decision},
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
			// PreFilter's reject path: short-circuit with NoSignal
			// so the conservative backstop runs. Surface the
			// PreFilter check so the operator can see why.
			return VerifyNoSignal, []OracleCheck{check}
		case OracleDeferDecision, OracleAbstain, OracleNoSignal:
			continue
		}
	}
	return VerifyNoSignal, nil
}
