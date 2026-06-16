package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/spf13/cobra"
)

const (
	portalPollInterval       = 2 * time.Second
	portalReadLimit          = 64 * 1024
	portalReadTimeout        = 250 * time.Millisecond
	portalAbortTimeout       = 5 * time.Second
	portalSocketProbeTimeout = 100 * time.Millisecond
	portalDefaultHost        = "127.0.0.1"
	portalReadHeaderTimeout  = 10 * time.Second
	portalWriteTimeout       = 30 * time.Second
	portalIdleTimeout        = 2 * time.Minute
	portalMaxBodyBytes       = 1 << 20
)

var portalANSISequence = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

var portalRunAborter = abortPortalRun
var portalPeerPID = resolvePortalPeerPID
var portalSignalProcess = signalPortalProcess

// portalStaleCleaner is the function invoked once per portal server
// startup to recover stale runs and clean up dead directories.
var portalStaleCleaner = func(repoRoot string) error {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	logPath := layout.EventsLogPath
	logPathLogger := &events.JSONLLogger{Path: logPath}
	eventsList, err := logPathLogger.Read()
	if err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	recovered, deadDirs, err := portalRunCleanStale(layout, eventsList, logPathLogger)
	if err != nil {
		return err
	}
	if recovered > 0 {
		log.Printf("portal: recovered %d stale runs as aborted across %d dead directories.", recovered, deadDirs)
	}
	return nil
}

// portalRunCleanStale is a package-level var so tests can substitute it.
var portalRunCleanStale = runCleanStale

type portalInstance struct {
	Name       string `json:"name"`
	SocketPath string `json:"socketPath"`
}

// NewPortalCmd creates the portal command.
func NewPortalCmd(deps Dependencies) *cobra.Command {
	defaultHost := portalDefaultHost
	if envHost := strings.TrimSpace(os.Getenv("SANDMAN_PORTAL_HOST")); envHost != "" {
		defaultHost = envHost
	}
	cmd := &cobra.Command{
		Use:   "portal",
		Short: "Serve a local portal for current Sandman runs",
		Long:  "Serve a portal for the current repository and poll .sandman/runs for live Sandman instances. By default the server binds to 127.0.0.1; pass --host or set SANDMAN_PORTAL_HOST to opt in to a different interface (e.g. 0.0.0.0).",
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := cmd.Flags().GetInt("port")
			if err != nil {
				return err
			}
			host, err := cmd.Flags().GetString("host")
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

			return runPortalServer(ctx, repoRoot, port, host, cmd.OutOrStdout(), launchData, cfg)
		},
	}

	cmd.Flags().Int("port", 5000, "Port to bind on the chosen host")
	cmd.Flags().String("host", defaultHost, "Host/interface to bind on (default 127.0.0.1; use 0.0.0.0 to expose on all interfaces, or set SANDMAN_PORTAL_HOST)")
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

func runPortalServer(ctx context.Context, repoRoot string, port int, host string, out io.Writer, launchData portalLaunchFormData, cfg *config.Config) error {
	bindHost := strings.TrimSpace(host)
	if bindHost == "" {
		bindHost = portalDefaultHost
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindHost, port))
	if err != nil {
		return fmt.Errorf("bind portal on %s:%d: %w", bindHost, port, err)
	}
	defer listener.Close()

	tcpAddr, _ := listener.Addr().(*net.TCPAddr)
	actualPort := port
	if tcpAddr != nil {
		actualPort = tcpAddr.Port
	}

	if _, err := fmt.Fprintf(out, "Portal listening on http://%s:%d\n", bindHost, actualPort); err != nil {
		return fmt.Errorf("write portal address: %w", err)
	}

	server := newPortalHTTPServer(repoRoot, launchData, cfg)
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

// newPortalHTTPServer constructs the hardened HTTP server that backs the
// portal command. The exported factory is the testable seam: tests assert that
// the timeouts are configured without having to drive a real connection.
func newPortalHTTPServer(repoRoot string, launchData portalLaunchFormData, cfg *config.Config) *http.Server {
	return &http.Server{
		Handler:           newPortalHandler(repoRoot, launchData, cfg),
		ReadTimeout:       portalReadHeaderTimeout,
		ReadHeaderTimeout: portalReadHeaderTimeout,
		WriteTimeout:      portalWriteTimeout,
		IdleTimeout:       portalIdleTimeout,
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
		if err := json.NewDecoder(io.LimitReader(r.Body, portalMaxBodyBytes)).Decode(&req); err != nil {
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

		logDir := filepath.Join(repoRoot, ".sandman", "logs")
		// Strip the legacy ".sandman/logs" prefix so the URL contract stays
		// the same, but the file is served from an fs.FS rooted at logDir.
		// http.ServeFileFS rejects requests where r.URL.Path contains a
		// ".." element, so any path that escapes the log directory is
		// refused. Missing files inside the directory get a 404.
		logPrefix := filepath.Join(".sandman", "logs")
		name := strings.TrimPrefix(relPath, logPrefix)
		name = strings.TrimPrefix(name, string(filepath.Separator))
		if name == "" {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if filepath.IsAbs(name) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		info, err := fs.Stat(os.DirFS(logDir), name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.Name()))
		http.ServeFileFS(w, r, os.DirFS(logDir), name)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		data, err := buildPortalPageData(repoRoot, launchData)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := portalPageTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("portal: stale cleanup panicked: %v", r)
			}
		}()
		if err := portalStaleCleaner(repoRoot); err != nil {
			log.Printf("portal: stale cleanup failed: %v", err)
		}
	}()
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
		if run.BatchKey == "" {
			return &portalAbortError{status: http.StatusConflict, message: fmt.Sprintf("daemon for run %q is no longer live", runKey)}
		}
		return &portalAbortError{status: http.StatusNotFound, message: fmt.Sprintf("active run %q not found", runKey)}
	}

	if _, err := os.Stat(run.SocketPath); os.IsNotExist(err) {
		return &portalAbortError{status: http.StatusConflict, message: fmt.Sprintf("daemon for run %q is no longer live", runKey)}
	}

	runDir := filepath.Dir(run.SocketPath)

	if !daemon.IsRunActive(runDir) {
		return &portalAbortError{status: http.StatusConflict, message: fmt.Sprintf("daemon for run %q is no longer live", runKey)}
	}

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
	if err := json.NewDecoder(io.LimitReader(conn, portalMaxBodyBytes)).Decode(&resp); err != nil {
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
	eventLog := &events.JSONLLogger{Path: filepath.Join(repoRoot, ".sandman", "events.jsonl")}
	view := &portalRunsView{}
	runs, err := view.compute(repoRoot, eventLog)
	if err != nil {
		return portalRun{}, err
	}
	for _, run := range runs {
		if run.Key == runKey {
			return run, nil
		}
	}
	return portalRun{}, &portalAbortError{status: http.StatusNotFound, message: fmt.Sprintf("run %q not found", runKey)}
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
