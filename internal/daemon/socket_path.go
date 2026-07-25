package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
)

// unixSocketPathLimit is the conservative filesystem pathname limit for Unix
// sockets. The actual limit is 104 bytes on Darwin and 108 on Linux.
func unixSocketPathLimit() int {
	if runtime.GOOS == "darwin" {
		return 103
	}
	return 107
}

func needsShortSocketPath(path string) bool {
	// Linux already has a working abstract-namespace fallback. Keep it for
	// compatibility with clients that use the listener address directly;
	// other Unix hosts need a filesystem socket plus a logical-path symlink.
	return runtime.GOOS != "linux" && len(path) > unixSocketPathLimit()
}

// shortSocketPath returns a stable path in the short /tmp namespace. The
// logical socket path remains the public address; callers bind this path and
// expose it through a symlink at the logical address.
func shortSocketPath(logicalPath string) string {
	hash := sha256.Sum256([]byte(logicalPath))
	return filepath.Join("/tmp", "sandman-"+hex.EncodeToString(hash[:12])+".sock")
}

func removeSocketPath(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// linkShortSocket exposes a short bound socket at its original logical path.
func linkShortSocket(logicalPath, actualPath string) error {
	if err := removeSocketPath(logicalPath); err != nil {
		return err
	}
	return os.Symlink(actualPath, logicalPath)
}
