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

	"github.com/rafaelromao/sandman/internal/daemon"
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

	handler := newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil)
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
	batchStartedAt := time.Now().Add(-10 * time.Minute)

	activeSock := filepath.Join(repoRoot, ".sandman", "runs", "run-1-100", "run.sock")
	if err := os.MkdirAll(filepath.Dir(activeSock), 0755); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteManifest(filepath.Dir(activeSock), daemon.BatchManifest{Issues: []int{1, 2, 3, 4}, CreatedAt: batchStartedAt}); err != nil {
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
		_, _ = conn.Write([]byte("[issue-1] 12:00:00 live output\n[issue-2] 12:00:00 hidden output\n"))
		_ = conn.Close()
	}()

	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: batchStartedAt.Add(1 * time.Minute), RunID: "run-1", Issue: 1, Payload: map[string]any{"branch": "sandman/1-fix"}},
		{Type: "run.started", Timestamp: batchStartedAt.Add(2 * time.Minute), RunID: "run-2", Issue: 2, Payload: map[string]any{"branch": "sandman/2-fix"}},
		{Type: "run.finished", Timestamp: batchStartedAt.Add(3 * time.Minute), RunID: "run-2", Issue: 2, Payload: map[string]any{"status": "success", "branch": "sandman/2-fix"}},
		{Type: "run.blocked", Timestamp: batchStartedAt.Add(4 * time.Minute), RunID: "run-3", Issue: 3, Payload: map[string]any{"blocked_by": []int{99}}},
		{Type: "run.started", Timestamp: batchStartedAt.Add(-30 * time.Minute), RunID: "run-9", Issue: 9, Payload: map[string]any{"branch": "sandman/9-fix"}},
		{Type: "run.finished", Timestamp: batchStartedAt.Add(-25 * time.Minute), RunID: "run-9", Issue: 9, Payload: map[string]any{"status": "success", "branch": "sandman/9-fix"}},
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
	if err := os.WriteFile(filepath.Join(repoRoot, ".sandman", "logs", "9.log"), []byte("issue nine log\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runs, err := loadPortalRuns(repoRoot)
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 5 {
		t.Fatalf("expected 5 runs, got %#v", runs)
	}
	byIssue := map[int]portalRun{}
	for _, run := range runs {
		byIssue[run.IssueNumber] = run
	}
	if run := byIssue[1]; run.Kind != "active" || run.IssueLabel != "#1" || run.Status != "active" {
		t.Fatalf("unexpected active run: %#v", run)
	}
	if run := byIssue[1]; !strings.Contains(run.Log, "live output") || strings.Contains(run.Log, "hidden output") || strings.Contains(run.Log, "\x1b[") {
		t.Fatalf("expected filtered active log, got %#v", run.Log)
	}
	if run := byIssue[2]; run.Status != "success" || run.Kind != "active" || !strings.Contains(run.Log, "issue two log") {
		t.Fatalf("unexpected completed run: %#v", run)
	}
	if run := byIssue[3]; run.Status != "blocked" || !strings.Contains(run.Log, "Blocked by #99.") {
		t.Fatalf("unexpected blocked run: %#v", run)
	}
	if run := byIssue[4]; run.Status != "queued" || !strings.Contains(run.Log, "Queued. Waiting to start.") {
		t.Fatalf("unexpected queued run: %#v", run)
	}
	if run := byIssue[9]; run.Status != "success" || run.Kind != "completed" || !strings.Contains(run.Log, "issue nine log") {
		t.Fatalf("unexpected historical completed run: %#v", run)
	}
}

func TestPortal_LoadPortalRunsTreatsCancelledAsTerminalFailure(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(-10 * time.Minute)
	writePortalLog(t, filepath.Join(repoRoot, ".sandman", "events.jsonl"), []events.Event{
		{Type: "run.started", Timestamp: startedAt, RunID: "run-42", Issue: 42, Payload: map[string]any{"branch": "sandman/42-fix"}},
		{Type: "run.cancelled", Timestamp: startedAt.Add(1 * time.Minute), RunID: "run-42", Issue: 42, Payload: map[string]any{"status": "failure", "branch": "sandman/42-fix"}},
	})

	runs, err := loadPortalRuns(repoRoot)
	if err != nil {
		t.Fatalf("load portal runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %#v", runs)
	}
	run := runs[0]
	if run.Kind != "completed" || run.Status != "failure" {
		t.Fatalf("expected cancelled run to project as completed failure, got %#v", run)
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

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
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

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
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
	for _, want := range []string{"Active only", "Log", "Events", "Details", "Download log", "settings-toggle", "theme-picker", "poll-interval", "Repo", "Updated", "Catppuccin Latte", "Catppuccin Frappe", "Catppuccin Macchiato", "Catppuccin Mocha", "Tokyo Night", "Gruvbox", "Everforest", "Nord", "Dracula", "Rose Pine", "Tokyo Night Day", "Everforest Light", "Solarized Light", "Nord Light", "GitHub Light", `const apiPath = "\/api\/runs";`, `data-action="toggle-run" data-run-key="`} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:min(800, len(content))])
		}
	}
	if strings.Contains(content, ">Output<") {
		t.Fatalf("page should not expose Output tab\n%s", content[:min(800, len(content))])
	}
}

func TestPortal_DetailPanelHasFixedHeightWithScroll(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
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

	if !strings.Contains(content, ".detail-panel") {
		t.Fatalf("page missing .detail-panel CSS block")
	}
	if !strings.Contains(content, "max-height: clamp(420px, 68vh, 780px)") {
		t.Fatalf(".detail-panel missing max-height clamp to match min-height")
	}
	if !strings.Contains(content, ".detail-panel { min-height: 0; max-height: none; }") {
		t.Fatalf(".detail-panel missing max-height:none at 960px breakpoint")
	}
}

func TestPortal_SyntaxHighlightingHasNoSizeCutoff(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
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
	if strings.Contains(content, "12000") {
		t.Fatalf("page should not contain old char limit cutoff (12000)")
	}
	if strings.Contains(content, "value.length > 12000") {
		t.Fatalf("page should not contain old size cutoff condition")
	}
}

func TestPortal_PageExposesCommandPanelShell(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
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
	for _, want := range []string{
		"Commands",
		`id="commands-toggle"`,
		`id="commands-panel"`,
		`id="command-picker"`,
		`id="command-panel-form"`,
		`id="command-panel-body"`,
		`id="command-execute-status"`,
		`value="run"`,
		`value="continue"`,
		`value="status"`,
		`value="history"`,
		`value="clean"`,
		`value="config"`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing %q\n%s", want, content[:min(1000, len(content))])
		}
	}
	if strings.Contains(content, "class=\"launcher\"") {
		t.Fatalf("expected launcher section to be removed\n%s", content[:min(1000, len(content))])
	}
}

func TestPortal_CommandsEndpointPersistsAsyncLaunches(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	prevRun := portalStartRun
	prevStart := portalStartCommand
	defer func() {
		portalStartRun = prevRun
		portalStartCommand = prevStart
	}()
	var gotRunArgs []string
	var gotStartArgs []string
	portalStartRun = func(ctx context.Context, repoRoot string, args []string) error {
		gotRunArgs = append([]string(nil), args...)
		return nil
	}
	portalStartCommand = func(ctx context.Context, repoRoot string, args []string) error {
		gotStartArgs = append([]string(nil), args...)
		return nil
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
	defer server.Close()

	t.Run("run", func(t *testing.T) {
		gotRunArgs = nil
		gotStartArgs = nil
		body := strings.NewReader(`{"command":"run","issues":"42","prompt":"finish the tests"}`)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/commands", body)
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
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d: %s", resp.StatusCode, data)
		}
		if len(gotRunArgs) == 0 || gotRunArgs[0] != "run" || !strings.Contains(strings.Join(gotRunArgs, " "), "42") {
			t.Fatalf("unexpected run args: %#v", gotRunArgs)
		}
		if len(gotStartArgs) != 0 {
			t.Fatalf("expected subcommand launcher to stay idle, got %#v", gotStartArgs)
		}

		commands := readPortalCommands(t, server.URL)
		runCmds := filterCommandsByPrefix(commands, "sandman run ")
		if len(runCmds) == 0 {
			t.Fatalf("expected run command persisted, got %#v", commands)
		}
	})

	t.Run("continue", func(t *testing.T) {
		gotRunArgs = nil
		gotStartArgs = nil
		body := strings.NewReader(`{"command":"continue","issue":42,"prompt":"finish the tests"}`)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/commands", body)
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
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d: %s", resp.StatusCode, data)
		}
		if len(gotRunArgs) == 0 || gotRunArgs[0] != "continue" || !strings.Contains(strings.Join(gotRunArgs, " "), "42") {
			t.Fatalf("unexpected run args: %#v", gotRunArgs)
		}
		if len(gotStartArgs) != 0 {
			t.Fatalf("expected subcommand launcher to stay idle, got %#v", gotStartArgs)
		}

		commands := readPortalCommands(t, server.URL)
		continueCmds := filterCommandsByPrefix(commands, "sandman continue ")
		if len(continueCmds) == 0 {
			t.Fatalf("expected continue command persisted, got %#v", commands)
		}
	})

	server.Close()
	server = startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
	defer server.Close()

	reloaded := readPortalCommands(t, server.URL)
	if len(reloaded) < 2 {
		t.Fatalf("expected persisted commands after restart, got %#v", reloaded)
	}
}

func filterCommandsByPrefix(commands []portalCommandRecord, prefix string) []portalCommandRecord {
	var out []portalCommandRecord
	for _, c := range commands {
		if strings.HasPrefix(c.Command, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestPortal_CommandsEndpointPersistsPresetLaunches(t *testing.T) {
	prevStart := portalStartCommand
	defer func() { portalStartCommand = prevStart }()

	cases := []struct {
		name string
		body string
		want []string
	}{
		{name: "status", body: `{"command":"status"}`, want: []string{"status"}},
		{name: "history", body: `{"command":"history"}`, want: []string{"history"}},
		{name: "clean", body: `{"command":"clean","cleanMode":"failed","confirmed":true}`, want: []string{"clean", "--failed"}},
		{name: "config", body: `{"command":"config","configMode":"set","configKey":"default_agent","configValue":"pi"}`, want: []string{"config", "set", "default_agent", "pi"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
				t.Fatal(err)
			}

			prevStart := portalStartCommand
			t.Cleanup(func() { portalStartCommand = prevStart })

			var gotArgs []string
			portalStartCommand = func(ctx context.Context, repoRoot string, args []string) error {
				gotArgs = append([]string(nil), args...)
				return nil
			}

			server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
			defer server.Close()

			req, err := http.NewRequest(http.MethodPost, server.URL+"/api/commands", strings.NewReader(tc.body))
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
				data, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 201, got %d: %s", resp.StatusCode, data)
			}
			if !sameStrings(gotArgs, tc.want) {
				t.Fatalf("unexpected launch args: %#v", gotArgs)
			}

			commands := readPortalCommands(t, server.URL)
			if len(commands) != 1 {
				t.Fatalf("expected 1 persisted command, got %#v", commands)
			}
			if commands[0].Command != strings.Join(append([]string{"sandman"}, tc.want...), " ") {
				t.Fatalf("unexpected command record: %#v", commands[0])
			}
		})
	}
}

func TestPortal_CommandsEndpointRejectsLaunchRoute(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/launch", "application/json", strings.NewReader(`{"command":"run"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestPortal_CommandsEndpointReturnsJSONErrors(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
	defer server.Close()

	t.Run("invalid payload", func(t *testing.T) {
		resp, err := http.Post(server.URL+"/api/commands", "application/json", strings.NewReader(`not json`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("expected application/json, got %q", ct)
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Error == "" {
			t.Fatal("expected non-empty error message")
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		resp, err := http.Post(server.URL+"/api/commands", "application/json", strings.NewReader(`{"command":"nope"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("expected application/json, got %q", ct)
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Error != "unknown command" {
			t.Fatalf("expected 'unknown command', got %q", body.Error)
		}
	})

	t.Run("launch failure", func(t *testing.T) {
		prevStart := portalStartCommand
		t.Cleanup(func() { portalStartCommand = prevStart })
		portalStartCommand = func(ctx context.Context, repoRoot string, args []string) error {
			return fmt.Errorf("exec: not found")
		}

		resp, err := http.Post(server.URL+"/api/commands", "application/json", strings.NewReader(`{"command":"status"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("expected application/json, got %q", ct)
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Error != "exec: not found" {
			t.Fatalf("expected 'exec: not found', got %q", body.Error)
		}
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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

	server := startPortalHTTPServer(t, newPortalHandler(repoRoot, portalLaunchDataFromConfig(nil), nil))
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
		errCh <- runPortalServer(ctx, t.TempDir(), port, out, portalLaunchFormData{}, nil)
	}()

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "bind portal on 0.0.0.0") {
			t.Fatalf("expected bind error on wildcard bind, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bind failure")
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
		done <- runPortalServer(ctx, repoRoot, 0, out, portalLaunchFormData{}, nil)
	}()

	select {
	case <-out.ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for portal startup")
	}

	match := regexp.MustCompile(`http://0\.0\.0\.0:(\d+)`).FindStringSubmatch(out.String())
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
