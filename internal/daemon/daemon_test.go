package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBatchDir_JoinsBaseBatchesAndDirID(t *testing.T) {
	dir := BatchDir("base", "abcd-260618113825-42+2")
	want := filepath.Join("base", "batches", "abcd-260618113825-42+2")
	if dir != want {
		t.Fatalf("expected %q, got %q", want, dir)
	}
}

func TestBatchDir_PreservesDirIDVerbatim(t *testing.T) {
	dirID := "abcd-260618113825-42+2"
	dir := BatchDir("base", dirID)
	if !strings.HasSuffix(dir, dirID) {
		t.Fatalf("expected path to end with %q, got %q", dirID, dir)
	}
}

func TestBatchDir_IsUnderBatches(t *testing.T) {
	dir := BatchDir("base", "anything")
	if !strings.HasPrefix(dir, filepath.Join("base", "batches")) {
		t.Fatalf("expected path under base/batches, got %q", dir)
	}
}

func TestBatchDir_SubdirNotCreated(t *testing.T) {
	dir := BatchDir(t.TempDir(), "anything")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("BatchDir should not create the directory")
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
