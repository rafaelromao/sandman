package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
)

// IsRunActive reports whether a batch directory is currently owned by a live
// daemon process. A batch is considered active when its `batch.sock` is
// connectable. Batch dirs that survived a crash (no live socket) are stale
// and safe to clean up.
func IsRunActive(batchPath string) bool {
	batchSock := filepath.Join(batchPath, "batch.sock")
	conn, err := net.DialTimeout("unix", batchSock, 100*time.Millisecond)
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
// manifests. Results are sorted lexicographically by RunDir for stable
// iteration. A batch dir with no `batch.json` is still returned with the
// zero-value BatchManifest. Returns (nil, nil) if <baseDir>/batches/ is
// missing so callers can treat a fresh repository the same as a clean
// one.
func FindDeadRunBatches(baseDir string) ([]DeadBatch, error) {
	batchesDir := filepath.Join(baseDir, "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read batches dir: %w", err)
	}

	var batches []DeadBatch
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		batchPath := filepath.Join(batchesDir, entry.Name())
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
	Issues     []int     `json:"issues,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	RunKind    string    `json:"runKind,omitempty"`
	BatchId    string    `json:"batchId,omitempty"`
	PR         *int      `json:"pr,omitempty"`
	Candidates []int     `json:"candidates,omitempty"`
	Query      string    `json:"query,omitempty"`
	Count      int       `json:"count,omitempty"`
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

// RunDir is a deprecated alias for BatchDir. It is kept for backward
// compatibility during the transition period (Slice 1-4). New code should
// use BatchDir. This alias will be removed when .sandman/runs/ is wiped
// in Slice 5.
func RunDir(baseDir, dirID string) string {
	return BatchDir(baseDir, dirID)
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
// ManifestPath(runDir). The file is created with mode 0644.
func WriteManifest(runDir string, manifest BatchManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal batch manifest: %w", err)
	}
	if err := os.WriteFile(ManifestPath(runDir), data, 0644); err != nil {
		return fmt.Errorf("write batch manifest: %w", err)
	}
	return nil
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
				BatchId:   runManifest.BatchID,
				CreatedAt: runManifest.CreatedAt,
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
	return filepath.Join(batchDir, "runs", runID)
}

// BatchSocketPath returns the path to the batch control socket at the batch root.
func BatchSocketPath(batchDir string) string {
	return filepath.Join(batchDir, "batch.sock")
}

// RunSocketPath returns the path to the per-run command socket inside a run folder.
func RunSocketPath(batchDir, runID string) string {
	return filepath.Join(RunFolder(batchDir, runID), "run.sock")
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
	dead, err := FindDeadRunBatches(baseDir)
	if err != nil {
		return 0, 0, err
	}

	runs := events.ProjectRunStates(eventsList)
	byIssue := make(map[int][]events.RunState)
	runsByID := make(map[string]events.RunState)
	for _, run := range runs {
		issue := run.IssueNumber()
		if issue > 0 {
			byIssue[issue] = append(byIssue[issue], run)
		}
		runsByID[run.RunID] = run
	}

	var recovered int
	recoveredRunIDs := make(map[string]struct{})
	emitOrphan := func(run events.RunState, issueNumber int) error {
		var issueRef *int
		if issueNumber > 0 {
			ref := issueNumber
			issueRef = &ref
		}
		event := events.Event{
			Type:     "run.aborted",
			RunID:    run.RunID,
			Issue:    issueNumber,
			IssueRef: issueRef,
			Payload:  map[string]any{"recovered": true},
		}
		if err := log.Log(event); err != nil {
			return fmt.Errorf("log run.aborted for issue %d: %w", issueNumber, err)
		}
		recovered++
		recoveredRunIDs[run.RunID] = struct{}{}
		return nil
	}
	for _, batch := range dead {
		if batch.Manifest.RunKind == "auto-select" {
			autoSelectRunID := batch.Manifest.BatchId
			run, ok := runsByID[autoSelectRunID]
			if !ok || !run.IsAutoSelect() {
				continue
			}
			if _, ok := recoveredRunIDs[run.RunID]; ok {
				continue
			}
			if !run.IsActive() && run.Status() != "queued" && run.Status() != "blocked" {
				continue
			}
			if !batch.Manifest.CreatedAt.IsZero() && run.Started.Timestamp.Before(batch.Manifest.CreatedAt) {
				continue
			}
			event := events.Event{
				Type:  "run.aborted",
				RunID: run.RunID,
				Payload: map[string]any{
					"recovered": true,
					"run_kind":  "auto-select",
				},
			}
			if err := log.Log(event); err != nil {
				return recovered, len(dead), fmt.Errorf("log run.aborted for auto-select run %s: %w", run.RunID, err)
			}
			recovered++
			recoveredRunIDs[run.RunID] = struct{}{}
			continue
		}
		latestTerminal := latestTerminalForIssues(batch.Manifest.Issues, byIssue)
		for _, issueNumber := range batch.Manifest.Issues {
			for _, run := range byIssue[issueNumber] {
				if _, ok := recoveredRunIDs[run.RunID]; ok {
					continue
				}
				if !run.IsActive() && run.Status() != "queued" && run.Status() != "blocked" {
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
			}
		}
	}

	orphanRecovered, orphanErr := recoverOrphanActiveRuns(baseDir, eventsList, log, recoveredRunIDs)
	if orphanErr != nil {
		return recovered, len(dead), orphanErr
	}
	recovered += orphanRecovered

	return recovered, len(dead), nil
}

// isSupersedingRun reports whether the RunState was created by a run.started
// or run.continued event AND has not been subsequently aborted or cancelled.
// A started run only truly supersedes an earlier placeholder when its work
// actually completed; a started run that was aborted or cancelled left the
// work undone, so the placeholder is still an orphan.
func isSupersedingRun(r events.RunState) bool {
	if r.Started.Type != "run.started" && r.Started.Type != "run.continued" {
		return false
	}
	if r.Finished != nil && (r.Finished.Type == "run.aborted" || r.Finished.Type == "run.cancelled") {
		return false
	}
	return true
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

// buildSupersededIssues returns a set of issue numbers for which a queued or
// blocked run placeholder was superseded by a later started run (different
// RunID) for the same issue. These are historical artifacts from a completed
// batch, not orphans from a dead daemon, and should not be recovered. A
// queued/blocked placeholder that was re-queued by a subsequent failed batch
// does NOT count as superseded — only actual work (run.started) does.
func buildSupersededIssues(runs []events.RunState) map[int]bool {
	byIssue := make(map[int][]events.RunState)
	for _, r := range runs {
		if issue := r.IssueNumber(); issue > 0 {
			byIssue[issue] = append(byIssue[issue], r)
		}
	}
	superseded := make(map[int]bool)
	for issue, sameIssue := range byIssue {
		if len(sameIssue) < 2 {
			continue
		}
		for _, s := range sameIssue {
			if !s.IsActive() && (s.Status() == "queued" || s.Status() == "blocked") {
				for _, other := range sameIssue {
					if other.RunID == s.RunID {
						continue
					}
					if !isSupersedingRun(other) {
						continue
					}
					if other.Started.Timestamp.After(s.Started.Timestamp) {
						superseded[issue] = true
						break
					}
				}
			}
			if superseded[issue] {
				break
			}
		}
	}
	return superseded
}

// recoverOrphanActiveRuns recovers active RunStates that have no matching
// batch directory under <baseDir>/batches/. In addition to truly active runs,
// queued and blocked runs are also recovered when no subsequent run.started
// exists for the same issue (meaning the queued/blocked state was never
// superseded by actual work — the batch was destroyed, not completed).
func recoverOrphanActiveRuns(baseDir string, eventsList []events.Event, log events.EventLog, skipRunIDs map[string]struct{}) (int, error) {
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
	batchesDir := filepath.Join(baseDir, "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return 0, fmt.Errorf("read batches dir for orphan scan: %w", err)
		}
	}
	if entries != nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			batchPath := filepath.Join(batchesDir, entry.Name())
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
	}

	// Build a set of issue numbers where a queued/blocked placeholder was
	// superseded by a later run (different RunID) for the same issue. These
	// are historical artifacts from a completed batch, not orphans.
	supersededIssues := buildSupersededIssues(runs)

	var recovered int
	for _, run := range runs {
		if !run.IsActive() && run.Status() != "queued" && run.Status() != "blocked" {
			continue
		}
		if _, ok := skipRunIDs[run.RunID]; ok {
			continue
		}
		if !run.IsActive() {
			if issueNum := run.IssueNumber(); issueNum > 0 && supersededIssues[issueNum] {
				continue
			}
		}

		issueNum := run.IssueNumber()
		isPromptOnly := run.IsPromptOnly()
		hasBatch := false
		for _, b := range batches {
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
			Type:     "run.aborted",
			RunID:    run.RunID,
			Issue:    issueNum,
			IssueRef: issueRef,
			Payload:  map[string]any{"recovered": true},
		}
		if err := log.Log(event); err != nil {
			return recovered, fmt.Errorf("log run.aborted for orphan %q: %w", run.RunID, err)
		}
		recovered++
	}
	return recovered, nil
}
