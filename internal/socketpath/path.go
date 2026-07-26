package socketpath

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

const sunPathLimit = 104

// Path returns the address used for a Unix socket. When the logical
// filesystem path fits inside the host `sun_path` limit, the logical
// path is returned verbatim so existing on-disk locations and
// permissions are preserved. When the logical path exceeds that limit,
// Path returns a deterministic short filesystem path under `/tmp`
// derived from the full logical path. The same contract applies on
// every host; the resolver has no platform branch.
//
// The short-path hash is a 12-byte (24-hex-char) prefix of the
// SHA-256 of the full logical path. Hashing the full path keeps
// different long review-socket paths from colliding on the same
// short name. The result is `/tmp/sandman-<hash>.sock` (~40 chars),
// well under both the Linux (108) and macOS (104) `sun_path` limits.
func Path(logicalPath string) string {
	if len(logicalPath) <= sunPathLimit {
		return logicalPath
	}
	hash := sha256.Sum256([]byte(logicalPath))
	return filepath.Join("/tmp", "sandman-"+hex.EncodeToString(hash[:12])+".sock")
}
