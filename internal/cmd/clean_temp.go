package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var tempDirPrefixes = []string{"sandman-smoke-prewarm-"}
var containerImagePrefix = "sandman-smoke-"

type TempCleaner interface {
	ResolveRuntime() string
	ScanTempDirs(tempDir string) ([]string, error)
	RemoveTempDir(path string) error
	ListContainerImages(runtime string) ([]string, error)
	RemoveContainerImage(runtime, tag string) error
}

type realTempCleaner struct{}

func (tc *realTempCleaner) ResolveRuntime() string {
	for _, candidate := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func (tc *realTempCleaner) ScanTempDirs(tempDir string) ([]string, error) {
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return nil, err
	}

	var matched []string
	for _, entry := range entries {
		name := entry.Name()
		for _, prefix := range tempDirPrefixes {
			if len(name) > len(prefix) && strings.HasPrefix(name, prefix) {
				path := filepath.Join(tempDir, name)
				matched = append(matched, path)
				break
			}
		}
	}
	return matched, nil
}

func (tc *realTempCleaner) RemoveTempDir(path string) error {
	return os.RemoveAll(path)
}

func (tc *realTempCleaner) ListContainerImages(runtime string) ([]string, error) {
	if runtime == "" {
		return nil, nil
	}
	cmd := exec.Command(runtime, "images", "--format", "{{.Repository}}:{{.Tag}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list images: %w: %s", err, out)
	}
	var images []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" && strings.HasPrefix(line, containerImagePrefix) {
			images = append(images, line)
		}
	}
	return images, nil
}

func (tc *realTempCleaner) RemoveContainerImage(runtime, tag string) error {
	if runtime == "" {
		return nil
	}
	cmd := exec.Command(runtime, "rmi", "-f", tag)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remove image %s: %w: %s", tag, err, out)
	}
	return nil
}
