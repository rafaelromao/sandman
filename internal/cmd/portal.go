package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/spf13/cobra"
)

const (
	portalPollInterval = 2 * time.Second
	portalReadLimit    = 64 * 1024
	portalReadTimeout  = 250 * time.Millisecond
)

type portalInstance struct {
	Name       string `json:"name"`
	SocketPath string `json:"socketPath"`
}

type portalEvent struct {
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Payload   map[string]any `json:"payload,omitempty"`
}

type portalRun struct {
	Key         string        `json:"key"`
	RunID       string        `json:"runId"`
	Kind        string        `json:"kind"`
	Status      string        `json:"status"`
	IssueLabel  string        `json:"issueLabel"`
	IssueNumber int           `json:"issueNumber,omitempty"`
	Branch      string        `json:"branch,omitempty"`
	StartedAt   time.Time     `json:"startedAt"`
	FinishedAt  *time.Time    `json:"finishedAt,omitempty"`
	Duration    string        `json:"duration,omitempty"`
	SocketPath  string        `json:"socketPath,omitempty"`
	LogPath     string        `json:"logPath,omitempty"`
	Output      string        `json:"output,omitempty"`
	Log         string        `json:"log,omitempty"`
	Events      []portalEvent `json:"events,omitempty"`
}

type portalActiveRun struct {
	Key         string
	SocketPath  string
	IssueNumber int
	ModTime     time.Time
}

// NewPortalCmd creates the portal command.
func NewPortalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "portal",
		Short: "Serve a local portal for current Sandman runs",
		Long:  "Serve a localhost portal for the current repository and poll .sandman/runs for live Sandman instances.",
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := cmd.Flags().GetInt("port")
			if err != nil {
				return err
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
			repoRoot, err := findRepoRoot(cwd)
			if err != nil {
				return err
			}

			ctx, stop := signalContext(cmd.Context())
			defer stop()

			return runPortalServer(ctx, repoRoot, port, cmd.OutOrStdout())
		},
	}

	cmd.Flags().Int("port", 5000, "Port to bind on 127.0.0.1")
	return cmd
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigCh)
		close(sigCh)
	}()
	return ctx, cancel
}

func runPortalServer(ctx context.Context, repoRoot string, port int, out io.Writer) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("bind portal on 127.0.0.1:%d: %w", port, err)
	}
	defer listener.Close()

	tcpAddr, _ := listener.Addr().(*net.TCPAddr)
	actualPort := port
	if tcpAddr != nil {
		actualPort = tcpAddr.Port
	}

	if _, err := fmt.Fprintf(out, "Portal listening on http://127.0.0.1:%d\n", actualPort); err != nil {
		return fmt.Errorf("write portal address: %w", err)
	}

	server := &http.Server{Handler: newPortalHandler(repoRoot)}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		err := <-errCh
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve portal: %w", err)
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve portal: %w", err)
	}
}

func newPortalHandler(repoRoot string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/instances", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		instances, err := discoverPortalInstances(repoRoot)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repoRoot":  repoRoot,
			"instances": instances,
		})
	})
	mux.HandleFunc("/api/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		runs, err := loadPortalRuns(repoRoot)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repoRoot": repoRoot,
			"runs":     runs,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		data := struct {
			RepoRoot       string
			PollInterval   int
			RunsPath       string
			InstancesPath  string
			RefreshPath    string
			PortalTitle    string
			PortalSubtitle string
		}{
			RepoRoot:       repoRoot,
			PollInterval:   int(portalPollInterval / time.Millisecond),
			RunsPath:       "/api/runs",
			InstancesPath:  "/api/instances",
			RefreshPath:    "/api/runs",
			PortalTitle:    "Sandman Portal",
			PortalSubtitle: "Merged active and completed runs for the current repository.",
		}
		_ = portalPageTemplate.Execute(w, data)
	})
	return mux
}

func findRepoRoot(start string) (string, error) {
	dir := start
	for {
		if hasGitMarker(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("locate repository root from %s: .git not found", start)
}

func hasGitMarker(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func discoverPortalInstances(repoRoot string) ([]portalInstance, error) {
	runsDir := filepath.Join(repoRoot, ".sandman", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs dir: %w", err)
	}

	instances := make([]portalInstance, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sockPath := filepath.Join(runsDir, entry.Name(), "run.sock")
		info, err := os.Lstat(sockPath)
		if err != nil || info.IsDir() || info.Mode()&os.ModeSocket == 0 {
			continue
		}
		instances = append(instances, portalInstance{Name: entry.Name(), SocketPath: sockPath})
	}

	sort.Slice(instances, func(i, j int) bool {
		return strings.Compare(instances[i].Name, instances[j].Name) < 0
	})
	return instances, nil
}

func loadPortalRuns(repoRoot string) ([]portalRun, error) {
	activeInstances, err := discoverPortalActiveRuns(repoRoot)
	if err != nil {
		return nil, err
	}

	eventLog := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}
	eventList, err := eventLog.Read()
	if err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}

	runStates := events.ProjectRunStates(eventList)
	eventsByRun := groupPortalEventsByRun(eventList)
	activeStates := make([]events.RunState, 0, len(runStates))
	completedStates := make([]events.RunState, 0, len(runStates))
	for _, run := range runStates {
		if run.IsActive() {
			activeStates = append(activeStates, run)
			continue
		}
		completedStates = append(completedStates, run)
	}

	matchedActive := matchPortalActiveRuns(activeInstances, activeStates)
	runs := make([]portalRun, 0, len(matchedActive)+len(completedStates))
	for _, match := range matchedActive {
		runs = append(runs, portalRunFromActiveMatch(repoRoot, match, eventsByRun))
	}
	for _, runState := range completedStates {
		runs = append(runs, portalRunFromState(repoRoot, runState, nil, eventsByRun))
	}

	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].Kind != runs[j].Kind {
			return runs[i].Kind == "active"
		}
		if runs[i].Kind == "active" {
			return runs[i].StartedAt.After(runs[j].StartedAt)
		}
		if runs[i].FinishedAt != nil && runs[j].FinishedAt != nil && !runs[i].FinishedAt.Equal(*runs[j].FinishedAt) {
			return runs[i].FinishedAt.After(*runs[j].FinishedAt)
		}
		if !runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].StartedAt.After(runs[j].StartedAt)
		}
		return runs[i].RunID > runs[j].RunID
	})

	for i := range runs {
		if runs[i].Output == "" && runs[i].Kind == "active" {
			runs[i].Output = "No live output captured yet."
		}
	}

	return runs, nil
}

func discoverPortalActiveRuns(repoRoot string) ([]portalActiveRun, error) {
	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		return nil, err
	}

	active := make([]portalActiveRun, 0, len(instances))
	for _, instance := range instances {
		info, err := os.Stat(instance.SocketPath)
		if err != nil {
			continue
		}
		issueNumber, _ := parseRunDirIssue(instance.Name)
		active = append(active, portalActiveRun{
			Key:         instance.Name,
			SocketPath:  instance.SocketPath,
			IssueNumber: issueNumber,
			ModTime:     info.ModTime(),
		})
	}
	return active, nil
}

func matchPortalActiveRuns(instances []portalActiveRun, activeStates []events.RunState) []portalRunMatch {
	used := make([]bool, len(activeStates))
	matches := make([]portalRunMatch, 0, len(instances))

	for _, instance := range instances {
		idx := matchPortalRunState(instance, activeStates, used)
		match := portalRunMatch{instance: instance}
		if idx >= 0 {
			used[idx] = true
			state := activeStates[idx]
			match.state = &state
		}
		matches = append(matches, match)
	}

	return matches
}

type portalRunMatch struct {
	instance portalActiveRun
	state    *events.RunState
}

func matchPortalRunState(instance portalActiveRun, states []events.RunState, used []bool) int {
	bestIdx := -1
	bestDelta := time.Duration(1<<63 - 1)

	for i := range states {
		if used[i] {
			continue
		}
		state := states[i]
		if instance.IssueNumber > 0 && state.IssueNumber() != instance.IssueNumber {
			continue
		}
		if instance.IssueNumber == 0 && state.IssueNumber() != 0 {
			continue
		}
		delta := instance.ModTime.Sub(state.Started.Timestamp)
		if delta < 0 {
			delta = -delta
		}
		if bestIdx == -1 || delta < bestDelta {
			bestIdx = i
			bestDelta = delta
		}
	}

	if bestIdx >= 0 {
		return bestIdx
	}

	for i := range states {
		if used[i] {
			continue
		}
		state := states[i]
		delta := instance.ModTime.Sub(state.Started.Timestamp)
		if delta < 0 {
			delta = -delta
		}
		if bestIdx == -1 || delta < bestDelta {
			bestIdx = i
			bestDelta = delta
		}
	}

	return bestIdx
}

func portalRunFromActiveMatch(repoRoot string, match portalRunMatch, eventsByRun map[string][]portalEvent) portalRun {
	if match.state != nil {
		return portalRunFromState(repoRoot, *match.state, &match.instance, eventsByRun)
	}

	startedAt := match.instance.ModTime
	issueLabel := "prompt-only"
	issueNumber := match.instance.IssueNumber
	if issueNumber > 0 {
		issueLabel = fmt.Sprintf("#%d", issueNumber)
	}
	logPath := portalLogPath(repoRoot, issueNumber, "")
	return portalRun{
		Key:         match.instance.Key,
		RunID:       match.instance.Key,
		Kind:        "active",
		Status:      "active",
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		StartedAt:   startedAt,
		Duration:    time.Since(startedAt).Round(time.Second).String(),
		SocketPath:  match.instance.SocketPath,
		LogPath:     logPath,
		Output:      readPortalSocketOutput(match.instance.SocketPath),
		Log:         readPortalTextFile(logPath),
		Events:      eventsByRun[match.instance.Key],
	}
}

func portalRunFromState(repoRoot string, runState events.RunState, active *portalActiveRun, eventsByRun map[string][]portalEvent) portalRun {
	runID := runState.RunID
	if runID == "" && active != nil {
		runID = active.Key
	}

	issueNumber := runState.IssueNumber()
	branch := runState.Branch()
	issueLabel := runState.IssueLabel()
	if issueLabel == "" {
		issueLabel = runID
	}

	status := runState.Status()
	if runState.IsActive() {
		status = "active"
	}
	startedAt := runState.Started.Timestamp
	var finishedAt *time.Time
	if runState.Finished != nil {
		finishedAt = &runState.Finished.Timestamp
	}

	logPath := portalLogPath(repoRoot, issueNumber, branch)
	output := ""
	if active != nil {
		output = readPortalSocketOutput(active.SocketPath)
	}

	portalRun := portalRun{
		Key:         runID,
		RunID:       runID,
		Kind:        kindForRun(runState),
		Status:      statusOrDefault(status, runState.IsActive()),
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		Branch:      branch,
		StartedAt:   startedAt,
		FinishedAt:  finishedAt,
		Duration:    durationForRun(runState),
		LogPath:     logPath,
		Output:      output,
		Log:         readPortalTextFile(logPath),
		Events:      eventsByRun[runID],
	}
	if active != nil {
		portalRun.SocketPath = active.SocketPath
	}
	return portalRun
}

func kindForRun(runState events.RunState) string {
	if runState.IsActive() {
		return "active"
	}
	return "completed"
}

func statusOrDefault(status string, active bool) string {
	status = strings.TrimSpace(status)
	if active {
		return "active"
	}
	if status == "" {
		return "completed"
	}
	return status
}

func durationForRun(runState events.RunState) string {
	if runState.IsActive() {
		return time.Since(runState.Started.Timestamp).Round(time.Second).String()
	}
	return runState.Duration().String()
}

func portalLogPath(repoRoot string, issueNumber int, branch string) string {
	logDir := filepath.Join(repoRoot, ".sandman", "logs")
	if issueNumber > 0 {
		return filepath.Join(logDir, fmt.Sprintf("%d.log", issueNumber))
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	return filepath.Join(logDir, sanitizePortalFilename(branch)+".log")
}

func sanitizePortalFilename(value string) string {
	value = strings.TrimSpace(value)
	value = strings.NewReplacer("/", "-", string(os.PathSeparator), "-", " ", "-").Replace(value)
	if value == "" {
		return "prompt-only"
	}
	return value
}

func groupPortalEventsByRun(eventsList []events.Event) map[string][]portalEvent {
	grouped := make(map[string][]portalEvent)
	for _, event := range eventsList {
		if event.RunID == "" {
			continue
		}
		grouped[event.RunID] = append(grouped[event.RunID], portalEvent{
			Type:      event.Type,
			Timestamp: event.Timestamp,
			Payload:   event.Payload,
		})
	}
	for runID := range grouped {
		sort.SliceStable(grouped[runID], func(i, j int) bool {
			return grouped[runID][i].Timestamp.Before(grouped[runID][j].Timestamp)
		})
	}
	return grouped
}

func readPortalTextFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > portalReadLimit {
		tail := data[len(data)-portalReadLimit:]
		return "[truncated]\n" + string(tail)
	}
	return string(data)
}

func readPortalSocketOutput(sockPath string) string {
	conn, err := net.DialTimeout("unix", sockPath, portalReadTimeout)
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(portalReadTimeout))

	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for buf.Len() < portalReadLimit {
		n, readErr := conn.Read(tmp)
		if n > 0 {
			remaining := portalReadLimit - buf.Len()
			if n > remaining {
				n = remaining
			}
			_, _ = buf.Write(tmp[:n])
		}
		if readErr != nil {
			if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
				break
			}
			break
		}
	}
	return buf.String()
}

func parseRunDirIssue(name string) (int, bool) {
	if !strings.HasPrefix(name, "run-") {
		return 0, false
	}
	parts := strings.Split(strings.TrimPrefix(name, "run-"), "-")
	if len(parts) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, false
	}
	return n, true
}

var portalPageTemplate = template.Must(template.New("portal").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.PortalTitle}}</title>
  <style>
    :root {
      color-scheme: light;
      --bg: oklch(0.975 0.006 240);
      --surface: oklch(0.99 0.004 240);
      --surface-2: oklch(0.955 0.008 240);
      --surface-3: oklch(0.93 0.012 240);
      --border: oklch(0.86 0.01 240);
      --text: oklch(0.23 0.015 240);
      --muted: oklch(0.5 0.015 240);
      --accent: oklch(0.57 0.13 245);
      --accent-weak: oklch(0.94 0.03 245);
      --success: oklch(0.58 0.13 150);
      --danger: oklch(0.58 0.13 28);
      --warning: oklch(0.7 0.1 85);
      --shadow: 0 1px 1px oklch(0.15 0.01 240 / 0.04), 0 10px 30px oklch(0.15 0.01 240 / 0.04);
    }
    * { box-sizing: border-box; }
    html, body { min-height: 100%; }
    body {
      margin: 0;
      font: 14px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
      color: var(--text);
      background:
        radial-gradient(circle at top, oklch(0.99 0.01 240), transparent 48%),
        linear-gradient(180deg, oklch(0.985 0.006 240), var(--bg));
    }
    a { color: inherit; }
    code, pre, .mono { font-family: ui-monospace, SFMono-Regular, SF Mono, Menlo, Consolas, monospace; }
    main {
      width: min(100%, 1400px);
      margin: 0 auto;
      padding: 24px 20px 28px;
    }
    .masthead {
      display: flex;
      justify-content: space-between;
      gap: 16px;
      align-items: end;
      padding-bottom: 18px;
      margin-bottom: 16px;
      border-bottom: 1px solid var(--border);
    }
    .eyebrow {
      margin: 0 0 4px;
      text-transform: uppercase;
      letter-spacing: 0.12em;
      font-size: 11px;
      color: var(--muted);
    }
    h1 {
      margin: 0;
      font-size: 28px;
      line-height: 1.1;
      letter-spacing: -0.03em;
    }
    .subtitle {
      margin: 8px 0 0;
      max-width: 72ch;
      color: var(--muted);
    }
    .meta {
      display: grid;
      gap: 4px;
      justify-items: end;
      text-align: right;
      color: var(--muted);
      font-size: 12px;
    }
    .toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      padding: 14px 16px;
      margin-bottom: 14px;
      border: 1px solid var(--border);
      border-radius: 16px;
      background: var(--surface);
      box-shadow: var(--shadow);
    }
    .filters {
      display: flex;
      flex-wrap: wrap;
      gap: 10px 12px;
      align-items: center;
    }
    .field {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      color: var(--muted);
      font-size: 12px;
      white-space: nowrap;
    }
    select, input[type="checkbox"] {
      accent-color: var(--accent);
    }
    select {
      min-width: 160px;
      border: 1px solid var(--border);
      border-radius: 10px;
      padding: 8px 10px;
      background: var(--surface-2);
      color: var(--text);
      font: inherit;
    }
    .status-pill {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      padding: 6px 10px;
      border-radius: 999px;
      border: 1px solid var(--border);
      background: var(--surface-2);
      color: var(--muted);
    }
    .status-pill strong { color: var(--text); }
    .status-pill .dot {
      width: 8px;
      height: 8px;
      border-radius: 999px;
      background: var(--accent);
    }
    .table-shell {
      border: 1px solid var(--border);
      border-radius: 18px;
      background: var(--surface);
      box-shadow: var(--shadow);
      overflow: auto;
    }
    table {
      width: 100%;
      min-width: 1040px;
      border-collapse: separate;
      border-spacing: 0;
    }
    thead th {
      position: sticky;
      top: 0;
      z-index: 1;
      background: var(--surface);
      color: var(--muted);
      text-transform: uppercase;
      letter-spacing: 0.08em;
      font-size: 11px;
      text-align: left;
      padding: 14px 14px;
      border-bottom: 1px solid var(--border);
    }
    tbody td {
      padding: 14px;
      border-bottom: 1px solid var(--border);
      vertical-align: top;
    }
    tbody tr.run-row:hover td {
      background: oklch(0.985 0.005 240);
    }
    tbody tr.run-row.active td {
      background: var(--accent-weak);
    }
    tbody tr.detail-row td {
      background: var(--surface-2);
      padding: 0;
    }
    .run-title {
      display: flex;
      flex-direction: column;
      gap: 4px;
    }
    .run-title .name {
      font-weight: 650;
      letter-spacing: -0.01em;
    }
    .run-title .meta-line,
    .muted {
      color: var(--muted);
    }
    .badge {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      padding: 4px 10px;
      border-radius: 999px;
      border: 1px solid var(--border);
      background: var(--surface-2);
      font-size: 12px;
      white-space: nowrap;
    }
    .badge .dot {
      width: 7px;
      height: 7px;
      border-radius: 999px;
      background: var(--muted);
    }
    .badge.active { background: var(--accent-weak); border-color: color-mix(in oklch, var(--accent) 25%, var(--border)); }
    .badge.active .dot { background: var(--accent); }
    .badge.success { background: color-mix(in oklch, var(--success) 12%, var(--surface)); border-color: color-mix(in oklch, var(--success) 24%, var(--border)); }
    .badge.success .dot { background: var(--success); }
    .badge.failure, .badge.error { background: color-mix(in oklch, var(--danger) 10%, var(--surface)); border-color: color-mix(in oklch, var(--danger) 24%, var(--border)); }
    .badge.failure .dot, .badge.error .dot { background: var(--danger); }
    .badge.warning { background: color-mix(in oklch, var(--warning) 12%, var(--surface)); border-color: color-mix(in oklch, var(--warning) 24%, var(--border)); }
    .badge.warning .dot { background: var(--warning); }
    .action-btn, .tab-btn {
      border: 1px solid var(--border);
      background: var(--surface-2);
      color: var(--text);
      font: inherit;
      border-radius: 10px;
      padding: 8px 10px;
      cursor: pointer;
      transition: background 160ms ease-out, border-color 160ms ease-out, color 160ms ease-out;
    }
    .action-btn:hover, .tab-btn:hover { background: var(--surface-3); }
    .action-btn:focus-visible, .tab-btn:focus-visible, select:focus-visible, input[type="checkbox"]:focus-visible {
      outline: 2px solid color-mix(in oklch, var(--accent) 70%, white);
      outline-offset: 2px;
    }
    .action-btn[aria-expanded="true"] {
      background: var(--accent);
      border-color: var(--accent);
      color: white;
    }
    .tabs {
      display: flex;
      flex-wrap: wrap;
      gap: 8px;
      margin-bottom: 12px;
    }
    .tab-btn[aria-pressed="true"] {
      background: var(--accent);
      border-color: var(--accent);
      color: white;
    }
    .detail-panel {
      padding: 16px;
      border-top: 1px solid var(--border);
    }
    .panel-grid {
      display: grid;
      grid-template-columns: minmax(0, 1fr) minmax(260px, 320px);
      gap: 16px;
      align-items: start;
    }
    .detail-box {
      border: 1px solid var(--border);
      border-radius: 14px;
      background: var(--surface);
      overflow: hidden;
    }
    .detail-box h3 {
      margin: 0;
      padding: 12px 14px 10px;
      border-bottom: 1px solid var(--border);
      font-size: 12px;
      text-transform: uppercase;
      letter-spacing: 0.08em;
      color: var(--muted);
    }
    pre {
      margin: 0;
      padding: 14px;
      white-space: pre-wrap;
      word-break: break-word;
      max-height: 420px;
      overflow: auto;
      line-height: 1.45;
    }
    .events-list {
      margin: 0;
      padding: 0;
      list-style: none;
      display: grid;
    }
    .events-list li {
      padding: 12px 14px;
      border-bottom: 1px solid var(--border);
    }
    .events-list li:last-child { border-bottom: 0; }
    .event-head {
      display: flex;
      justify-content: space-between;
      gap: 10px;
      align-items: baseline;
      margin-bottom: 8px;
      font-size: 12px;
    }
    .event-type { font-weight: 650; }
    .event-time { color: var(--muted); font-variant-numeric: tabular-nums; }
    .event-payload {
      margin: 0;
      white-space: pre-wrap;
      word-break: break-word;
      color: var(--muted);
      font-size: 12px;
    }
    .empty-state {
      padding: 28px 16px;
      text-align: center;
      color: var(--muted);
    }
    .detail-meta {
      display: grid;
      gap: 8px;
      min-width: 0;
    }
    .kv {
      display: grid;
      gap: 2px;
    }
    .kv span {
      color: var(--muted);
      font-size: 12px;
      text-transform: uppercase;
      letter-spacing: 0.08em;
    }
    .kv strong, .kv code {
      font-weight: 500;
      color: var(--text);
      word-break: break-word;
    }
    .error-banner {
      display: none;
      margin-bottom: 12px;
      padding: 12px 14px;
      border-radius: 14px;
      border: 1px solid color-mix(in oklch, var(--danger) 24%, var(--border));
      background: color-mix(in oklch, var(--danger) 8%, var(--surface));
      color: var(--text);
    }
    @media (max-width: 960px) {
      .masthead, .toolbar, .panel-grid {
        grid-template-columns: 1fr;
        display: grid;
      }
      .masthead { align-items: start; }
      .meta { justify-items: start; text-align: left; }
      .toolbar { justify-content: start; }
    }
    @media (max-width: 760px) {
      main { padding-inline: 12px; }
      h1 { font-size: 24px; }
      .toolbar { padding: 12px; }
      .table-shell { border-radius: 14px; }
      .detail-panel { padding: 12px; }
    }
  </style>
</head>
<body>
  <main>
    <header class="masthead">
      <div>
        <p class="eyebrow">Local portal</p>
        <h1>{{.PortalTitle}}</h1>
        <p class="subtitle">{{.PortalSubtitle}}</p>
      </div>
      <div class="meta">
        <div>Repo <code>{{.RepoRoot}}</code></div>
        <div>Polls <code>{{.PollInterval}}ms</code> via <code>{{.RunsPath}}</code></div>
        <div id="last-updated">Waiting for first refresh</div>
      </div>
    </header>

    <div id="error-banner" class="error-banner" role="status" aria-live="polite"></div>

    <section class="toolbar" aria-label="Run filters">
      <div class="filters">
        <label class="field">
          Status
          <select id="status-filter" aria-label="Filter runs by status">
            <option value="all">All statuses</option>
          </select>
        </label>
        <label class="field">
          <input id="active-only" type="checkbox">
          Active only
        </label>
      </div>
      <div id="summary-pill" class="status-pill"><span class="dot"></span><strong>0</strong> runs visible</div>
    </section>

    <section class="table-shell" aria-label="Sandman runs">
      <table>
        <thead>
          <tr>
            <th>Run</th>
            <th>Status</th>
            <th>Started</th>
            <th>Duration</th>
            <th>Branch</th>
            <th>Source</th>
            <th>Details</th>
          </tr>
        </thead>
        <tbody id="runs-body">
          <tr><td colspan="7"><div class="empty-state">Loading runs.</div></td></tr>
        </tbody>
      </table>
    </section>
  </main>

  <script>
    const apiPath = {{printf "%q" .RefreshPath}};
    const pollInterval = {{.PollInterval}};
    const statusFilter = document.getElementById('status-filter');
    const activeOnlyToggle = document.getElementById('active-only');
    const runsBody = document.getElementById('runs-body');
    const errorBanner = document.getElementById('error-banner');
    const summaryPill = document.getElementById('summary-pill');
    const lastUpdated = document.getElementById('last-updated');

    const state = {
      runs: [],
      expanded: new Set(),
      tabs: new Map(),
      selectedStatus: 'all',
      activeOnly: false,
      loading: true,
    };

    function escapeHTML(value) {
      return String(value)
        .replaceAll('&', '&amp;')
        .replaceAll('<', '&lt;')
        .replaceAll('>', '&gt;')
        .replaceAll('"', '&quot;')
        .replaceAll("'", '&#39;');
    }

    function formatTime(value) {
      if (!value) return '—';
      const date = new Date(value);
      if (Number.isNaN(date.getTime())) return '—';
      return new Intl.DateTimeFormat(undefined, {
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        month: 'short',
        day: '2-digit',
      }).format(date);
    }

    function formatDuration(value) {
      return value && String(value).trim() ? value : '—';
    }

    function formatBranch(run) {
      return run.branch && String(run.branch).trim() ? run.branch : '—';
    }

    function formatSource(run) {
      if (run.kind === 'active' && run.socketPath) return run.socketPath;
      if (run.logPath) return run.logPath;
      return '—';
    }

    function sortStatusValues(values) {
      return Array.from(values).sort((a, b) => {
        if (a === 'active') return -1;
        if (b === 'active') return 1;
        return a.localeCompare(b);
      });
    }

    function syncStatusOptions(runs) {
      const statuses = new Set(['all']);
      for (const run of runs) statuses.add(run.status || 'unknown');
      const current = statusFilter.value || state.selectedStatus || 'all';
      const nextStatuses = sortStatusValues(statuses);
      const currentOptions = Array.from(statusFilter.options).map((option) => option.value);
      const nextOptions = nextStatuses;
      if (currentOptions.length === nextOptions.length && currentOptions.every((value, index) => value === nextOptions[index])) {
        if (nextOptions.includes(current)) {
          statusFilter.value = current;
          state.selectedStatus = current;
        } else {
          statusFilter.value = 'all';
          state.selectedStatus = 'all';
        }
        return;
      }
      statusFilter.innerHTML = '';
      for (const value of nextOptions) {
        const option = document.createElement('option');
        option.value = value;
        option.textContent = value === 'all' ? 'All statuses' : value;
        statusFilter.appendChild(option);
      }
      statusFilter.value = nextOptions.includes(current) ? current : 'all';
      state.selectedStatus = statusFilter.value;
    }

    function shouldShowRun(run) {
      if (state.activeOnly && run.kind !== 'active') return false;
      if (state.selectedStatus !== 'all' && run.status !== state.selectedStatus) return false;
      return true;
    }

    function runStatusClass(run) {
      const status = (run.status || '').toLowerCase();
      if (run.kind === 'active') return 'active';
      if (status === 'success') return 'success';
      if (status === 'failure' || status === 'failed' || status === 'error') return 'failure';
      if (status === 'warning' || status === 'stale' || status === 'blocked') return 'warning';
      return status || 'default';
    }

    function renderStatusBadge(run) {
      const klass = runStatusClass(run);
      const label = run.kind === 'active' ? 'active' : (run.status || 'completed');
      return '<span class="badge ' + escapeHTML(klass) + '"><span class="dot"></span>' + escapeHTML(label) + '</span>';
    }

    function renderRunMeta(run) {
      const parts = [];
      if (run.runId) parts.push('ID ' + run.runId);
      if (run.issueLabel) parts.push(run.issueLabel);
      return parts.length ? parts.join(' · ') : 'Run';
    }

    function renderRunRow(run) {
      const isOpen = state.expanded.has(run.key);
      const detailsButtonLabel = isOpen ? 'Hide details' : 'Show details';
      const tabName = state.tabs.get(run.key) || 'output';
      const output = run.output && String(run.output).trim()
        ? run.output
        : (run.kind === 'active' ? 'No live output captured yet.' : 'Run completed. Open Log or Events for persisted details.');
      const log = run.log && String(run.log).trim() ? run.log : 'No log file yet.';
      const events = Array.isArray(run.events) ? run.events : [];

      return [
        '<tr class="run-row ' + escapeHTML(run.kind || '') + '" data-run-key="' + escapeHTML(run.key) + '">',
        '  <td>',
        '    <div class="run-title">',
        '      <span class="name">' + escapeHTML(run.issueLabel || run.key) + '</span>',
        '      <span class="meta-line mono">' + escapeHTML(renderRunMeta(run)) + '</span>',
        '    </div>',
        '  </td>',
        '  <td>' + renderStatusBadge(run) + '</td>',
        '  <td class="mono">' + escapeHTML(formatTime(run.startedAt)) + '</td>',
        '  <td class="mono">' + escapeHTML(formatDuration(run.duration)) + '</td>',
        '  <td class="mono">' + escapeHTML(formatBranch(run)) + '</td>',
        '  <td class="mono">' + escapeHTML(formatSource(run)) + '</td>',
        '  <td><button class="action-btn" type="button" data-action="toggle-run" aria-expanded="' + (isOpen ? 'true' : 'false') + '">' + escapeHTML(detailsButtonLabel) + '</button></td>',
        '</tr>',
        isOpen ? (
          '<tr class="detail-row" data-detail-for="' + escapeHTML(run.key) + '">'
          + '<td colspan="7">'
          + '<div class="detail-panel">'
          + '<div class="tabs" role="tablist" aria-label="Run details tabs">'
          + renderTabButton(run.key, 'output', 'Output', tabName)
          + renderTabButton(run.key, 'log', 'Log', tabName)
          + renderTabButton(run.key, 'events', 'Events', tabName)
          + '</div>'
          + '<div class="panel-grid">'
          + '<section class="detail-box">'
          + '<h3>' + escapeHTML(currentTabLabel(tabName)) + '</h3>'
          + '<pre>' + escapeHTML(renderTabContent(run, tabName, output, log, events)) + '</pre>'
          + '</section>'
          + '<aside class="detail-box">'
          + '<h3>Details</h3>'
          + '<div class="detail-meta">'
          + renderDetailKV('Key', run.key)
          + renderDetailKV('Run ID', run.runId)
          + renderDetailKV('Status', run.status)
          + renderDetailKV('Started', formatTime(run.startedAt))
          + renderDetailKV('Finished', formatTime(run.finishedAt))
          + renderDetailKV('Duration', formatDuration(run.duration))
          + renderDetailKV('Branch', formatBranch(run))
          + renderDetailKV('Source', formatSource(run))
          + '</div>'
          + '</aside>'
          + '</div>'
          + '</div>'
          + '</td>'
          + '</tr>'
        ) : ''
      ].join('');
    }

    function renderTabButton(runKey, tabKey, label, activeTab) {
      return '<button type="button" class="tab-btn" data-action="set-tab" data-run-key="' + escapeHTML(runKey) + '" data-tab="' + escapeHTML(tabKey) + '" role="tab" aria-pressed="' + (activeTab === tabKey ? 'true' : 'false') + '">' + escapeHTML(label) + '</button>';
    }

    function currentTabLabel(tabKey) {
      if (tabKey === 'log') return 'Log';
      if (tabKey === 'events') return 'Events';
      return 'Output';
    }

    function renderDetailKV(label, value) {
      const text = value && String(value).trim() ? value : '—';
      return '<div class="kv"><span>' + escapeHTML(label) + '</span><strong>' + escapeHTML(text) + '</strong></div>';
    }

    function renderTabContent(run, tabKey, output, log, events) {
      if (tabKey === 'events') {
        if (!events.length) return 'No events captured for this run yet.';
        return events.map((event) => {
          const payload = event.payload && Object.keys(event.payload).length ? JSON.stringify(event.payload, null, 2) : '';
          return [
            event.timestamp ? new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(new Date(event.timestamp)) : '—',
            event.type || 'event',
            payload ? '\n' + payload : ''
          ].join(' · ');
        }).join('\n\n');
      }
      if (tabKey === 'log') return log;
      return output;
    }

    function renderEmpty(message) {
      return '<tr><td colspan="7"><div class="empty-state">' + escapeHTML(message) + '</div></td></tr>';
    }

    function render() {
      syncStatusOptions(state.runs);
      const filtered = state.runs.filter(shouldShowRun);
      summaryPill.innerHTML = '<span class="dot"></span><strong>' + filtered.length + '</strong> runs visible';
      if (!state.runs.length) {
        runsBody.innerHTML = renderEmpty(state.loading ? 'Loading runs.' : 'No Sandman runs found.');
        return;
      }
      if (!filtered.length) {
        runsBody.innerHTML = renderEmpty('No runs match the current filters.');
        return;
      }
      runsBody.innerHTML = filtered.map(renderRunRow).join('');
    }

    function updateLastUpdated() {
      const now = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(new Date());
      lastUpdated.textContent = 'Updated ' + now;
    }

    async function refresh() {
      try {
        const res = await fetch(apiPath, { cache: 'no-store' });
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const payload = await res.json();
        state.runs = Array.isArray(payload.runs) ? payload.runs : [];
        state.loading = false;
        errorBanner.style.display = 'none';
        errorBanner.textContent = '';
        updateLastUpdated();
        render();
      } catch (err) {
        state.loading = false;
        errorBanner.textContent = 'Refresh failed: ' + err.message;
        errorBanner.style.display = 'block';
      }
    }

    runsBody.addEventListener('click', (event) => {
      const button = event.target.closest('button[data-action]');
      if (!button) return;
      const runKey = button.getAttribute('data-run-key');
      if (!runKey) return;
      const action = button.getAttribute('data-action');
      if (action === 'toggle-run') {
        if (state.expanded.has(runKey)) state.expanded.delete(runKey);
        else state.expanded.add(runKey);
        render();
        return;
      }
      if (action === 'set-tab') {
        const tab = button.getAttribute('data-tab') || 'output';
        state.tabs.set(runKey, tab);
        state.expanded.add(runKey);
        render();
      }
    });

    statusFilter.addEventListener('change', () => {
      state.selectedStatus = statusFilter.value || 'all';
      render();
    });

    activeOnlyToggle.addEventListener('change', () => {
      state.activeOnly = activeOnlyToggle.checked;
      render();
    });

    refresh();
    setInterval(refresh, pollInterval);
  </script>
</body>
</html>`))
