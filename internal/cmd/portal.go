package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/rafaelromao/sandman/internal/batchindex"
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

// portalRunLivenessProbe is a package-level var so tests can substitute it.
var portalRunLivenessProbe = daemon.IsRunActive

// portalRunArchiver moves a batch directory from .sandman/batches/<batch-id> to
// .sandman/archive/<batch-id>. It is a package-level var so tests can substitute
// a deterministic move without touching the real filesystem. The default
// delegates to archivePortalRun, which keeps the portal and the CLI on a
// single move implementation. The handler performs the liveness check and
// destination collision check before invoking the archiver, so this var
// only needs to perform the move.
var portalRunArchiver = archivePortalRun

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
		Long:  "Serve a portal for the current repository and poll .sandman/batches for live Sandman instances. By default the server binds to 127.0.0.1; pass --host or set SANDMAN_PORTAL_HOST to opt in to a different interface (e.g. 0.0.0.0).",
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := cmd.Flags().GetInt("port")
			if err != nil {
				return err
			}
			host, err := cmd.Flags().GetString("host")
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

			return runPortalServer(ctx, repoRoot, port, host, cmd.OutOrStdout())
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

func runPortalServer(ctx context.Context, repoRoot string, port int, host string, out io.Writer) error {
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

	server := newPortalHTTPServer(repoRoot)
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
func newPortalHTTPServer(repoRoot string) *http.Server {
	return &http.Server{
		Handler:           newPortalHandler(repoRoot),
		ReadTimeout:       portalReadHeaderTimeout,
		ReadHeaderTimeout: portalReadHeaderTimeout,
		WriteTimeout:      portalWriteTimeout,
		IdleTimeout:       portalIdleTimeout,
	}
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

type portalArchiveError struct {
	status  int
	message string
}

func (e *portalArchiveError) Error() string { return e.message }

// portalArchiveDir returns the absolute archive directory path for a
// given repo root and run id.
func portalArchiveDir(repoRoot, runID string) string {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	return filepath.Join(layout.ArchiveDir, runID)
}

// archivePortalRunHandler performs the daemon-liveness and destination
// collision checks, then hands off to portalRunArchiver. The function
// returns portalArchiveError values so the HTTP handler can map each
// failure mode to a specific status code; the archiver itself is only
// invoked on the happy path.
//
// The supplied runID may be either the batch index batch id (e.g.
// "abcd-260618113825-42+1") or the per-row run id the portal UI sends
// (e.g. "abcd-260618113825-43"). resolveBatchFromRunIDFastOrScan
// resolves either form to the batch index batch; the rest of the
// handler then uses batch.ID so the archive directory name and the
// response payload are coherent across both forms. The first return
// value is the resolved batch id, surfaced in the success response so
// the portal UI sees the canonical id even when the request body used
// the per-row form.
func archivePortalRunHandler(repoRoot, runID string) (string, error) {
	layout := paths.NewLayout(&config.Config{}, repoRoot)

	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return "", &portalArchiveError{status: http.StatusInternalServerError, message: fmt.Sprintf("load batches index: %v", err)}
	}

	batch := resolveBatchFromRunIDFastOrScan(idx, runID)
	if batch == nil {
		return "", &portalArchiveError{status: http.StatusNotFound, message: fmt.Sprintf("batch %q not found in index", runID)}
	}

	if batch.Status != batchindex.StatusActive {
		return batch.ID, &portalArchiveError{status: http.StatusConflict, message: fmt.Sprintf("batch %q is not active (status=%s)", batch.ID, batch.Status)}
	}

	if portalRunLivenessProbe(batch.Path) {
		return batch.ID, &portalArchiveError{status: http.StatusConflict, message: fmt.Sprintf("batch %q is still active (daemon socket); stop the daemon before archiving", batch.ID)}
	}

	archiveDir := portalArchiveDir(repoRoot, batch.ID)
	if info, err := os.Stat(archiveDir); err == nil {
		if info.IsDir() {
			return batch.ID, &portalArchiveError{status: http.StatusConflict, message: fmt.Sprintf("archive %q already exists", batch.ID)}
		}
	} else if !os.IsNotExist(err) {
		return batch.ID, &portalArchiveError{status: http.StatusInternalServerError, message: fmt.Sprintf("stat archive target: %v", err)}
	}

	if err := portalRunArchiver(repoRoot, batch.ID); err != nil {
		return batch.ID, &portalArchiveError{status: http.StatusInternalServerError, message: err.Error()}
	}
	return batch.ID, nil
}

// resolveBatchFromRunIDFastOrScan returns the batch index batch that
// the given run id identifies, accepting either the public batch id
// (the batches.json Batch.ID == folder basename) or the per-row
// run id (the row RunID the portal UI sends). It returns nil when no
// batch matches either form.
//
// The fast path is idx.ResolveBatch(runID), which matches the public
// batch id directly and is the only signal for batches whose per-row
// run id equals their batch id (auto-select, single-issue issue runs,
// the first row of a multi-issue batch when the first subject happens
// to match, --continue issue runs that resume the existing batch dir).
//
// The fallback path is a stat-only scan: for each indexed batch the
// helper checks whether runs/<runID>/run.json exists on disk and
// returns the first batch whose file exists. This stat-only path is
// the hot path used by the archive endpoint, which trusts that any
// batch with a per-row run folder on disk owns that row — see
// internal/cmd/portal_runs_view.go's sibling helper
// (resolveBatchFromRowID) for the parse-then-resolve variant used by
// the log/portal runs-view endpoints.
//
// The fallback path scans each batch's runs/<runID>/run.json on disk
// and returns the first batch that hosts the per-row manifest. This
// is the signal the portal UI relies on for multi-issue batches,
// reviews (where reviewRunIDFor produces e.g.
// "abcd-260618113825-42-PR99" while the batch id is
// "abcd-260618113825-PR42"), and prompt-only runs (where req.RunID
// is the user-supplied string and the batch id is
// "{shortid}-{ts}-{userid}").
//
// The helper returns the batch regardless of its Status; callers
// apply any active/archived check separately so the 404/409/500 paths
// stay observable per kind.
func resolveBatchFromRunIDFastOrScan(idx *batchindex.Index, runID string) *batchindex.Batch {
	if idx == nil || runID == "" {
		return nil
	}
	if batch := idx.ResolveBatch(runID); batch != nil {
		return batch
	}
	for i := range idx.Batches {
		batch := &idx.Batches[i]
		if batch.Path == "" {
			continue
		}
		manifestPath := filepath.Join(batch.Path, "runs", runID, "run.json")
		if _, err := os.Stat(manifestPath); err == nil {
			return batch
		}
	}
	return nil
}

// abortPortalRun sends an abort command to the run's cmd.sock for a single issue row.
func abortPortalRun(ctx context.Context, repoRoot, runKey string, issueNumber int) error {
	run, err := portalRunForKey(repoRoot, runKey)
	if err != nil {
		return err
	}
	if run.SocketPath == "" {
		if run.Status == "queued" || run.Status == "blocked" {
			if err := emitRunAbortedForQueuedRun(repoRoot, run, issueNumber); err != nil {
				return err
			}
			return nil
		}
		if run.BatchKey == "" {
			return &portalAbortError{status: http.StatusConflict, message: fmt.Sprintf("daemon for run %q is no longer live", runKey)}
		}
		return &portalAbortError{status: http.StatusNotFound, message: fmt.Sprintf("active run %q not found", runKey)}
	}

	if _, err := os.Stat(run.SocketPath); os.IsNotExist(err) {
		return &portalAbortError{status: http.StatusConflict, message: fmt.Sprintf("daemon for run %q is no longer live", runKey)}
	}

	runDir := filepath.Dir(run.SocketPath)

	// For batch-level sockets (batch.sock), resolve to the per-run folder.
	cmdSock := daemon.CommandSocketPath(runDir)
	if _, err := os.Stat(cmdSock); err != nil {
		// If the command socket isn't at the batch level, check the per-run folder.
		if run.RunID != "" && run.BatchKey != "" {
			perRunID := run.RunID
			// Post-#1675: manifest.BatchId ALWAYS equals the per-row RunID
			// (the orchestrator's emitted run_id) for every batch kind,
			// not the batch dir name. The previous fallback that used
			// `filepath.Base(runDir)` (= batchDirName) was correct only
			// for orphan reviews where batchDirName == perRowRunID, and
			// would have regressed issue runs where batchDirName carries
			// the `+N` suffix. `run.RunID` is now the canonical per-row
			// id in all cases, so use it directly.
			perRunDir := filepath.Join(runDir, "runs", perRunID)
			perRunSock := daemon.CommandSocketPath(perRunDir)
			if _, statErr := os.Stat(perRunSock); statErr == nil {
				runDir = perRunDir
				cmdSock = perRunSock
			}
		}
	}
	cmdInfo, err := os.Stat(cmdSock)
	if err != nil {
		if os.IsNotExist(err) {
			return &portalAbortError{status: http.StatusNotFound, message: fmt.Sprintf("active run %q not found", runKey)}
		}
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("could not inspect abort command socket for run %q", runKey)}
	}
	if cmdInfo.Mode().Type()&os.ModeSocket == 0 {
		return &portalAbortError{status: http.StatusBadGateway, message: fmt.Sprintf("could not connect to the agent daemon for run %q", runKey)}
	}

	pid, err := portalPeerPID(cmdSock)
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
	return getPortalRunsIndex(repoRoot).FindByKey(context.Background(), runKey)
}

func emitRunAbortedForQueuedRun(repoRoot string, run portalRun, issueNumber int) error {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	logger := &events.JSONLLogger{Path: layout.EventsLogPath}
	runID := run.RunID
	if runID == "" {
		runID = run.BatchKey
	}
	ev := events.Event{
		Type:      "run.aborted",
		Timestamp: time.Now(),
		RunID:     runID,
		Issue:     issueNumber,
		Payload:   map[string]any{"status": "aborted", "branch": run.Branch},
	}
	if err := logger.Log(ev); err != nil {
		return &portalAbortError{status: http.StatusInternalServerError, message: fmt.Sprintf("failed to emit run.aborted event: %v", err)}
	}
	return nil
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
	layout := paths.NewLayout(nil, repoRoot)

	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return nil, fmt.Errorf("load batches index: %w", err)
	}

	if idx.MarkUnavailable() {
		if err := idx.Save(layout.BatchesIndexPath); err != nil {
			return nil, fmt.Errorf("save batches index: %w", err)
		}
	}

	instances := make([]portalInstance, 0, len(idx.Batches))
	for _, entry := range idx.Batches {
		if entry.Status != batchindex.StatusActive && entry.Status != batchindex.StatusArchived {
			continue
		}

		batchDir := entry.Path
		sockPath := filepath.Join(batchDir, "batch.sock")
		info, err := os.Lstat(sockPath)
		if err != nil || info.IsDir() || info.Mode()&os.ModeSocket == 0 {
			continue
		}
		if !portalRunLivenessProbe(batchDir) {
			continue
		}
		instances = append(instances, portalInstance{Name: entry.ID, SocketPath: sockPath})
	}

	sort.Slice(instances, func(i, j int) bool {
		return strings.Compare(instances[i].Name, instances[j].Name) < 0
	})
	return instances, nil
}
