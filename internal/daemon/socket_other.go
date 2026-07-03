//go:build !linux

package daemon

import (
	"fmt"
	"runtime"
)

// shouldFallbackToAbstractSocket reports whether the abstract-socket
// fallback should be attempted. On non-Linux platforms the fallback
// is suppressed: abstract sockets are a Linux-only kernel extension
// and on other systems (notably macOS darwin) attempting it can mask
// the real bind failure.
func shouldFallbackToAbstractSocket(sockPath string, err error) bool {
	return false
}

// nonLinuxPlatformError returns an error naming the host platform
// and the Unix sun_path limit when a socket bind fails on a path that
// exceeds that limit.
func nonLinuxPlatformError(sockPath string) error {
	return fmt.Errorf("create command socket on %s: path length %d exceeds the Unix sun_path limit; shorten the repo path", runtime.GOOS, len(sockPath))
}
