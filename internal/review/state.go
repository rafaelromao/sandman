package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// reviewStateFile is the on-disk JSON shape persisted by ReviewStateStore.
// It is file-local because the store's contract is per-run and multi-PR;
// the exported batchindex.ReviewState struct in internal/batchindex
// captures a different (per-PR) shape used by other tools.
type reviewStateFile struct {
	Claims map[string]reviewClaim `json:"claims"`
}

// reviewClaim is one entry in the claims map. Status is one of:
//   - "claimed": the (pr, commentID) pair is currently held by a worker
//   - "success" / "failure" / "aborted": terminal status from a previous tick
//   - "superseded": a newer comment superseded this one on the same PR
type reviewClaim struct {
	Status    string    `json:"status"`
	Since     time.Time `json:"since"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ReviewStateStore tracks (prNumber, commentID) pairs against a single
// review-state.json file. The store is the source of truth for two
// invariants enforced by the review daemon:
//
//  1. Dedup by (prNumber, commentID) — TryClaim returns false for a pair
//     that is already claimed or already terminal-seen.
//  2. Inline claim locks — claims are stored in the JSON file, not as
//     separate lock files. There is no <storePath>/claims/ directory.
//
// The store is intra-process safe (sync.Mutex). It is NOT cross-process
// safe: two daemons loading the same file would race the os.Rename
// in Save; ADR-0034 §3 accepts this trade-off (the rename-loser
// re-processes the same comment).
type ReviewStateStore struct {
	path string
	mu   sync.Mutex
	data reviewStateFile
}

// NewReviewStateStore loads the state from path. A missing file is not
// an error: the store starts empty. Any I/O or parse error other than
// ENOENT is returned to the caller.
func NewReviewStateStore(path string) (*ReviewStateStore, error) {
	s := &ReviewStateStore{
		path: path,
		data: reviewStateFile{Claims: map[string]reviewClaim{}},
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("open review state: %w", err)
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&s.data); err != nil {
		return nil, fmt.Errorf("decode review state: %w", err)
	}
	if s.data.Claims == nil {
		s.data.Claims = map[string]reviewClaim{}
	}
	return s, nil
}

// keyFor builds the map key for a (prNumber, commentID) pair. The colon
// separator is safe because GitHub comment IDs do not contain colons.
func keyFor(prNumber int, commentID string) string {
	return strconv.Itoa(prNumber) + ":" + commentID
}

// IsSeen reports whether the (prNumber, commentID) pair has a terminal
// status recorded (success / failure / aborted / superseded). A pair that
// is merely claimed (no terminal status yet) is not seen.
func (s *ReviewStateStore) IsSeen(prNumber int, commentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.data.Claims[keyFor(prNumber, commentID)]
	if !ok {
		return false
	}
	return c.Status != "claimed"
}

// IsClaimed reports whether the (prNumber, commentID) pair is currently
// held by a worker or has reached a terminal status.
func (s *ReviewStateStore) IsClaimed(prNumber int, commentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data.Claims[keyFor(prNumber, commentID)]
	return ok
}

// TryClaim atomically reserves a (prNumber, commentID) pair. Returns true
// when the caller is the first to claim the pair; false if the pair is
// already claimed or terminal-seen. The reservation is in-memory; it is
// persisted by Save or by MarkSeen.
func (s *ReviewStateStore) TryClaim(prNumber int, commentID string) bool {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyFor(prNumber, commentID)
	if _, ok := s.data.Claims[k]; ok {
		return false
	}
	s.data.Claims[k] = reviewClaim{Status: "claimed", Since: time.Now()}
	return true
}

// Release drops a (prNumber, commentID) claim without marking it seen.
// Useful when a worker fails before reaching a terminal state and the
// pair should be retried on the next tick.
func (s *ReviewStateStore) Release(prNumber int, commentID string) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Claims, keyFor(prNumber, commentID))
}

// MarkSeen records a terminal status for the (prNumber, commentID) pair
// and persists the store via atomic-rename. status must be non-empty;
// the typical values are "success", "failure", "aborted", or
// "superseded".
func (s *ReviewStateStore) MarkSeen(prNumber int, commentID, status string) error {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return fmt.Errorf("empty comment id")
	}
	if strings.TrimSpace(status) == "" {
		return fmt.Errorf("empty status")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyFor(prNumber, commentID)
	now := time.Now()
	prev, hadPrev := s.data.Claims[k]
	entry := reviewClaim{Status: status, Since: now, UpdatedAt: now}
	if hadPrev {
		if prev.Since.IsZero() {
			entry.Since = now
		} else {
			entry.Since = prev.Since
		}
	}
	s.data.Claims[k] = entry
	return s.saveLocked()
}

// Save persists the in-memory state to disk via atomic-rename. The
// destination file is replaced as a whole; a crash between the temp
// write and the rename leaves the previous file intact.
func (s *ReviewStateStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// saveLocked writes to <path>.tmp and renames over <path>. The caller
// must hold s.mu.
func (s *ReviewStateStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmpPath := s.path + ".tmp"
	data, err := json.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("marshal review state: %w", err)
	}
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write review state tmp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("rename review state: %w", err)
	}
	return nil
}
