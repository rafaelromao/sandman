//go:build linux

package daemon

// shouldFallbackToAbstractSocket reports whether the abstract-socket
// fallback should be attempted. On Linux, the predicate is true when
// a path that exceeds the sun_path limit fails to bind.
func shouldFallbackToAbstractSocket(sockPath string, err error) bool {
	return len(sockPath) > 107 && isPathTooLong(err)
}

// nonLinuxPlatformError is unused on Linux; the no-op keeps the
// command-server call site compiling uniformly on every platform.
func nonLinuxPlatformError(sockPath string) error {
	return nil
}
