package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunDir_CreatesUniquePath(t *testing.T) {
	d1 := RunDir("", []int{42}, "")
	d2 := RunDir("", []int{42}, "")
	if d1 == d2 {
		t.Fatal("expected unique run dirs")
	}
}

func TestRunDir_ContainsIssueInPath(t *testing.T) {
	dir := RunDir("", []int{42}, "")
	if !strings.Contains(dir, "42") {
		t.Fatalf("expected issue 42 in path, got %q", dir)
	}
}

func TestRunDir_IsUnderRuns(t *testing.T) {
	dir := RunDir("base", []int{1}, "")
	if !strings.HasPrefix(dir, filepath.Join("base", "runs")) {
		t.Fatalf("expected path under base/runs, got %q", dir)
	}
}

func TestRunDir_NoIssues(t *testing.T) {
	dir := RunDir("", []int{}, "")
	parts := strings.Split(dir, string(filepath.Separator))
	if len(parts) < 1 {
		t.Fatal("expected at least one path component")
	}
	last := parts[len(parts)-1]
	if !strings.HasPrefix(last, "20") {
		t.Fatalf("expected timestamp prefix for prompt-only run, got %q", last)
	}
	if !strings.HasSuffix(last, "-prompt-only") {
		t.Fatalf("expected -prompt-only suffix, got %q", last)
	}
}

func TestRunDir_SubdirNotCreated(t *testing.T) {
	dir := RunDir(t.TempDir(), []int{1}, "")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("RunDir should not create the directory")
	}
}

func TestRunDir_WithRunID(t *testing.T) {
	dir := RunDir("base", nil, "my-run")
	want := filepath.Join("base", "runs", "my-run")
	if dir != want {
		t.Fatalf("expected %q, got %q", want, dir)
	}
}

func TestRunDir_WithRunID_IsUnderRuns(t *testing.T) {
	dir := RunDir("base", nil, "my-run")
	if !strings.HasPrefix(dir, filepath.Join("base", "runs")) {
		t.Fatalf("expected path under base/runs, got %q", dir)
	}
}

func TestRunDir_WithRunID_SubdirNotCreated(t *testing.T) {
	dir := RunDir(t.TempDir(), nil, "my-run")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("RunDir should not create the directory")
	}
}

func TestManifest_RoundTrip(t *testing.T) {
	runDir := t.TempDir()
	manifest := BatchManifest{Issues: []int{1, 2, 3}, CreatedAt: time.Now().UTC().Round(time.Second)}
	if err := WriteManifest(runDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	got, err := ReadManifest(runDir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !reflect.DeepEqual(got.Issues, manifest.Issues) {
		t.Fatalf("expected issues %v, got %v", manifest.Issues, got.Issues)
	}
	if !got.CreatedAt.Equal(manifest.CreatedAt) {
		t.Fatalf("expected createdAt %v, got %v", manifest.CreatedAt, got.CreatedAt)
	}
}
