package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/batchindex"
)

// SeenCacheInvalidator is the seam between ReviewStateStore and the
// daemon's per-process seen cache. It keeps the store independent of
// daemon internals — production wires *Daemon, tests inject fakes.
// Issue #1480 slice A.
type SeenCacheInvalidator interface {
	MarkTerminalSeen(prNumber int, commentID string)
	Forget(prNumber int, commentID string)
}

// noopInvalidator is the default invalidator used when no daemon is
// wired (e.g. tests that do not care about cache side effects).
type noopInvalidator struct{}

func (noopInvalidator) MarkTerminalSeen(int, string) {}
func (noopInvalidator) Forget(int, string)           {}

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
	prNumber    int
	path        string
	mu          sync.Mutex
	state       batchindex.ReviewState
	invalidator SeenCacheInvalidator
}

// NewReviewStateStore loads the state for the given PR number from
// path. A missing file is not an error: the store starts empty with the
// PR number pre-set. Any I/O or parse error other than ENOENT is
// returned to the caller.
//
// An optional SeenCacheInvalidator receives MarkTerminalSeen / Forget
// notifications so the daemon's per-process seen cache stays in sync
// with on-disk writes. Pass nil to disable the hook (the store falls
// back to a no-op invalidator).
func NewReviewStateStore(path string, prNumber int, invalidator SeenCacheInvalidator) (*ReviewStateStore, error) {
	if invalidator == nil {
		invalidator = noopInvalidator{}
	}
	s := &ReviewStateStore{
		prNumber:    prNumber,
		path:        path,
		invalidator: invalidator,
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
// should be retried on the next tick. If a SeenCacheInvalidator is
// wired, Forget is fired so the daemon's per-process seen cache drops
// the comment.
func (s *ReviewStateStore) Release(commentID string) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state.Claims, commentID)
	s.invalidator.Forget(s.prNumber, commentID)
}

// MarkSeen records a terminal status for commentID and persists the
// store via atomic-rename. status must be non-empty; the typical
// values are "success", "failure", "superseded", or "aborted".
// "pending" is also accepted by the on-disk schema (issue #1847 S4
// rehydrate walker): it represents a row whose <runDir>/decision.md
// exists but whose daemon-side post step did not complete. The
// daemon does not write "pending" to review-state.json anymore
// (issue #1849 S6 — the lazy-verify walker is gone); pending rows
// only appear on disk after a daemon restart during the rehydrate
// window. The seen cache hook does NOT fire for "pending".
//
// The comment is recorded in SeenComments with the timestamp; the
// Claims map is removed for that comment so a later worker does not
// see it as merely claimed.
//
// When status == "success" the Attempts counter on the SeenComment
// row is reset to zero — issue #2209 §"cleared on the next success".
// Other statuses preserve any previously recorded Attempts so the
// retry-budget evidence remains inspectable. Use MarkSeenWithAttempts
// when the caller needs explicit control over the Attempts field.
//
// Cache side-effect: the SeenCacheInvalidator hook (slice A) fires
// MarkTerminalSeen only when shouldSkipDedupStatus(status) is true
// (success / superseded). A "failure" or "aborted" save does NOT
// cache, preserving the rename-loser retry semantics from
// ADR-0034 §3. If the on-disk Save fails, the hook is NOT fired —
// the cache is advisory and only short-circuits what is also
// persisted.
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
			Status:    status,
			Timestamp: time.Now(),
		})
	} else {
		for i, sc := range s.state.SeenComments {
			if sc.CommentID == commentID {
				s.state.SeenComments[i].Status = status
				s.state.SeenComments[i].Timestamp = time.Now()
				if status == "success" {
					s.state.SeenComments[i].Attempts = 0
				}
				break
			}
		}
	}
	delete(s.state.Claims, commentID)
	if err := reviewStateSave(s); err != nil {
		return err
	}
	if shouldSkipDedupStatus(status) {
		s.invalidator.MarkTerminalSeen(s.prNumber, commentID)
	}
	return nil
}

// MarkSeenWithAttempts is a MarkSeen variant that records an
// explicit attempts count alongside the terminal status. Use this
// from the launch-failure path to preserve the post-increment retry
// budget on the SeenComment row:
//
//	n := ReadFailureAttempts(s, commentID)
//	_ = s.MarkSeenWithAttempts(commentID, "failure", n+1)
//
// The attempts count is stored verbatim — MarkSeenWithAttempts does
// not auto-reset the counter on any status; only the success path
// through MarkSeen clears Attempts. The same save + cache-hook
// invariants as MarkSeen apply: invalidator.MarkTerminalSeen fires
// only when shouldSkipDedupStatus(status) is true, and the hook is
// skipped on save failure.
func (s *ReviewStateStore) MarkSeenWithAttempts(commentID, status string, attempts int) error {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return fmt.Errorf("empty comment id")
	}
	if strings.TrimSpace(status) == "" {
		return fmt.Errorf("empty status")
	}
	if attempts < 0 {
		return fmt.Errorf("negative attempts: %d", attempts)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !s.isSeenLocked(commentID) {
		s.state.SeenComments = append(s.state.SeenComments, batchindex.SeenComment{
			CommentID: commentID,
			Status:    status,
			Timestamp: now,
			Attempts:  attempts,
		})
	} else {
		for i, sc := range s.state.SeenComments {
			if sc.CommentID == commentID {
				s.state.SeenComments[i].Status = status
				s.state.SeenComments[i].Timestamp = now
				s.state.SeenComments[i].Attempts = attempts
				break
			}
		}
	}
	delete(s.state.Claims, commentID)
	if err := reviewStateSave(s); err != nil {
		return err
	}
	if shouldSkipDedupStatus(status) {
		s.invalidator.MarkTerminalSeen(s.prNumber, commentID)
	}
	return nil
}

// ReadFailureAttempts returns the launch-retry attempt count
// recorded for commentID in s, or 0 if no SeenComment entry exists
// for that comment. On-disk files written before the Attempts
// field was introduced decode with Attempts = 0, so this helper
// never errors for a properly-loaded store.
//
// The read is taken under the store mutex so it is safe to call
// concurrently with MarkSeen / MarkSeenWithAttempts. Callers must
// not assume the returned value is stable across another writer;
// re-read after a MarkSeenWithAttempts call if a follow-up write
// could race.
func ReadFailureAttempts(s *ReviewStateStore, commentID string) int {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sc := range s.state.SeenComments {
		if sc.CommentID == commentID {
			return sc.Attempts
		}
	}
	return 0
}

// reviewStateSave is an internal seam so tests can intercept the
// atomic-rename write and confirm the cache hook does not fire on
// Save errors. Production wires it to (*ReviewStateStore).saveLocked.
var reviewStateSave = func(s *ReviewStateStore) error {
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

// saveLocked writes the state to s.path via a unique temp file plus
// os.Rename. The unique temp name closes the concurrency window the
// old fixed-name <path>.tmp writer had: two concurrent saves could
// otherwise race on the same tmp file. The caller must hold s.mu.
func (s *ReviewStateStore) saveLocked() error {
	return atomicfs.WriteAtomicJSON(s.path, s.state, 0644)
}
