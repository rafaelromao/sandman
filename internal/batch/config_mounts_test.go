package batch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

func TestPrepareContainerConfigMounts_StoresSnapshotUnderRunDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	runDir := filepath.Join(t.TempDir(), "runs", "run-42-1")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}

	opencodeDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(opencodeDir, 0755); err != nil {
		t.Fatalf("mkdir opencode dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opencodeDir, "auth.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	oldLookup := lookupGHToken
	lookupGHToken = func() (string, error) { return "gho_run_dir_token", nil }
	t.Cleanup(func() { lookupGHToken = oldLookup })

	opts := sandbox.StartOptions{
		AgentConfigDirs: []string{opencodeDir},
	}

	cleanup, err := PrepareContainerConfigMounts(t.TempDir(), runDir, &opts)
	if err != nil {
		t.Fatalf("prepare container config mounts: %v", err)
	}
	defer cleanup()

	if len(opts.ConfigMounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(opts.ConfigMounts))
	}
	source := opts.ConfigMounts[0].Source
	expectedRoot := filepath.Join(runDir, "config")
	if !strings.HasPrefix(source, expectedRoot) {
		t.Errorf("expected mount source %q to live under run-owned snapshot root %q", source, expectedRoot)
	}
	if info, err := os.Stat(expectedRoot); err != nil || !info.IsDir() {
		t.Errorf("expected run-owned snapshot root to exist as a directory: %v", err)
	}
}

func TestPrepareContainerConfigMounts_RewritesGitConfigCopiesSSHAndHydratesGH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoDir := t.TempDir()
	gitConfig := filepath.Join(home, ".gitconfig")
	gitConfigContent := fmt.Sprintf("[url \"file://%s/.sandman/remote\"]\n\tinsteadOf = git@github.com:rafaelromao/sandman.git\n", repoDir)
	if err := os.WriteFile(gitConfig, []byte(gitConfigContent), 0644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}

	gitConfigDir := filepath.Join(home, ".config", "git")
	if err := os.MkdirAll(gitConfigDir, 0755); err != nil {
		t.Fatalf("mkdir git config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitConfigDir, "config"), []byte("[user]\n\tname = Test\n"), 0644); err != nil {
		t.Fatalf("write git config dir file: %v", err)
	}

	ghConfigDir := filepath.Join(home, ".config", "gh")
	if err := os.MkdirAll(ghConfigDir, 0755); err != nil {
		t.Fatalf("mkdir gh config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ghConfigDir, "hosts.yml"), []byte("github.com:\n    user: test-user\n    git_protocol: ssh\n"), 0600); err != nil {
		t.Fatalf("write gh hosts file: %v", err)
	}

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir ssh dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519"), []byte("PRIVATE KEY"), 0600); err != nil {
		t.Fatalf("write ssh key: %v", err)
	}

	opencodeDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(opencodeDir, 0755); err != nil {
		t.Fatalf("mkdir opencode dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opencodeDir, "auth.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write opencode auth file: %v", err)
	}

	oldLookup := lookupGHToken
	lookupGHToken = func() (string, error) { return "gho_test_token", nil }
	t.Cleanup(func() { lookupGHToken = oldLookup })

	opts := sandbox.StartOptions{
		GitConfigPath:    gitConfig,
		AgentConfigDirs:  []string{opencodeDir, ghConfigDir, gitConfigDir},
		AgentConfigFiles: nil,
		SSH:              true,
	}

	cleanup, err := PrepareContainerConfigMounts(repoDir, "", &opts)
	if err != nil {
		t.Fatalf("prepare container config mounts: %v", err)
	}
	defer cleanup()

	if opts.GitConfigPath != "" {
		t.Fatalf("expected GitConfigPath to be cleared, got %q", opts.GitConfigPath)
	}
	if opts.SSH {
		t.Fatal("expected SSH direct mount to be disabled after temp-copy mount prep")
	}

	seen := map[string]string{}
	for _, mount := range opts.ConfigMounts {
		seen[mount.Target] = mount.Source
	}
	for _, target := range []string{"/.gitconfig", "/.config/git", "/.config/gh", "/.ssh", "/root/.ssh", "/.config/opencode"} {
		if seen[target] == "" {
			t.Fatalf("expected mount target %q, got %v", target, seen)
		}
	}

	gitData, err := os.ReadFile(seen["/.gitconfig"])
	if err != nil {
		t.Fatalf("read mounted gitconfig: %v", err)
	}
	if strings.Contains(string(gitData), repoDir) {
		t.Fatalf("expected mounted gitconfig to not contain host repo path, got:\n%s", gitData)
	}
	if !strings.Contains(string(gitData), "/workspace/.sandman/remote") {
		t.Fatalf("expected mounted gitconfig to contain /workspace path, got:\n%s", gitData)
	}

	ghData, err := os.ReadFile(filepath.Join(seen["/.config/gh"], "hosts.yml"))
	if err != nil {
		t.Fatalf("read mounted gh hosts file: %v", err)
	}
	if !strings.Contains(string(ghData), "oauth_token: gho_test_token") {
		t.Fatalf("expected mounted gh hosts file to include oauth_token, got:\n%s", ghData)
	}

	if _, err := os.Stat(filepath.Join(seen["/.ssh"], "id_ed25519")); err != nil {
		t.Fatalf("expected copied ssh key in mounted dir: %v", err)
	}
	if seen["/.ssh"] != seen["/root/.ssh"] {
		t.Fatalf("expected /.ssh and /root/.ssh to reuse the same copied source, got %q and %q", seen["/.ssh"], seen["/root/.ssh"])
	}
}

func TestPrepareContainerConfigMounts_ErrorsWhenGHTokenMissingFromCopiedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ghConfigDir := filepath.Join(home, ".config", "gh")
	if err := os.MkdirAll(ghConfigDir, 0755); err != nil {
		t.Fatalf("mkdir gh config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ghConfigDir, "hosts.yml"), []byte("github.com:\n    user: test-user\n"), 0600); err != nil {
		t.Fatalf("write gh hosts file: %v", err)
	}

	oldLookup := lookupGHToken
	lookupGHToken = func() (string, error) { return "", fmt.Errorf("no token available") }
	t.Cleanup(func() { lookupGHToken = oldLookup })

	opts := sandbox.StartOptions{AgentConfigDirs: []string{ghConfigDir}}
	cleanup, err := PrepareContainerConfigMounts(t.TempDir(), "", &opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Fatal("expected error when gh hosts file has no oauth_token and host token lookup fails")
	}
	if !strings.Contains(err.Error(), "resolve gh token") {
		t.Fatalf("expected gh token error, got: %v", err)
	}
}

func TestPrepareContainerConfigMounts_HonorsAgentConfigExcludes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	opencodeDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(opencodeDir, 0755); err != nil {
		t.Fatalf("mkdir opencode dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opencodeDir, "auth.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	tokenOptimizer := filepath.Join(opencodeDir, "token-optimizer")
	if err := os.MkdirAll(tokenOptimizer, 0755); err != nil {
		t.Fatalf("mkdir token-optimizer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tokenOptimizer, "blob.bin"), []byte("LARGE"), 0644); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	opts := sandbox.StartOptions{
		AgentConfigDirs:     []string{opencodeDir},
		AgentConfigExcludes: []string{tokenOptimizer},
	}
	cleanup, err := PrepareContainerConfigMounts(t.TempDir(), "", &opts)
	if err != nil {
		t.Fatalf("prepare container config mounts: %v", err)
	}
	defer cleanup()

	if len(opts.ConfigMounts) == 0 {
		t.Fatal("expected ConfigMounts to be populated")
	}
	var snapshotSource string
	for _, mount := range opts.ConfigMounts {
		if mount.Target == "/.local/share/opencode" {
			snapshotSource = mount.Source
			break
		}
	}
	if snapshotSource == "" {
		t.Fatalf("expected mount for /.local/share/opencode, got %v", opts.ConfigMounts)
	}

	if _, err := os.Stat(filepath.Join(snapshotSource, "auth.json")); err != nil {
		t.Errorf("expected auth.json in snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshotSource, "token-optimizer")); !os.IsNotExist(err) {
		t.Errorf("expected token-optimizer to be excluded from snapshot, got: %v", err)
	}
}

func TestPrepareContainerConfigMounts_AppendsLiveMountsForExistingPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	opencodeDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(opencodeDir, 0755); err != nil {
		t.Fatalf("mkdir opencode dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opencodeDir, "auth.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	dbPath := filepath.Join(opencodeDir, "opencode.db")
	if err := os.WriteFile(dbPath, []byte("DB"), 0644); err != nil {
		t.Fatalf("write db: %v", err)
	}
	missingShm := filepath.Join(opencodeDir, "opencode.db-shm")

	opts := sandbox.StartOptions{
		AgentConfigDirs: []string{opencodeDir},
		LiveMounts:      []string{dbPath, missingShm},
	}
	cleanup, err := PrepareContainerConfigMounts(t.TempDir(), "", &opts)
	if err != nil {
		t.Fatalf("prepare container config mounts: %v", err)
	}
	defer cleanup()

	var dbMount *sandbox.ConfigMount
	var snapshotIdx, liveIdx int
	for i := range opts.ConfigMounts {
		mount := &opts.ConfigMounts[i]
		if mount.Target == "/.local/share/opencode/opencode.db" {
			dbMount = mount
			liveIdx = i
		}
		if mount.Target == "/.local/share/opencode" {
			snapshotIdx = i
		}
	}
	if dbMount == nil {
		t.Fatalf("expected live mount for /.local/share/opencode/opencode.db, got %v", opts.ConfigMounts)
	}
	if dbMount.Source != dbPath {
		t.Errorf("expected live mount source %q, got %q", dbPath, dbMount.Source)
	}
	if liveIdx <= snapshotIdx {
		t.Errorf("expected live mount (idx %d) to be appended after snapshot mount (idx %d) so Docker layers it on top", liveIdx, snapshotIdx)
	}
	for _, mount := range opts.ConfigMounts {
		if mount.Target == "/.local/share/opencode/opencode.db-shm" {
			t.Errorf("expected missing live mount path to be silently skipped, got mount %v", mount)
		}
	}

	if _, err := os.Stat(filepath.Join(dbMount.Source, "..")); err != nil {
		// the live mount source must remain the original host file
		t.Errorf("expected live mount source to exist: %v", err)
	}
}
