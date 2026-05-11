package sandbox

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Container represents a running Docker or Podman container.
type Container interface {
	ID() string
	Stop() error
}

// ContainerRuntime starts and manages containers.
type ContainerRuntime struct {
	binary string
}

// NewContainerRuntime creates a ContainerRuntime for the given binary (docker or podman).
func NewContainerRuntime(binary string) *ContainerRuntime {
	return &ContainerRuntime{binary: binary}
}

// Start launches a new container with the given image and repo mount.
func (r *ContainerRuntime) Start(image, repoPath string) (Container, error) {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}
	cmd := exec.Command(r.binary, "run", "-d", "--rm", "-v", absRepo+":/workspace", image, "sleep", "3600")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("start container: %w\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))
	return &dockerContainer{id: id, binary: r.binary}, nil
}

type dockerContainer struct {
	id     string
	binary string
}

func (c *dockerContainer) ID() string {
	return c.id
}

func (c *dockerContainer) Stop() error {
	cmd := exec.Command(c.binary, "stop", c.id)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("stop container: %w\n%s", err, out)
	}
	return nil
}
