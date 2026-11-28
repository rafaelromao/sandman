package atomicfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAtomic_RenameOverPrevious(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "config.yaml")

	prev := []byte("previous: stale\nvalue: 1\n")
	if err := os.WriteFile(dst, prev, 0644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}

	next := []byte("previous: fresh\nvalue: 2\n")
	if err := WriteAtomic(dst, next, 0644); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != string(next) {
		t.Fatalf("destination content = %q, want %q", got, next)
	}
	if strings.Contains(string(got), string(prev)) {
		t.Fatalf("destination still contains previous bytes: %q", got)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp.*"))
	if err != nil {
		t.Fatalf("glob tmp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files remain after WriteAtomic: %v", matches)
	}
}

func TestWriteAtomic_CleansUpTempOnError(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bad.json")

	err := WriteAtomicJSON(dst, make(chan int), 0644)
	if err == nil {
		t.Fatal("expected WriteAtomicJSON to fail on unmarshalable value")
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp.*"))
	if err != nil {
		t.Fatalf("glob tmp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files leaked after marshal failure: %v", matches)
	}

	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("destination must not exist after marshal failure: stat err = %v", err)
	}
}

func TestOpenAppend_CreatesAndAppends(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "run.log")
	const perm os.FileMode = 0644

	first, err := OpenAppend(dst, perm)
	if err != nil {
		t.Fatalf("OpenAppend (create): %v", err)
	}
	if _, err := first.WriteString("first\n"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat after create: %v", err)
	}
	if info.Mode().Perm() != perm {
		t.Fatalf("created file perm = %o, want %o", info.Mode().Perm(), perm)
	}

	second, err := OpenAppend(dst, perm)
	if err != nil {
		t.Fatalf("OpenAppend (existing): %v", err)
	}
	if _, err := second.WriteString("second\n"); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read appended file: %v", err)
	}
	want := "first\nsecond\n"
	if string(got) != want {
		t.Fatalf("appended content = %q, want %q", got, want)
	}
}

// An archive must never replace what is already at its path.
func TestWriteAtomicNew_NeverReplacesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	root := openRoot(t, dir)
	path := filepath.Join(dir, "history", "archive.md")

	if err := WriteAtomicNew(root, "history/archive.md", []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := WriteAtomicNew(root, "history/archive.md", []byte("second"), 0o644)

	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want os.ErrExist", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "first" {
		t.Fatalf("content = %q, want the first write", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("left temp files behind: %v", entries)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
}

// A file system that cannot hard-link fails the write; no partial archive is
// published.
func TestWriteAtomicNew_FailedPublicationLeavesNothing(t *testing.T) {
	failure := errors.New("operation not permitted")
	saved := link
	link = func(*os.Root, string, string) error { return failure }
	t.Cleanup(func() { link = saved })
	dir := t.TempDir()

	err := WriteAtomicNew(openRoot(t, dir), "archive.md", []byte("content"), 0o644)

	if !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the link failure", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left files behind: %v", entries)
	}
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// A symlink that leads out of the root fails the write.
func TestWriteAtomicNew_NeverWritesOutsideItsRoot(t *testing.T) {
	for name, target := range map[string]func(root, outside string) string{
		"absolute": func(_, outside string) string { return outside },
		"relative": func(root, outside string) string { rel, _ := filepath.Rel(root, outside); return rel },
	} {
		t.Run(name, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			if err := os.Symlink(target(dir, outside), filepath.Join(dir, "history")); err != nil {
				t.Fatal(err)
			}

			err := WriteAtomicNew(openRoot(t, dir), "history/archive.md", []byte("content"), 0o644)

			if entries, _ := os.ReadDir(outside); err == nil || len(entries) != 0 {
				t.Fatalf("err = %v, wrote outside: %v", err, entries)
			}
		})
	}
}

// WriteAtomicIn replaces a file inside its root, and a symlink that leads out
// of the root fails the write without touching what it points at.
func TestWriteAtomicIn_ReplacesOnlyInsideItsRoot(t *testing.T) {
	dir := t.TempDir()
	root := openRoot(t, dir)
	for _, data := range []string{"first", "second"} {
		if err := WriteAtomicIn(root, "state/file", []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "state", "file")); err != nil || string(got) != "second" {
		t.Fatalf("file = %q, %v", got, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "state")); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("sibling"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomicIn(root, "escape/file", []byte("new"), 0o644); err == nil {
		t.Fatal("wrote through a symlink leading out of the root")
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "file")); string(got) != "sibling" {
		t.Fatalf("outside file = %q", got)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("wrote outside: %v", entries)
	}
}
