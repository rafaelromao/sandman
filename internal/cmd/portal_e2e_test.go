//go:build e2e

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
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

func TestPortal_E2E_AbortStopsOneIssueAndBatchContinues(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}
	if !portalAbortSupported() {
		t.Skip("skip abort e2e on unsupported platform")
	}

	binPath := buildSandmanBinary(t)

	repoDir := shortTempDir(t)
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)
	writeAbortE2EConfig(t, repoDir)

	ghShimDir := shortTempDir(t)
	writeFakeGHShim(t, ghShimDir)
	writeBlockingOpencodeShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	portalURL := startPortalBinary(t, binPath, repoDir, ghShimDir)
	waitForPortalReady(t, portalURL)

	_ = startSandmanRun(t, binPath, repoDir, ghShimDir, "run", "1", "2")

	waitForPortalRunCountAndStatus(t, portalURL, 2, "active")

	runs := fetchPortalRuns(t, portalURL)
	runByIssue := portalRunsByIssue(runs)
	abortRun, ok := runByIssue[1]
	if !ok {
		t.Fatalf("expected issue 1 run in %v", runs)
	}
	abortBodyCode, abortBody := writeAbortRequest(t, portalURL, abortRun.Key, 1)
	if abortBodyCode != http.StatusOK {
		t.Fatalf("expected 200 abort response, got %d: %s", abortBodyCode, abortBody)
	}
	if len(abortBody) == 0 {
		t.Fatal("expected non-empty abort response body")
	}
	var abortResp map[string]any
	if err := json.Unmarshal(abortBody, &abortResp); err != nil {
		t.Fatalf("parse abort response: %v", err)
	}
	if abortResp["status"] != "aborted" || abortResp["scope"] != "issue" {
		t.Fatalf("unexpected abort response: %v", abortResp)
	}

	waitForPortalRun(t, portalURL, 1, func(run portalRun) bool {
		return run.Kind == "completed" && run.Status == "aborted"
	})

	issue2Run := waitForPortalRun(t, portalURL, 2, func(run portalRun) bool {
		return run.Kind == "active" || (run.Kind == "completed" && run.Status == "success")
	})
	if issue2Run.Kind == "completed" && issue2Run.Status != "success" {
		t.Fatalf("expected issue 2 to stay active or finish successfully, got %#v", issue2Run)
	}

	eventLog := &events.JSONLLogger{Path: filepath.Join(repoDir, ".sandman", "events.jsonl")}
	eventsWritten, err := eventLog.Read()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var aborted []events.Event
	for _, event := range eventsWritten {
		if event.Type == "run.aborted" {
			aborted = append(aborted, event)
		}
	}
	if len(aborted) != 1 {
		t.Fatalf("expected exactly one run.aborted event, got %v", aborted)
	}
	if aborted[0].Issue != 1 {
		t.Fatalf("expected run.aborted for issue 1, got %#v", aborted[0])
	}
}

func TestPortal_E2E_AbortReturns404ForUnknownRun(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("skip e2e in CI")
	}
	if !portalAbortSupported() {
		t.Skip("skip abort e2e on unsupported platform")
	}

	binPath := buildSandmanBinary(t)

	repoDir := shortTempDir(t)
	t.Chdir(repoDir)
	initRunIntegrationRepoWithRemote(t, repoDir)
	writeAbortE2EConfig(t, repoDir)

	ghShimDir := shortTempDir(t)
	writeFakeGHShim(t, ghShimDir)
	prependPath(t, ghShimDir)

	portalURL := startPortalBinary(t, binPath, repoDir, ghShimDir)
	waitForPortalReady(t, portalURL)

	statusCode, body := writeAbortRequest(t, portalURL, "run-does-not-exist", 42)
	if statusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", statusCode, body)
	}
	if len(body) == 0 {
		t.Fatal("expected non-empty 404 response body")
	}
	if !strings.Contains(string(body), "run-does-not-exist") {
		t.Fatalf("expected 404 body to mention missing run, got %s", body)
	}
}

func writeAbortE2EConfig(t *testing.T, repoDir string) {
	t.Helper()

	configPath := filepath.Join(repoDir, ".sandman", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	configYAML := []byte("default_agent: opencode\nsandbox: worktree\ngit:\n  base_branch: main\n")
	if err := os.WriteFile(configPath, configYAML, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "sm-")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func writeBlockingOpencodeShim(t *testing.T, dir string) {
	t.Helper()

	script := `#!/bin/sh
set -eu

repo_root=$(dirname "$(dirname "$(dirname "$(dirname "$PWD")")")")

case "$*" in
  *"Implement GitHub issue #1"*)
    child=0
    trap 'if [ "$child" -ne 0 ]; then kill "$child" >/dev/null 2>&1 || true; fi; exit 130' INT
    mkdir -p "$repo_root/.sandman/logs"
    cat > "$repo_root/.sandman/logs/1.log" <<'EOF'
--- run 0 ---
# Todos
- [ ] fake opencode issue 1 still running
EOF
    sleep 600 &
    child=$!
    wait "$child"
    ;;
  *"Implement GitHub issue #2"*)
    exec sleep 600
    ;;
  *)
    exec sleep 600
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0755); err != nil {
		t.Fatalf("write blocking opencode shim: %v", err)
	}
}

func startSandmanRun(t *testing.T, binPath, repoDir, shimDir string, args ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(binPath, args...)
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_TOKEN=fake",
		"GITHUB_TOKEN=fake",
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sandman run: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		t.Logf("sandman run stdout:\n%s", stdout.String())
		t.Logf("sandman run stderr:\n%s", stderr.String())
	})
	return cmd
}

func waitForPortalRunCountAndStatus(t *testing.T, baseURL string, want int, wantKind string) []portalRun {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		runs := fetchPortalRuns(t, baseURL)
		if len(runs) >= want {
			allMatch := true
			for _, run := range runs {
				if run.Kind != wantKind {
					allMatch = false
					break
				}
			}
			if allMatch {
				return runs
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	runs := fetchPortalRuns(t, baseURL)
	t.Fatalf("expected %d runs with kind %q, got %#v", want, wantKind, runs)
	return nil
}

func waitForPortalRun(t *testing.T, baseURL string, issue int, match func(portalRun) bool) portalRun {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, run := range fetchPortalRuns(t, baseURL) {
			if run.IssueNumber == issue && match(run) {
				return run
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	runs := fetchPortalRuns(t, baseURL)
	t.Fatalf("timed out waiting for issue %d to match; runs=%#v", issue, runs)
	return portalRun{}
}

func portalRunsByIssue(runs []portalRun) map[int]portalRun {
	byIssue := make(map[int]portalRun, len(runs))
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}
	return byIssue
}

func writeAbortRequest(t *testing.T, baseURL, runKey string, issue int) (int, []byte) {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"runKey": runKey,
		"issue":  issue,
	})
	if err != nil {
		t.Fatalf("marshal abort request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/runs/abort", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create abort request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send abort request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read abort response: %v", err)
	}
	return resp.StatusCode, body
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
