package cmd

import "errors"

// ExitCodedError carries a process exit code alongside an error. main.go uses
// errors.As on this type to choose os.Exit(code) over the default non-zero
// exit, and cobra prints Msg once via Error() to avoid double-printing.
type ExitCodedError struct {
	Code int
	Msg  string
	Err  error
}

func (e *ExitCodedError) Error() string { return e.Msg }

func (e *ExitCodedError) Unwrap() error { return e.Err }

// Is allows errors.Is(err, batch.ErrAborted) to match when the wrapped
// sentinel is reachable through Unwrap.
func (e *ExitCodedError) Is(target error) bool {
	return errors.Is(e.Err, target)
}
