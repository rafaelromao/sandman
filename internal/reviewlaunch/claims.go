// Package reviewlaunch fences request/head-scoped reviewer artifact ownership.
package reviewlaunch

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var ErrLaunchOwned = errors.New("reviewer launch already owned")

// ClaimLaunch fences execution and publication across daemon processes. Keep the
// claim until artifact cleanup finishes. Historical retry ledgers are not read.
func ClaimLaunch(stateDir string, pr int, trigger, head string) (*os.File, error) {
	return lockClaim(claimPath(stateDir, pr, trigger, head))
}

func lockClaim(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLaunchOwned
		}
		return nil, err
	}
	return file, nil
}

func claimPath(stateDir string, pr int, trigger, head string) string {
	if strings.TrimSpace(head) == "" {
		head = "unknown-head"
	}
	key := sha256.Sum256([]byte(trigger + "\x00" + strings.ToLower(head)))
	// Retain the lock path so old and new daemon processes contend on the
	// same inode while ignoring obsolete count-based budget files.
	return filepath.Join(stateDir, fmt.Sprintf("%d.review-launch-%x.json.launch.lock", pr, key))
}
