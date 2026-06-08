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
	portalAbortTimeout = 5 * time.Second
)

var portalANSISequence = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

var portalRunAborter = abortPortalRun
var portalPeerPID = resolvePortalPeerPID
var portalSignalProcess = signalPortalProcess

type portalInstance struct {
	Name       string `json:"name"`
	SocketPath string `json:"socketPath"`
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
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return fmt.Errorf("bind portal on 0.0.0.0:%d: %w", port, err)
	}
	defer listener.Close()

	tcpAddr, _ := listener.Addr().(*net.TCPAddr)
	actualPort := port
	if tcpAddr != nil {
		actualPort = tcpAddr.Port
	}

	if _, err := fmt.Fprintf(out, "Portal listening on http://0.0.0.0:%d\n", actualPort); err != nil {
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

		eventLog := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}
		view := &portalRunsView{}
		runs, err := view.compute(repoRoot, eventLog)
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
	mux.HandleFunc("/api/runs/abort", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !portalAbortSupported() {
			writeJSONError(w, "abort issue is unsupported on this platform", http.StatusNotImplemented)
			return
		}

		var req struct {
			RunKey string `json:"runKey"`
			Issue  int    `json:"issue"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, "invalid run payload", http.StatusBadRequest)
			return
		}
		req.RunKey = strings.TrimSpace(req.RunKey)
		if req.RunKey == "" {
			writeJSONError(w, "missing runKey", http.StatusBadRequest)
			return
		}
		if req.Issue <= 0 {
			writeJSONError(w, "missing issue", http.StatusBadRequest)
			return
		}

		if err := portalRunAborter(r.Context(), repoRoot, req.RunKey, req.Issue); err != nil {
			var abortErr *portalAbortError
			if errors.As(err, &abortErr) {
				writeJSONError(w, abortErr.Error(), abortErr.status)
				return
			}
			writeJSONError(w, "abort failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"runKey": req.RunKey, "issue": req.Issue, "status": "aborted", "scope": "issue"})
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
			PortalScrollJS        template.JS
			PortalDiffJS          template.JS
			PortalAbortSupported  bool
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
			PortalScrollJS:        portalScrollJS,
			PortalDiffJS:          portalDiffJS,
			PortalAbortSupported:  portalAbortSupported(),
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

type portalAbortError struct {
	status  int
	message string
}

func (e *portalAbortError) Error() string { return e.message }

// abortPortalRun sends an abort command to the run's cmd.sock for a single issue row.
func abortPortalRun(ctx context.Context, repoRoot, runKey string, issueNumber int) error {
	run, err := portalRunForKey(repoRoot, runKey)
	if err != nil {
		return err
	}
	if run.SocketPath == "" {
		return &portalAbortError{status: http.StatusNotFound, message: fmt.Sprintf("active run %q not found", runKey)}
	}

	runDir := filepath.Dir(run.SocketPath)
	cmdSock := filepath.Join(runDir, "cmd.sock")
	if _, err := os.Stat(cmdSock); err != nil {
		if os.IsNotExist(err) {
			return &portalAbortError{status: http.StatusNotFound, message: fmt.Sprintf("active run %q not found", runKey)}
		}
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("could not inspect abort command socket for run %q", runKey)}
	}

	pid, err := portalPeerPID(run.SocketPath)
	if err != nil {
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("could not resolve the active run process for run %q", runKey)}
	}
	if pid <= 0 {
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("daemon for run %q not responding", runKey)}
	}

	dialCtx, cancel := context.WithTimeout(ctx, portalAbortTimeout)
	defer cancel()

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(dialCtx, "unix", cmdSock)
	if err != nil {
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("could not connect to the agent daemon for run %q", runKey)}
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("could not prepare abort command for run %q", runKey)}
	}

	req := struct {
		Action string `json:"action"`
		Issue  int    `json:"issue"`
	}{Action: "abort", Issue: issueNumber}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("could not send abort request for run %q", runKey)}
	}

	var resp daemon.CommandResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("could not read abort response for run %q", runKey)}
	}
	if resp.Status == "error" {
		return &portalAbortError{status: http.StatusConflict, message: resp.Message}
	}
	if resp.Status != "ok" {
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("unexpected abort response for run %q", runKey)}
	}

	return nil
}

func portalRunForKey(repoRoot, runKey string) (portalRun, error) {
	runs, err := loadPortalRuns(repoRoot)
	if err != nil {
		return portalRun{}, err
	}
	for _, run := range runs {
		if run.Key == runKey && run.Kind == "active" {
			return run, nil
		}
	}
	return portalRun{}, &portalAbortError{status: http.StatusNotFound, message: fmt.Sprintf("active run %q not found", runKey)}
}

func signalPortalProcess(pid int, sig syscall.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(sig)
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
	eventLog := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}
	view := &portalRunsView{}
	return view.compute(repoRoot, eventLog)
}

func dedupPortalRunGroup(runs []portalRun) []portalRun {
	view := &portalRunsView{}
	return view.dedupRunGroup(runs)
}

func portalRunFromActiveBatchIssue(repoRoot string, active portalActiveRun, issueNumber int, state *events.RunState, blocked *events.Event, liveOutput string, eventsByRun map[string][]portalEvent) portalRun {
	view := &portalRunsView{}
	return view.runFromActiveBatchIssue(repoRoot, active, issueNumber, state, blocked, liveOutput, eventsByRun)
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
	for {
		n, readErr := conn.Read(tmp)
		if n > 0 {
			_, _ = buf.Write(tmp[:n])
		}
		if readErr != nil {
			if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
				break
			}
			break
		}
	}
	if buf.Len() > portalReadLimit {
		data := buf.Bytes()
		buf = *bytes.NewBuffer(append([]byte(nil), data[len(data)-portalReadLimit:]...))
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
