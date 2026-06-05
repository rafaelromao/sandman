package cmd

type ExitCodedError struct {
	Code int
	Msg  string
	Err  error
}

func (e *ExitCodedError) Error() string { return e.Msg }

func (e *ExitCodedError) Unwrap() error { return e.Err }
