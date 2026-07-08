package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestTempCleaner_ScanTempDirs_FiltersByPrefix(t *testing.T) {
	tc := &realTempCleaner{}

	tempDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(tempDir, "sandman-smoke-prewarm-alpha"), 0755); err != nil {
		t.Fatalf("create sandman temp dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tempDir, "sandman-smoke-prewarm-beta"), 0755); err != nil {
		t.Fatalf("create sandman temp dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tempDir, "unrelated-dir"), 0755); err != nil {
		t.Fatalf("create unrelated temp dir: %v", err)
	}

	got, err := tc.ScanTempDirs(tempDir)
	if err != nil {
		t.Fatalf("ScanTempDirs: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("expected 2 sandman temp dirs, got %d: %v", len(got), got)
	}

	want := map[string]bool{
		filepath.Join(tempDir, "sandman-smoke-prewarm-alpha"): true,
		filepath.Join(tempDir, "sandman-smoke-prewarm-beta"):  true,
	}
	for _, path := range got {
		if !want[path] {
			t.Errorf("unexpected path returned: %s", path)
		}
	}
}

func TestTempCleaner_ScanTempDirs_IgnoresUnrelated(t *testing.T) {
	tc := &realTempCleaner{}

	tempDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(tempDir, "random-other-thing"), 0755); err != nil {
		t.Fatalf("create unrelated temp dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tempDir, "another-unrelated"), 0755); err != nil {
		t.Fatalf("create unrelated temp dir: %v", err)
	}

	got, err := tc.ScanTempDirs(tempDir)
	if err != nil {
		t.Fatalf("ScanTempDirs: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("expected 0 unrelated dirs, got %d: %v", len(got), got)
	}
}

func TestTempCleaner_ScanTempDirs_EmptyDir(t *testing.T) {
	tc := &realTempCleaner{}

	tempDir := t.TempDir()

	got, err := tc.ScanTempDirs(tempDir)
	if err != nil {
		t.Fatalf("ScanTempDirs: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("expected 0 entries for empty dir, got %d: %v", len(got), got)
	}
}

func TestTempCleaner_RemoveTempDir_RemovesTarget(t *testing.T) {
	tc := &realTempCleaner{}

	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "sandman-smoke-prewarm-test")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	if err := tc.RemoveTempDir(target); err != nil {
		t.Fatalf("RemoveTempDir: %v", err)
	}

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("expected target to be removed, but it still exists")
	}
}

func TestTempCleaner_RemoveTempDir_NonExistent(t *testing.T) {
	tc := &realTempCleaner{}

	tempDir := t.TempDir()
	nonExistent := filepath.Join(tempDir, "does-not-exist")

	if err := tc.RemoveTempDir(nonExistent); err != nil {
		t.Errorf("RemoveTempDir on non-existent path should not error, got: %v", err)
	}
}

type fakeTempCleaner struct {
	resolveRuntimeReturn string
	scanTempDirsCalled   bool
	scanTempDirsTempDir  string
	scanTempDirsReturn   []string
	scanTempDirsErr      error
	removeTempDirCalled  bool
	removeTempDirPath    string
	removeTempDirErr     error
	listContainerCalled  bool
	listContainerRuntime string
	listContainerReturn  []string
	listContainerErr     error
	removeImageCalled    bool
	removeImageRuntime   string
	removeImageTag       string
	removeImageErr       error
}

func (f *fakeTempCleaner) ResolveRuntime() string {
	return f.resolveRuntimeReturn
}

func (f *fakeTempCleaner) ScanTempDirs(tempDir string) ([]string, error) {
	f.scanTempDirsCalled = true
	f.scanTempDirsTempDir = tempDir
	return f.scanTempDirsReturn, f.scanTempDirsErr
}

func (f *fakeTempCleaner) RemoveTempDir(path string) error {
	f.removeTempDirCalled = true
	f.removeTempDirPath = path
	return f.removeTempDirErr
}

func (f *fakeTempCleaner) ListContainerImages(runtime string) ([]string, error) {
	f.listContainerCalled = true
	f.listContainerRuntime = runtime
	return f.listContainerReturn, f.listContainerErr
}

func (f *fakeTempCleaner) RemoveContainerImage(runtime, tag string) error {
	f.removeImageCalled = true
	f.removeImageRuntime = runtime
	f.removeImageTag = tag
	return f.removeImageErr
}

func TestClean_Default_CleansTemps(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	fakeTC := &fakeTempCleaner{
		scanTempDirsReturn: []string{"/tmp/sandman-smoke-prewarm-test"},
	}
	deps.TempCleaner = fakeTC

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fakeTC.scanTempDirsCalled {
		t.Errorf("expected ScanTempDirs to be called")
	}
	if fakeTC.removeTempDirPath == "" {
		t.Errorf("expected RemoveTempDir to be called with a path")
	}
}

func TestClean_DryRun_PrintsTempRemovals(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	fakeTC := &fakeTempCleaner{
		scanTempDirsReturn: []string{"/tmp/sandman-smoke-prewarm-test"},
	}
	deps.TempCleaner = fakeTC

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fakeTC.scanTempDirsCalled {
		t.Errorf("expected ScanTempDirs to be called")
	}
	if fakeTC.removeTempDirCalled {
		t.Errorf("expected RemoveTempDir NOT to be called in dry-run mode")
	}
	output := buf.String()
	if output == "" {
		t.Errorf("expected output to contain temp cleanup info")
	}
}

func TestClean_Stale_CleansTemps(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	fakeTC := &fakeTempCleaner{
		scanTempDirsReturn: []string{"/tmp/sandman-smoke-prewarm-test"},
	}
	deps.TempCleaner = fakeTC

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--stale"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fakeTC.scanTempDirsCalled {
		t.Errorf("expected ScanTempDirs to be called in stale mode")
	}
	if fakeTC.removeTempDirPath == "" {
		t.Errorf("expected RemoveTempDir to be called in stale mode")
	}
}

func TestClean_Orphaned_CleansTemps(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}
	deps.RunActivityProbe = func(string) bool { return false }

	fakeTC := &fakeTempCleaner{
		scanTempDirsReturn: []string{"/tmp/sandman-smoke-prewarm-orphan"},
	}
	deps.TempCleaner = fakeTC

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--orphaned"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fakeTC.scanTempDirsCalled {
		t.Errorf("expected ScanTempDirs to be called in orphaned mode")
	}
	if fakeTC.removeTempDirPath == "" {
		t.Errorf("expected RemoveTempDir to be called in orphaned mode")
	}
}

func TestClean_DryRun_Orphaned_PrintsTempRemovals(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}
	deps.RunActivityProbe = func(string) bool { return false }

	fakeTC := &fakeTempCleaner{
		scanTempDirsReturn: []string{"/tmp/sandman-smoke-prewarm-orphan"},
	}
	deps.TempCleaner = fakeTC

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--orphaned", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fakeTC.scanTempDirsCalled {
		t.Errorf("expected ScanTempDirs to be called")
	}
	if fakeTC.removeTempDirCalled {
		t.Errorf("expected RemoveTempDir NOT to be called in dry-run mode")
	}
	output := buf.String()
	if output == "" {
		t.Errorf("expected output to contain temp cleanup info")
	}
}

func TestClean_Archived_CleansTemps(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	fakeTC := &fakeTempCleaner{
		scanTempDirsReturn: []string{"/tmp/sandman-smoke-prewarm-archived"},
	}
	deps.TempCleaner = fakeTC

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--archived"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fakeTC.scanTempDirsCalled {
		t.Errorf("expected ScanTempDirs to be called in archived mode")
	}
	if fakeTC.removeTempDirPath == "" {
		t.Errorf("expected RemoveTempDir to be called in archived mode")
	}
}

func TestClean_Archived_DryRun_PrintsTempRemovals(t *testing.T) {
	deps := newRunDepsAuto(t, &fakeBatchRunner{})
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	deps.ConfigStore = &fakeStore{config: &config.Config{WorktreeDir: filepath.Join(dir, ".sandman", "worktrees")}}
	deps.EventLog = &fakeEventLog{}
	deps.GitRunner = &fakeGitRunner{}

	fakeTC := &fakeTempCleaner{
		scanTempDirsReturn: []string{"/tmp/sandman-smoke-prewarm-archived"},
	}
	deps.TempCleaner = fakeTC

	var buf bytes.Buffer
	cmd := NewCleanCmd(deps)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--archived", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fakeTC.scanTempDirsCalled {
		t.Errorf("expected ScanTempDirs to be called")
	}
	if fakeTC.removeTempDirCalled {
		t.Errorf("expected RemoveTempDir NOT to be called in dry-run mode")
	}
	output := buf.String()
	if output == "" {
		t.Errorf("expected output to contain temp cleanup info")
	}
}
