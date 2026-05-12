package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
)

func TestValidateAgentConfig_AcceptsFileBasedAuth(t *testing.T) {
	agent := config.Agent{Name: "opencode", Command: "opencode", KeychainAuth: false}
	if err := ValidateAgentConfig(agent); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAgentConfig_RejectsKeychainAuth(t *testing.T) {
	agent := config.Agent{Name: "opencode", Command: "opencode", KeychainAuth: true}
	err := ValidateAgentConfig(agent)
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

func TestContainerRuntime_Start_MountsAgentConfigDirs(t *testing.T) {
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

	configDir := filepath.Join(home, ".config", "opencode")
	localDir := filepath.Join(home, ".local", "share", "opencode")
	_, err = rt.Start("alpine", ".", StartOptions{AgentConfigDirs: []string{configDir, localDir}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	foundConfig := false
	foundLocal := false
	for i := 0; i < len(captured)-1; i++ {
		if captured[i] == "-v" && captured[i+1] == configDir+":/.config/opencode" {
			foundConfig = true
		}
		if captured[i] == "-v" && captured[i+1] == localDir+":/.local/share/opencode" {
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
