package socketpath

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"
)

// Path returns the address used for a Unix socket. Linux keeps the logical
// path so its abstract-socket fallback remains available. Other Unix hosts
// use a short deterministic /tmp address when the filesystem path is too long.
func Path(logicalPath string) string {
	if runtime.GOOS == "linux" || len(logicalPath) <= limit() {
		return logicalPath
	}
	hash := sha256.Sum256([]byte(logicalPath))
	return filepath.Join("/tmp", "sandman-"+hex.EncodeToString(hash[:12])+".sock")
}

func limit() int {
	if runtime.GOOS == "darwin" {
		return 103
	}
	return 107
}
