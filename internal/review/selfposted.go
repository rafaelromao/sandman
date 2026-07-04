package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// selfPostEntry is one record in self-posted.json. It records the
// SHA-256 hash of a comment body the bot posted, the PR it was posted
// on, when it was posted, and (optionally) the runID that produced
// the post. (pr_number, sha256) is the composite primary key;
// posted_at / run_id are diagnostic metadata retained for the audit
// trail and the eventual retention step.
//
// The sha256 field is preserved on disk so the entry remains
// self-describing even when the on-disk key is the composite
// "pr-<N>-<sha>" string.
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
// Issue #1756 reshaped the dedup key from sha256(body) alone to
// (prNumber, sha256(body)). Per-PR scoping closes the cross-PR
// poisoning failure mode observed on PR 1752: a trigger hash
// recorded against PR A no longer silently drops the same trigger
// body on PR B. The on-disk JSON object keys become "pr-<N>-<sha>"
// to keep the single-object layout and preserve the existing
// atomic temp-file + rename persistence path.
//
// Bodies are normalized before hashing: lower-cased and trimmed of
// trailing whitespace. The daemon's `ParseTrigger` and
// `IsSelfPosted` use the same normalization so a comment recorded
// via `Record` matches a check via `IsSelfPosted` even if the
// agent's exact byte sequence varies by trailing whitespace.
//
// Concurrency: the in-memory hash set and the on-disk file are
// guarded by a single sync.Mutex. Concurrent Record calls do not
// corrupt the file (verified by `TestSelfPostStore_ConcurrentRecord`
// under -race).
type SelfPostStore struct {
	path string
	mu   sync.Mutex
	// entries is the in-memory set, keyed first by prNumber, then
	// by hex(sha256(body)). A per-PR lookup is one map access.
	entries map[int]map[string]selfPostEntry
}

// NewSelfPostStore loads the store from path. Issue #1756 makes
// the loader greenfield: any pre-existing `self-posted.json` at
// load time is renamed out of the way (to
// `self-posted.json.ignore-<ts>.bak`) exactly once at startup, the
// in-memory store starts empty, and the loader returns no error.
//
// Rationale: the per-PR scoped dedup key is incompatible with any
// file produced under the legacy single-body-hash key shape. A
// silent merge would either ignore the legacy entries (silent data
// loss) or attempt to attribute them to an arbitrary PR (silent
// mis-scoping). The rename-and-start-empty invariant gives
// operators a recoverable artifact (the .bak file) without
// smuggling stale state into the new key space.
//
// A missing file is also acceptable: the store starts empty with
// no rename to perform. The new file is created via the existing
// atomic temp-file + rename path on the next Record.
func NewSelfPostStore(path string) (*SelfPostStore, error) {
	s := &SelfPostStore{
		path:    path,
		entries: map[int]map[string]selfPostEntry{},
	}
	if _, err := os.Stat(path); err == nil {
		bakPath := fmt.Sprintf("%s.ignore-%d.bak", path, time.Now().UTC().UnixNano())
		if renameErr := os.Rename(path, bakPath); renameErr != nil {
			return s, fmt.Errorf("archive legacy self-posted (%s -> %s): %w", path, bakPath, renameErr)
		}
		log.Printf("self-post store: archived legacy self-posted.json to %s, starting greenfield", filepath.Base(bakPath))
	} else if !errors.Is(err, os.ErrNotExist) {
		return s, fmt.Errorf("stat self-posted: %w", err)
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

// compositeKey returns the on-disk composite key string used as
// the JSON object key in self-posted.json.
func compositeKey(prNumber int, sha string) string {
	return fmt.Sprintf("pr-%d-%s", prNumber, sha)
}

// Record stores the (prNumber, sha256(body)) composite entry in the
// in-memory set and persists the store to disk. The body is
// normalized identically on record and check sites. Idempotent per
// (prNumber, sha) pair: a re-record of the same body on the same PR
// is a no-op (the same entry is already present, and the earliest
// PostedAt / RunID is preserved).
//
// Per-PR scoping: the same body recorded against a different PR is
// a distinct entry — closing the cross-PR poisoning failure mode
// from PR 1752 (issue #1756).
//
// Returns an error only if the on-disk write fails; in-memory state
// is still updated so a follow-up call can retry the write.
func (s *SelfPostStore) Record(prNumber int, body, runID string) error {
	h := hashBody(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	prBucket, ok := s.entries[prNumber]
	if !ok {
		prBucket = map[string]selfPostEntry{}
		s.entries[prNumber] = prBucket
	}
	if _, exists := prBucket[h]; exists {
		return nil
	}
	prBucket[h] = selfPostEntry{
		Hash:     h,
		PRNumber: prNumber,
		RunID:    runID,
		PostedAt: time.Now().UTC(),
	}
	return s.saveLocked()
}

// IsSelfPosted reports whether (prNumber, sha256(body)) is in the
// store. The check is constant-time over the in-memory map; the
// on-disk file is not consulted. Per issue #1756, the PR is part
// of the key: a body that is self-posted on PR A is NOT considered
// self-posted on PR B, closing the cross-PR poisoning failure.
func (s *SelfPostStore) IsSelfPosted(prNumber int, body string) bool {
	h := hashBody(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	prBucket, ok := s.entries[prNumber]
	if !ok {
		return false
	}
	_, found := prBucket[h]
	return found
}

// Hashes returns a snapshot of the composite keys recorded in the
// store. Used by tests to assert membership and by the (future)
// retention step. Each value in the returned slice is the composite
// key in the form "pr-<N>-<sha>".
func (s *SelfPostStore) Hashes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for prNumber, bucket := range s.entries {
		for sha := range bucket {
			out = append(out, compositeKey(prNumber, sha))
		}
	}
	return out
}

// saveLocked writes the in-memory set to <path>.tmp and renames
// over <path>. The caller must hold s.mu. The on-disk layout is
// a single JSON object keyed by "pr-<N>-<sha>" composite keys;
// the value is the selfPostEntry diagnostic metadata.
func (s *SelfPostStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("create self-posted dir: %w", err)
	}
	disk := map[string]selfPostEntry{}
	for prNumber, bucket := range s.entries {
		for sha, entry := range bucket {
			disk[compositeKey(prNumber, sha)] = entry
		}
	}
	tmpPath := s.path + ".tmp"
	data, err := json.MarshalIndent(disk, "", "  ")
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
