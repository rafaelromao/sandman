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
// for a single PR. State is persisted as a JSONL file at <baseDir>/<pr>/seen-comments.jsonl.
//
// The store is safe for concurrent use. Mark appends a single line and is
// the only operation that mutates the file; Has reads the file when needed
// and caches the set in memory for the lifetime of the process so callers
// can call Has frequently without paying disk cost.
type SeenCommentsStore struct {
	prDir string

	mu    sync.Mutex
	known map[string]struct{}
}

// NewSeenCommentsStore opens (and lazily reads) the seen-comments file for
// the given PR directory. The directory must exist; the file is created on
// the first Mark.
func NewSeenCommentsStore(prDir string) (*SeenCommentsStore, error) {
	s := &SeenCommentsStore{
		prDir: prDir,
		known: make(map[string]struct{}),
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

// Has reports whether the given comment ID has been marked as seen.
func (s *SeenCommentsStore) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.known[id]
	return ok
}

// Mark records the given comment ID as seen. It appends a single line to
// the seen-comments file and updates the in-memory set atomically. The
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
	return nil
}
