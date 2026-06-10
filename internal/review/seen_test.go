package review

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestSeenCommentsStore_StartsEmpty(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Has("abc") {
		t.Error("empty store should not contain any IDs")
	}
}

func TestSeenCommentsStore_LoadsExistingIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seen-comments.jsonl")
	content := "100\n\n200\n  300  \n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range []string{"100", "200", "300"} {
		if !store.Has(id) {
			t.Errorf("expected store to know %q", id)
		}
	}
	if store.Has("missing") {
		t.Error("store should not report missing IDs")
	}
}

func TestSeenCommentsStore_MarkAppendsLine(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := store.Mark("42"); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if !store.Has("42") {
		t.Error("Marked ID should be reported as seen")
	}

	data, err := os.ReadFile(filepath.Join(dir, "seen-comments.jsonl"))
	if err != nil {
		t.Fatalf("read seen file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "42" {
		t.Errorf("file should contain 42, got %q", got)
	}

	if err := store.Mark("43"); err != nil {
		t.Fatalf("Mark(43): %v", err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "seen-comments.jsonl"))
	if err != nil {
		t.Fatalf("read seen file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"42", "43"}
	sort.Strings(lines)
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d: got %q, want %q", i, lines[i], w)
		}
	}
}

func TestSeenCommentsStore_MarkIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := store.Mark("42"); err != nil {
			t.Fatalf("Mark: %v", err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "seen-comments.jsonl"))
	if err != nil {
		t.Fatalf("read seen file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line, got %d: %q", len(lines), string(data))
	}
}

func TestSeenCommentsStore_RejectsEmptyID(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := store.Mark("   "); err == nil {
		t.Fatal("expected error when marking empty ID")
	}
}

func TestSeenCommentsStore_TryClaimReturnsTrueForNewID(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !store.TryClaim("new-id") {
		t.Error("TryClaim should return true for unseen ID")
	}
}

func TestSeenCommentsStore_TryClaimReturnsFalseForSeenID(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	store.TryClaim("abc")
	if store.TryClaim("abc") {
		t.Error("TryClaim should return false for already-claimed ID")
	}
}

func TestSeenCommentsStore_TryClaimMakesHasReturnTrue(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	store.TryClaim("seen-me")
	if !store.Has("seen-me") {
		t.Error("Has should return true after TryClaim succeeds")
	}
}

func TestSeenCommentsStore_TryClaimThenMarkIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	store.TryClaim("mark-me")
	if err := store.Mark("mark-me"); err != nil {
		t.Fatalf("Mark after TryClaim: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "seen-comments.jsonl"))
	if err != nil {
		t.Fatalf("read seen file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 || lines[0] != "mark-me" {
		t.Errorf("expected exactly one line 'mark-me', got %q", string(data))
	}

	// Second Mark should not duplicate the line.
	if err := store.Mark("mark-me"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "seen-comments.jsonl"))
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Errorf("second Mark should not append duplicate, got %d lines: %q", len(lines), string(data))
	}
}

func TestSeenCommentsStore_TryClaimIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	winners := make(chan int, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if store.TryClaim("concurrent") {
				winners <- 1
			}
		}()
	}
	wg.Wait()
	close(winners)

	count := 0
	for range winners {
		count++
	}
	if count != 1 {
		t.Errorf("exactly one goroutine should claim the ID, got %d", count)
	}
}

func TestSeenCommentsStore_TryClaimAgainstLoadedIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seen-comments.jsonl")
	if err := os.WriteFile(path, []byte("existing\n"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := NewSeenCommentsStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.TryClaim("existing") {
		t.Error("TryClaim should return false for pre-loaded ID")
	}
	if !store.TryClaim("new-one") {
		t.Error("TryClaim should return true for genuinely new ID")
	}
}
