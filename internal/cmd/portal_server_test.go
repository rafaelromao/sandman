package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

type portalTestOutput struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func newPortalTestOutput() *portalTestOutput {
	return &portalTestOutput{ready: make(chan struct{})}
}

func (o *portalTestOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.buf.Write(p)
	o.once.Do(func() { close(o.ready) })
	return n, err
}

func (o *portalTestOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func TestPortal_FindsRepoRootFromSubdir(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(repoRoot, "nested", "dir")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}

	found, err := findRepoRoot(subdir)
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	if found != repoRoot {
		t.Fatalf("expected repo root %q, got %q", repoRoot, found)
	}
}

func TestPortal_APIRescansRunsOnEachRequest(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	createUnixRunSocket(t, filepath.Join(repoRoot, ".sandman", "runs", "run-1", "run.sock"))

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	first := readPortalInstances(t, server.URL)
	if len(first) != 1 || first[0].Name != "run-1" {
		t.Fatalf("expected 1 run-1 instance, got %#v", first)
	}

	createUnixRunSocket(t, filepath.Join(repoRoot, ".sandman", "runs", "run-2", "run.sock"))

	second := readPortalInstances(t, server.URL)
	if len(second) != 2 {
		t.Fatalf("expected 2 instances after late start, got %#v", second)
	}
	if second[1].Name != "run-2" {
		t.Fatalf("expected late-starting run-2 to appear on next poll, got %#v", second)
	}
}

func TestPortal_IgnoresNonSocketRunFiles(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "runs", "run-file"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "runs", "run-file", "run.sock"), []byte("not a socket"), 0644); err != nil {
		t.Fatal(err)
	}

	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		t.Fatalf("discover instances: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("expected no instances for regular file, got %#v", instances)
	}
}

func TestPortal_LoadPortalRunsMergesActiveAndCompletedRuns(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	activeSock := filepath.Join(repoRoot, ".sandman", "runs", "run-1-100", "run.sock")
	if err := os.MkdirAll(filepath.Dir(activeSock), 0755); err != nil {
		t.Fatal(err)
	}
	activeLn, err := net.Listen("unix", activeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = activeLn.Close() })
	go func() {
		conn, err := activeLn.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("\x1b[0mlive output\n"))
		_ = conn.Close()
	}()

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.started", Timestamp: time.Now().Add(-20 * time.Minute), RunID: "run-2", Issue: 2, Payload: map[string]any{"branch": "sandman/2-fix"}},
		{Type: "run.finished", Timestamp: time.Now().Add(-15 * time.Minute), RunID: "run-2", Issue: 2, Payload: map[string]any{"status": "success", "branch": "sandman/2-fix"}},
		{Type: "run.continued", Timestamp: time.Now().Add(-12 * time.Minute), RunID: "run-3", Issue: 3, Payload: map[string]any{"branch": "sandman/3-fix"}},
		{Type: "run.finished", Timestamp: time.Now().Add(-8 * time.Minute), RunID: "run-3", Issue: 3, Payload: map[string]any{"status": "success", "branch": "sandman/3-fix"}},
	})

	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "1.log"), []byte("issue one log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "2.log"), []byte("\x1b[0missue two log\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "3.log"), []byte("continued run log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runs, err := loadPortalRuns(repoRoot)
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %#v", runs)
	}
	byID := map[string]portalRun{}
	for _, run := range runs {
		byID[run.RunID] = run
	}
	if run := byID["run-1"]; run.Kind != "active" || run.IssueLabel != "#1" {
		t.Fatalf("unexpected active run: %#v", run)
	}
	if run := byID["run-1"]; !strings.Contains(run.Output, "live output") || strings.Contains(run.Output, "\x1b[") {
		t.Fatalf("expected active run output, got %#v", run.Output)
	}
	if run := byID["run-2"]; run.Status != "success" || run.Kind != "completed" {
		t.Fatalf("unexpected completed run: %#v", run)
	}
	if run := byID["run-3"]; run.Status != "success" || run.Kind != "completed" || run.IssueLabel != "#3" || run.Branch != "sandman/3-fix" {
		t.Fatalf("unexpected continued run: %#v", run)
	}
	if run := byID["run-3"]; !strings.Contains(run.Log, "continued run log") {
		t.Fatalf("expected continued run log, got %#v", run.Log)
	}
}

func TestPortal_RunsEndpointIncludesContinuedRun(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: time.Now().Add(-25 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: time.Now().Add(-20 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
		{Type: "run.continued", Timestamp: time.Now().Add(-15 * time.Minute), RunID: "run-2", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.finished", Timestamp: time.Now().Add(-10 * time.Minute), RunID: "run-2", Issue: 1, Payload: map[string]any{"status": "success", "branch": "sandman/1-fix"}},
	})
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "1.log"), []byte("continued run log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	runs := readPortalRuns(t, server.URL)
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %#v", runs)
	}
	byID := map[string]portalRun{}
	for _, run := range runs {
		byID[run.RunID] = run
	}
	continued, ok := byID["run-2"]
	if !ok {
		t.Fatal("expected continued run in API response")
	}
	if continued.IssueLabel != "#1" || continued.Status != "success" || continued.Kind != "completed" {
		t.Fatalf("unexpected continued run payload: %#v", continued)
	}
	if continued.Branch != "sandman/1-fix" || !strings.Contains(continued.Log, "continued run log") {
		t.Fatalf("expected continued run metadata, got %#v", continued)
	}
}

func TestPortal_PageExposesFiltersAndTabs(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{"Active only", "Output", "Log", "Events", "Details", "Download log", "settings-toggle", "theme-picker", "poll-interval", "Repo", "Updated", "Catppuccin Latte", "Catppuccin Frappe", "Catppuccin Macchiato", "Catppuccin Mocha", "Tokyo Night", "Gruvbox", "Everforest", "Nord", "Dracula", "Rose Pine", "Tokyo Night Day", "Everforest Light", "Solarized Light", "Nord Light", "GitHub Light", `const apiPath = "\/api\/runs";`, `data-action="toggle-run" data-run-key="`} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:min(800, len(content))])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestPortal_DownloadsLogFiles(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".sandman", "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(repoRoot, ".sandman", "logs", "1.log")
	if err := os.WriteFile(logPath, []byte("full log\nline two\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	href := "/api/logs?path=" + url.QueryEscape(filepath.Join(".sandman", "logs", "1.log"))
	resp, err := http.Get(server.URL + href)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Fatalf("expected attachment download, got %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "full log\nline two\n" {
		t.Fatalf("unexpected log body: %q", string(body))
	}
}

func TestPortal_CommandLauncherPersistsAndReturnsLiveOutput(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	record := launchPortalCommand(t, server.URL, `sh -lc 'printf "hello\n"; sleep 20'`)
	if record.Status != "running" {
		t.Fatalf("expected running command, got %#v", record)
	}

	waitForPortalCommand(t, server.URL, record.ID, func(cmd portalCommandRecord) bool {
		return strings.Contains(cmd.Output, "hello")
	})

	server.Close()
	server = startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	commands := readPortalCommands(t, server.URL)
	if len(commands) != 1 {
		t.Fatalf("expected 1 persisted command, got %#v", commands)
	}
	if commands[0].ID != record.ID || !strings.Contains(commands[0].Output, "hello") {
		t.Fatalf("expected persisted command output, got %#v", commands[0])
	}

	stopPortalCommand(t, server.URL, record.ID)
	waitForPortalCommand(t, server.URL, record.ID, func(cmd portalCommandRecord) bool {
		return cmd.Status == "stopped"
	})
}

func TestPortal_CommandLauncherStopsAndRelaunchesCommands(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	record := launchPortalCommand(t, server.URL, `sh -lc 'trap "exit 0" TERM; printf "running\n"; while :; do sleep 1; done'`)
	stopPortalCommand(t, server.URL, record.ID)

	stopped := waitForPortalCommand(t, server.URL, record.ID, func(cmd portalCommandRecord) bool {
		return cmd.Status == "stopped"
	})
	if stopped.FinishedAt == nil {
		t.Fatalf("expected stopped command to have finished timestamp: %#v", stopped)
	}

	relaunched := relaunchPortalCommand(t, server.URL, record.ID)
	if relaunched.ID == record.ID {
		t.Fatalf("expected relaunch to create new record, got %#v", relaunched)
	}
	if relaunched.RelaunchOf != record.ID {
		t.Fatalf("expected relaunch link to original command, got %#v", relaunched)
	}
	if relaunched.Status != "running" {
		t.Fatalf("expected relaunched command to be running, got %#v", relaunched)
	}

	stopPortalCommand(t, server.URL, relaunched.ID)
	waitForPortalCommand(t, server.URL, relaunched.ID, func(cmd portalCommandRecord) bool {
		return cmd.Status == "stopped"
	})
}

func TestPortal_BindsToLocalhostAndFailsWhenPortBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	port := busy.Addr().(*net.TCPAddr).Port
	out := newPortalTestOutput()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runPortalServer(ctx, t.TempDir(), port, out)
	}()

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "bind portal on 127.0.0.1") {
			t.Fatalf("expected bind error on wildcard bind, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bind failure")
	}
}

func TestPortal_PageExposesLauncherSection(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	for _, want := range []string{"Launcher", "Start command", "launcher-toggle", "sandman.portal.launcher.collapsed", "commandsApiPath", "/api/commands"} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:min(1000, len(content))])
		}
	}
}

func TestPortal_PrintListeningURL(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := newPortalTestOutput()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runPortalServer(ctx, repoRoot, 0, out)
	}()

	select {
	case <-out.ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for portal startup")
	}

	match := regexp.MustCompile(`http://127\.0\.0\.1:(\d+)`).FindStringSubmatch(out.String())
	if len(match) != 2 {
		cancel()
		t.Fatalf("startup output missing listening URL: %q", out.String())
	}
	port, err := strconv.Atoi(match[1])
	if err != nil {
		cancel()
		t.Fatalf("parse startup port: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/instances", port))
	if err != nil {
		cancel()
		t.Fatalf("portal request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("expected 200 from portal, got %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected portal stop error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for portal shutdown")
	}
}

func createUnixRunSocket(t *testing.T, sockPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func writePortalLog(t *testing.T, path string, entries []events.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	log := &events.JSONLLogger{Path: path}
	for _, entry := range entries {
		if err := log.Log(entry); err != nil {
			t.Fatal(err)
		}
	}
}

func readPortalInstances(t *testing.T, baseURL string) []portalInstance {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Instances []portalInstance `json:"instances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Instances
}

func readPortalRuns(t *testing.T, baseURL string) []portalRun {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Runs []portalRun `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Runs
}

func readPortalCommands(t *testing.T, baseURL string) []portalCommandRecord {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/commands")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Commands []portalCommandRecord `json:"commands"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Commands
}

func launchPortalCommand(t *testing.T, baseURL, command string) portalCommandRecord {
	t.Helper()
	body := strings.NewReader(`{"command":` + strconv.Quote(command) + `}`)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/commands", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var record portalCommandRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		t.Fatal(err)
	}
	return record
}

func stopPortalCommand(t *testing.T, baseURL, id string) portalCommandRecord {
	t.Helper()
	return portalCommandAction(t, baseURL, id, "stop")
}

func relaunchPortalCommand(t *testing.T, baseURL, id string) portalCommandRecord {
	t.Helper()
	return portalCommandAction(t, baseURL, id, "relaunch")
}

func portalCommandAction(t *testing.T, baseURL, id, action string) portalCommandRecord {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/commands/"+url.PathEscape(id)+"/"+action, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var record portalCommandRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		t.Fatal(err)
	}
	return record
}

func waitForPortalCommand(t *testing.T, baseURL, id string, predicate func(portalCommandRecord) bool) portalCommandRecord {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, record := range readPortalCommands(t, baseURL) {
			if record.ID == id && predicate(record) {
				return record
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for command %s", id)
	return portalCommandRecord{}
}

type portalHTTPServer struct {
	URL   string
	Close func()
}

func startPortalHTTPServer(t *testing.T, handler http.Handler) *portalHTTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(ln) }()
	closeFn := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
	t.Cleanup(closeFn)
	return &portalHTTPServer{URL: "http://" + ln.Addr().String(), Close: closeFn}
}
