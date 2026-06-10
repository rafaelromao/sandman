package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ClaimStore provides cross-process atomic claim safety for review comment
// processing. It uses O_CREAT|O_EXCL lock files in <prDir>/claims/ so that
// multiple daemon instances (or concurrent goroutines) cannot process the
// same comment. Stale files older than the stale threshold are purged on
// init for crash recovery.
type ClaimStore struct {
	claimsDir string
	created   map[string]struct{}
	mu        sync.Mutex
}

// NewClaimStore creates the claims directory under prDir, purges lock files
// older than staleThreshold, and returns a ready-to-use ClaimStore.
func NewClaimStore(prDir string, staleThreshold time.Duration) (*ClaimStore, error) {
	claimsDir := filepath.Join(prDir, "claims")
	if err := os.MkdirAll(claimsDir, 0755); err != nil {
		return nil, fmt.Errorf("create claims dir: %w", err)
	}

	entries, err := os.ReadDir(claimsDir)
	if err != nil {
		return nil, fmt.Errorf("read claims dir: %w", err)
	}
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > staleThreshold {
			path := filepath.Join(claimsDir, entry.Name())
			os.Remove(path)
		}
	}

	return &ClaimStore{
		claimsDir: claimsDir,
		created:   make(map[string]struct{}),
	}, nil
}

// TryClaim attempts to atomically claim a comment ID by creating a lock
// file with O_CREAT|O_EXCL. Returns (true, nil) on success, (false, nil)
// if the comment was already claimed, or (false, error) on I/O failure.
func (c *ClaimStore) TryClaim(commentID string) (bool, error) {
	id := strings.TrimSpace(commentID)
	if id == "" {
		return false, fmt.Errorf("empty comment id")
	}

	lockPath := filepath.Join(c.claimsDir, id)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("create claim file: %w", err)
	}
	f.Close()

	c.mu.Lock()
	c.created[id] = struct{}{}
	c.mu.Unlock()

	return true, nil
}

// Release removes the claim file for the given comment ID. It is safe to
// call multiple times or on IDs that were never claimed — the call is a
// no-op when the file doesn't exist or was already released.
func (c *ClaimStore) Release(commentID string) error {
	id := strings.TrimSpace(commentID)
	if id == "" {
		return nil
	}

	c.mu.Lock()
	if _, ok := c.created[id]; !ok {
		c.mu.Unlock()
		return nil
	}
	delete(c.created, id)
	c.mu.Unlock()

	lockPath := filepath.Join(c.claimsDir, id)
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove claim file: %w", err)
	}
	return nil
}

// Close releases all claim files created by this store.
func (c *ClaimStore) Close() error {
	c.mu.Lock()
	ids := make([]string, 0, len(c.created))
	for id := range c.created {
		ids = append(ids, id)
	}
	c.mu.Unlock()

	for _, id := range ids {
		lockPath := filepath.Join(c.claimsDir, id)
		os.Remove(lockPath)
	}
	c.created = nil
	return nil
}
