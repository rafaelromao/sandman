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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
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

var portalANSISequence = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

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
	LogURL      string        `json:"logUrl,omitempty"`
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
		Long:  "Serve a portal for the current repository and poll .sandman/runs for live Sandman instances.",
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
	launcher, launcherErr := newPortalLauncher(repoRoot)
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
	mux.HandleFunc("/api/commands", func(w http.ResponseWriter, r *http.Request) {
		if launcherErr != nil {
			http.Error(w, launcherErr.Error(), http.StatusInternalServerError)
			return
		}
		switch r.Method {
		case http.MethodGet:
			commands, err := launcher.list()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]any{"repoRoot": repoRoot, "commands": commands})
		case http.MethodPost:
			var payload struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid command payload", http.StatusBadRequest)
				return
			}
			command, err := launcher.launch(payload.Command)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(command)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/commands/", func(w http.ResponseWriter, r *http.Request) {
		if launcherErr != nil {
			http.Error(w, launcherErr.Error(), http.StatusInternalServerError)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/commands/"), "/")
		if len(parts) != 2 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		id, action := parts[0], parts[1]
		switch r.Method {
		case http.MethodPost:
			var (
				command portalCommandRecord
				err     error
			)
			switch action {
			case "stop":
				command, err = launcher.stop(id)
			case "relaunch":
				command, err = launcher.relaunch(id)
			default:
				http.NotFound(w, r)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(command)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
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
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		relPath := strings.TrimSpace(r.URL.Query().Get("path"))
		if relPath == "" {
			http.Error(w, "missing path", http.StatusBadRequest)
			return
		}

		cleanPath := filepath.Clean(relPath)
		if filepath.IsAbs(cleanPath) || strings.HasPrefix(cleanPath, "..") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		logDir := filepath.Join(repoRoot, ".sandman", "logs")
		fullPath := filepath.Join(repoRoot, cleanPath)
		relToLogs, err := filepath.Rel(logDir, fullPath)
		if err != nil || strings.HasPrefix(relToLogs, "..") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		if _, err := os.Stat(fullPath); err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(fullPath)))
		http.ServeFile(w, r, fullPath)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		data := struct {
			RepoRoot            string
			PollInterval        int
			CommandsPath        string
			RunsPath            string
			InstancesPath       string
			RefreshPath         string
			PortalTitle         string
			PortalSubtitle      string
			ThemeOptionsHTML    template.HTML
			SupportedThemesJSON template.JS
		}{
			RepoRoot:            repoRoot,
			PollInterval:        int(portalPollInterval / time.Millisecond),
			CommandsPath:        "/api/commands",
			RunsPath:            "/api/runs",
			InstancesPath:       "/api/instances",
			RefreshPath:         "/api/runs",
			PortalTitle:         "Sandman Portal",
			PortalSubtitle:      "A control room for your Sandman runs.",
			ThemeOptionsHTML:    portalThemeOptionsHTML,
			SupportedThemesJSON: portalSupportedThemesJSON,
		}
		if err := portalPageTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
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
		LogURL:      portalLogDownloadURL(repoRoot, issueNumber, ""),
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
		LogURL:      portalLogDownloadURL(repoRoot, issueNumber, branch),
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

func portalLogDownloadURL(repoRoot string, issueNumber int, branch string) string {
	logPath := portalLogPath(repoRoot, issueNumber, branch)
	if logPath == "" {
		return ""
	}
	relPath, err := filepath.Rel(repoRoot, logPath)
	if err != nil {
		return ""
	}
	return "/api/logs?path=" + url.QueryEscape(relPath)
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
		return cleanPortalText("[truncated]\n" + string(tail))
	}
	return cleanPortalText(string(data))
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
	return cleanPortalText(buf.String())
}

func cleanPortalText(text string) string {
	text = portalANSISequence.ReplaceAllString(text, "")
	text = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		case '\r':
			return -1
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
	return text
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
