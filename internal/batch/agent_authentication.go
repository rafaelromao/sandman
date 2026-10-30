package batch

import "errors"

// ErrAgentAuthentication marks a launch whose agent could not authenticate
// even after its credentials were reconciled with the host. Every relaunch
// would fail the same way until the operator signs the agent in again.
var ErrAgentAuthentication = errors.New("agent could not authenticate")

// authenticationFailureReporter is implemented by parsers that recognise
// the agent's own authentication failure in its raw output.
type authenticationFailureReporter interface {
	AuthenticationFailed() bool
}

func parsersFailedAuthentication(outputs ...outputParser) bool {
	for _, output := range outputs {
		if reporter, ok := output.(authenticationFailureReporter); ok && reporter.AuthenticationFailed() {
			return true
		}
	}
	return false
}
