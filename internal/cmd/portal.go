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

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
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
	Log         string        `json:"log,omitempty"`
	Events      []portalEvent `json:"events,omitempty"`
}

type portalActiveRun struct {
	Key          string
	Dir          string
	SocketPath   string
	IssueNumber  int
	IssueNumbers []int
	StartedAt    time.Time
	ModTime      time.Time
}

// NewPortalCmd creates the portal command.
func NewPortalCmd(deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "portal",
		Short: "Serve a local portal for current Sandman runs",
		Long:  "Serve a portal for the current repository and poll .sandman/runs for live Sandman instances.",
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := cmd.Flags().GetInt("port")
			if err != nil {
				return err
			}

			cfg, err := loadPortalLaunchConfig(deps.ConfigStore)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			launchData := portalLaunchDataFromConfig(cfg)

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

			return runPortalServer(ctx, repoRoot, port, cmd.OutOrStdout(), launchData, cfg)
		},
	}

	cmd.Flags().Int("port", 5000, "Port to bind on 0.0.0.0")
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

func runPortalServer(ctx context.Context, repoRoot string, port int, out io.Writer, launchData portalLaunchFormData, cfg *config.Config) error {
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

	server := &http.Server{Handler: newPortalHandler(repoRoot, launchData, cfg)}
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

func newPortalHandler(repoRoot string, launchData portalLaunchFormData, cfg *config.Config) http.Handler {
	launcher, launcherErr := newPortalLauncher(repoRoot)
	mux := http.NewServeMux()
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
			req, err := parsePortalUnifiedLaunchRequest(r)
			if err != nil {
				writeJSONError(w, "invalid command payload", http.StatusBadRequest)
				return
			}

			var (
				args []string
				resp any
			)
			switch strings.TrimSpace(req.Command) {
			case "run":
				args, err = buildPortalRunArgs(repoRoot, cfg, req.runRequest())
				if err != nil {
					writeJSONError(w, err.Error(), http.StatusBadRequest)
					return
				}
				if err := portalStartRun(r.Context(), repoRoot, args); err != nil {
					writeJSONError(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if _, err := launcher.record(args); err != nil {
					writeJSONError(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp = portalLaunchResponse{Message: "Started sandman run.", Args: args}
			case "continue":
				commandReq := req.commandRequest()
				commandReq.Preset = req.Command
				args, err = buildPortalCommandArgs(commandReq)
				if err != nil {
					writeJSONError(w, err.Error(), http.StatusBadRequest)
					return
				}
				if err := portalStartRun(r.Context(), repoRoot, args); err != nil {
					writeJSONError(w, err.Error(), http.StatusInternalServerError)
					return
				}
				command, err := launcher.record(args)
				if err != nil {
					writeJSONError(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp = command
			case "status", "history", "clean", "config":
				commandReq := req.commandRequest()
				commandReq.Preset = req.Command
				args, err = buildPortalCommandArgs(commandReq)
				if err != nil {
					writeJSONError(w, err.Error(), http.StatusBadRequest)
					return
				}
				command, err := launcher.launch(args)
				if err != nil {
					writeJSONError(w, err.Error(), http.StatusInternalServerError)
					return
				}
				resp = command
			default:
				writeJSONError(w, "unknown command", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
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
		launchDataJSON, err := json.Marshal(struct {
			Agent             string `json:"agent"`
			Model             string `json:"model"`
			BaseBranch        string `json:"baseBranch"`
			Sandbox           string `json:"sandbox"`
			Parallel          int    `json:"parallel"`
			StartDelay        int    `json:"startDelay"`
			ContainerCapacity int    `json:"containerCapacity"`
			MaxContainers     int    `json:"maxContainers"`
			Ralph             int    `json:"ralph"`
		}{
			Agent:             launchData.Agent,
			Model:             launchData.Model,
			BaseBranch:        launchData.BaseBranch,
			Sandbox:           launchData.Sandbox,
			Parallel:          launchData.Parallel,
			StartDelay:        launchData.StartDelay,
			ContainerCapacity: launchData.ContainerCapacity,
			MaxContainers:     launchData.MaxContainers,
			Ralph:             launchData.Ralph,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data := struct {
			RepoRoot              string
			PollInterval          int
			CommandsPath          string
			RunsPath              string
			InstancesPath         string
			RefreshPath           string
			PortalTitle           string
			PortalSubtitle        string
			PortalStateStorageKey string
			LaunchData            portalLaunchFormData
			LaunchDataJSON        template.JS
			ThemeOptionsHTML      template.HTML
			SupportedThemesJSON   template.JS
			PortalStateJS         template.JS
		}{
			RepoRoot:              repoRoot,
			PollInterval:          int(portalPollInterval / time.Millisecond),
			CommandsPath:          "/api/commands",
			RunsPath:              "/api/runs",
			InstancesPath:         "/api/instances",
			RefreshPath:           "/api/runs",
			PortalTitle:           "Sandman Portal",
			PortalSubtitle:        "A control room for your Sandman runs.",
			PortalStateStorageKey: fmt.Sprintf("sandman.portal.view-state.v1:%s", repoRoot),
			LaunchData:            launchData,
			LaunchDataJSON:        template.JS(launchDataJSON),
			ThemeOptionsHTML:      portalThemeOptionsHTML,
			SupportedThemesJSON:   portalSupportedThemesJSON,
			PortalStateJS:         portalStateJS,
		}
		if err := portalPageTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	return mux
}

func writeJSONError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
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
	for _, run := range runStates {
		if run.IsActive() {
			activeStates = append(activeStates, run)
		}
	}

	runs := make([]portalRun, 0, len(runStates)+len(activeInstances))
	consumedRunIDs := make(map[string]struct{})
	promptActive := make([]portalActiveRun, 0, len(activeInstances))
	for _, active := range activeInstances {
		if len(active.IssueNumbers) == 0 {
			promptActive = append(promptActive, active)
			continue
		}
		batchRuns, usedRunIDs := portalRunsFromActiveBatch(repoRoot, active, runStates, eventList, eventsByRun)
		runs = append(runs, batchRuns...)
		for runID := range usedRunIDs {
			consumedRunIDs[runID] = struct{}{}
		}
	}

	matchedActive := matchPortalActiveRuns(promptActive, activeStates)
	for _, match := range matchedActive {
		run := portalRunFromActiveMatch(repoRoot, match, eventsByRun)
		runs = append(runs, run)
		if run.RunID != "" {
			consumedRunIDs[run.RunID] = struct{}{}
		}
	}
	for _, runState := range runStates {
		if _, ok := consumedRunIDs[runState.RunID]; ok {
			continue
		}
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
		runDir := filepath.Dir(instance.SocketPath)
		manifest, manifestErr := daemon.ReadManifest(runDir)
		issueNumber, _ := parseRunDirIssue(instance.Name)
		issueNumbers := []int(nil)
		startedAt := info.ModTime()
		if manifestErr == nil {
			issueNumbers = append(issueNumbers, manifest.Issues...)
			if !manifest.CreatedAt.IsZero() {
				startedAt = manifest.CreatedAt
			}
		}
		if len(issueNumbers) == 0 && issueNumber > 0 {
			issueNumbers = []int{issueNumber}
		}
		active = append(active, portalActiveRun{
			Key:          instance.Name,
			Dir:          runDir,
			SocketPath:   instance.SocketPath,
			IssueNumber:  issueNumber,
			IssueNumbers: issueNumbers,
			StartedAt:    startedAt,
			ModTime:      info.ModTime(),
		})
	}
	return active, nil
}

func portalRunsFromActiveBatch(repoRoot string, active portalActiveRun, runStates []events.RunState, eventList []events.Event, eventsByRun map[string][]portalEvent) ([]portalRun, map[string]struct{}) {
	batchStart := active.StartedAt
	if batchStart.IsZero() {
		batchStart = active.ModTime
	}
	liveOutput := readPortalSocketOutput(active.SocketPath)
	runs := make([]portalRun, 0, len(active.IssueNumbers))
	usedRunIDs := make(map[string]struct{})
	for _, issueNumber := range active.IssueNumbers {
		state := latestPortalRunStateForIssue(runStates, issueNumber, batchStart)
		blocked := latestPortalBlockedEventForIssue(eventList, issueNumber, batchStart)
		runs = append(runs, portalRunFromActiveBatchIssue(repoRoot, active, issueNumber, state, blocked, liveOutput, eventsByRun))
		if state != nil && state.RunID != "" {
			usedRunIDs[state.RunID] = struct{}{}
		}
	}
	return runs, usedRunIDs
}

func latestPortalRunStateForIssue(runStates []events.RunState, issueNumber int, batchStart time.Time) *events.RunState {
	var latest *events.RunState
	for i := range runStates {
		state := runStates[i]
		if state.IssueNumber() != issueNumber {
			continue
		}
		if !portalEventBelongsToBatch(state.Started.Timestamp, batchStart) {
			continue
		}
		if latest == nil || state.Started.Timestamp.After(latest.Started.Timestamp) {
			copy := state
			latest = &copy
		}
	}
	return latest
}

func latestPortalBlockedEventForIssue(eventList []events.Event, issueNumber int, batchStart time.Time) *events.Event {
	var latest *events.Event
	for i := range eventList {
		event := eventList[i]
		if event.Type != "run.blocked" || event.Issue != issueNumber {
			continue
		}
		if !portalEventBelongsToBatch(event.Timestamp, batchStart) {
			continue
		}
		if latest == nil || event.Timestamp.After(latest.Timestamp) {
			copy := event
			latest = &copy
		}
	}
	return latest
}

func portalEventBelongsToBatch(timestamp, batchStart time.Time) bool {
	if batchStart.IsZero() {
		return true
	}
	return !timestamp.Before(batchStart.Add(-time.Second))
}

func portalRunFromActiveBatchIssue(repoRoot string, active portalActiveRun, issueNumber int, state *events.RunState, blocked *events.Event, liveOutput string, eventsByRun map[string][]portalEvent) portalRun {
	issueLabel := fmt.Sprintf("#%d", issueNumber)
	run := portalRun{
		Key:         fmt.Sprintf("%s-issue-%d", active.Key, issueNumber),
		Kind:        "active",
		Status:      "queued",
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		StartedAt:   active.StartedAt,
		SocketPath:  active.SocketPath,
		LogPath:     portalLogPath(repoRoot, issueNumber, ""),
		LogURL:      portalLogDownloadURL(repoRoot, issueNumber, ""),
		Log:         "Queued. Waiting to start.",
	}
	if state != nil {
		run.Key = state.RunID
		run.RunID = state.RunID
		run.Status = statusOrDefault(state.Status(), state.IsActive())
		run.Branch = state.Branch()
		run.StartedAt = state.Started.Timestamp
		run.Duration = durationForRun(*state)
		run.Events = eventsByRun[state.RunID]
		run.LogPath = portalLogPath(repoRoot, issueNumber, state.Branch())
		run.LogURL = portalLogDownloadURL(repoRoot, issueNumber, state.Branch())
		if state.Finished != nil {
			finishedAt := state.Finished.Timestamp
			run.FinishedAt = &finishedAt
			run.Log = readPortalTextFile(run.LogPath)
			if strings.TrimSpace(run.Log) == "" {
				run.Log = "No log file yet."
			}
		} else {
			run.Log = filterPortalIssueOutput(liveOutput, issueNumber)
			if strings.TrimSpace(run.Log) == "" {
				run.Log = "No live output captured yet."
			}
		}
		return run
	}
	if blocked != nil {
		run.Key = blocked.RunID
		run.RunID = blocked.RunID
		run.Status = "blocked"
		run.StartedAt = blocked.Timestamp
		run.Events = []portalEvent{{Type: blocked.Type, Timestamp: blocked.Timestamp, Payload: blocked.Payload}}
		run.Log = portalBlockedMessage(blocked.Payload)
	}
	return run
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
		Log:         readPortalSocketOutput(match.instance.SocketPath),
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
	logContent := readPortalTextFile(logPath)
	if active != nil {
		logContent = readPortalSocketOutput(active.SocketPath)
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
		Log:         logContent,
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

func filterPortalIssueOutput(text string, issueNumber int) string {
	prefix := fmt.Sprintf("[issue-%d] ", issueNumber)
	lines := strings.Split(text, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			filtered = append(filtered, line)
		}
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func portalBlockedMessage(payload map[string]any) string {
	blockers := portalBlockedByIssues(payload)
	if len(blockers) == 0 {
		return "Blocked. Waiting on unresolved blockers."
	}
	parts := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		parts = append(parts, fmt.Sprintf("#%d", blocker))
	}
	return fmt.Sprintf("Blocked by %s.", strings.Join(parts, ", "))
}

func portalBlockedByIssues(payload map[string]any) []int {
	if payload == nil {
		return nil
	}
	raw, ok := payload["blocked_by"]
	if !ok {
		return nil
	}
	switch values := raw.(type) {
	case []int:
		return append([]int(nil), values...)
	case []any:
		issues := make([]int, 0, len(values))
		for _, value := range values {
			switch n := value.(type) {
			case float64:
				issues = append(issues, int(n))
			case int:
				issues = append(issues, n)
			}
		}
		return issues
	default:
		return nil
	}
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
