package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/rafaelromao/sandman/internal/events"
)

// CleanupOrphanedTestBatches removes batch directories under
// <sandmanDir>/batches/ that are no longer referenced by any
// run.started event and that are not currently owned by a live
// daemon. It is intended for one-shot cleanup of orphaned test
// batch directories left behind by un-isolated unit tests.
//
// A batch directory is considered orphaned when ALL of the
// following hold:
//   - the directory contains a batch.json manifest;
//   - no run.started event in log has a RunID matching the batch
//     directory name, nor matching the basename of any entry under
//     <batch>/runs/;
//   - isActive(batchDir) reports false (no live batch.sock and no
//     live runs/*/run.sock).
//
// The function does not touch .sandman/archive/ or .sandman/events.jsonl.
// If log.Read fails, the function returns the error and removes
// nothing (fail-closed semantics). Returns the absolute paths of the
// removed directories sorted ascending by filepath.Base for
// deterministic output.
func CleanupOrphanedTestBatches(sandmanDir string, log events.EventLog, isActive func(string) bool) ([]string, error) {
	batchesDir := filepath.Join(sandmanDir, "batches")
	entries, err := os.ReadDir(batchesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read batches dir: %w", err)
	}

	eventList, err := log.Read()
	if err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}

	runIDs := collectRunStartedIDs(eventList)

	candidates := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		batchPath := filepath.Join(batchesDir, entry.Name())
		if _, err := os.Stat(ManifestPath(batchPath)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat manifest for %s: %w", batchPath, err)
		}
		if hasMatchingRunID(batchPath, runIDs) {
			continue
		}
		if isActive != nil && isActive(batchPath) {
			continue
		}
		candidates = append(candidates, batchPath)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return filepath.Base(candidates[i]) < filepath.Base(candidates[j])
	})

	removed := make([]string, 0, len(candidates))
	for _, batchPath := range candidates {
		if err := os.RemoveAll(batchPath); err != nil {
			return removed, fmt.Errorf("remove %s: %w", batchPath, err)
		}
		removed = append(removed, batchPath)
	}
	return removed, nil
}

func collectRunStartedIDs(eventList []events.Event) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, ev := range eventList {
		if ev.Type != "run.started" {
			continue
		}
		if ev.RunID == "" {
			continue
		}
		ids[ev.RunID] = struct{}{}
	}
	return ids
}

func hasMatchingRunID(batchPath string, runIDs map[string]struct{}) bool {
	if _, ok := runIDs[filepath.Base(batchPath)]; ok {
		return true
	}
	runsDir := filepath.Join(batchPath, "runs")
	runEntries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		return false
	}
	for _, runEntry := range runEntries {
		if _, ok := runIDs[runEntry.Name()]; ok {
			return true
		}
	}
	return false
}