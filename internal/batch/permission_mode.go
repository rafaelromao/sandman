package batch

import "errors"

// ErrUnattendedPermissionMode marks a launch stopped because the agent session
// started in a permission mode that cannot make changes. An unattended run has
// no one to leave that mode, and a relaunch starts in it again until the
// operator changes the agent's settings or its permission_mode.
var ErrUnattendedPermissionMode = errors.New("agent session started in a read-only permission mode")

// PermissionModeError is the operational error of a launch stopped for its
// effective permission mode. Its text is the strategy's one-line diagnostic.
type PermissionModeError struct {
	Mode       string
	Diagnostic string
}

func (e *PermissionModeError) Error() string { return e.Diagnostic }

func (e *PermissionModeError) Is(target error) bool { return target == ErrUnattendedPermissionMode }

// permissionModeGuard is implemented by parsers that recognise a session
// starting in a read-only permission mode. The parser calls the stop callback
// once, when it sees that mode.
type permissionModeGuard interface {
	setPermissionModeStop(onStop func())
	// ReadOnlyPermissionMode returns the stopping diagnostic, or nil.
	ReadOnlyPermissionMode() *PermissionModeError
}

func parsersReadOnlyPermissionMode(outputs ...outputParser) *PermissionModeError {
	for _, output := range outputs {
		if guard, ok := output.(permissionModeGuard); ok {
			if stopped := guard.ReadOnlyPermissionMode(); stopped != nil {
				return stopped
			}
		}
	}
	return nil
}
