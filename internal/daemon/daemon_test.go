package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDir_CreatesUniquePath(t *testing.T) {
	d1 := RunDir("", []int{42})
	d2 := RunDir("", []int{42})
	if d1 == d2 {
		t.Fatal("expected unique run dirs")
	}
}

func TestRunDir_ContainsIssueInPath(t *testing.T) {
	dir := RunDir("", []int{42})
	if !strings.Contains(dir, "42") {
		t.Fatalf("expected issue 42 in path, got %q", dir)
	}
}

func TestRunDir_IsUnderRuns(t *testing.T) {
	dir := RunDir("base", []int{1})
	if !strings.HasPrefix(dir, filepath.Join("base", "runs")) {
		t.Fatalf("expected path under base/runs, got %q", dir)
	}
}

func TestRunDir_NoIssues(t *testing.T) {
	dir := RunDir("", nil)
	parts := strings.Split(dir, string(filepath.Separator))
	if len(parts) < 1 {
		t.Fatal("expected at least one path component")
	}
	last := parts[len(parts)-1]
	if !strings.HasPrefix(last, "run-") {
		t.Fatalf("expected run- prefix, got %q", last)
	}
	if strings.Count(last, "-") != 1 {
		t.Fatalf("expected no embedded issue number in path with no issues: %q", last)
	}
}

func TestRunDir_SubdirNotCreated(t *testing.T) {
	dir := RunDir(t.TempDir(), []int{1})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("RunDir should not create the directory")
	}
}
