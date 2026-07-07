package atomicfs

import (
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
