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
	"regexp"
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

// newShapeKeyRegex matches every on-disk key the post-#1756 daemon
// writes: "pr-<int>-<64-hex>". It is the load-time shape probe the
// new layered loader (issue #1821) uses to distinguish a steady-state
// new-shape file (loaded in-memory) from a legacy / mixed / corrupt
// file (renamed aside, started empty). The regex is intentionally
// strict: anything that does not match — legacy sha-only keys,
// future-shape keys, mixed files, corrupt JSON — falls through to
// the original #1756 one-shot rename + greenfield path. Silent
// partial-merges are not safe (cross-PR poisoning from PR 1752 is
// the live failure mode that motivated the rename policy).
var newShapeKeyRegex = regexp.MustCompile(`^pr-(\d+)-([0-9a-f]{64})$`)

// NewSelfPostStore loads the store from path. Issue #1756 reshaped
// the on-disk key from `sha256(body)` to `pr-<N>-<sha256(body)>`,
// and the original #1756 loader was *greenfield*: it renamed any
// pre-existing file out of the way and started the in-memory store
// empty. The rename policy was correct for the one-shot key-shape
// migration (a sha-only file could not be merged silently into the
// new key space without mis-attributing bodies to an arbitrary PR
// — the cross-PR poisoning failure observed on PR 1752). It was
// not, however, safe to keep applying on every daemon start: every
// restart lost every prior bot-post entry, and a bot review body
// whose body contained the literal `/sandman review` substring
// (typical of `## Previous review progress` sections) re-matched
// the trigger regex on the next session. The daemon re-launched a
// self-loop review on its own previous output (live failure on PR
// #1809, run `4f33-260705092651-PR1809`).
//
// The issue #1821 loader is layered:
//
//   - If the file is missing: start empty, no rename to perform.
//   - If every on-disk key matches `pr-<N>-<sha256>`: load the
//     entries into the in-memory store, leave the file in place,
//     and return. This is the steady state every post-#1756 daemon
//     has been writing since the #1764 migration.
//   - Otherwise (legacy sha-only keys, mixed-shape keys, future
//     keys, corrupt JSON): keep the issue #1756 one-shot rename +
//     greenfield behaviour. A silent partial-merge would either
//     drop bodies (data loss) or attribute them to an arbitrary
//     PR (cross-PR poisoning), so the only safe recovery is to
//     move the file aside and let the new key space start clean.
//
// Operators who want to retain a legacy / mixed file for forensics
// read the `.ignore-<ts>.bak` exactly as before; the rename is the
// migration seam, not a destructive operation.
func NewSelfPostStore(path string) (*SelfPostStore, error) {
	s := &SelfPostStore{
		path:    path,
		entries: map[int]map[string]selfPostEntry{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, fmt.Errorf("stat self-posted: %w", err)
	}
	if !isNewShapeFile(data) {
		bakPath := fmt.Sprintf("%s.ignore-%d.bak", path, time.Now().UTC().UnixNano())
		if renameErr := os.Rename(path, bakPath); renameErr != nil {
			return s, fmt.Errorf("archive legacy self-posted (%s -> %s): %w", path, bakPath, renameErr)
		}
		log.Printf("self-post store: archived legacy self-posted.json to %s, starting greenfield", filepath.Base(bakPath))
		return s, nil
	}
	var disk map[string]selfPostEntry
	if err := json.Unmarshal(data, &disk); err != nil {
		// New-shape file but the bytes are not valid JSON (a
		// race with a concurrent writer that left a truncated
		// temp file behind, for example). Treat as legacy:
		// rename aside, start empty. The next Record rewrites
		// the file via the atomic temp-file + rename path.
		bakPath := fmt.Sprintf("%s.ignore-%d.bak", path, time.Now().UTC().UnixNano())
		if renameErr := os.Rename(path, bakPath); renameErr != nil {
			return s, fmt.Errorf("archive torn self-posted (%s -> %s): %w", path, bakPath, renameErr)
		}
		log.Printf("self-post store: archived torn self-posted.json to %s, starting greenfield", filepath.Base(bakPath))
		return s, nil
	}
	for _, entry := range disk {
		if entry.Hash == "" || entry.PRNumber == 0 {
			continue
		}
		prBucket, ok := s.entries[entry.PRNumber]
		if !ok {
			prBucket = map[string]selfPostEntry{}
			s.entries[entry.PRNumber] = prBucket
		}
		prBucket[entry.Hash] = entry
	}
	return s, nil
}

// isNewShapeFile reports whether the on-disk bytes at path are a
// JSON object whose keys all match the post-#1756 `pr-<N>-<sha>`
// shape. Mixed-shape files, legacy sha-only files, future shapes,
// and non-JSON payloads all return false and fall through to the
// rename + greenfield path. The check is structural: every key in
// the JSON object MUST match the regex. A file that decodes to a
// JSON object with zero keys is treated as new-shape (load zero
// entries and keep the file).
func isNewShapeFile(data []byte) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	for k := range probe {
		if !newShapeKeyRegex.MatchString(k) {
			return false
		}
	}
	return true
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
