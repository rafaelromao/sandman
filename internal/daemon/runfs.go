package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

// IsRunActive reports whether a batch directory is currently owned by a live
// daemon process. A batch is considered active when its `batch.sock` is
// connectable, or when any per-run `run.sock` inside `runs/` is connectable.
// Batch dirs that survived a crash (no live socket) are stale and safe to
// clean up.
func IsRunActive(batchPath string) bool {
	location := paths.BatchLocation{Dir: batchPath}
	batchSock := location.SocketPath()
	if isConnectableSocket(batchSock) {
		return true
	}
	runDirs, err := filepath.Glob(filepath.Join(batchPath, "runs", "*"))
	if err != nil {
		return false
	}
	for _, runDir := range runDirs {
		runSock := (paths.RunLocation{Dir: runDir}).SocketPath()
		if isConnectableSocket(runSock) {
			return true
		}
	}
	return false
}

func isConnectableSocket(sockPath string) bool {
	conn, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// DeadBatch describes a batch directory under <baseDir>/batches/ whose daemon
// process is no longer live, paired with the batch manifest that the
// directory persisted. RunDir is the absolute path to the batch directory.
// A batch dir with no manifest file is returned with the zero-value
// BatchManifest; only a malformed manifest is treated as an error.
type DeadBatch struct {
	RunDir   string
	Manifest BatchManifest
}

// RunTimestamp returns the timestamp callers should use to age-sort a
// dead batch. The manifest's CreatedAt is preferred when present; the
// run directory's modification time is used as a fallback so unmanif-
// ested runs can still be archived by age. Returns the zero time when
// neither source is available.
func (d DeadBatch) RunTimestamp() time.Time {
	if !d.Manifest.CreatedAt.IsZero() {
		return d.Manifest.CreatedAt
	}
	info, err := os.Stat(d.RunDir)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// FindDeadRunBatches scans <baseDir>/batches/ for batch directories that are
// not currently owned by a live daemon and returns their parsed
// manifests. Archived batches (<baseDir>/archive/) are not scanned because
// they live in a separate directory. Results are sorted lexicographically
// by RunDir for stable iteration. A batch dir with no `batch.json` is
// still returned with the zero-value BatchManifest. Returns (nil, nil) if
// <baseDir>/batches/ is missing so callers can treat a fresh repository
// the same as a clean one.
func FindDeadRunBatches(baseDir string) ([]DeadBatch, error) {
	locations, err := batchindex.DiscoverBatchLocations(layoutForBaseDir(baseDir))
	if err != nil {
		return nil, err
	}
	return findDeadBatchLocations(locations)
}

func findDeadBatchLocations(locations []paths.BatchLocation) ([]DeadBatch, error) {
	var batches []DeadBatch
	for _, location := range locations {
		batchPath := location.Dir
		if _, err := os.Stat(batchPath); os.IsNotExist(err) {
			continue
		}
		if IsRunActive(batchPath) {
			continue
		}
		manifest, err := ReadManifest(batchPath)
		if err != nil {
			if os.IsNotExist(err) {
				manifest = BatchManifest{}
			} else {
				return nil, fmt.Errorf("read manifest for %s: %w", batchPath, err)
			}
		}
		batches = append(batches, DeadBatch{RunDir: batchPath, Manifest: manifest})
	}
	sort.SliceStable(batches, func(i, j int) bool {
		return batches[i].RunDir < batches[j].RunDir
	})
	return batches, nil
}

// CleanupStaleRunSnapshots removes `<baseDir>/batches/<id>/config/` subtrees
// for batch dirs that are not currently active (no live `batch.sock`). Returns
// the number of snapshot directories removed. The batch dir itself and its
// manifest are left in place so operators can inspect them; the snapshot
// subtree, which can contain secrets copied from the host, is the part
// that must not accumulate after crashes.
func CleanupStaleRunSnapshots(baseDir string) (int, error) {
	dead, err := FindDeadRunBatches(baseDir)
	if err != nil {
		return 0, err
	}

	var removed int
	for _, batch := range dead {
		snapshotPath := filepath.Join(batch.RunDir, "config")
		info, err := os.Stat(snapshotPath)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			continue
		}
		if err := os.RemoveAll(snapshotPath); err != nil {
			continue
		}
		removed++
	}
	return removed, nil
}

// BatchManifest records the issues included in a batch run and when the
// batch was started. It is persisted to disk via WriteManifest and read
// back via ReadManifest so other sandman commands (status, portal) can
// inspect a live or completed run.
type BatchManifest struct {
	Issues       []int     `json:"issues,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	RunKind      string    `json:"runKind,omitempty"`
	BatchId      string    `json:"batchId,omitempty"`
	RunTS        string    `json:"runTs,omitempty"`
	RunShortID   string    `json:"runShortId,omitempty"`
	PR           *int      `json:"pr,omitempty"`
	PortalHidden bool      `json:"portalHidden,omitempty"`
}

// BatchDir returns a batch directory path under baseDir/batches/. The dirID
// argument is the pre-built batch identifier (the result of
// runid.NewBatchID for issue-driven batches, or a user-supplied
// --run-id for prompt-only mode — see runid.IsValidUserRunID for
// the validation rules the caller is expected to apply before
// passing the value in). BatchDir joins it verbatim without
// auto-generation. The directory itself is not created; callers
// decide when to mkdir.
func BatchDir(baseDir, dirID string) string {
	return filepath.Join(baseDir, "batches", dirID)
}

// ManifestPath returns the on-disk path of the batch manifest file
// within a batch directory.
func ManifestPath(batchDir string) string {
	return filepath.Join(batchDir, "batch.json")
}

// BatchesIndexPath returns the path to the batches index file.
func BatchesIndexPath(baseDir string) string {
	return filepath.Join(baseDir, "batches.json")
}

// WriteManifest serialises a BatchManifest as JSON and writes it to
// ManifestPath(runDir). The file is created with mode 0644 and the
// write is rename-style atomic so a crash between write and rename
// leaves the previous manifest intact.
func WriteManifest(runDir string, manifest BatchManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal batch manifest: %w", err)
	}
	return atomicfs.WriteAtomic(ManifestPath(runDir), data, 0644)
}

// ReadManifest decodes the batch manifest stored at runDir. It first
// tries run.json (new format), then falls back to batch.json (old format).
// The returned BatchManifest is the zero value if neither file exists.
func ReadManifest(runDir string) (BatchManifest, error) {
	runManifestPath := filepath.Join(runDir, "run.json")
	if data, err := os.ReadFile(runManifestPath); err == nil {
		var runManifest batchindex.RunManifest
		if err := json.Unmarshal(data, &runManifest); err == nil {
			manifest := BatchManifest{
				BatchId:      runManifest.BatchID,
				CreatedAt:    runManifest.CreatedAt,
				PortalHidden: runManifest.PortalHidden,
			}
			if runManifest.Issue > 0 {
				manifest.Issues = []int{runManifest.Issue}
			}
			return manifest, nil
		}
	}

	data, err := os.ReadFile(ManifestPath(runDir))
	if err != nil {
		return BatchManifest{}, err
	}
	var manifest BatchManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return BatchManifest{}, fmt.Errorf("decode batch manifest: %w", err)
	}
	return manifest, nil
}

// RunFolder returns the per-run folder path for a given batch root and run ID.
// It joins batchDir/runs/runID verbatim without auto-generation.
func RunFolder(batchDir, runID string) string {
	return (paths.BatchLocation{Dir: batchDir}).Run(runID).Dir
}

// BatchSocketPath returns the path to the batch control socket at the batch root.
func BatchSocketPath(batchDir string) string {
	return (paths.BatchLocation{Dir: batchDir}).SocketPath()
}

// RunSocketPath returns the path to the per-run command socket inside a run folder.
func RunSocketPath(batchDir, runID string) string {
	return (paths.BatchLocation{Dir: batchDir}).Run(runID).SocketPath()
}

// WriteRunManifest writes a RunManifest to the per-run folder under the batch.
// It creates the run folder if it does not exist.
func WriteRunManifest(batchDir, runID string, manifest batchindex.RunManifest) error {
	runDir := RunFolder(batchDir, runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	return batchindex.WriteManifest(runDir, manifest)
}

// ReadRunManifest reads a RunManifest from the per-run folder under the batch.
func ReadRunManifest(batchDir, runID string) (batchindex.RunManifest, error) {
	runDir := RunFolder(batchDir, runID)
	return batchindex.ReadManifest(runDir)
}

// UpdateRunManifestStatus reads an existing run.json under the batch, updates
// its Status field, and rewrites the file atomically. It returns an error if
// the manifest is missing or cannot be written.
func UpdateRunManifestStatus(batchDir, runID string, status batchindex.RunManifestStatus) error {
	manifest, err := ReadRunManifest(batchDir, runID)
	if err != nil {
		return fmt.Errorf("read run manifest for status update: %w", err)
	}
	manifest.Status = status
	if err := WriteRunManifest(batchDir, runID, manifest); err != nil {
		return fmt.Errorf("write run manifest for status update: %w", err)
	}
	return nil
}

// RecoverStaleRuns scans dead run batches under baseDir and emits a
// run.aborted event (with payload {"recovered": true}) via the supplied
// event log for each manifest issue whose RunState in the event log has
// not reached a terminal event and whose most-recent run.started /
// run.continued timestamp falls within the batch's time window
// (Started.Timestamp >= manifest.CreatedAt). Returns the number of runs
// recovered and the number of dead directories processed. Runs whose
// manifest has no CreatedAt are recovered regardless of the start time.
//
// After processing dead batches, RecoverStaleRuns also recovers orphaned
// active runs whose batch directory has been cleaned up (no directory
// under <baseDir>/runs/ mentions the run's issue or, for prompt-only runs,
// has zero issues in its manifest).
func RecoverStaleRuns(baseDir string, eventsList []events.Event, log events.EventLog) (int, int, error) {
	layout := layoutForBaseDir(baseDir)
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		return 0, 0, err
	}
	locations, err := idx.BatchLocations(layout)
	if err != nil {
		return 0, 0, err
	}
	dead, err := findDeadBatchLocations(locations)
	if err != nil {
		return 0, 0, err
	}

	runs := events.ProjectRunStates(eventsList)
	byIssue := make(map[int][]events.RunState)
	for _, run := range runs {
		issue := run.IssueNumber()
		if issue > 0 {
			byIssue[issue] = append(byIssue[issue], run)
		}
	}

	var recovered int
	recoveredRunIDs := make(map[string]struct{})
	// recoveredAt is the wall-clock time at which this recovery sweep
	// started. It is stamped onto every synthesized run.aborted event
	// so downstream consumers (portal Duration, archive --older-than,
	// clean --stale) reason about recovered runs using their real
	// recovery time instead of the zero-value timestamp. Captured once
	// per call so all events emitted by a single sweep share a coherent
	// timestamp.
	recoveredAt := time.Now().UTC()
	emitOrphan := func(run events.RunState, issueNumber int) error {
		var issueRef *int
		if issueNumber > 0 {
			ref := issueNumber
			issueRef = &ref
		}
		event := events.Event{
			Type:      "run.aborted",
			Timestamp: recoveredAt,
			RunID:     run.RunID,
			Issue:     issueNumber,
			IssueRef:  issueRef,
			Payload:   map[string]any{"recovered": true},
		}
		if err := log.Log(event); err != nil {
			return fmt.Errorf("log run.aborted for issue %d: %w", issueNumber, err)
		}
		recovered++
		recoveredRunIDs[run.RunID] = struct{}{}
		return nil
	}
	for _, batch := range dead {
		// A persisted row establishes exact physical ownership for every
		// run kind, including prompt-only and orphan Review Runs. The issue
		// window heuristics below remain the fallback for legacy batches.
		for _, run := range runs {
			if run.IsCapacityQueued() || run.IsTerminal() || (run.Started.Type != "run.started" && run.Started.Type != "run.continued") {
				continue
			}
			if _, done := recoveredRunIDs[run.RunID]; done {
				continue
			}
			if bid := run.BatchID(); bid != "" && !batchIdentityMatches(idx, layout, bid, batch.RunDir) {
				continue
			}
			if !batch.Manifest.CreatedAt.IsZero() && run.Started.Timestamp.Before(batch.Manifest.CreatedAt) {
				continue
			}
			manifest, err := ReadRunManifest(batch.RunDir, run.RunID)
			if err != nil || manifest.RunID != run.RunID {
				continue
			}
			if err := emitOrphan(run, run.IssueNumber()); err != nil {
				return recovered, len(dead), err
			}
			_ = UpdateRunManifestStatus(batch.RunDir, run.RunID, batchindex.RunManifestStatusAborted)
		}
		latestTerminal := latestTerminalForIssues(batch.Manifest.Issues, byIssue)
		for _, issueNumber := range batch.Manifest.Issues {
			for _, run := range byIssue[issueNumber] {
				if run.IsCapacityQueued() {
					// The ready continuation is durably queued for a later
					// scheduler admission. Preserve its worktree and event
					// state so the next run command can rehydrate it.
					continue
				}
				if _, ok := recoveredRunIDs[run.RunID]; ok {
					continue
				}
				if run.IsTerminal() || (run.Started.Type != "run.started" && run.Started.Type != "run.continued") {
					continue
				}
				// Batch-identity guard: when the candidate run was
				// started/continued under a known batch_id (the
				// on-disk batch directory basename stamped by the
				// orchestrator on run.started/run.continued), only
				// recover it for the dead batch it actually belongs
				// to. Without this check the byIssue walk above
				// pulls in runs from unrelated live batches that
				// happen to share an issue number, falsely emits
				// run.aborted recovered:true for them, and locks
				// the portal's event-log fold on a run the next
				// orchestrator is about to (or is already)
				// resuming — issue #2083. Legacy runs without
				// batch_id fall through to the CreatedAt /
				// latestTerminal heuristics below; that path is
				// exercised by every existing fixture in
				// recover_stale_test.go.
				bid := run.BatchID()
				if bid != "" && !batchIdentityMatches(idx, layout, bid, batch.RunDir) {
					continue
				}
				if !batch.Manifest.CreatedAt.IsZero() && run.Started.Timestamp.Before(batch.Manifest.CreatedAt) {
					continue
				}
				// A candidate is covered by this batch when its start
				// falls at or before the batch's last terminal event. A
				// candidate that started after the batch's last terminal
				// is an orphan from a later batch. A dead batch with no
				// terminal events has no activity to anchor the
				// candidate — treat the run as an orphan from the moment
				// the batch was created.
				if !latestTerminal.IsZero() && !run.Started.Timestamp.After(latestTerminal) {
					continue
				}
				if err := emitOrphan(run, issueNumber); err != nil {
					return recovered, len(dead), err
				}
				_ = UpdateRunManifestStatus(batch.RunDir, run.RunID, batchindex.RunManifestStatusAborted)
			}
		}
	}

	orphanRecovered, orphanErr := recoverOrphanActiveRuns(layout, idx, locations, eventsList, log, recoveredRunIDs, recoveredAt)
	if orphanErr != nil {
		return recovered, len(dead), orphanErr
	}
	recovered += orphanRecovered

	return recovered, len(dead), nil
}

// latestTerminalForIssues returns the latest real terminal timestamp
// across all runs in byIssue whose issue appears in issues. A real
// terminal event is run.finished, run.aborted, or run.cancelled (the
// kinds that signal actual work completed or was stopped). Queued and
// blocked placeholders are excluded — they are not real completions,
// just records of work that never started. The zero time is returned
// when no real terminal event exists for any of the issues, which
// signals that the batch is dead but no issue ever reached a real
// terminal state — the candidate (with a non-zero Started.Timestamp)
// is then strictly after the batch's last activity and is an orphan.
func latestTerminalForIssues(issues []int, byIssue map[int][]events.RunState) time.Time {
	var latest time.Time
	for _, issue := range issues {
		for _, run := range byIssue[issue] {
			if run.Finished == nil {
				continue
			}
			switch run.Finished.Type {
			case "run.finished", "run.aborted", "run.cancelled":
			default:
				continue
			}
			if run.Finished.Timestamp.After(latest) {
				latest = run.Finished.Timestamp
			}
		}
	}
	return latest
}

// recoverOrphanActiveRuns recovers active RunStates that have no matching
// batch directory under <baseDir>/batches/. Artifact loss never revises an
// existing terminal event, including queued and blocked placeholders.
func recoverOrphanActiveRuns(layout paths.Layout, idx *batchindex.Index, locations []paths.BatchLocation, eventsList []events.Event, log events.EventLog, skipRunIDs map[string]struct{}, recoveredAt time.Time) (int, error) {
	runs := events.ProjectRunStates(eventsList)

	byIssue := make(map[int][]events.RunState)
	for _, run := range runs {
		issue := run.IssueNumber()
		if issue > 0 {
			byIssue[issue] = append(byIssue[issue], run)
		}
	}

	// Collect all batch manifests under batches/ (both live and dead dirs).
	type batchInfo struct {
		dir      string
		manifest BatchManifest
	}
	var batches []batchInfo
	for _, location := range locations {
		batchPath := location.Dir
		if _, err := os.Stat(batchPath); os.IsNotExist(err) {
			continue
		}
		manifest, err := ReadManifest(batchPath)
		if err != nil {
			if os.IsNotExist(err) {
				manifest = BatchManifest{}
			} else {
				return 0, fmt.Errorf("read manifest for orphan scan %s: %w", batchPath, err)
			}
		}
		batches = append(batches, batchInfo{dir: batchPath, manifest: manifest})
	}

	var recovered int
	for _, run := range runs {
		if run.IsCapacityQueued() {
			continue
		}
		// Diagnostics without a start/continuation are unknown lifecycle,
		// not execution that recovery can declare aborted.
		if run.IsTerminal() || (run.Started.Type != "run.started" && run.Started.Type != "run.continued") {
			continue
		}
		if _, ok := skipRunIDs[run.RunID]; ok {
			continue
		}

		issueNum := run.IssueNumber()
		isPromptOnly := run.IsPromptOnly()
		hasBatch := false
		for _, b := range batches {
			if bid := run.BatchID(); bid != "" && !batchIdentityMatches(idx, layout, bid, b.dir) {
				continue
			}
			if isPromptOnly {
				if len(b.manifest.Issues) > 0 {
					continue
				}
				// A 0-issue batch that exists on disk but is dead means
				// the prompt-only daemon died — the run is orphaned.
				if !IsRunActive(b.dir) {
					continue
				}
			} else {
				hasIssue := false
				for _, issue := range b.manifest.Issues {
					if issue == issueNum {
						hasIssue = true
						break
					}
				}
				if !hasIssue {
					continue
				}
			}
			// A run that started before the batch was created predates
			// this batch entirely — the batch cannot cover it.
			if !b.manifest.CreatedAt.IsZero() && run.Started.Timestamp.Before(b.manifest.CreatedAt) {
				continue
			}
			// Zero CreatedAt means we can't determine the window —
			// conservatively assume this batch might cover the run.
			if b.manifest.CreatedAt.IsZero() {
				hasBatch = true
				break
			}
			// A live batch may still be processing the issue — assume
			// it covers the run regardless of timestamps.
			if IsRunActive(b.dir) {
				hasBatch = true
				break
			}
			// A dead batch covers the run only when the run's start
			// falls at or before the batch's last terminal event. A run
			// that started after the batch's last terminal is an orphan
			// from a later batch, not a stale run from this one. A dead
			// batch with no terminal events has no activity to anchor
			// the candidate — treat the run as an orphan from the
			// moment the batch was created.
			latestTerminal := latestTerminalForIssues(b.manifest.Issues, byIssue)
			if latestTerminal.IsZero() {
				continue
			}
			if run.Started.Timestamp.After(latestTerminal) {
				continue
			}
			hasBatch = true
			break
		}
		if hasBatch {
			continue
		}

		var issueRef *int
		if issueNum > 0 {
			issueRef = &issueNum
		}
		event := events.Event{
			Type:      "run.aborted",
			Timestamp: recoveredAt,
			RunID:     run.RunID,
			Issue:     issueNum,
			IssueRef:  issueRef,
			Payload:   map[string]any{"recovered": true},
		}
		if err := log.Log(event); err != nil {
			return recovered, fmt.Errorf("log run.aborted for orphan %q: %w", run.RunID, err)
		}
		recovered++
	}
	return recovered, nil
}

// baseDir is the Sandman data root, including for callers with a custom root.
func layoutForBaseDir(baseDir string) paths.Layout {
	return paths.Layout{RepoRoot: filepath.Dir(baseDir), SandmanDir: baseDir,
		BatchesDir: filepath.Join(baseDir, "batches"), ArchiveDir: filepath.Join(baseDir, "archive"),
		BatchesIndexPath: filepath.Join(baseDir, "batches.json")}
}

func batchIdentityMatches(idx *batchindex.Index, layout paths.Layout, id, dir string) bool {
	return idx.MatchesBatchLocation(layout, id, dir)
}
