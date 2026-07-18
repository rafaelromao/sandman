package cmd

import (
	"fmt"
	"os/exec"
	"sync"
)

var lookPathCache sync.Map

func cachedLookPath(name string) (string, error) {
	if v, ok := lookPathCache.Load(name); ok {
		res := v.(struct {
			path string
			err  error
		})
		return res.path, res.err
	}

	path, err := exec.LookPath(name)
	lookPathCache.Store(name, struct {
		path string
		err  error
	}{path: path, err: err})
	return path, err
}

func cachedContainerRuntime() (string, error) {
	for _, candidate := range []string{"podman", "docker"} {
		if path, err := cachedLookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no container runtime (podman or docker) found in PATH")
}
