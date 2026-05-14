package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

// DefaultContainerImage is the image used for sandbox containers.
const DefaultContainerImage = "alpine"

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

// StartOptions configures container startup.
type StartOptions struct {
	GitConfigPath   string
	AgentConfigDirs []string
	UserID          string
	SSH             bool
	RemoteScheme    string
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

	if opts.UserID != "" {
		args = append(args, "--user", opts.UserID)
		args = append(args, "--env", "HOME=/")
	}

	if opts.GitConfigPath != "" {
		args = append(args, "-v", opts.GitConfigPath+":/.gitconfig")
	}

	for _, dir := range opts.AgentConfigDirs {
		containerPath := toContainerPath(dir)
		args = append(args, "-v", dir+":"+containerPath)
	}

	if opts.SSH {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir for ssh mount: %w", err)
		}
		sshPath := filepath.Join(home, ".ssh")
		args = append(args, "-v", sshPath+":/.ssh")
	}

	args = append(args, "-v", absRepo+":/workspace", image, "sleep", "3600")

	cmd := r.execFn(r.binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("start container: %w\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))

	if opts.RemoteScheme == "https" {
		setupCmd := r.execFn(r.binary, "exec", id, "gh", "auth", "setup-git")
		if setupOut, setupErr := setupCmd.CombinedOutput(); setupErr != nil {
			_ = r.execFn(r.binary, "stop", id).Run()
			return nil, fmt.Errorf("gh auth setup-git: %w\n%s", setupErr, setupOut)
		}
	}

	return &runningContainer{id: id, binary: r.binary, execFn: r.execFn}, nil
}

const CustomImageTag = "sandman-custom:latest"

// BuildImage builds a container image from .sandman/Dockerfile.
// The image is always tagged "sandman-custom:latest" and rebuilt on every
// invocation; no caching or hash-based dedup is performed. Callers should
// expect a full build each time.
func (r *ContainerRuntime) BuildImage(repoPath string) (string, error) {
	dockerfile := filepath.Join(repoPath, ".sandman", "Dockerfile")
	if _, err := os.Stat(dockerfile); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf(".sandman/Dockerfile not found at %s; container mode requires a Dockerfile in the .sandman directory", dockerfile)
		}
		return "", fmt.Errorf("check .sandman/Dockerfile: %w", err)
	}

	args := []string{"build", "-t", CustomImageTag, "-f", dockerfile, repoPath}
	cmd := r.execFn(r.binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build image from .sandman/Dockerfile: %w\n%s", err, out)
	}

	return CustomImageTag, nil
}

type runningContainer struct {
	id     string
	binary string
	execFn func(name string, arg ...string) *exec.Cmd
}

func (c *runningContainer) ID() string {
	return c.id
}

func (c *runningContainer) Stop() error {
	cmd := c.execFn(c.binary, "stop", c.id)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("stop container: %w\n%s", err, out)
	}
	return nil
}

func toContainerPath(hostPath string) string {
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
