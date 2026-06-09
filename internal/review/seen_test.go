package review

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
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
