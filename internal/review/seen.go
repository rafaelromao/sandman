package review

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SeenCommentsStore tracks comment IDs that have already been processed
// for a single PR. State is persisted as a newline-delimited file at
// <baseDir>/<pr>/seen-comments.jsonl where each line is a single comment
// ID (despite the .jsonl extension, the values are plain integers rather
// than JSON objects — the extension is preserved for grep-ability and
// forward compatibility should per-comment metadata be added later).
//
// The store is safe for concurrent use. Two internal sets track state:
// known  — IDs loaded from disk or persisted via Mark.
// claimed — IDs claimed via TryClaim but not yet written to disk by Mark.
// Has reports true for IDs in either set. This separation allows TryClaim
// to atomically reserve an ID without disk I/O, while Mark persists to
// disk after the work is done.
type SeenCommentsStore struct {
	prDir   string
	mu      sync.Mutex
	known   map[string]struct{}
	claimed map[string]struct{}
}

// NewSeenCommentsStore opens (and lazily reads) the seen-comments file for
// the given PR directory. The directory must exist; the file is created on
// the first Mark.
func NewSeenCommentsStore(prDir string) (*SeenCommentsStore, error) {
	s := &SeenCommentsStore{
		prDir:   prDir,
		known:   make(map[string]struct{}),
		claimed: make(map[string]struct{}),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// seenPath returns the on-disk path of the seen-comments file.
func (s *SeenCommentsStore) seenPath() string {
	return filepath.Join(s.prDir, "seen-comments.jsonl")
}

// load reads the seen-comments file into memory. Missing file is not an
// error: the store simply starts empty.
func (s *SeenCommentsStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.seenPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open seen comments: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		s.known[line] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read seen comments: %w", err)
	}
	return nil
}

// TryClaim atomically checks whether the given comment ID has already been
// seen (in known or claimed) and, if not, reserves it in the claimed set.
// It returns true when the caller successfully claimed the ID. The
// persistent write to disk is deferred to Mark. This replaces the Has+Mark
// pair for concurrent use where multiple goroutines may race to process the
// same comment.
func (s *SeenCommentsStore) TryClaim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.known[id]; ok {
		return false
	}
	if _, ok := s.claimed[id]; ok {
		return false
	}
	s.claimed[id] = struct{}{}
	return true
}

// ReleaseClaim removes the given comment ID from the claimed set without
// marking it as seen. Callers use this when a claim attempt must be retried
// later because a downstream lock could not be acquired.
func (s *SeenCommentsStore) ReleaseClaim(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}

	s.mu.Lock()
	delete(s.claimed, id)
	s.mu.Unlock()
}

// Has reports whether the given comment ID has been marked as seen or
// claimed (in known or claimed).
func (s *SeenCommentsStore) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, inKnown := s.known[id]
	_, inClaimed := s.claimed[id]
	return inKnown || inClaimed
}

// Mark records the given comment ID as seen. It appends a single line to
// the seen-comments file and updates the in-memory known set atomically,
// moving the ID from claimed to known if it was previously claimed. The
// comment ID is written as-is; callers should pass a stable identifier
// (for example, the GitHub comment ID).
func (s *SeenCommentsStore) Mark(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("empty comment id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.known[id]; ok {
		delete(s.claimed, id)
		return nil
	}

	f, err := os.OpenFile(s.seenPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open seen comments for append: %w", err)
	}
	defer f.Close()

	if _, err := fmt.Fprintln(f, id); err != nil {
		return fmt.Errorf("write seen comment: %w", err)
	}

	s.known[id] = struct{}{}
	delete(s.claimed, id)
	return nil
}
