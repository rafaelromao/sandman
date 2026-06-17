package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunDir_JoinsBaseRunsAndDirID(t *testing.T) {
	dir := RunDir("base", "20250617-143052-abcd-1-issues-first-42")
	want := filepath.Join("base", "runs", "20250617-143052-abcd-1-issues-first-42")
	if dir != want {
		t.Fatalf("expected %q, got %q", want, dir)
	}
}

func TestRunDir_PreservesDirIDVerbatim(t *testing.T) {
	dirID := "20250617-143052-abcd-3-issues-first-42"
	dir := RunDir("base", dirID)
	if !strings.HasSuffix(dir, dirID) {
		t.Fatalf("expected path to end with %q, got %q", dirID, dir)
	}
}

func TestRunDir_IsUnderRuns(t *testing.T) {
	dir := RunDir("base", "anything")
	if !strings.HasPrefix(dir, filepath.Join("base", "runs")) {
		t.Fatalf("expected path under base/runs, got %q", dir)
	}
}

func TestRunDir_SubdirNotCreated(t *testing.T) {
	dir := RunDir(t.TempDir(), "anything")
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
