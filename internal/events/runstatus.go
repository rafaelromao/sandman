package events

// RunStatus is the typed status of an agent run as projected from the
// append-only event log. It is the single source of truth for the
// orchestrator and portal when classifying a run's outcome.
//
// RunStatus is a struct (not a plain int) so the Unknown arm can carry
// a raw payload string and round-trip it through String(). The
// underlying discriminant is unexported; use the named package-level
// constants and the Is* predicate methods to compare, never == on the
// struct itself (two Unknown values with the same raw are equal by
// String() but not by ==).
//
// The String() method preserves the exact byte-level contract of
// RunState.Status() — the portal and every other consumer keeps seeing
// the same status strings after this enum is introduced.
type RunStatus struct {
	code runStatusCode
	raw  string
}

// runStatusCode is the discriminant for RunStatus. The zero value
// (runStatusCodeZero) corresponds to the unset/unknown state; its
// String() returns "" so an unfinished run projects to the empty
// string the portal already relies on.
type runStatusCode int

const (
	runStatusCodeZero runStatusCode = iota
	runStatusCodeSuccess
	runStatusCodeFailure
	runStatusCodeAborted
	runStatusCodeBlocked
	runStatusCodeQueued
	runStatusCodeUnknown
)

// Named status constants. These are the values the orchestrator and
// portal compare against. Use the Is* predicate methods to branch on
// them, not direct == comparisons — the predicate methods stay correct
// even if the underlying discriminant evolves.
var (
	RunStatusZero    = RunStatus{code: runStatusCodeZero}
	RunStatusSuccess = RunStatus{code: runStatusCodeSuccess}
	RunStatusFailure = RunStatus{code: runStatusCodeFailure}
	RunStatusAborted = RunStatus{code: runStatusCodeAborted}
	RunStatusBlocked = RunStatus{code: runStatusCodeBlocked}
	RunStatusQueued  = RunStatus{code: runStatusCodeQueued}
	RunStatusUnknown = RunStatus{code: runStatusCodeUnknown}
)

// String returns the status string the portal and orchestrator have
// always seen. For the named constants it returns the lower-case name;
// for RunStatusUnknown it returns the carried raw payload string; for
// the zero value it returns the empty string (preserving the contract
// for an unfinished run).
func (s RunStatus) String() string {
	switch s.code {
	case runStatusCodeZero:
		return ""
	case runStatusCodeSuccess:
		return "success"
	case runStatusCodeFailure:
		return "failure"
	case runStatusCodeAborted:
		return "aborted"
	case runStatusCodeBlocked:
		return "blocked"
	case runStatusCodeQueued:
		return "queued"
	case runStatusCodeUnknown:
		return s.raw
	}
	return ""
}

// IsTerminal reports whether the status is a terminal outcome of a run
// (the run will not transition to a different status).
func (s RunStatus) IsTerminal() bool {
	switch s.code {
	case runStatusCodeSuccess, runStatusCodeFailure, runStatusCodeAborted, runStatusCodeBlocked:
		return true
	}
	return false
}

// IsSuccess reports whether the run completed successfully.
func (s RunStatus) IsSuccess() bool { return s.code == runStatusCodeSuccess }

// IsFailure reports whether the run failed on its own merits.
func (s RunStatus) IsFailure() bool { return s.code == runStatusCodeFailure }

// IsAborted reports whether the run was interrupted by context
// cancellation before it could finish on its own.
func (s RunStatus) IsAborted() bool { return s.code == runStatusCodeAborted }

// RunStatusFromPayload maps a payload status string to a RunStatus.
// Named strings map to their named constants; any other string maps
// to RunStatusUnknown carrying the raw payload value, so the original
// contract (verdict strings passed through unchanged) is preserved.
func RunStatusFromPayload(s string) RunStatus {
	switch s {
	case "success":
		return RunStatusSuccess
	case "failure":
		return RunStatusFailure
	case "aborted":
		return RunStatusAborted
	case "blocked":
		return RunStatusBlocked
	case "queued":
		return RunStatusQueued
	}
	return RunStatus{code: runStatusCodeUnknown, raw: s}
}
