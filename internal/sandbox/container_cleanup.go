package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ContainerCleanupError means a startup's container identity could not be
// conclusively removed within the cleanup budget. Allocators must not provision
// replacements for this unresolved reservation; existing leases remain valid.
type ContainerCleanupError struct {
	Name string
	Err  error
}

func (e *ContainerCleanupError) Error() string {
	return fmt.Sprintf("unresolved cleanup for container %s: %v", e.Name, e.Err)
}

func (e *ContainerCleanupError) Unwrap() error { return e.Err }

func containerNotFound(out []byte) bool {
	message := strings.ToLower(string(out))
	for _, text := range []string{"no such container", "no such object", "does not exist", "no container with name"} {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

func (r *ContainerRuntime) removePartialContainer(name string, pendingCreation bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerCleanupTimeout)
	defer cancel()
	if pendingCreation {
		// Killing the CLI can precede daemon-side name registration. Absence on
		// the first inspection is not proof that this creation has completed.
		// In particular Podman rm -f may succeed even for an absent name.
		for {
			out, err := runContainerCommand(ctx, r.execFn(r.binary, "inspect", "-f", "{{.Id}}", name))
			if err == nil {
				break
			}
			if ctx.Err() != nil || !containerNotFound(out) {
				return &ContainerCleanupError{Name: name, Err: fmt.Errorf("observe partial creation: %w\n%s", err, out)}
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return &ContainerCleanupError{Name: name, Err: ctx.Err()}
			}
		}
	}
	out, err := runContainerCommand(ctx, r.execFn(r.binary, "rm", "-f", name))
	if err != nil && (ctx.Err() != nil || !containerNotFound(out)) {
		return &ContainerCleanupError{Name: name, Err: fmt.Errorf("remove partial container: %w\n%s", err, out)}
	}
	return nil
}
