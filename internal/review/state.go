package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
)

// ReviewStateStore manages the (prNumber, commentID) dedup state for a
// single review run. The store is bound to one on-disk file — the
// `review-state.json` inside a run folder — and one PR number. It
// provides:
//
//   - TryClaim / Release: reserve or drop a claim on a comment without
//     committing to a terminal status. In-memory only until persisted.
//   - MarkSeen: persist a terminal status (success / failure /
//     superseded / aborted) for a comment via atomic-rename.
//   - IsSeen / IsClaimed: in-memory queries for the dedup decision.
//
// The store is intra-process safe (sync.Mutex). It is NOT cross-process
// safe: two daemons loading the same file would race the os.Rename in
// Save; ADR-0034 §3 accepts this trade-off (the rename-loser re-processes
// the same comment). There is no separate claims/ directory on disk —
// claims live inline in the JSON file's claims map.
//
// Slice 2 NOTE: The on-disk shape is batchindex.ReviewState, which is
// per-PR (the documented schema in ADR-0034). The store wraps that
// shape with the atomic-rename writer and the in-memory dedup set so
// callers don't need to know the file format.
type ReviewStateStore struct {
	prNumber int
	path     string
	mu       sync.Mutex
	state    batchindex.ReviewState
}

// NewReviewStateStore loads the state for the given PR number from
// path. A missing file is not an error: the store starts empty with the
// PR number pre-set. Any I/O or parse error other than ENOENT is
// returned to the caller.
func NewReviewStateStore(path string, prNumber int) (*ReviewStateStore, error) {
	s := &ReviewStateStore{
		prNumber: prNumber,
		path:     path,
		state: batchindex.ReviewState{
			PR:           prNumber,
			SeenComments: nil,
			Claims:       map[string]batchindex.Claim{},
		},
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("open review state: %w", err)
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&s.state); err != nil {
		return nil, fmt.Errorf("decode review state: %w", err)
	}
	if s.state.Claims == nil {
		s.state.Claims = map[string]batchindex.Claim{}
	}
	if s.state.PR == 0 {
		s.state.PR = prNumber
	}
	return s, nil
}

// PR returns the PR number this store is bound to.
func (s *ReviewStateStore) PR() int {
	return s.prNumber
}

// IsSeen reports whether commentID has a terminal status recorded
// (success / failure / superseded / aborted). A comment that is merely
// claimed (no terminal status yet) is not seen.
func (s *ReviewStateStore) IsSeen(commentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isSeenLocked(commentID)
}

func (s *ReviewStateStore) isSeenLocked(commentID string) bool {
	for _, sc := range s.state.SeenComments {
		if sc.CommentID == commentID {
			return true
		}
	}
	return false
}

// IsClaimed reports whether commentID is currently held (claimed or
// terminal).
func (s *ReviewStateStore) IsClaimed(commentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.state.Claims[commentID]
	return ok
}

// TryClaim atomically reserves commentID. Returns true when the caller
// is the first to claim the comment; false if the comment is already
// claimed or terminal-seen. The reservation is in-memory; it is
// persisted by Save or by MarkSeen.
func (s *ReviewStateStore) TryClaim(commentID string) bool {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Claims[commentID]; ok {
		return false
	}
	if s.isSeenLocked(commentID) {
		return false
	}
	s.state.Claims[commentID] = batchindex.Claim{Holder: "local", Since: time.Now()}
	return true
}

// Release drops a claim on commentID without marking it seen. Useful
// when a worker fails before reaching a terminal state and the comment
// should be retried on the next tick.
func (s *ReviewStateStore) Release(commentID string) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state.Claims, commentID)
}

// MarkSeen records a terminal status for commentID and persists the
// store via atomic-rename. status must be non-empty; the typical
// values are "success", "failure", "superseded", or "aborted".
//
// The comment is recorded in SeenComments with the timestamp; the
// Claims map is removed for that comment so a later worker does not
// see it as merely claimed.
func (s *ReviewStateStore) MarkSeen(commentID, status string) error {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return fmt.Errorf("empty comment id")
	}
	if strings.TrimSpace(status) == "" {
		return fmt.Errorf("empty status")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isSeenLocked(commentID) {
		s.state.SeenComments = append(s.state.SeenComments, batchindex.SeenComment{
			CommentID: commentID,
			Timestamp: time.Now(),
		})
	}
	delete(s.state.Claims, commentID)
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
	data, err := json.MarshalIndent(s.state, "", "  ")
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
