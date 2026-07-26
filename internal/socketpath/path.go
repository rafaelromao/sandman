package socketpath

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// SunPathLimit is the maximum number of bytes a Unix socket path may
// occupy on the most restrictive host sandman supports. Paths longer
// than this are mapped to a short deterministic /tmp address so the
// bind always succeeds and every consumer (bind, dial, os.Stat,
// liveness probes, attach, portal discovery) sees the same effective
// path on every host.
const SunPathLimit = 103

// Path returns the address used for a Unix socket. When the logical
// filesystem path fits inside SunPathLimit, the logical path is
// returned verbatim so existing on-disk locations and permissions
// are preserved. When the logical path exceeds that limit, Path
// returns a deterministic short filesystem path under /tmp derived
// from the full logical path. The same contract applies on every
// host; the resolver has no platform branch.
//
// The short-path hash is a 12-byte (24-hex-char) prefix of the
// SHA-256 of the full logical path. Hashing the full path keeps
// different long socket paths from colliding on the same short
// name. The result is `/tmp/sandman-<hash>.sock`, which is well
// under any host's sun_path limit.
func Path(logicalPath string) string {
	if len(logicalPath) <= SunPathLimit {
		return logicalPath
	}
	hash := sha256.Sum256([]byte(logicalPath))
	return filepath.Join("/tmp", "sandman-"+hex.EncodeToString(hash[:12])+".sock")
}
