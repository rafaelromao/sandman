package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// IsRunActive reports whether a run directory is currently owned by a live
// daemon process. A run is considered active when its `run.sock` is
// connectable. Run dirs that survived a crash (no live socket) are stale and
// safe to clean up.
func IsRunActive(runPath string) bool {
	cmdSock := filepath.Join(runPath, "cmd.sock")
	conn, err := net.DialTimeout("unix", cmdSock, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return true
	}

	runSock := filepath.Join(runPath, "run.sock")
	conn, err = net.DialTimeout("unix", runSock, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// DeadBatch describes a run directory under <baseDir>/runs/ whose daemon
// process is no longer live, paired with the batch manifest that the
// directory persisted. RunDir is the absolute path to the run directory.
// A run dir with no manifest file is returned with the zero-value
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

// FindDeadRunBatches scans <baseDir>/runs/ for run directories that are
// not currently owned by a live daemon and returns their parsed
// manifests. Results are sorted lexicographically by RunDir for stable
// iteration. A run dir with no `batch.json` is still returned with the
// zero-value BatchManifest. Returns (nil, nil) if <baseDir>/runs/ is
// missing so callers can treat a fresh repository the same as a clean
// one.
func FindDeadRunBatches(baseDir string) ([]DeadBatch, error) {
	runsDir := filepath.Join(baseDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs dir: %w", err)
	}

	var batches []DeadBatch
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runPath := filepath.Join(runsDir, entry.Name())
		if IsRunActive(runPath) {
			continue
		}
		manifest, err := ReadManifest(runPath)
		if err != nil {
			if os.IsNotExist(err) {
				manifest = BatchManifest{}
			} else {
				return nil, fmt.Errorf("read manifest for %s: %w", runPath, err)
			}
		}
		batches = append(batches, DeadBatch{RunDir: runPath, Manifest: manifest})
	}
	sort.SliceStable(batches, func(i, j int) bool {
		return batches[i].RunDir < batches[j].RunDir
	})
	return batches, nil
}

// CleanupStaleRunSnapshots removes `<baseDir>/runs/<id>/config/` subtrees
// for run dirs that are not currently active (no live `run.sock`). Returns
// the number of snapshot directories removed. The run dir itself and its
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
	Issues    []int     `json:"issues"`
	CreatedAt time.Time `json:"createdAt"`
}

// RunDir returns a unique run directory path under baseDir/runs/.
// The directory itself is not created; callers decide when to mkdir.
func RunDir(baseDir string, issues []int) string {
	id := fmt.Sprintf("run-%d", time.Now().UnixNano())
	if len(issues) > 0 {
		id = fmt.Sprintf("run-%d-%d", issues[0], time.Now().UnixNano())
	}
	return filepath.Join(baseDir, "runs", id)
}

// ManifestPath returns the on-disk path of the batch manifest file
// within a run directory.
func ManifestPath(runDir string) string {
	return filepath.Join(runDir, "batch.json")
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

// ReadManifest decodes the batch manifest stored at ManifestPath(runDir).
// The returned BatchManifest is the zero value if the file does not exist.
func ReadManifest(runDir string) (BatchManifest, error) {
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
	byIssue := make(map[int]events.RunState, len(runs))
	for _, run := range runs {
		issue := run.IssueNumber()
		if issue > 0 {
			byIssue[issue] = run
		}
	}

	var recovered int
	recoveredRunIDs := make(map[string]struct{})
	for _, batch := range dead {
		for _, issueNumber := range batch.Manifest.Issues {
			run, ok := byIssue[issueNumber]
			if !ok {
				continue
			}
			if !run.IsActive() && run.Status() != "queued" && run.Status() != "blocked" {
				continue
			}
			if !batch.Manifest.CreatedAt.IsZero() && run.Started.Timestamp.Before(batch.Manifest.CreatedAt) {
				continue
			}
			issueRef := issueNumber
			event := events.Event{
				Type:     "run.aborted",
				RunID:    run.RunID,
				Issue:    issueNumber,
				IssueRef: &issueRef,
				Payload:  map[string]any{"recovered": true},
			}
			if err := log.Log(event); err != nil {
				return recovered, len(dead), fmt.Errorf("log run.aborted for issue %d: %w", issueNumber, err)
			}
			recovered++
			recoveredRunIDs[run.RunID] = struct{}{}
		}
	}

	orphanRecovered, orphanErr := recoverOrphanActiveRuns(baseDir, eventsList, log, recoveredRunIDs)
	if orphanErr != nil {
		return recovered, len(dead), orphanErr
	}
	recovered += orphanRecovered

	return recovered, len(dead), nil
}

// buildSupersededIssues returns a set of issue numbers for which a queued or
// blocked run placeholder was superseded by a later run (different RunID)
// for the same issue. These are historical artifacts from a completed batch,
// not orphans from a dead daemon, and should not be recovered.
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
// batch directory under <baseDir>/runs/. A run is orphaned when no directory
// manifest mentions its issue number (or, for prompt-only runs, has zero
// issues) AND the run's StartedAt falls outside any matching batch's time
// window. Runs whose RunID appears in skipRunIDs are not processed.
func recoverOrphanActiveRuns(baseDir string, eventsList []events.Event, log events.EventLog, skipRunIDs map[string]struct{}) (int, error) {
	runs := events.ProjectRunStates(eventsList)

	// Collect all batch manifests under runs/ (both live and dead dirs).
	type batchInfo struct {
		dir      string
		manifest BatchManifest
	}
	var batches []batchInfo
	runsDir := filepath.Join(baseDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return 0, fmt.Errorf("read runs dir for orphan scan: %w", err)
		}
	} else {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			runPath := filepath.Join(runsDir, entry.Name())
			manifest, err := ReadManifest(runPath)
			if err != nil {
				if os.IsNotExist(err) {
					manifest = BatchManifest{}
				} else {
					return 0, fmt.Errorf("read manifest for orphan scan %s: %w", runPath, err)
				}
			}
			batches = append(batches, batchInfo{dir: runPath, manifest: manifest})
		}
	}

	// Build a set of issue numbers that have a subsequent run (different RunID
	// or within the same RunState via run.continued). If a queued or blocked
	// run was superseded by a later run for the same issue, it is a historical
	// placeholder from a completed batch — not an orphan from a dead daemon.
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
			issueNum := run.IssueNumber()
			if issueNum > 0 && supersededIssues[issueNum] {
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
			// Zero CreatedAt means we can't determine the window —
			// conservatively assume this batch might cover the run.
			if b.manifest.CreatedAt.IsZero() || !run.Started.Timestamp.Before(b.manifest.CreatedAt) {
				hasBatch = true
				break
			}
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
