//go:build !linux

package daemon

import (
	"fmt"
	"runtime"
)

// shouldFallbackToAbstractSocket reports whether the abstract-socket
// fallback should be attempted. Abstract sockets are a Linux kernel
// extension; on non-Linux platforms (notably macOS darwin) the
// fallback cannot succeed and risks producing a confusing
// error downstream. Returning false here forces callers to surface a
// dedicated error instead of attempting the fallback path.
func shouldFallbackToAbstractSocket(sockPath string, err error) bool {
	return false
}

// nonLinuxPlatformError returns a dedicated error naming the host
// platform and the path-length limit when a command/control socket
// bind fails on a path that exceeds the Unix sun_path cap. The
// abstract-socket fallback used to mask this condition on Linux; on
// non-Linux we surface it directly so the operator can shorten the
// repo path.
func nonLinuxPlatformError(sockPath string) error {
	return fmt.Errorf("create command socket on %s: command socket path length %d exceeds the Unix sun_path limit (107); shorten the repo path or run from a directory whose absolute path fits within sun_path", runtime.GOOS, len(sockPath))
}
