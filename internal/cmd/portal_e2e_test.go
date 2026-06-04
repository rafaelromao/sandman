//go:build e2e

package cmd

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
)

func TestPortal_E2E_TwoLiveRuns(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}

	binPath := buildSandmanBinary(t)

	repoDir := t.TempDir()
	t.Chdir(repoDir)
	initRunIntegrationRepo(t, repoDir)

	ghShimDir := t.TempDir()
	writeFakeGHShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	portalURL := startPortalBinary(t, binPath, repoDir, ghShimDir)
	waitForPortalReady(t, portalURL)

	instancesBefore := fetchPortalInstances(t, portalURL)
	t.Logf("instances before creating runs: %d", len(instancesBefore))

	run1Dir := createPromptOnlyRunSocket(t, repoDir, "run-1", 1)
	time.Sleep(500 * time.Millisecond)
	run2Dir := createPromptOnlyRunSocket(t, repoDir, "run-2", 2)
	time.Sleep(500 * time.Millisecond)

	instancesAfter := fetchPortalInstances(t, portalURL)
	t.Logf("instances after creating runs: %d", len(instancesAfter))

	t.Cleanup(func() {
		_ = os.RemoveAll(run1Dir)
		_ = os.RemoveAll(run2Dir)
	})

	waitForRunCount(t, portalURL, 2)

	runs := fetchPortalRuns(t, portalURL)
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d: %v", len(runs), runs)
	}

	runKeys := make(map[string]bool)
	for _, run := range runs {
		runKeys[run.Key] = true
	}
	if len(runKeys) != 2 {
		t.Fatalf("expected 2 distinct run keys, got %v", runKeys)
	}
}

func createPromptOnlyRunSocket(t *testing.T, repoDir, runName string, issueNumber int) string {
	runDir := filepath.Join(repoDir, ".sandman", "runs", runName)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatalf("create run dir: %v", err)
	}

	manifest := daemon.BatchManifest{
		Issues:    []int{},
		CreatedAt: time.Now(),
	}
	if issueNumber > 0 {
		manifest.Issues = []int{issueNumber}
	}
	if err := daemon.WriteManifest(runDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	ln, err := net.Listen("unix", filepath.Join(runDir, "run.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("prompt output\n"))
			_ = conn.Close()
		}
	}()

	return runDir
}

func startPortalBinary(t *testing.T, binPath, repoDir, ghBinDir string) string {
	cmd := exec.Command(binPath, "portal", "--port", "0")
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		"PATH="+ghBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_TOKEN=fake",
		"GITHUB_TOKEN=fake",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start portal: %v", err)
	}

	urlCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := stdout.Read(buf)
			if err != nil {
				return
			}
			output := string(buf[:n])
			if idx := strings.Index(output, "http://"); idx >= 0 {
				url := output[idx:]
				url = strings.TrimSpace(url)
				url = strings.TrimSuffix(url, "\n")
				url = strings.TrimSuffix(url, "\r")
				urlCh <- url
				return
			}
		}
	}()

	select {
	case url := <-urlCh:
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		return url
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("portal did not start within 10s")
		return ""
	}
}

func waitForPortalReady(t *testing.T, baseURL string) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/api/runs")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("portal not ready at %s", baseURL)
}

func waitForRunCount(t *testing.T, baseURL string, want int) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		runs := fetchPortalRuns(t, baseURL)
		if len(runs) >= want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	runs := fetchPortalRuns(t, baseURL)
	t.Fatalf("expected at least %d runs after polling, got %d: %v", want, len(runs), runs)
}

func fetchPortalRuns(t *testing.T, baseURL string) []portalRun {
	resp, err := http.Get(baseURL + "/api/runs")
	if err != nil {
		t.Fatalf("fetch runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode runs: %v", err)
	}
	return payload.Runs
}

func fetchPortalInstances(t *testing.T, baseURL string) []portalInstance {
	resp, err := http.Get(baseURL + "/api/instances")
	if err != nil {
		t.Fatalf("fetch instances: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Instances []portalInstance `json:"instances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode instances: %v", err)
	}
	return payload.Instances
}
