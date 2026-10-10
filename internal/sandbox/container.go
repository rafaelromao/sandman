package sandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/paths"
)

// Container represents a running Docker or Podman container.
type Container interface {
	ID() string
	Stop() error
}

// ContainerStarter is the interface for starting containers.
// Implementations must honor cancellation and finish command shutdown and
// partial-resource cleanup before returning; callers retain capacity until then.
type ContainerStarter interface {
	Start(ctx context.Context, image, repoPath string, opts StartOptions) (Container, error)
	BuildImage(ctx context.Context, repoPath string) (string, error)
}

// ConfigMount represents a bind mount from a resolved host path to a container path.
type ConfigMount struct {
	Source string
	Target string
}

// StartOptions configures container startup.
type StartOptions struct {
	GitConfigPath       string
	AgentConfigDirs     []string
	AgentConfigFiles    []string
	AgentConfigExcludes []string
	LiveMounts          []string
	ConfigMounts        []ConfigMount
	UserID              string
	SSH                 bool
	RemoteScheme        string
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
func (r *ContainerRuntime) Start(ctx context.Context, image, repoPath string, opts StartOptions) (_ Container, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}

	// Reserve an identity before run: the daemon may create the container even
	// when cancellation prevents the CLI from returning its ID.
	name := "sandman-" + rand.Text()
	args := []string{"run", "-d", "--rm", "--init", "--name", name}
	cleanup := func() {}
	runAttempted, published := false, false
	defer func() {
		if !published {
			if runAttempted {
				if out, err := boundedContainerCommand(r.execFn, r.binary, "rm", "-f", name); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("remove partial container %s: %w\n%s", name, err, out))
				}
			}
			cleanup()
		}
	}()
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

	if opts.ConfigMounts != nil {
		for _, mount := range opts.ConfigMounts {
			args = append(args, "-v", mount.Source+":"+mount.Target)
			mountedTargets[mount.Target] = true
		}
	} else {
		for _, dir := range opts.AgentConfigDirs {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				continue
			}
			containerPath := toContainerPath(dir)
			args = append(args, "-v", dir+":"+containerPath)
			mountedTargets[containerPath] = true
		}

		for _, file := range opts.AgentConfigFiles {
			if _, err := os.Stat(file); os.IsNotExist(err) {
				continue
			}
			containerPath := toContainerPath(file)
			args = append(args, "-v", file+":"+containerPath)
			mountedTargets[containerPath] = true
		}
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
	args = append(args, "-v", absRepo+":/workspace", image, "tail", "-f", "/dev/null")

	runAttempted = true
	out, err := runContainerCommand(ctx, r.execFn(r.binary, args...))
	if err != nil {
		return nil, fmt.Errorf("start container: %w\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))

	if opts.RemoteScheme == "https" {
		if setupOut, setupErr := runContainerCommand(ctx, r.execFn(r.binary, "exec", id, "gh", "auth", "setup-git")); setupErr != nil {
			return nil, fmt.Errorf("gh auth setup-git: %w\n%s", setupErr, setupOut)
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	published = true
	return &runningContainer{id: id, binary: r.binary, execFn: r.execFn, cleanup: cleanup}, nil
}

// BuildImage builds a container image from .sandman/Dockerfile.
// The tag is scoped to the repo path and Dockerfile contents to prevent
// collisions when sandman manages multiple repositories concurrently while
// still rebuilding when the scaffold changes.
func (r *ContainerRuntime) BuildImage(ctx context.Context, repoPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dockerfile := paths.NewLayout(&config.Config{}, repoPath).DockerfilePath()
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
	dockerfileData, err := os.ReadFile(dockerfile)
	if err != nil {
		return "", fmt.Errorf("read .sandman/Dockerfile: %w", err)
	}

	h := sha256.Sum256(append([]byte(absPath), dockerfileData...))
	tag := fmt.Sprintf("sandman-custom-%x:latest", h[:8])

	args := []string{"build", "-t", tag, "-f", dockerfile, repoPath}
	out, err := runContainerCommand(ctx, r.execFn(r.binary, args...))
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

func (c *runningContainer) Alive() (bool, error) {
	return c.AliveContext(context.Background())
}

func (c *runningContainer) AliveContext(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, containerCleanupTimeout)
	defer cancel()
	out, err := runContainerCommand(ctx, c.execFn(c.binary, "inspect", "-f", "{{.State.Running}}", c.id))
	if err != nil {
		return false, fmt.Errorf("inspect container: %w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

func (c *runningContainer) Stop() error {
	if c.cleanup != nil {
		defer c.cleanup()
	}
	if out, err := boundedContainerCommand(c.execFn, c.binary, "stop", c.id); err != nil {
		stopErr := fmt.Errorf("stop container: %w\n%s", err, out)
		if removeOut, removeErr := boundedContainerCommand(c.execFn, c.binary, "rm", "-f", c.id); removeErr != nil {
			return errors.Join(stopErr, fmt.Errorf("remove container: %w\n%s", removeErr, removeOut))
		}
		return stopErr
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

func toContainerPath(hostPath string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(hostPath, home) {
		rel := strings.TrimPrefix(hostPath, home)
		return "/" + strings.TrimPrefix(rel, "/")
	}
	return hostPath
}

// NewLiveConfigMount returns a ConfigMount that bind-mounts the given host
// path directly into the container at its corresponding HOME=/ path, instead
// of copying it into the snapshot. Use this when the file or directory is
// mutable runtime state that should remain inspectable on the host after the
// container run completes (for example, a SQLite session database).
func NewLiveConfigMount(hostPath string) ConfigMount {
	return ConfigMount{Source: hostPath, Target: toContainerPath(hostPath)}
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

// ResolveConfigMounts creates a `config` subdirectory under parentDir and copies each
// dir/file from the given lists into it, resolving symlinks. Any source path that
// matches an entry in excludes (after filepath.Clean) is skipped during the copy,
// so callers can opt large mutable subtrees out of the snapshot. Returns the mount
// pairs and a cleanup function that removes the `config` subdirectory. The caller
// owns parentDir; passing a run-owned parent path keeps the snapshot lifecycle
// tied to that run.
func ResolveConfigMounts(parentDir string, dirs, files, excludes []string) ([]ConfigMount, func(), error) {
	if parentDir == "" {
		return nil, nil, fmt.Errorf("parent dir is required for config snapshot")
	}
	if err := os.RemoveAll(filepath.Join(parentDir, "config")); err != nil {
		return nil, nil, fmt.Errorf("clear stale config snapshot: %w", err)
	}
	snapshotDir, err := os.MkdirTemp(parentDir, ".config-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create config snapshot dir: %w", err)
	}
	publishedDir := filepath.Join(parentDir, "config")
	if err := os.Rename(snapshotDir, publishedDir); err != nil {
		_ = os.RemoveAll(snapshotDir)
		return nil, nil, fmt.Errorf("publish config snapshot dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(publishedDir) }

	excludeSet := buildExcludeSet(excludes)

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
		if excludeSet[filepath.Clean(dir)] {
			continue
		}
		target := toContainerPath(dir)
		if usedTargets[target] {
			continue
		}
		usedTargets[target] = true
		dst := filepath.Join(publishedDir, strings.TrimPrefix(target, "/"))
		if err := copyResolved(dir, dst, 0, excludeSet); err != nil {
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
		if excludeSet[filepath.Clean(file)] {
			continue
		}
		target := toContainerPath(file)
		if usedTargets[target] {
			continue
		}
		usedTargets[target] = true
		dst := filepath.Join(publishedDir, strings.TrimPrefix(target, "/"))
		if err := copyResolved(file, dst, 0, excludeSet); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("copy config file %q: %w", file, err)
		}
		mounts = append(mounts, ConfigMount{Source: dst, Target: target})
	}

	return mounts, cleanup, nil
}

func buildExcludeSet(excludes []string) map[string]bool {
	if len(excludes) == 0 {
		return nil
	}
	set := make(map[string]bool, len(excludes))
	for _, e := range excludes {
		if e == "" {
			continue
		}
		set[filepath.Clean(e)] = true
	}
	return set
}

// copyResolved copies src to dst, resolving symlinks. If src is a
// directory, it is copied recursively. Broken symlinks are logged and
// skipped. Depth is bounded by maxCopyDepth to guard against circular
// symlinks. Any source path in excludeSet is skipped.
func copyResolved(src, dst string, depth int, excludeSet map[string]bool) error {
	if depth >= maxCopyDepth {
		return fmt.Errorf("max copy depth %d reached", maxCopyDepth)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return nil
	}
	if !srcInfo.Mode().IsRegular() && !srcInfo.IsDir() {
		return nil
	}

	if srcInfo.IsDir() {
		return copyResolvedDir(src, dst, depth, excludeSet)
	}

	return copyResolvedFile(src, dst)
}

func copyResolvedDir(src, dst string, depth int, excludeSet map[string]bool) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		if excludeSet[filepath.Clean(srcPath)] {
			continue
		}
		dstPath := filepath.Join(dst, entry.Name())
		if err := copyResolved(srcPath, dstPath, depth+1, excludeSet); err != nil {
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
