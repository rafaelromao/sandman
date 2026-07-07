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

// portalRunArchiver is the per-row archive dispatcher used by the
// HTTP handler. It receives the resolved batch entry id and the
// per-row run id and must perform the move + index update. It is a
// package-level var so tests can substitute a deterministic move
// without touching the real filesystem. The default implementation
// resolves the entry via the per-row index, calls daemon.ArchiveRow,
// and writes the resulting RunRecord into the index's Runs slice.
var portalRunArchiver = archivePortalRowArchiver

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

// writeJSONArchiveError writes a structured 409 body for the per-row
// archive endpoint. The body carries the error message and (when
// supplied) the existing ArchivePath the operator can inspect.
func writeJSONArchiveError(w http.ResponseWriter, msg, archivePath string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]string{"error": msg}
	if archivePath != "" {
		payload["archivePath"] = archivePath
	}
	_ = json.NewEncoder(w).Encode(payload)
}

type portalAbortError struct {
	status  int
	message string
}

func (e *portalAbortError) Error() string { return e.message }

type portalArchiveError struct {
	status  int
	message string
	path    string
}

func (e *portalArchiveError) Error() string {
	if e.path == "" {
		return e.message
	}
	return fmt.Sprintf("%s (archivePath=%q)", e.message, e.path)
}

// portalArchiveDir returns the absolute archive directory path for a
// given repo root and run id.
func portalArchiveDir(repoRoot, runID string) string {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	return filepath.Join(layout.ArchiveDir, runID)
}

// archivePortalRunHandler performs the per-row terminal-check and
// destination collision checks, then hands off to portalRunArchiver.
// The function returns portalArchiveError values so the HTTP handler
// can map each failure mode to a specific status code; the archiver
// itself is only invoked on the happy path.
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
		return "", &portalArchiveError{status: http.StatusNotFound, message: fmt.Sprintf("run %q not found", runID)}
	}

	if batch.Status != batchindex.StatusActive {
		return batch.ID, &portalArchiveError{status: http.StatusConflict, message: fmt.Sprintf("batch %q is not active (status=%s)", batch.ID, batch.Status)}
	}

	status, statusErr := portalRowStatusProbe(batch, runID)
	if statusErr != nil {
		return batch.ID, &portalArchiveError{status: http.StatusInternalServerError, message: statusErr.Error()}
	}
	if !isTerminalRunManifestStatus(status) {
		return batch.ID, &portalArchiveError{status: http.StatusConflict, message: fmt.Sprintf("run %q is not in a terminal status", runID)}
	}

	if rec := idx.RunRecordFor(batch.ID, runID); rec != nil && rec.Status == batchindex.RunRecordStatusArchived && rec.ArchivePath != "" {
		return batch.ID, &portalArchiveError{
			status:  http.StatusConflict,
			message: fmt.Sprintf("run %q is already archived at %q", runID, rec.ArchivePath),
			path:    rec.ArchivePath,
		}
	}

	// On-disk collision check: if the per-row destination already
	// exists (legacy state from before the index recorded Runs[]),
	// surface 409 with the existing path so the operator can inspect
	// it.
	relArchive := filepath.Join(".sandman", "archive", batch.ID, "runs", runID)
	if _, err := os.Stat(filepath.Join(repoRoot, relArchive)); err == nil {
		return batch.ID, &portalArchiveError{
			status:  http.StatusConflict,
			message: fmt.Sprintf("run %q is already archived at %q", runID, relArchive),
			path:    relArchive,
		}
	} else if !os.IsNotExist(err) {
		return batch.ID, &portalArchiveError{status: http.StatusInternalServerError, message: fmt.Sprintf("stat archive target: %v", err)}
	}

	if err := portalRunArchiver(repoRoot, batch.ID, runID); err != nil {
		var archived *daemon.AlreadyArchivedError
		if errors.As(err, &archived) {
			return batch.ID, &portalArchiveError{
				status:  http.StatusConflict,
				message: fmt.Sprintf("run %q is already archived at %q", runID, archived.ArchivePath),
				path:    archived.ArchivePath,
			}
		}
		var nonTerminal *daemon.NonTerminalRowError
		if errors.As(err, &nonTerminal) {
			return batch.ID, &portalArchiveError{status: http.StatusConflict, message: fmt.Sprintf("run %q is not in a terminal status", runID)}
		}
		return batch.ID, &portalArchiveError{status: http.StatusInternalServerError, message: err.Error()}
	}
	return batch.ID, nil
}

// isTerminalRunManifestStatus reports whether the supplied run.json
// Status is one of the terminal values (success / failure / aborted
// / blocked). The per-row archive contract requires a terminal row,
// because the per-run command socket is idle in that state and the
// run folder can be safely relocated.
func isTerminalRunManifestStatus(s batchindex.RunManifestStatus) bool {
	switch s {
	case batchindex.RunManifestStatusSuccess,
		batchindex.RunManifestStatusFailure,
		batchindex.RunManifestStatusAborted,
		batchindex.RunManifestStatusBlocked:
		return true
	}
	return false
}

// portalRowStatusProbe is the seam the archive handler uses to read
// the targeted row's run.json Status. It is a package-level var so
// tests can simulate non-terminal rows without touching the real
// filesystem. The default reads runs/<runID>/run.json and returns the
// Status field.
var portalRowStatusProbe = portalReadRowStatus

// portalReadRowStatus reads runs/<runID>/run.json from the batch's
// live directory and returns the Status field. A missing manifest
// produces a NotFound error; a malformed manifest produces a Decode
// error. The default implementation of portalRowStatusProbe.
func portalReadRowStatus(batch *batchindex.Batch, runID string) (batchindex.RunManifestStatus, error) {
	manifestPath := filepath.Join(batch.Path, "runs", runID, "run.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	var manifest batchindex.RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("decode run manifest: %w", err)
	}
	return manifest.Status, nil
}

// archivePortalRowArchiver is the default implementation of
// portalRunArchiver: it resolves the entry's path, calls
// daemon.ArchiveRow for the targeted run, and writes the resulting
// RunRecord into the entry's Runs slice. It also appends an active
// RunRecord for the row if the index has not seen it yet so the
// record survives crash recovery.
func archivePortalRowArchiver(repoRoot string, entryID, runID string) error {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return fmt.Errorf("load batches index: %w", err)
	}
	entry := idx.ResolveBatch(entryID)
	if entry == nil {
		return fmt.Errorf("entry %q not found", entryID)
	}
	if idx.RunRecordFor(entryID, runID) == nil {
		idx.AddRun(entryID, batchindex.RunRecord{RunID: runID, Status: batchindex.RunRecordStatusActive})
	}
	rec, err := daemon.ArchiveRow(repoRoot, entry, runID)
	if err != nil {
		return err
	}
	if err := idx.MarkRunArchived(entryID, runID, rec.ArchivePath); err != nil {
		return fmt.Errorf("mark run archived: %w", err)
	}
	if err := idx.Save(layout.BatchesIndexPath); err != nil {
		return fmt.Errorf("save batches index: %w", err)
	}
	return nil
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
// The second path consults each entry's Runs[] records so already-
// archived rows can still be resolved from the index after their live
// folder has moved. This is what stops the per-row archive endpoint
// from 404'ing on archived rows when the operator paginates back to
// them in the portal UI.
//
// The final fallback path scans each entry's runs/<runID>/run.json
// on disk and returns the first entry that hosts the per-row manifest.
// This is the signal the portal UI relies on for multi-issue batches,
// reviews (where reviewRunIDFor produces e.g. "abcd-260618113825-42-PR99"
// while the batch entry id is "abcd-260618113825-PR42"), and
// prompt-only runs (where req.RunID is the user-supplied string and
// the batch entry id is "{shortid}-{ts}-{userid}").
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
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		for j := range entry.Runs {
			if entry.Runs[j].RunID == runID {
				return entry
			}
		}
	}
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		if entry.Path == "" {
			continue
		}
		manifestPath := filepath.Join(entry.Path, "runs", runID, "run.json")
		if _, err := os.Stat(manifestPath); err == nil {
			return entry
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
