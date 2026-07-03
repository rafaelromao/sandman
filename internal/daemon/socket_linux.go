//go:build linux

package daemon

// shouldFallbackToAbstractSocket reports whether the abstract-socket
// fallback should be attempted. The fallback uses Linux abstract
// namespace semantics (a leading "@" socket) and is therefore
// Linux-only. On Linux, the predicate is the original gate: a path
// that exceeds the sun_path limit on bind-time.
func shouldFallbackToAbstractSocket(sockPath string, err error) bool {
	return len(sockPath) > 107 && isPathTooLong(err)
}

// nonLinuxPlatformError is a build-tagged no-op on Linux so the
// command-server call site can compile uniformly; the Linux call site
// preserves the original "create command socket: %w" error message.
func nonLinuxPlatformError(sockPath string) error {
	return nil
}
