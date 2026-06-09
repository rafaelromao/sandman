package cmd

import "github.com/spf13/cobra"

// ExitCodedError carries a process exit code alongside an error message,
// allowing commands to signal non-1 exit codes (e.g. 130 for SIGINT aborts).
type ExitCodedError struct {
	Code int
	Msg  string
	Err  error
}

func (e *ExitCodedError) Error() string { return e.Msg }

func (e *ExitCodedError) Unwrap() error { return e.Err }

// UsageError marks an error as a CLI invocation mistake (bad flag, missing
// arg, invalid format) so main.go can print usage text alongside the error.
// Runtime errors are returned unwrapped to keep the output focused on the
// failure cause.
type UsageError struct {
	Err error
}

func (e *UsageError) Error() string { return e.Err.Error() }

func (e *UsageError) Unwrap() error { return e.Err }

// MarkUsage wraps err in a UsageError. main.go inspects Execute's return
// value with errors.As to decide whether to print usage.
func MarkUsage(err error) error {
	if err == nil {
		return nil
	}
	return &UsageError{Err: err}
}

// wrapArgs adapts a cobra.PositionalArgs validator so any error it returns
// is wrapped in UsageError. Cobra's built-in validators return plain errors
// that would otherwise be indistinguishable from runtime errors.
func wrapArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return MarkUsage(err)
		}
		return nil
	}
}
