package cmd

import "github.com/spf13/cobra"

type ExitCodedError struct {
	Code int
	Msg  string
	Err  error
}

func (e *ExitCodedError) Error() string { return e.Msg }

func (e *ExitCodedError) Unwrap() error { return e.Err }

type UsageError struct {
	Err error
}

func (e *UsageError) Error() string { return e.Err.Error() }

func (e *UsageError) Unwrap() error { return e.Err }

func MarkUsage(err error) error {
	if err == nil {
		return nil
	}
	return &UsageError{Err: err}
}

func wrapArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return MarkUsage(err)
		}
		return nil
	}
}
