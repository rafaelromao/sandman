package atomicfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteAtomic_RenameOverPrevious(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	previous := []byte("previous content\n")
	if err := os.WriteFile(target, previous, 0o600); err != nil {
		t.Fatalf("seed previous file: %v", err)
	}

	updated := []byte("updated content\n")
	if err := WriteAtomic(target, updated, 0o600); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target after WriteAtomic: %v", err)
	}
	if string(got) != string(updated) {
		t.Fatalf("target content = %q, want %q", got, updated)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "out.json.tmp.*"))
	if err != nil {
		t.Fatalf("glob tmp leftovers: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("WriteAtomic left temp leftovers: %v", matches)
	}
}

func TestWriteAtomic_CleansUpTempOnError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	bad := make(chan int)
	if err := WriteAtomicJSON(target, bad, 0o600); err == nil {
		t.Fatalf("WriteAtomicJSON with chan int: want error, got nil")
	}

	matches, err := filepath.Glob(filepath.Join(dir, "out.json.tmp.*"))
	if err != nil {
		t.Fatalf("glob tmp leftovers: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("WriteAtomicJSON left temp leftovers after marshal error: %v", matches)
	}
}

func TestOpenAppend_CreatesAndAppends(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "appended.log")

	oldMask := syscall.Umask(0)
	defer syscall.Umask(oldMask)

	wantPerm := os.FileMode(0o600)

	f, err := OpenAppend(target, wantPerm)
	if err != nil {
		t.Fatalf("OpenAppend (create): %v", err)
	}
	if _, err := f.WriteString("first\n"); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close after first write: %v", err)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat after first open: %v", err)
	}
	if fi.Mode().Perm() != wantPerm {
		t.Fatalf("created file perm = %o, want %o", fi.Mode().Perm(), wantPerm)
	}

	g, err := OpenAppend(target, wantPerm)
	if err != nil {
		t.Fatalf("OpenAppend (append): %v", err)
	}
	if _, err := g.WriteString("second\n"); err != nil {
		t.Fatalf("write second: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("close after second write: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	want := "first\nsecond\n"
	if string(got) != want {
		t.Fatalf("file content = %q, want %q", got, want)
	}
}
