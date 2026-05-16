package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

const DefaultContainerImage = "alpine"
const CustomImageTag = "sandman-custom:latest"

func TestValidateAgentConfig_AcceptsFileBasedAuth(t *testing.T) {
	agent := config.Agent{Name: "opencode", Command: "opencode", KeychainAuth: false}
	if err := ValidateAgentConfig("opencode", agent); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAgentConfig_RejectsKeychainAuth(t *testing.T) {
	agent := config.Agent{Name: "opencode", Command: "opencode", KeychainAuth: true}
	err := ValidateAgentConfig("opencode", agent)
	if err == nil {
		t.Fatal("expected error for keychain auth agent")
	}
	if !strings.Contains(err.Error(), "keychain") {
		t.Errorf("error should mention keychain, got: %v", err)
	}
	if !strings.Contains(err.Error(), "file-based") {
		t.Errorf("error should mention file-based auth, got: %v", err)
	}
}

func TestDetectRemoteScheme_ReturnsSSHForGitAtURL(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	cmd := exec.Command("git", "remote", "add", "origin", "git@github.com:owner/repo.git")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v\n%s", err, out)
	}

	if got := DetectRemoteScheme(dir); got != "ssh" {
		t.Errorf("expected ssh, got %q", got)
	}
}

func TestDetectRemoteScheme_ReturnsSSHForSSHURL(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	cmd := exec.Command("git", "remote", "add", "origin", "ssh://git@github.com/owner/repo.git")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v\n%s", err, out)
	}

	if got := DetectRemoteScheme(dir); got != "ssh" {
		t.Errorf("expected ssh, got %q", got)
	}
}

func TestDetectRemoteScheme_ReturnsHTTPSForHTTPSURL(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	cmd := exec.Command("git", "remote", "add", "origin", "https://github.com/owner/repo.git")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v\n%s", err, out)
	}

	if got := DetectRemoteScheme(dir); got != "https" {
		t.Errorf("expected https, got %q", got)
	}
}

func TestDetectRemoteScheme_DefaultsToHTTPSWhenNoRemote(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	if got := DetectRemoteScheme(dir); got != "https" {
		t.Errorf("expected https, got %q", got)
	}
}

func TestContainerRuntime_Start_MountsGitConfig(t *testing.T) {
	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "abc123")
	}

	_, err := rt.Start("alpine", ".", StartOptions{GitConfigPath: "/home/user/.gitconfig"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "-v" && captured[i+1] == "/home/user/.gitconfig:/.gitconfig" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected gitconfig mount flag, got args: %v", captured)
	}
}

func TestContainerRuntime_Start_SetsContainerUser(t *testing.T) {
	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "abc123")
	}

	_, err := rt.Start("alpine", ".", StartOptions{UserID: "1000"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	foundUser := false
	foundHome := false
	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "--user" && captured[i+1] == "1000" {
			foundUser = true
		}
		if captured[i] == "--env" && captured[i+1] == "HOME=/" {
			foundHome = true
		}
	}
	if !foundUser {
		t.Errorf("expected --user flag, got args: %v", captured)
	}
	if !foundHome {
		t.Errorf("expected HOME=/ env, got args: %v", captured)
	}
}

func TestContainerRuntime_Start_UsesKeepIDForPodmanUser(t *testing.T) {
	rt := NewContainerRuntime("podman")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "abc123")
	}

	_, err := rt.Start("alpine", ".", StartOptions{UserID: "1000"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	foundKeepID := false
	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "--userns" && captured[i+1] == "keep-id" {
			foundKeepID = true
			break
		}
	}
	if !foundKeepID {
		t.Errorf("expected --userns keep-id for podman, got args: %v", captured)
	}
}

func expandTilde(p string) string {
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func TestContainerMount_RendersBuiltInAgentPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for name, preset := range config.BuiltInAgentPresets {
		t.Run(name, func(t *testing.T) {
			var expandedDirs []string
			for _, d := range preset.ConfigDirs {
				expanded := expandTilde(d)
				if err := os.MkdirAll(expanded, 0755); err != nil {
					t.Fatalf("mkdir %s: %v", expanded, err)
				}
				expandedDirs = append(expandedDirs, expanded)
			}

			var expandedFiles []string
			for _, f := range preset.ConfigFiles {
				expanded := expandTilde(f)
				if err := os.WriteFile(expanded, []byte("{}"), 0644); err != nil {
					t.Fatalf("write %s: %v", expanded, err)
				}
				expandedFiles = append(expandedFiles, expanded)
			}

			rt := NewContainerRuntime("docker")
			var captured []string
			rt.execFn = func(name string, arg ...string) *exec.Cmd {
				captured = append([]string{name}, arg...)
				return exec.Command("echo", "abc123")
			}

			_, err := rt.Start("alpine", ".", StartOptions{
				AgentConfigDirs:  expandedDirs,
				AgentConfigFiles: expandedFiles,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for _, expanded := range expandedDirs {
				containerPath := toContainerPath(expanded)
				expected := expanded + ":" + containerPath
				found := false
				for i := 0; i < len(captured)-1; i++ {
					if captured[i] == "-v" && captured[i+1] == expected {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing mount for ConfigDir %q (expected %q), args: %v", expanded, expected, captured)
				}
			}

			for _, expanded := range expandedFiles {
				containerPath := toContainerPath(expanded)
				expected := expanded + ":" + containerPath
				found := false
				for i := 0; i < len(captured)-1; i++ {
					if captured[i] == "-v" && captured[i+1] == expected {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing mount for ConfigFile %q (expected %q), args: %v", expanded, expected, captured)
				}
			}
		})
	}
}

func TestContainerRuntime_Start_MountsAgentConfigFiles(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	configFile := filepath.Join(home, ".config", "sandman-test-file.json")
	if err := os.WriteFile(configFile, []byte("{}"), 0644); err != nil {
		t.Fatalf("write test config file: %v", err)
	}
	t.Cleanup(func() { os.Remove(configFile) })

	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "abc123")
	}

	_, err = rt.Start("alpine", ".", StartOptions{AgentConfigFiles: []string{configFile}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := configFile + ":/.config/sandman-test-file.json"
	found := false
	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "-v" && captured[i+1] == expected {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected file mount %q, got args: %v", expected, captured)
	}
}

func TestContainerRuntime_Start_SkipsMissingAgentConfigDirs(t *testing.T) {
	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "abc123")
	}

	missingDir := "/nonexistent/path/for/sandman-test"
	_, err := rt.Start("alpine", ".", StartOptions{AgentConfigDirs: []string{missingDir}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "-v" && strings.Contains(captured[i+1], missingDir) {
			t.Errorf("expected missing dir %q to be skipped, got mount: %v", missingDir, captured[i+1])
		}
	}
}

func TestContainerRuntime_Start_SkipsMissingAgentConfigFiles(t *testing.T) {
	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "abc123")
	}

	missingFile := "/nonexistent/path/for/sandman-test.json"
	_, err := rt.Start("alpine", ".", StartOptions{AgentConfigFiles: []string{missingFile}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "-v" && strings.Contains(captured[i+1], missingFile) {
			t.Errorf("expected missing file %q to be skipped, got mount: %v", missingFile, captured[i+1])
		}
	}
}

func TestContainerRuntime_Start_MountsAgentConfigDirs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	configDir := filepath.Join(home, ".config", "sandman-test-dir")
	localDir := filepath.Join(home, ".local", "share", "sandman-test-dir")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("mkdir local dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(configDir); os.RemoveAll(localDir) })

	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "abc123")
	}

	_, err = rt.Start("alpine", ".", StartOptions{AgentConfigDirs: []string{configDir, localDir}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	foundConfig := false
	foundLocal := false
	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "-v" && captured[i+1] == configDir+":/.config/sandman-test-dir" {
			foundConfig = true
		}
		if captured[i] == "-v" && captured[i+1] == localDir+":/.local/share/sandman-test-dir" {
			foundLocal = true
		}
	}
	if !foundConfig {
		t.Errorf("expected config dir mount, got args: %v", captured)
	}
	if !foundLocal {
		t.Errorf("expected local dir mount, got args: %v", captured)
	}
}

func TestContainerRuntime_Start_MountsSSHForSSHRemotes(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "abc123")
	}

	_, err = rt.Start("alpine", ".", StartOptions{SSH: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := filepath.Join(home, ".ssh") + ":/.ssh"
	found := false
	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "-v" && captured[i+1] == expected {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ssh mount flag %q, got args: %v", expected, captured)
	}
}

func TestContainerRuntime_Start_RunsGhAuthSetupGitForHTTPS(t *testing.T) {
	rt := NewContainerRuntime("docker")
	var commands [][]string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		commands = append(commands, append([]string{name}, arg...))
		if arg[0] == "run" {
			return exec.Command("echo", "abc123")
		}
		return exec.Command("true")
	}

	_, err := rt.Start("alpine", ".", StartOptions{RemoteScheme: "https"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(commands) < 2 {
		t.Fatalf("expected 2 commands, got %d", len(commands))
	}
	execCmd := commands[1]
	if execCmd[0] != "docker" || execCmd[1] != "exec" || execCmd[2] != "abc123" {
		t.Errorf("expected docker exec abc123, got %v", execCmd)
	}
	if execCmd[3] != "gh" || execCmd[4] != "auth" || execCmd[5] != "setup-git" {
		t.Errorf("expected gh auth setup-git, got %v", execCmd)
	}
}

func TestResolveRuntime_PodmanAvailable_ReturnsPodman(t *testing.T) {
	dir := t.TempDir()
	podmanPath := filepath.Join(dir, "podman")
	if err := os.WriteFile(podmanPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write podman: %v", err)
	}
	t.Setenv("PATH", dir)

	got, err := ResolveRuntime("podman")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "podman" {
		t.Errorf("expected podman, got %q", got)
	}
}

func TestResolveRuntime_PodmanMissingDockerAvailable_ReturnsDocker(t *testing.T) {
	dir := t.TempDir()
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write docker: %v", err)
	}
	t.Setenv("PATH", dir)

	got, err := ResolveRuntime("podman")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "docker" {
		t.Errorf("expected docker, got %q", got)
	}
}

func TestResolveRuntime_EmptyDefaultsToPodman(t *testing.T) {
	dir := t.TempDir()
	podmanPath := filepath.Join(dir, "podman")
	if err := os.WriteFile(podmanPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write podman: %v", err)
	}
	t.Setenv("PATH", dir)

	got, err := ResolveRuntime("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "podman" {
		t.Errorf("expected podman, got %q", got)
	}
}

func TestResolveRuntime_NeitherAvailable_ReturnsError(t *testing.T) {
	t.Setenv("PATH", "")

	_, err := ResolveRuntime("podman")
	if err == nil {
		t.Fatal("expected error when neither runtime is available")
	}
	if !strings.Contains(err.Error(), "podman") || !strings.Contains(err.Error(), "docker") {
		t.Errorf("expected error to mention podman and docker, got: %v", err)
	}
}

func TestResolveRuntime_DockerExplicitlyRequested_Missing_ReturnsError(t *testing.T) {
	t.Setenv("PATH", "")

	_, err := ResolveRuntime("docker")
	if err == nil {
		t.Fatal("expected error when docker is unavailable")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("expected error to mention docker, got: %v", err)
	}
}

func TestResolveRuntime_Worktree_ReturnsWorktree(t *testing.T) {
	got, err := ResolveRuntime("worktree")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "worktree" {
		t.Errorf("expected worktree, got %q", got)
	}
}

func TestContainerRuntime_Start_ReturnsErrorWhenGhAuthFails(t *testing.T) {
	rt := NewContainerRuntime("docker")
	var commands [][]string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		commands = append(commands, append([]string{name}, arg...))
		if arg[0] == "run" {
			return exec.Command("echo", "abc123")
		}
		if arg[0] == "exec" {
			return exec.Command("false")
		}
		return exec.Command("true")
	}

	_, err := rt.Start("alpine", ".", StartOptions{RemoteScheme: "https"})
	if err == nil {
		t.Fatal("expected error when gh auth setup-git fails")
	}
	if !strings.Contains(err.Error(), "gh auth setup-git") {
		t.Errorf("expected error to mention gh auth setup-git, got: %v", err)
	}
}

func TestContainerRuntime_BuildImage_ResolvesSandmanDockerfile(t *testing.T) {
	dir := t.TempDir()
	dockerfileDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(dockerfileDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerfileDir, "Dockerfile"), []byte("FROM alpine\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "success")
	}

	tag, err := rt.BuildImage(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag == "" {
		t.Fatal("expected non-empty image tag")
	}

	if len(captured) < 6 {
		t.Fatalf("expected full build command, got %v", captured)
	}

	expectedDockerfile := filepath.Join(dir, ".sandman", "Dockerfile")
	if captured[4] != "-f" {
		t.Errorf("expected -f flag, got %q", captured[4])
	}
	if captured[5] != expectedDockerfile {
		t.Errorf("expected dockerfile path %q, got %q", expectedDockerfile, captured[5])
	}
}

func TestContainerRuntime_BuildImage_BuildsFromDockerfile(t *testing.T) {
	dir := t.TempDir()
	dockerfileDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(dockerfileDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerfileDir, "Dockerfile"), []byte("FROM alpine\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "success")
	}

	tag, err := rt.BuildImage(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag == "" {
		t.Fatal("expected non-empty image tag")
	}

	if len(captured) < 5 {
		t.Fatalf("expected build command, got %v", captured)
	}
	if captured[0] != "docker" {
		t.Errorf("expected docker, got %q", captured[0])
	}
	if captured[1] != "build" {
		t.Errorf("expected build, got %q", captured[1])
	}
	if captured[2] != "-t" {
		t.Errorf("expected -t, got %q", captured[2])
	}
	if captured[3] != tag {
		t.Errorf("expected tag %q, got %q", tag, captured[3])
	}
	if !strings.HasPrefix(tag, "sandman-custom-") || !strings.HasSuffix(tag, ":latest") {
		t.Errorf("expected tag format sandman-custom-<hash>:latest, got %q", tag)
	}
	if len(tag) != len("sandman-custom-")+16+len(":latest") {
		t.Errorf("expected 16-char hex hash in tag, got %q (len %d)", tag, len(tag))
	}
}

func TestContainerRuntime_BuildImage_MissingDockerfile(t *testing.T) {
	dir := t.TempDir()
	rt := NewContainerRuntime("docker")
	_, err := rt.BuildImage(dir)
	if err == nil {
		t.Fatal("expected error for missing .sandman/Dockerfile")
	}
	if !strings.Contains(err.Error(), ".sandman/Dockerfile not found") {
		t.Errorf("expected error to mention .sandman/Dockerfile not found, got: %v", err)
	}
}

func TestContainerRuntime_BuildImage_BuildFailure(t *testing.T) {
	dir := t.TempDir()
	dockerfileDir := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(dockerfileDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerfileDir, "Dockerfile"), []byte("FROM alpine\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rt := NewContainerRuntime("docker")
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		return exec.Command("false")
	}

	_, err := rt.BuildImage(dir)
	if err == nil {
		t.Fatal("expected error when build fails")
	}
	if !strings.Contains(err.Error(), "build image from .sandman/Dockerfile") {
		t.Errorf("expected error to mention build failure, got: %v", err)
	}
}

func TestToContainerPath_MapsDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	hostPath := filepath.Join(home, ".config", "sandman")
	got := toContainerPath(hostPath)
	want := "/.config/sandman"
	if got != want {
		t.Errorf("toContainerPath(%q) = %q, want %q", hostPath, got, want)
	}
}

func TestToContainerPath_MapsFile(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	hostPath := filepath.Join(home, ".claude.json")
	got := toContainerPath(hostPath)
	want := "/.claude.json"
	if got != want {
		t.Errorf("toContainerPath(%q) = %q, want %q", hostPath, got, want)
	}
}

func TestToContainerPath_ReturnsHostPathWhenNotUnderHome(t *testing.T) {
	got := toContainerPath("/opt/custom/path")
	want := "/opt/custom/path"
	if got != want {
		t.Errorf("toContainerPath(%q) = %q, want %q", "/opt/custom/path", got, want)
	}
}

func TestResolveConfigMounts_ResolvesFileSymlink(t *testing.T) {
	realFile := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(realFile, []byte(`{"key": "value"}`), 0644); err != nil {
		t.Fatal(err)
	}

	symFile := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(realFile, symFile); err != nil {
		t.Fatal(err)
	}

	mounts, cleanup, err := ResolveConfigMounts(nil, []string{symFile})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}

	data, err := os.ReadFile(mounts[0].Source)
	if err != nil {
		t.Fatalf("read resolved content: %v", err)
	}
	if string(data) != `{"key": "value"}` {
		t.Errorf("expected content %q, got %q", `{"key": "value"}`, string(data))
	}
}

func TestResolveConfigMounts_InternalSymlinks(t *testing.T) {
	realDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(realDir, "a.txt"), []byte("aaa"), 0644); err != nil {
		t.Fatal(err)
	}
	// Internal symlink: b.txt -> a.txt
	if err := os.Symlink("a.txt", filepath.Join(realDir, "b.txt")); err != nil {
		t.Fatal(err)
	}

	mounts, cleanup, err := ResolveConfigMounts([]string{realDir}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}

	source := mounts[0].Source
	data, err := os.ReadFile(filepath.Join(source, "a.txt"))
	if err != nil {
		t.Fatalf("read a.txt: %v", err)
	}
	if string(data) != "aaa" {
		t.Errorf("expected 'aaa', got %q", string(data))
	}

	bData, err := os.ReadFile(filepath.Join(source, "b.txt"))
	if err != nil {
		t.Fatalf("read b.txt: %v", err)
	}
	if string(bData) != "aaa" {
		t.Errorf("expected 'aaa' for resolved symlink, got %q", string(bData))
	}
}

func TestResolveConfigMounts_BrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "valid.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/target", filepath.Join(dir, "broken")); err != nil {
		t.Fatal(err)
	}

	mounts, cleanup, err := ResolveConfigMounts([]string{dir}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	source := mounts[0].Source
	validContent, err := os.ReadFile(filepath.Join(source, "valid.txt"))
	if err != nil {
		t.Fatalf("read valid.txt: %v", err)
	}
	if string(validContent) != "ok" {
		t.Errorf("expected 'ok', got %q", string(validContent))
	}

	if _, err := os.Stat(filepath.Join(source, "broken")); !os.IsNotExist(err) {
		t.Errorf("expected broken symlink to be skipped, but it exists or other error: %v", err)
	}
}

func TestResolveConfigMounts_CircularSymlink(t *testing.T) {
	dir := t.TempDir()
	// Create a -> b -> a circular chain
	aPath := filepath.Join(dir, "a")
	bPath := filepath.Join(dir, "b")
	if err := os.Symlink(bPath, aPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(aPath, bPath); err != nil {
		t.Fatal(err)
	}

	_, cleanup, err := ResolveConfigMounts([]string{dir}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()
}

func TestResolveConfigMounts_CleanedUp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	_, cleanup, err := ResolveConfigMounts([]string{dir}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cleanup()

	// The temp dir should be gone, but we can't check it directly since we don't have the path
	// Just verify cleanup doesn't error (already called successfully)
}

func TestResolveConfigMounts_MissingDir(t *testing.T) {
	mounts, cleanup, err := ResolveConfigMounts([]string{"/nonexistent/test-dir"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	if len(mounts) != 0 {
		t.Errorf("expected 0 mounts for missing dirs, got %d", len(mounts))
	}
}

func TestContainerRuntime_Start_UsesConfigMounts(t *testing.T) {
	mountsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mountsDir, "cfg.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	mounts, cleanup, err := ResolveConfigMounts([]string{mountsDir}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	rt := NewContainerRuntime("docker")
	var captured []string
	rt.execFn = func(name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.Command("echo", "abc123")
	}

	_, err = rt.Start("alpine", ".", StartOptions{
		ConfigMounts:     mounts,
		AgentConfigDirs:  []string{"/should/not/be/mounted"},
		AgentConfigFiles: []string{"/should/not/be/mounted.json"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify only ConfigMounts are used, not AgentConfigDirs/Files
	mountCount := 0
	for i := 0; i < len(captured); i++ {
		if captured[i] == "-v" {
			mountCount++
		}
	}
	// Should have 1 mount from ConfigMounts, not 2 from the ignored fields
	if mountCount < 1 {
		t.Fatalf("expected at least 1 mount from ConfigMounts, got %d", mountCount)
	}

	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "-v" && strings.Contains(captured[i+1], "/should/not/be/mounted") {
			t.Errorf("AgentConfigDirs/Files should not be mounted when ConfigMounts is set")
		}
	}
}

func TestResolveConfigMounts_ResolvesDirSymlink(t *testing.T) {
	realDir := t.TempDir()
	realContent := filepath.Join(realDir, "config.json")
	if err := os.WriteFile(realContent, []byte(`{"key": "value"}`), 0644); err != nil {
		t.Fatal(err)
	}

	symDir := filepath.Join(t.TempDir(), "config-link")
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Fatal(err)
	}

	mounts, cleanup, err := ResolveConfigMounts([]string{symDir}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}

	source := mounts[0].Source
	resolvedContent := filepath.Join(source, "config.json")
	data, err := os.ReadFile(resolvedContent)
	if err != nil {
		t.Fatalf("read resolved content: %v", err)
	}
	if string(data) != `{"key": "value"}` {
		t.Errorf("expected content %q, got %q", `{"key": "value"}`, string(data))
	}

	// Verify source is not a symlink
	info, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("expected resolved source to not be a symlink")
	}
}
