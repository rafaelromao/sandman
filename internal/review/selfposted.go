package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// selfPostEntry is one record in self-posted.json. It records the
// SHA-256 hash of a comment body the bot posted, the PR it was posted
// on, when it was posted, and (optionally) the runID that produced
// the post. Hash is the primary key; pr_number / posted_at / run_id
// are diagnostic metadata retained for the audit trail and the
// eventual retention step.
type selfPostEntry struct {
	Hash     string    `json:"sha256"`
	PRNumber int       `json:"pr_number"`
	RunID    string    `json:"run_id,omitempty"`
	PostedAt time.Time `json:"posted_at"`
}

// SelfPostStore tracks the SHA-256 hashes of every comment the bot
// has posted on a PR, so the review daemon can ignore its own
// comments when scanning for `/sandman review` triggers and when
// verifying that an agent's review comment has been observed. This
// is identity-agnostic — both the implementor and reviewer agents
// share the host user's `gh` config, so author login is not a
// reliable signal. See ADR-0014 "Self-posted comment filter" for the
// full rationale.
//
// Bodies are normalized before hashing: lower-cased and trimmed of
// trailing whitespace. The daemon's `ParseTrigger` and `IsSelfPosted`
// use the same normalization so a comment recorded via `Record`
// matches a check via `IsSelfPosted` even if the agent's exact byte
// sequence varies by trailing whitespace.
//
// Concurrency: the in-memory hash set and the on-disk file are
// guarded by a single sync.Mutex. Concurrent Record calls do not
// corrupt the file (verified by `TestSelfPostStore_ConcurrentRecord`
// under -race).
type SelfPostStore struct {
	path string
	mu   sync.Mutex
	// hashes is the in-memory set, keyed by hex(sha256(body)).
	hashes map[string]selfPostEntry
}

// NewSelfPostStore loads the store from path. A missing file is not
// an error: the store starts empty. Any I/O or parse error other
// than ENOENT yields an empty store and the error is returned; the
// daemon treats this as a non-fatal degraded mode and logs the
// failure (it does not refuse to start, because the existing
// `seenCache` and `ReviewStateStore` are still the primary dedup
// mechanisms — `SelfPostStore` is an *additional* filter).
func NewSelfPostStore(path string) (*SelfPostStore, error) {
	s := &SelfPostStore{
		path:   path,
		hashes: map[string]selfPostEntry{},
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, fmt.Errorf("open self-posted: %w", err)
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&s.hashes); err != nil {
		// Corrupt file: degrade to empty store, return the error
		// so the caller can log it. The next Record will overwrite
		// the file via temp-file + rename.
		return s, fmt.Errorf("decode self-posted: %w", err)
	}
	if s.hashes == nil {
		s.hashes = map[string]selfPostEntry{}
	}
	return s, nil
}

// hashBody normalizes a comment body the same way on record and
// check sites: lower-case and trimmed of trailing whitespace.
func hashBody(body string) string {
	normalized := strings.ToLower(strings.TrimRight(body, " \t\n"))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// Record stores the SHA-256 hash of body in the in-memory set and
// persists the store to disk. Idempotent: a re-record of the same
// body is a no-op (the same hash is already present, and the
// earliest PostedAt / RunID is preserved). Returns an error only if
// the on-disk write fails; in-memory state is still updated so a
// follow-up call can retry the write.
func (s *SelfPostStore) Record(prNumber int, body, runID string) error {
	h := hashBody(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.hashes[h]; exists {
		return nil
	}
	s.hashes[h] = selfPostEntry{
		Hash:     h,
		PRNumber: prNumber,
		RunID:    runID,
		PostedAt: time.Now().UTC(),
	}
	return s.saveLocked()
}

// IsSelfPosted reports whether body's hash is in the store. The
// check is constant-time over the in-memory map; the on-disk file
// is not consulted.
func (s *SelfPostStore) IsSelfPosted(body string) bool {
	h := hashBody(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.hashes[h]
	return ok
}

// Hashes returns a snapshot of the recorded hashes. Used by tests
// to assert membership and by the (future) retention step.
func (s *SelfPostStore) Hashes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.hashes))
	for h := range s.hashes {
		out = append(out, h)
	}
	return out
}

// saveLocked writes the in-memory set to <path>.tmp and renames
// over <path>. The caller must hold s.mu.
func (s *SelfPostStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("create self-posted dir: %w", err)
	}
	tmpPath := s.path + ".tmp"
	data, err := json.MarshalIndent(s.hashes, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal self-posted: %w", err)
	}
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write self-posted tmp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("rename self-posted: %w", err)
	}
	return nil
}
