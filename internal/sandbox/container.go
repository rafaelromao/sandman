package sandbox

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

// Container represents a running Docker or Podman container.
type Container interface {
	ID() string
	Stop() error
}

// ContainerStarter is the interface for starting containers.
type ContainerStarter interface {
	Start(image, repoPath string, opts StartOptions) (Container, error)
	BuildImage(repoPath string) (string, error)
}

// ConfigMount represents a bind mount from a resolved host path to a container path.
type ConfigMount struct {
	Source string
	Target string
}

// StartOptions configures container startup.
type StartOptions struct {
	GitConfigPath    string
	AgentConfigDirs  []string
	AgentConfigFiles []string
	ConfigMounts     []ConfigMount
	UserID           string
	SSH              bool
	RemoteScheme     string
}

// ContainerRuntime starts and manages containers.
type ContainerRuntime struct {
	binary string
	execFn func(name string, arg ...string) *exec.Cmd
}

// NewContainerRuntime creates a ContainerRuntime for the given binary (docker or podman).
func NewContainerRuntime(binary string) *ContainerRuntime {
	return &ContainerRuntime{binary: binary, execFn: exec.Command}
}

// Start launches a new container with the given image and repo mount.
func (r *ContainerRuntime) Start(image, repoPath string, opts StartOptions) (Container, error) {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}

	args := []string{"run", "-d", "--rm"}
	cleanup := func() {}
	mountedTargets := map[string]bool{}

	if opts.UserID != "" {
		if r.binary != "podman" {
			args = append(args, "--user", opts.UserID)
		}
		args = append(args, "--env", "HOME=/")
	}

	if opts.GitConfigPath != "" {
		mountedGitConfig, gitConfigCleanup, err := prepareMountedGitConfig(opts.GitConfigPath, absRepo)
		if err != nil {
			return nil, err
		}
		cleanup = gitConfigCleanup
		args = append(args, "-v", mountedGitConfig+":/.gitconfig")
	}

	for _, mount := range opts.ConfigMounts {
		args = append(args, "-v", mount.Source+":"+mount.Target)
		mountedTargets[mount.Target] = true
	}

	for _, dir := range opts.AgentConfigDirs {
		containerPath := ToContainerPath(dir)
		if mountedTargets[containerPath] {
			continue
		}
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		args = append(args, "-v", dir+":"+containerPath)
		mountedTargets[containerPath] = true
	}

	for _, file := range opts.AgentConfigFiles {
		containerPath := ToContainerPath(file)
		if mountedTargets[containerPath] {
			continue
		}
		if _, err := os.Stat(file); os.IsNotExist(err) {
			continue
		}
		args = append(args, "-v", file+":"+containerPath)
		mountedTargets[containerPath] = true
	}

	if opts.SSH {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir for ssh mount: %w", err)
		}
		sshPath := filepath.Join(home, ".ssh")
		args = append(args, "-v", sshPath+":/.ssh")
	}

	for _, target := range []string{"/.config", "/.local", "/.cache"} {
		if mountedTargets[target] {
			continue
		}
		args = append(args, "--mount", "type=tmpfs,destination="+target)
	}
	args = append(args, "-v", absRepo+":/workspace", image, "sleep", "3600")

	cmd := r.execFn(r.binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("start container: %w\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))

	if opts.RemoteScheme == "https" {
		setupCmd := r.execFn(r.binary, "exec", id, "gh", "auth", "setup-git")
		if setupOut, setupErr := setupCmd.CombinedOutput(); setupErr != nil {
			cleanup()
			_ = r.execFn(r.binary, "stop", id).Run()
			return nil, fmt.Errorf("gh auth setup-git: %w\n%s", setupErr, setupOut)
		}
	}

	return &runningContainer{id: id, binary: r.binary, execFn: r.execFn, cleanup: cleanup}, nil
}

// BuildImage builds a container image from .sandman/Dockerfile.
// The tag is scoped to the repo path to prevent collisions when sandman
// manages multiple repositories concurrently. Layer caching by the
// container engine may speed up subsequent builds; no explicit cache
// invalidation is performed.
func (r *ContainerRuntime) BuildImage(repoPath string) (string, error) {
	dockerfile := filepath.Join(repoPath, ".sandman", "Dockerfile")
	if _, err := os.Stat(dockerfile); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf(".sandman/Dockerfile not found at %s; container mode requires a Dockerfile in the .sandman directory", dockerfile)
		}
		return "", fmt.Errorf("check .sandman/Dockerfile: %w", err)
	}

	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}

	h := sha256.Sum256([]byte(absPath))
	tag := fmt.Sprintf("sandman-custom-%x:latest", h[:8])

	args := []string{"build", "-t", tag, "-f", dockerfile, repoPath}
	cmd := r.execFn(r.binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build image from .sandman/Dockerfile: %w\n%s", err, out)
	}

	return tag, nil
}

type runningContainer struct {
	id      string
	binary  string
	execFn  func(name string, arg ...string) *exec.Cmd
	cleanup func()
}

func (c *runningContainer) ID() string {
	return c.id
}

func (c *runningContainer) Stop() error {
	if c.cleanup != nil {
		defer c.cleanup()
	}
	cmd := c.execFn(c.binary, "stop", c.id)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("stop container: %w\n%s", err, out)
	}
	return nil
}

func prepareMountedGitConfig(path, absRepo string) (string, func(), error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, func() {}, nil
		}
		return "", nil, fmt.Errorf("read gitconfig: %w", err)
	}
	if !strings.Contains(string(data), absRepo) {
		return path, func() {}, nil
	}

	updated := strings.ReplaceAll(string(data), absRepo, "/workspace")
	tmp, err := os.CreateTemp("", "sandman-gitconfig-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp gitconfig: %w", err)
	}
	if _, err := tmp.WriteString(updated); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("write temp gitconfig: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("close temp gitconfig: %w", err)
	}
	return tmp.Name(), func() { _ = os.Remove(tmp.Name()) }, nil
}

func ToContainerPath(hostPath string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(hostPath, home) {
		rel := strings.TrimPrefix(hostPath, home)
		return "/" + strings.TrimPrefix(rel, "/")
	}
	return hostPath
}

// ValidateAgentConfig rejects agents that rely on OS keychain auth.
func ValidateAgentConfig(name string, agent config.Agent) error {
	if agent.KeychainAuth {
		return fmt.Errorf("agent %q uses OS keychain auth, which is not supported in containers; switch to file-based auth", name)
	}
	return nil
}

// ResolveRuntime detects the available container runtime.
// If preferred is "worktree", it returns "worktree" without probing.
// If preferred is "podman" or empty, it probes for podman first, then docker.
// If preferred is "docker", it probes for docker only.
// If neither runtime is found, it returns an error.
func ResolveRuntime(preferred string) (string, error) {
	if preferred == "worktree" {
		return "worktree", nil
	}
	if preferred == "podman" || preferred == "" {
		if _, err := exec.LookPath("podman"); err == nil {
			return "podman", nil
		}
		if _, err := exec.LookPath("docker"); err == nil {
			return "docker", nil
		}
		return "", fmt.Errorf("neither podman nor docker found; install a container runtime or set sandbox: worktree")
	}
	if preferred == "docker" {
		if _, err := exec.LookPath("docker"); err == nil {
			return "docker", nil
		}
		return "", fmt.Errorf("docker not found; install docker or set sandbox: worktree")
	}
	return preferred, nil
}

const maxCopyDepth = 50

// ResolveConfigMounts creates a temp directory and copies each dir/file
// from the given lists into it, resolving symlinks. Returns the mount
// pairs and a cleanup function to remove the temp directory.
func ResolveConfigMounts(dirs, files []string) ([]ConfigMount, func(), error) {
	tmpDir, err := os.MkdirTemp("", "sandman-config-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create config temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	var mounts []ConfigMount
	usedTargets := make(map[string]bool)

	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("stat config dir %q: %w", dir, err)
		}
		if !info.IsDir() {
			continue
		}
		target := ToContainerPath(dir)
		if usedTargets[target] {
			continue
		}
		usedTargets[target] = true
		dst := filepath.Join(tmpDir, strings.TrimPrefix(target, "/"))
		if err := copyResolved(dir, dst, 0); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("copy config dir %q: %w", dir, err)
		}
		mounts = append(mounts, ConfigMount{Source: dst, Target: target})
	}

	for _, file := range files {
		info, err := os.Stat(file)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("stat config file %q: %w", file, err)
		}
		if info.IsDir() {
			continue
		}
		target := ToContainerPath(file)
		if usedTargets[target] {
			continue
		}
		usedTargets[target] = true
		dst := filepath.Join(tmpDir, strings.TrimPrefix(target, "/"))
		if err := copyResolved(file, dst, 0); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("copy config file %q: %w", file, err)
		}
		mounts = append(mounts, ConfigMount{Source: dst, Target: target})
	}

	return mounts, cleanup, nil
}

// copyResolved copies src to dst, resolving symlinks. If src is a
// directory, it is copied recursively. Broken symlinks are logged and
// skipped. Depth is bounded by maxCopyDepth to guard against circular
// symlinks.
func copyResolved(src, dst string, depth int) error {
	if depth >= maxCopyDepth {
		return fmt.Errorf("max copy depth %d reached", maxCopyDepth)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("warning: broken symlink skipped: %s", src)
			return nil
		}
		log.Printf("warning: cannot stat %s: %v — skipping", src, err)
		return nil
	}

	if srcInfo.IsDir() {
		return copyResolvedDir(src, dst, depth)
	}

	return copyResolvedFile(src, dst)
}

func copyResolvedDir(src, dst string, depth int) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if err := copyResolved(srcPath, dstPath, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func copyResolvedFile(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	return os.Chmod(dst, srcInfo.Mode().Perm())
}

// DetectRemoteScheme returns "ssh" or "https" for the origin remote.
func DetectRemoteScheme(repoPath string) string {
	cmd := exec.Command("git", "config", "--get", "remote.origin.url")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		cmd = exec.Command("git", "remote", "get-url", "origin")
		cmd.Dir = repoPath
		out, err = cmd.Output()
	}
	if err != nil {
		return "https"
	}
	url := strings.TrimSpace(string(out))
	if strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://") {
		return "ssh"
	}
	return "https"
}
