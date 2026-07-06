package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/runid"
)

type portalEvent struct {
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Payload   map[string]any `json:"payload,omitempty"`
}

type portalRun struct {
	Key         string     `json:"key"`
	RunID       string     `json:"runId"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	IssueLabel  string     `json:"issueLabel"`
	IssueNumber int        `json:"issueNumber,omitempty"`
	Branch      string     `json:"branch,omitempty"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Duration    string     `json:"duration,omitempty"`
	// LastOutputAt is the staleness signal for active runs: the mtime of
	// the run-folder log (<batchDir>/runs/<runID>/run.log, opened with
	// O_APPEND during AgentRun.Execute), falling back to StartedAt when no
	// log file exists yet. It is populated only for active rows and omitted
	// from JSON for terminal rows, so the /api/runs contract carries
	// staleness only where it is meaningful. The portal renders this as
	// a "stale · Ns" chip past an idle threshold.
	LastOutputAt *time.Time    `json:"lastOutputAt,omitempty"`
	SocketPath   string        `json:"socketPath,omitempty"`
	LogPath      string        `json:"logPath,omitempty"`
	LogURL       string        `json:"logUrl,omitempty"`
	Log          string        `json:"log,omitempty"`
	Events       []portalEvent `json:"events,omitempty"`
	// Review flags runs whose run.started event carried payload.review = true.
	// The field is omitted from JSON when false to preserve the existing /api/runs
	// contract for implementation runs.
	Review bool `json:"review,omitempty"`
	// ReviewCount summarizes child review runs owned by a canonical issue row.
	// Kept on the struct for forward compat with the JSON wire shape; the
	// portal no longer stamps this from the event log after issue #1825
	// (no Go writer remains), and the orphan-review JS path
	// (portal.html visibleRunForIssueGroup) synthesizes its own value
	// for review-only groups.
	ReviewCount int `json:"reviewCount,omitempty"`
	// ReviewVerdict carries latest terminal child-review status for canonical
	// issue rows. Kept on the struct for the same reason as ReviewCount:
	// the JSON wire shape is preserved, no Go writer remains, and the
	// orphan-review JS path synthesizes its own value for review-only
	// groups.
	ReviewVerdict string `json:"reviewVerdict,omitempty"`
	// GroupedReview marks review rows that are owned by an issue-parent row.
	// Kept on the struct for the same reason as ReviewCount and
	// ReviewVerdict: the JSON wire shape is preserved. The portal no
	// longer sets this from the event log; the orphan-review JS path
	// hardcodes groupedReview=false on its synthetic row.
	GroupedReview bool `json:"groupedReview,omitempty"`
	// PRNumber mirrors payload.pr_number from the run.started event. Only
	// meaningful when Review is true; omitted from JSON otherwise.
	PRNumber int `json:"prNumber,omitempty"`
	// BatchKey ties a row to the batch (active runDir) that produced it.
	// Active-batch derived rows carry the active runDir's name; historical
	// rows from the event log carry "". Dedup only collapses rows that share
	// the same (IssueNumber, BatchKey) so a current active row is never hidden
	// by a historical aborted row from another batch.
	BatchKey string `json:"batchKey,omitempty"`
	// BatchIssues carries the full ordered list of issues in the batch the
	// row came from. Populated for active-batch derived rows; omitted for
	// historical or prompt-only runs. When len(BatchIssues) > 1 the row is
	// part of a mixed batch and must not be mistaken for a private
	// single-issue run.
	BatchIssues []int `json:"batchIssues,omitempty"`
	// IssueTitle carries the human-readable GitHub issue title from the event
	// payload. Empty for historical or prompt-only runs.
	IssueTitle string `json:"issueTitle,omitempty"`
	// Reason is the run-kind label rendered in the chip column for
	// auto-select and review runs; empty for issue-driven and prompt-only
	// runs.
	Reason string `json:"reason,omitempty"`
	// Candidates carries the issue numbers considered by the auto-select
	// agent during the selection phase. Populated from run.started
	// payload.candidates; omitted for non-auto-select runs.
	Candidates []int `json:"candidates,omitempty"`
	// RetriesTotal is the number of retry attempts the orchestrator allowed
	// for the run. Omitted when the run has not finished.
	RetriesTotal int `json:"retriesTotal,omitempty"`
	// RetriesDone is the number of retry attempts the run actually consumed.
	// Omitted when the run has not finished.
	RetriesDone int `json:"retriesDone,omitempty"`
	// Attempts is the live attempt count for active runs (highest
	// `attempt` value across the run's `run.retry` events, sourced from
	// events.RunState.LiveAttempt) and the finished-payload retry count
	// (Finished.Payload["retries_done"]) for finished runs. The finished
	// payload wins when both signals are present, matching the
	// acceptance contract for slice 1 of #1499. Omitted when the run has
	// not retried (zero value).
	Attempts int `json:"attempts,omitempty"`
	// LastRetryReason is the `reason` of the most recent `run.retry`
	// event for the run, regardless of whether the run has finished. The
	// finished payload does not carry a `reason`, so this is the
	// only place the field can be sourced. Omitted when no retries
	// exist or when the most recent retry payload omits the key
	// (the current orchestrator shape; slice 3 of #1499 will populate
	// it).
	LastRetryReason string `json:"lastRetryReason,omitempty"`
	// Archived is true when a completed batch's directory has been
	// relocated from .sandman/batches/<batch-id> to .sandman/archive/<batch-id>
	// by `sandman archive`. The field is always present in JSON so the
	// /api/runs contract carries an "archived" key for every row.
	// Active runs are never marked archived, even when an archive
	// directory with the matching RunID happens to exist on disk.
	Archived bool `json:"archived"`
	// Unavailable is true when the batchindex entry backing this row has
	// been flipped to StatusUnavailable (lazy flip on read in
	// discoverPortalInstances, when the on-disk batch directory is gone).
	// Surfaced alongside Archived so the portal can mark the row as
	// read-only and badge it as unavailable. Active runs are never
	// marked unavailable; only completed historical rows that lost their
	// backing directory reach this state.
	Unavailable bool `json:"unavailable"`
	// SourceExists reports whether the run still has a backing directory
	// under .sandman/batches/<batch-id>/runs/<run-id>. The portal uses this
	// to avoid showing Archive actions for stale historical rows whose
	// source directory is already gone.
	SourceExists bool `json:"sourceExists"`
}

type portalActiveRun struct {
	Key          string
	Dir          string
	SocketPath   string
	LiveOutput   string
	LastOutputAt time.Time
	IssueNumber  int
	IssueNumbers []int
	PRNumber     int
	BatchID      string
	RunID        string
	RunTS        string
	RunShortID   string
	StartedAt    time.Time
	ModTime      time.Time
}

type portalRunMatch struct {
	instance portalActiveRun
	state    *events.RunState
}

type portalRunsView struct {
	mu            sync.Mutex
	manifestCache map[string]portalManifestCacheEntry
}

type portalManifestCacheEntry struct {
	size     int64
	modTime  time.Time
	manifest daemon.BatchManifest
}

func (v *portalRunsView) readManifestCached(runDir string) (daemon.BatchManifest, error) {
	path := daemon.ManifestPath(runDir)
	info, err := os.Stat(path)
	if err != nil {
		return daemon.BatchManifest{}, err
	}
	v.mu.Lock()
	if v.manifestCache == nil {
		v.manifestCache = make(map[string]portalManifestCacheEntry)
	}
	if entry, ok := v.manifestCache[path]; ok && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
		manifest := entry.manifest
		v.mu.Unlock()
		return manifest, nil
	}
	v.mu.Unlock()

	manifest, err := daemon.ReadManifest(runDir)
	if err != nil {
		return daemon.BatchManifest{}, err
	}
	v.mu.Lock()
	v.manifestCache[path] = portalManifestCacheEntry{size: info.Size(), modTime: info.ModTime(), manifest: manifest}
	v.mu.Unlock()
	return manifest, nil
}

type runLocator struct {
	batchID string
	runID   string
}

func batchIDFromRunID(runID string) string {
	if runID == "" {
		return ""
	}
	dashCount := 0
	for i := 0; i < len(runID); i++ {
		if runID[i] == '-' {
			dashCount++
			if dashCount == 2 {
				return runID[:i]
			}
		}
	}
	return runID
}

func batchKeyForActive(active portalActiveRun) string {
	if active.Key != "" {
		return active.Key
	}
	if active.BatchID != "" {
		return active.BatchID
	}
	base := filepath.Base(active.Dir)
	if base != "" && base != "." && base != "/" {
		return base
	}
	return "active-" + active.RunID
}

func activeKeyForActive(active portalActiveRun) string {
	if active.Key != "" {
		return active.Key
	}
	if active.BatchID != "" {
		return active.BatchID
	}
	if !strings.HasSuffix(active.Dir, "/") {
		base := filepath.Base(active.Dir)
		if base != "" && base != "." && base != "/" {
			return base
		}
	}
	return "active-" + active.RunID
}

const portalViewDegradeLogInterval = 30 * time.Second

var (
	portalViewDegradeLogMu   sync.Mutex
	portalViewDegradeLogSeen = make(map[string]time.Time)
	// reviewSectionDecisionHeading matches the bare "## Decision"
	// heading on a line by itself (case-insensitive, optional trailing
	// whitespace). The match is anchored to the whole line so headings
	// like "## Decisions" or "## Decision Tree" do not collide with
	// this section's verdict scan (issue #1729 review feedback).
	reviewSectionDecisionHeading = regexp.MustCompile(`(?i)^## decision\s*$`)
	// reviewVerdictMarkerLine matches a whole line whose only content
	// is the literal **MARKER** form. The original narrow
	// `^\*\*([A-Z_]+)\*\*$` (issue #1729) anchored the whole line
	// and rejected any line whose marker was followed by anything
	// else, so a stray character rendered the verdict "Unclear".
	// Issue #1767 broadened the trailing to `"?\s*$` to tolerate
	// the bash closing quote from `gh pr comment --body "..."`,
	// but production log captures (d9f0-260704185852-1779-PR1789)
	// showed the shell frequently also leaves a redirect-and-pipe
	// trailer on the same line — e.g. a marker line ending in
	// `" 2>&1 | tail -5` — which the narrower regex still
	// rejected. The current rules therefore:
	//
	//  1. Accept the bare marker line (no trailing characters).
	//  2. Accept a marker followed by a non-whitespace sentinel
	//     (closing quote, backtick, period, dash, ampersand, pipe,
	//     or single quote) and then any characters to end-of-line.
	//     The sentinel rules out mid-line prose such as
	//     `**APPROVED** is unrelated prose` (which would start
	//     with a space) without rejecting the production shell
	//     debris shapes. Issue #1792 (follow-up to #1767, itself
	//     a follow-up to #1729).
	reviewVerdictMarkerLineBare       = regexp.MustCompile(`^\*\*([A-Z_]+)\*\*$`)
	reviewVerdictMarkerLineWithDebris = regexp.MustCompile("^\\*\\*([A-Z_]+)\\*\\*[\"'`\\.\\-|&][^\\n]*$")
	// reviewLogTimestampPrefix strips the "[<runID>] HH:MM:SS " log
	// prefix that the agent output stream adds to each line.
	reviewLogTimestampPrefix = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}\s+`)
)

// logPortalViewDegrade rate-limits repeated portal-view degradation logs per
// key so hot-path failures (e.g. a missing socket or a dead run.sock) do not
// flood the user's terminal every 2s. The goal is observability, not chatter:
// the first failure is surfaced immediately, repeats are coalesced for a short
// window, and a different path/socket gets its own first log.
func logPortalViewDegrade(key, format string, args ...any) {
	now := time.Now()
	portalViewDegradeLogMu.Lock()
	if last, ok := portalViewDegradeLogSeen[key]; ok && now.Sub(last) < portalViewDegradeLogInterval {
		portalViewDegradeLogMu.Unlock()
		return
	}
	portalViewDegradeLogSeen[key] = now
	portalViewDegradeLogMu.Unlock()
	log.Printf("portal view: "+format, args...)
}

// compute is the entry point for computing displayable portal runs.
func (v *portalRunsView) compute(repoRoot string, eventLog events.EventLog) ([]portalRun, error) {
	eventList, err := eventLog.Read()
	if err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}
	return v.computeFromEvents(repoRoot, eventList)
}

func (v *portalRunsView) computeFromEvents(repoRoot string, eventList []events.Event) ([]portalRun, error) {
	eventsByRun := v.groupEventsByRun(eventList)

	activeInstances, err := v.discoverActiveRuns(repoRoot, eventsByRun)
	if err != nil {
		return nil, err
	}
	return v.computeWithActiveRuns(repoRoot, eventList, eventsByRun, activeInstances)
}

func (v *portalRunsView) computeWithActiveRuns(repoRoot string, eventList []events.Event, eventsByRun map[string][]portalEvent, activeInstances []portalActiveRun) ([]portalRun, error) {
	idx := v.loadBatchesIndex(repoRoot)
	return v.computeWithActiveRunsAndIndex(repoRoot, eventList, eventsByRun, activeInstances, idx)
}

func (v *portalRunsView) computeWithActiveRunsAndIndex(repoRoot string, eventList []events.Event, eventsByRun map[string][]portalEvent, activeInstances []portalActiveRun, idx *batchindex.Index) ([]portalRun, error) {
	runStates := events.ProjectRunStates(eventList)
	activeStates := make([]events.RunState, 0, len(runStates))
	activeBatchStart := time.Time{}
	for _, run := range runStates {
		if run.IsActive() {
			activeStates = append(activeStates, run)
		}
	}

	var dirIDs map[string]string
	var deadBatches []daemon.DeadBatch
	var err error

	for i := range activeInstances {
		if activeInstances[i].SocketPath == "" {
			continue
		}
		activeInstances[i].LiveOutput = v.readPortalSocketOutput(activeInstances[i].SocketPath)
	}

	runs := make([]portalRun, 0, len(runStates)+len(activeInstances))
	consumedRunIDs := make(map[string]struct{})
	promptActive := make([]portalActiveRun, 0, len(activeInstances))
	deadBatches = v.deadBatchesFromIndex(idx, activeInstances)
	_, dirIDs, err = v.deadBatchDirIDsByRunID(idx)
	if err != nil {
		return nil, err
	}
	// Load the batch index once so the post-loop can stamp rows whose
	// backing directory was deleted (StatusUnavailable) without doing a
	// separate Stat per run. Active entries only matter here for the
	// StatusUnavailable lookup; the existing discoverPortalInstances path
	// still owns the live-socket probe and the lazy flip itself.
	unavailableRunIDs := v.unavailableRunIDsByBatchIndex(idx)
	for _, active := range activeInstances {
		if activeBatchStart.IsZero() && !active.StartedAt.IsZero() {
			activeBatchStart = active.StartedAt
		}
		if len(active.IssueNumbers) == 0 {
			promptActive = append(promptActive, active)
			continue
		}
		batchRuns, usedRunIDs := v.runsFromActiveBatch(repoRoot, active, runStates, eventList, eventsByRun, deadBatches)
		runs = append(runs, batchRuns...)
		for runID := range usedRunIDs {
			consumedRunIDs[runID] = struct{}{}
		}
	}

	matchedActive := v.matchActiveRuns(promptActive, activeStates)
	// Unmatched prompt-only instances whose RunID has a terminal state in
	// the event log adopt that state, so the dedup below collapses the
	// active and terminal rows into one. Without this pass, an
	// auto-select or review daemon that emits run.finished while its
	// socket is still alive would leave the row stuck on "running" /
	// "reviewing" forever.
	for i := range matchedActive {
		if matchedActive[i].state != nil {
			continue
		}
		for j := range runStates {
			if runStates[j].RunID != matchedActive[i].instance.Key {
				continue
			}
			if _, ok := consumedRunIDs[runStates[j].RunID]; ok {
				continue
			}
			state := runStates[j]
			matchedActive[i].state = &state
			consumedRunIDs[state.RunID] = struct{}{}
			break
		}
	}
	for _, match := range matchedActive {
		run := v.runFromActiveMatch(repoRoot, match, eventsByRun, deadBatches)
		runs = append(runs, run)
		if run.RunID != "" {
			consumedRunIDs[run.RunID] = struct{}{}
		}
	}
	for _, runState := range runStates {
		if _, ok := consumedRunIDs[runState.RunID]; ok {
			continue
		}
		if runState.Status() == "queued" && !activeBatchStart.IsZero() && v.eventBelongsToBatch(runState.Started.Timestamp, activeBatchStart) {
			continue
		}
		runs = append(runs, v.runFromState(repoRoot, runState, nil, eventsByRun, deadBatches))
	}
	runs = append(runs, v.synthesizedDeadBatchRows(deadBatches, runStates)...)

	// Assign BatchKey to event-backed dead-batch rows so dedup can
	// collapse them with synthetic rows for the same issue/batch.
	// Without this, event rows keep empty BatchKey and survive dedup
	// alongside synthetic rows that carry the batch basename.
	runStatesByID := make(map[string]events.RunState, len(runStates))
	for _, rs := range runStates {
		if rs.RunID != "" {
			runStatesByID[rs.RunID] = rs
		}
	}
	deadBatchNames := make(map[string]struct{}, len(deadBatches))
	for _, db := range deadBatches {
		deadBatchNames[filepath.Base(db.RunDir)] = struct{}{}
	}
	for i := range runs {
		if runs[i].BatchKey != "" {
			continue
		}
		rs, ok := runStatesByID[runs[i].RunID]
		if !ok {
			continue
		}
		if bid := rs.BatchID(); bid != "" {
			if _, isDead := deadBatchNames[bid]; isDead {
				runs[i].BatchKey = bid
			}
		} else if batchKey, ok := dirIDs[runs[i].RunID]; ok {
			runs[i].BatchKey = batchKey
		}
	}

	// Strip synthetic rows that shadow event-backed rows with the same
	// (issue, batch) pair after BatchKey enrichment. A row is synthetic
	// when it has no events. Without this, the higher priority "aborted"
	// status on synthetic rows would suppress the real event row in
	// dedupRunGroup.
	eventsBacked := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		if run.IssueNumber > 0 && run.BatchKey != "" && len(run.Events) > 0 {
			eventsBacked[issueBatchKey(run.IssueNumber, run.BatchKey)] = struct{}{}
		}
	}
	filtered := make([]portalRun, 0, len(runs))
	for _, run := range runs {
		if run.IssueNumber > 0 && run.BatchKey != "" && len(run.Events) == 0 {
			if _, hasEvent := eventsBacked[issueBatchKey(run.IssueNumber, run.BatchKey)]; hasEvent {
				continue
			}
		}
		filtered = append(filtered, run)
	}
	runs = filtered

	runs = v.dedupRuns(runs)
	runs = v.demoteOrphanedActiveRunsFromDeadBatches(repoRoot, runs)
	for i := range runs {
		// Active runs are never marked archived, even if a directory
		// matching the run ID happens to exist under .sandman/archive.
		// Skipping the disk probe for active rows also keeps the hot
		// path allocation-free when the portal polls every few seconds.
		if runs[i].Kind != "completed" {
			runs[i].SourceExists = true
			continue
		}
		if runs[i].BatchKey == "" && len(dirIDs) > 0 {
			if batchKey, ok := dirIDs[runs[i].RunID]; ok {
				runs[i].BatchKey = batchKey
			}
		}
		locator := v.sourceDirID(idx, runs[i])
		runs[i].Archived = v.isRunArchived(idx, locator)
		runs[i].SourceExists = v.runDirExists(repoRoot, locator)
		// Unavailable is keyed by batchID (the index Entry.ID)
		// because the batchindex entry is the source of truth for the
		// unavailable flip (set by MarkUnavailable when the backing dir
		// is missing). MarkUnavailable skips archived entries, so
		// Archived and Unavailable stay mutually exclusive in normal
		// operation.
		if _, ok := unavailableRunIDs[locator.batchID]; ok {
			runs[i].Unavailable = true
		}

		// For completed archived rows, the saved log moved with the batch
		// directory. Recompute the log path and URL from the index entry's
		// recorded path, refresh the preview, and correct SourceExists.
		if runs[i].Kind == "completed" && runs[i].Archived && idx != nil {
			if entry := idx.Resolve(locator.batchID); entry != nil && entry.Path != "" {
				archivedLogPath := filepath.Join(entry.Path, "runs", runs[i].RunID, "run.log")
				runs[i].LogPath = archivedLogPath
				runs[i].LogURL = v.portalLogDownloadURLForPath(repoRoot, archivedLogPath)
				if info, err := os.Stat(filepath.Dir(archivedLogPath)); err == nil && info.IsDir() {
					runs[i].SourceExists = true
				} else {
					runs[i].SourceExists = false
				}
				if strings.TrimSpace(runs[i].Log) == "" {
					runs[i].Log = v.readPortalTextFile(archivedLogPath)
				}
			}
		}
	}
	// Staleness signal for active rows: the saved-run-log mtime (with a
	// StartedAt fallback). Computed here so every runFrom* constructor and
	// every caller of compute() (the /api/runs handler, the abort lookup)
	// sees the same value without each having to know how staleness is
	// derived. One os.Stat per active run per poll; the saved log is the
	// same file the portal already reads for terminal rows.
	for i := range runs {
		if runs[i].Kind != "active" {
			continue
		}
		at := v.lastOutputAt(runs[i])
		if at.IsZero() {
			continue
		}
		runs[i].LastOutputAt = &at
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

func seenIssuesForBatch(runStates []events.RunState, batch daemon.DeadBatch) map[int]struct{} {
	seen := make(map[int]struct{})
	batchName := filepath.Base(batch.RunDir)
	for _, runState := range runStates {
		issue := runState.IssueNumber()
		if issue <= 0 || runState.RunID == "" || runState.IsReview() {
			continue
		}
		// Use exact batch identity from the event payload's batch_id
		// field (written at event-emission time by the orchestrator).
		// When batch_id matches this batch, the issue started here and
		// must not be synthesized. Events without batch_id (legacy)
		// are not counted as seen — the event-log row survives via the
		// normal portal path and may coexist with a synthesized row.
		if runState.BatchID() == batchName {
			seen[issue] = struct{}{}
		}
	}
	return seen
}

func (v *portalRunsView) deadBatchesFromIndex(idx *batchindex.Index, activeInstances []portalActiveRun) []daemon.DeadBatch {
	if idx == nil || len(idx.Entries) == 0 {
		return nil
	}
	activeBatchIDs := make(map[string]struct{}, len(activeInstances))
	for _, active := range activeInstances {
		if active.Key != "" {
			activeBatchIDs[active.Key] = struct{}{}
		}
		if active.BatchID != "" {
			activeBatchIDs[active.BatchID] = struct{}{}
		}
		// A batches-index entry's Path is the same live batch
		// directory the active instance was loaded from. Including it
		// in the lookup set prevents a live batch whose index entry
		// has an empty "id" (the pre-#1657 shape) from being
		// misclassified as a dead batch. Without this,
		// synthesizedDeadBatchRows would emit kind=completed
		// status=aborted rows for the active batch's still-queued
		// issues (whose run.queued events carry an empty RunID and
		// are absent from ProjectRunStates), and dedupRunGroup would
		// prefer the synthesized aborted row over the live
		// kind=active row (issue #1659).
		if active.Dir != "" {
			activeBatchIDs[active.Dir] = struct{}{}
		}
	}
	deadBatches := make([]daemon.DeadBatch, 0, len(idx.Entries))
	for i := range idx.Entries {
		entry := idx.Entries[i]
		if entry.Path == "" {
			continue
		}
		if _, ok := activeBatchIDs[entry.ID]; ok {
			continue
		}
		// Match the live batch directory as well so an index entry
		// whose ID is empty but whose Path points at a live batch is
		// not treated as dead.
		if _, ok := activeBatchIDs[entry.Path]; ok {
			continue
		}
		manifest, err := daemon.ReadManifest(entry.Path)
		if err != nil {
			if !os.IsNotExist(err) {
				logPortalViewDegrade("dead-batch-manifest:"+entry.ID, "read manifest for dead batch %q: %v", entry.Path, err)
			}
			manifest = daemon.BatchManifest{}
		}
		if manifest.RunKind == "" {
			manifest.RunKind = string(entry.Kind)
		}
		deadBatches = append(deadBatches, daemon.DeadBatch{RunDir: entry.Path, Manifest: manifest})
	}
	return deadBatches
}

func issueBatchKey(issue int, batch string) string {
	return fmt.Sprintf("%d/%s", issue, batch)
}

func missingManifestIssues(manifest daemon.BatchManifest, seen map[int]struct{}) []int {
	if manifest.RunKind == "auto-select" || manifest.RunKind == "review" {
		return nil
	}
	missing := make([]int, 0, len(manifest.Issues))
	seenManifest := make(map[int]struct{}, len(manifest.Issues))
	for _, issue := range manifest.Issues {
		if _, ok := seenManifest[issue]; ok {
			continue
		}
		seenManifest[issue] = struct{}{}
		if _, ok := seen[issue]; ok {
			continue
		}
		missing = append(missing, issue)
	}
	return missing
}

func (v *portalRunsView) synthesizedDeadBatchRows(deadBatches []daemon.DeadBatch, runStates []events.RunState) []portalRun {
	if len(deadBatches) == 0 {
		return nil
	}
	sortedBatches := append([]daemon.DeadBatch(nil), deadBatches...)
	sort.SliceStable(sortedBatches, func(i, j int) bool {
		ii := sortedBatches[i].RunTimestamp()
		jj := sortedBatches[j].RunTimestamp()
		if !ii.Equal(jj) {
			return ii.Before(jj)
		}
		return sortedBatches[i].RunDir < sortedBatches[j].RunDir
	})
	rows := make([]portalRun, 0)
	for _, batch := range sortedBatches {
		missing := missingManifestIssues(batch.Manifest, seenIssuesForBatch(runStates, batch))
		if len(missing) == 0 {
			continue
		}
		batchKey := filepath.Base(batch.RunDir)
		startedAt := batch.RunTimestamp()
		if startedAt.IsZero() {
			startedAt = batch.Manifest.CreatedAt
		}
		for _, issueNumber := range missing {
			runID := perRowRunIDForManifest(batch.Manifest.RunTS, batch.Manifest.RunShortID, 0, issueNumber, nil)
			finishedAt := startedAt
			run := portalRun{
				Key:         runID,
				RunID:       runID,
				Kind:        "completed",
				Status:      "aborted",
				IssueLabel:  fmt.Sprintf("#%d", issueNumber),
				IssueNumber: issueNumber,
				StartedAt:   startedAt,
				FinishedAt:  &finishedAt,
				Duration:    "0s",
				BatchKey:    batchKey,
			}
			if len(batch.Manifest.Issues) > 1 {
				run.BatchIssues = append([]int(nil), batch.Manifest.Issues...)
			}
			rows = append(rows, run)
		}
	}
	return rows
}

// dedupRuns collapses duplicate rows per issue per batch. Two rows for
// the same issue only dedup when they share the same BatchKey, so a current
// active row (BatchKey=active.Key) is never hidden by a historical row
// (BatchKey="") from a different batch.
//
// A review row (PRNumber > 0) is never deduped against implementation
// rows for the same IssueNumber, because review runs and implementation
// runs are different work even when they target the same issue. A review
// row's IssueNumber is derived from payload["issue_number"] or from the
// orchestrator stamping `issue: N` on the finished event, so it can
// legitimately equal an implementation row's IssueNumber without
// describing the same run.
func (v *portalRunsView) dedupRuns(runs []portalRun) []portalRun {
	byIssue := make(map[int][]portalRun)
	issueOrder := make([]int, 0)
	for _, run := range runs {
		if _, ok := byIssue[run.IssueNumber]; !ok {
			issueOrder = append(issueOrder, run.IssueNumber)
		}
		byIssue[run.IssueNumber] = append(byIssue[run.IssueNumber], run)
	}
	result := make([]portalRun, 0, len(byIssue))
	for _, issueNumber := range issueOrder {
		issueRuns := byIssue[issueNumber]
		if len(issueRuns) == 1 {
			result = append(result, issueRuns[0])
			continue
		}
		// Split review rows from implementation rows so a review
		// run for the same issue never replaces a live impl row.
		var implRuns, reviewRuns []portalRun
		for _, run := range issueRuns {
			if run.PRNumber > 0 || run.Review {
				reviewRuns = append(reviewRuns, run)
			} else {
				implRuns = append(implRuns, run)
			}
		}
		for _, group := range [][]portalRun{implRuns, reviewRuns} {
			if len(group) == 0 {
				continue
			}
			byBatch := make(map[string][]portalRun)
			batchOrder := make([]string, 0)
			for _, run := range group {
				if _, ok := byBatch[run.BatchKey]; !ok {
					batchOrder = append(batchOrder, run.BatchKey)
				}
				byBatch[run.BatchKey] = append(byBatch[run.BatchKey], run)
			}
			for _, batchKey := range batchOrder {
				result = append(result, v.dedupRunGroup(byBatch[batchKey])...)
			}
		}
	}
	return result
}

func (v *portalRunsView) demoteOrphanedActiveRunsFromDeadBatches(repoRoot string, runs []portalRun) []portalRun {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	allDead, err := daemon.FindDeadRunBatches(layout.SandmanDir)
	if err != nil {
		logPortalViewDegrade("orphan-demotion", "FindDeadRunBatches: %v", err)
		return runs
	}
	if len(allDead) == 0 {
		return runs
	}
	for i := range runs {
		if runs[i].Kind != "active" || runs[i].SocketPath != "" || runs[i].BatchKey == "" {
			continue
		}
		if runs[i].Status != "running" && runs[i].Status != "reviewing" && runs[i].Status != "auto-selecting" {
			continue
		}
		var db *daemon.DeadBatch
		for j := range allDead {
			if filepath.Base(allDead[j].RunDir) == runs[i].BatchKey {
				db = &allDead[j]
				break
			}
		}
		if db == nil {
			continue
		}
		if runs[i].RunID == "" {
			continue
		}
		runSockPath := daemon.RunSocketPath(db.RunDir, runs[i].RunID)
		if v.isSocketAlive(runSockPath) {
			continue
		}
		runs[i].Kind = "completed"
		runs[i].Status = "aborted"
		ts := runs[i].StartedAt
		runs[i].FinishedAt = &ts
	}
	return runs
}

func reviewVerdictFromRunLog(logText string) (string, bool) {
	// Scan the log for a "## Decision" heading; the first non-empty
	// line inside that section must be the verdict marker. This
	// matches the prompt convention at internal/prompt/default_pr_review_prompt.md
	// step "Posting the Review" (issue #1729).
	//
	// Each line is prefixed with "[<runID>] HH:MM:SS " by the agent
	// output streaming (see CONTEXT.md Saved Run Log), so we strip
	// everything up to and including the timestamp before matching
	// the section heading or the marker. The marker match is anchored
	// to the entire line (after stripping the prefix and trimming
	// whitespace) — see reviewVerdictMarkerLineBare and
	// reviewVerdictMarkerLineWithDebris for the exact rule. The
	// with-debris regex requires a non-whitespace sentinel
	// immediately after the closing `**` so that mid-line prose
	// such as `**APPROVED** is unrelated prose` is still rejected
	// while the production shell-piped shape
	// `**APPROVED**" 2>&1 | tail -5` is accepted. Lowercase markers
	// and the space-inside-asterisks variant are still rejected
	// by both regexes; trailing periods and trailing quotes
	// individually are now accepted (issues #1767, #1792).
	lines := strings.Split(logText, "\n")
	inDecision := false
	for _, raw := range lines {
		payload := stripLogLabel(raw)
		line := strings.TrimSpace(payload)
		line = reviewLogTimestampPrefix.ReplaceAllString(line, "")
		if !inDecision && reviewSectionDecisionHeading.MatchString(line) {
			inDecision = true
			continue
		}
		if !inDecision {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			return "", false
		}
		if line == "" {
			continue
		}
		matches := reviewVerdictMarkerLineBare.FindStringSubmatch(line)
		if matches == nil {
			matches = reviewVerdictMarkerLineWithDebris.FindStringSubmatch(line)
		}
		if matches == nil {
			continue
		}
		switch matches[1] {
		case "APPROVED":
			return "Approved", true
		case "CHANGES_REQUESTED":
			return "Changes requested", true
		}
	}
	return "", false
}

// dedupRunGroup collapses duplicate rows for one issue within one batch.
// It strips queued and blocked rows when any non-waiting row exists
// (queued/blocked only describe the wait state of an AgentRun and are
// superseded by any later non-waiting row for the same AgentRun), and
// returns all remaining rows as-is — no priority-based collapsing. The
// active vs terminal reconciliation for the same RunID happens in
// compute() before this pass; unrelated terminal rows for the same issue
// (e.g. a recovered failure plus a fresh success) must surface as two
// rows, and an aborted run must not suppress a successful run for the
// same issue within the same batch.
func (v *portalRunsView) dedupRunGroup(runs []portalRun) []portalRun {
	if len(runs) <= 1 {
		return runs
	}
	terminal := make([]portalRun, 0, len(runs))
	waiting := make([]portalRun, 0, len(runs))
	for _, run := range runs {
		if run.Status == "queued" || run.Status == "blocked" {
			waiting = append(waiting, run)
		} else {
			terminal = append(terminal, run)
		}
	}
	if len(terminal) == 0 {
		if len(waiting) <= 1 {
			return waiting
		}
		bestIdx := 0
		for i := 1; i < len(waiting); i++ {
			if waiting[i].StartedAt.After(waiting[bestIdx].StartedAt) {
				bestIdx = i
			}
		}
		return []portalRun{waiting[bestIdx]}
	}
	return terminal
}

func (v *portalRunsView) discoverActiveRuns(repoRoot string, eventsByRun map[string][]portalEvent) ([]portalActiveRun, error) {
	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		return nil, err
	}

	active := make([]portalActiveRun, 0, len(instances))
	for _, instance := range instances {
		info, err := os.Stat(instance.SocketPath)
		if err != nil {
			if !os.IsNotExist(err) {
				logPortalViewDegrade("stat-socket:"+instance.SocketPath, "stat %q: %v", instance.SocketPath, err)
			}
			continue
		}
		runDir := filepath.Dir(instance.SocketPath)
		manifest, manifestErr := v.readManifestCached(runDir)
		prNumber := 0
		batchID := instance.Name
		runID := filepath.Base(runDir)
		reviewIssueNumber := 0
		if manifestErr == nil && manifest.BatchId != "" {
			batchID = manifest.BatchId
			if manifest.RunKind == "review" {
				if manifest.PR != nil {
					prNumber = *manifest.PR
				}
				runID, reviewIssueNumber = v.reviewRunIdentityForBatchDir(runDir)
				if runID == "" {
					runID = filepath.Base(runDir)
				}
			} else {
				if perRowID, ok := v.canonicalIssueRunIDForBatchDir(runDir); ok {
					runID = perRowID
				}
				prNumber = v.prNumberFromEvent(eventsByRun[runID])
			}
		}
		if reviewIssueNumber == 0 {
			// The review identity (resolved runID, batchID, or the
			// batches-index instance name) encodes the issue as a
			// `<token>-<ts>-<issue>-PR<n>` tail. When the per-run
			// folder is absent or not parseable, recover the issue
			// number from one of these identities so the review row
			// groups under the canonical issue row instead of escaping
			// as a standalone passthrough row.
			reviewIssueNumber = reviewIssueNumberFromIdentity(runID, batchID, instance.Name)
		}
		if reviewIssueNumber == 0 {
			reviewIssueNumber = v.reviewIssueNumberForBatch(eventsByRun, batchID, instance.Name, runID)
		}
		issueNumbers := []int(nil)
		issueNumber := 0
		startedAt := info.ModTime()
		// Issue identity is taken from the batch manifest only. The
		// directory name (e.g. run-NNN-<ts>) is not consulted, so a
		// mixed batch can no longer be mistaken for a private
		// single-issue run by reading the sibling issue's dir name.
		if manifestErr == nil {
			if !manifest.CreatedAt.IsZero() {
				startedAt = manifest.CreatedAt
			}
			if manifest.RunKind == "review" && reviewIssueNumber > 0 {
				issueNumbers = []int{reviewIssueNumber}
			} else {
				issueNumbers = append(issueNumbers, manifest.Issues...)
				if len(issueNumbers) == 0 && reviewIssueNumber > 0 {
					issueNumbers = []int{reviewIssueNumber}
				}
			}
		} else if reviewIssueNumber > 0 {
			// No readable manifest (e.g. a ghost review batch whose
			// index entry and socket survive but whose batch.json was
			// evicted or corrupt), yet the review identity encoded the
			// linked issue. Use it so the active row still groups under
			// the canonical issue row instead of escaping as a
			// passthrough row (residual #1615).
			issueNumbers = []int{reviewIssueNumber}
		}
		if len(issueNumbers) > 0 {
			issueNumber = issueNumbers[0]
		}
		lastOutputAt := startedAt
		if logInfo, err := os.Stat(filepath.Join(runDir, "runs", runID, "run.log")); err == nil && !logInfo.IsDir() {
			lastOutputAt = logInfo.ModTime()
		}
		entry := portalActiveRun{
			Key:          runID,
			Dir:          runDir,
			SocketPath:   instance.SocketPath,
			LastOutputAt: lastOutputAt,
			IssueNumber:  issueNumber,
			IssueNumbers: issueNumbers,
			PRNumber:     prNumber,
			BatchID:      batchID,
			RunID:        runID,
			RunTS:        manifest.RunTS,
			RunShortID:   manifest.RunShortID,
			StartedAt:    startedAt,
			ModTime:      info.ModTime(),
		}
		entry.Key = activeKeyForActive(entry)
		active = append(active, entry)
	}
	return active, nil
}

func (v *portalRunsView) reviewIssueNumberForBatch(eventsByRun map[string][]portalEvent, batchIDs ...string) int {
	var best *portalEvent
	bestRunID := ""
	for runID, events := range eventsByRun {
		for i := range events {
			event := events[i]
			if event.Type != "run.started" {
				continue
			}
			review, ok := event.Payload["review"].(bool)
			if !ok || !review {
				continue
			}
			payloadBatchID, _ := event.Payload["batch_id"].(string)
			if payloadBatchID == "" {
				continue
			}
			matched := false
			for _, batchID := range batchIDs {
				if batchID != "" && payloadBatchID == batchID {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			if best == nil || event.Timestamp.After(best.Timestamp) || (event.Timestamp.Equal(best.Timestamp) && runID < bestRunID) {
				copy := event
				best = &copy
				bestRunID = runID
			}
		}
	}
	if best == nil {
		return 0
	}
	return v.reviewIssueNumber(best.Payload)
}

func (v *portalRunsView) reviewRunIdentityForBatchDir(batchDir string) (string, int) {
	runsDir := filepath.Join(batchDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return "", 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if issueNumber := reviewIssueNumberFromRunID(entry.Name()); issueNumber > 0 {
			return entry.Name(), issueNumber
		}
	}
	return "", 0
}

// canonicalIssueRunIDForBatchDir mirrors the (now-removed) index-side
// canonicalIssueRunID helper. For non-review batches the per-row RunID lives
// in `runs/<rowID>/run.json` and is distinct from the on-disk dir name
// (which carries the "+N" suffix) — see ADR-0036 and issue #1715.
func (v *portalRunsView) canonicalIssueRunIDForBatchDir(batchDir string) (string, bool) {
	entries, err := os.ReadDir(filepath.Join(batchDir, "runs"))
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, err := daemon.ReadRunManifest(batchDir, entry.Name())
		if err != nil || manifest.RunID == "" {
			continue
		}
		return manifest.RunID, true
	}
	return "", false
}

func reviewIssueNumberFromRunID(runID string) int {
	parts := strings.Split(runID, "-")
	if len(parts) < 4 {
		return 0
	}
	if !strings.HasPrefix(parts[len(parts)-1], "PR") {
		return 0
	}
	issueNumber, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || issueNumber <= 0 {
		return 0
	}
	return issueNumber
}

// reviewIssueNumberFromIdentity returns the first parseable review issue number
// from the given candidate identity strings (resolved runID, batchID, instance
// name), in priority order. It reuses the strict `<token>-<ts>-<issue>-PR<n>`
// parser so it cannot misfire on non-review (issue-only) identities.
func reviewIssueNumberFromIdentity(candidates ...string) int {
	for _, candidate := range candidates {
		if issueNumber := reviewIssueNumberFromRunID(candidate); issueNumber > 0 {
			return issueNumber
		}
	}
	return 0
}

func (v *portalRunsView) runsFromActiveBatch(repoRoot string, active portalActiveRun, runStates []events.RunState, eventList []events.Event, eventsByRun map[string][]portalEvent, deadBatches []daemon.DeadBatch) ([]portalRun, map[string]struct{}) {
	batchStart := active.StartedAt
	if batchStart.IsZero() {
		batchStart = active.ModTime
	}
	runs := make([]portalRun, 0, len(active.IssueNumbers))
	usedRunIDs := make(map[string]struct{})
	for _, issueNumber := range active.IssueNumbers {
		blocked := v.latestBlockedEventForIssue(eventList, issueNumber, batchStart)
		queued := v.latestQueuedEventForIssue(eventList, issueNumber, batchStart)
		state := v.latestRunStateForIssue(runStates, issueNumber, batchStart)
		if state != nil && !state.IsActive() && (state.Status() == "queued" || (state.Status() == "blocked" && blocked == nil)) {
			state = nil
		}
		run := v.runFromActiveBatchIssue(repoRoot, active, issueNumber, state, blocked, queued, active.LiveOutput, eventsByRun, deadBatches)
		runs = append(runs, run)
		if state != nil && state.RunID != "" {
			if !(state.IsActive() && run.Kind == "completed") {
				usedRunIDs[state.RunID] = struct{}{}
			}
		}
	}
	return runs, usedRunIDs
}

func (v *portalRunsView) latestRunStateForIssue(runStates []events.RunState, issueNumber int, batchStart time.Time) *events.RunState {
	var latest *events.RunState
	for i := range runStates {
		state := runStates[i]
		if state.IssueNumber() != issueNumber {
			continue
		}
		if !v.stateStartsInBatch(state.Started.Timestamp, batchStart) {
			continue
		}
		if latest == nil || state.Started.Timestamp.After(latest.Started.Timestamp) {
			copy := state
			latest = &copy
		}
	}
	return latest
}

func (v *portalRunsView) latestBlockedEventForIssue(eventList []events.Event, issueNumber int, batchStart time.Time) *events.Event {
	var latest *events.Event
	for i := range eventList {
		event := eventList[i]
		if event.Type != "run.blocked" || event.Issue != issueNumber {
			continue
		}
		if !v.eventBelongsToBatch(event.Timestamp, batchStart) {
			continue
		}
		if latest == nil || event.Timestamp.After(latest.Timestamp) {
			copy := event
			latest = &copy
		}
	}
	return latest
}

func (v *portalRunsView) latestQueuedEventForIssue(eventList []events.Event, issueNumber int, batchStart time.Time) *events.Event {
	var latest *events.Event
	for i := range eventList {
		event := eventList[i]
		if event.Type != "run.queued" || event.Issue != issueNumber {
			continue
		}
		if !v.eventBelongsToBatch(event.Timestamp, batchStart) {
			continue
		}
		if latest == nil || event.Timestamp.After(latest.Timestamp) {
			copy := event
			latest = &copy
		}
	}
	return latest
}

func (v *portalRunsView) eventBelongsToBatch(timestamp, batchStart time.Time) bool {
	if batchStart.IsZero() {
		return true
	}
	return !timestamp.Before(batchStart.Add(-time.Second))
}

// stateStartsInBatch reports whether a run state started during the
// active batch. Unlike eventBelongsToBatch (which keeps a 1-second
// tolerance for clock skew on isolated events), this check is strict so that
// an older run whose Started.Timestamp sits inside the tolerance window does
// not steal the active batch's row.
func (v *portalRunsView) stateStartsInBatch(timestamp, batchStart time.Time) bool {
	if batchStart.IsZero() {
		return true
	}
	return !timestamp.Before(batchStart)
}

// perRowRunIDForActive derives the per-row RunID for an issue within an
// active batch, before run.started lands in the event log. It uses the
// batch manifest's (RunTS, RunShortID) pair to construct the canonical
// ADR-0030 per-row RunID (<shortid>-<ts>-<issueNum> for issue-driven
// runs, <shortid>-<ts>-<linkedIssue>-PR<pr> for review runs with a
// linked issue), falling back to the run.queued event's RunID when
// manifest fields are absent.
func perRowRunIDForActive(active portalActiveRun, issueNumber int, queued *events.Event) string {
	return perRowRunIDForManifest(active.RunTS, active.RunShortID, active.PRNumber, issueNumber, queued)
}

// perRowRunIDForManifest constructs the per-row RunID from the batch
// manifest's (RunTS, RunShortID) pair and the issue/pr identity. Falls
// back to the queued event's RunID when manifest fields are absent.
func perRowRunIDForManifest(runTS, runShortID string, prNumber, issueNumber int, queued *events.Event) string {
	if runTS != "" && runShortID != "" {
		if prNumber > 0 {
			subject := fmt.Sprintf("PR%d", prNumber)
			if issueNumber > 0 {
				subject = fmt.Sprintf("%d-PR%d", issueNumber, prNumber)
			}
			return runid.NewRunID(runid.KindReview, subject, runTS, runShortID)
		}
		return runid.NewRunID(runid.KindIssue, fmt.Sprintf("%d", issueNumber), runTS, runShortID)
	}
	if queued != nil && queued.RunID != "" {
		return queued.RunID
	}
	return ""
}

func (v *portalRunsView) runFromActiveBatchIssue(repoRoot string, active portalActiveRun, issueNumber int, state *events.RunState, blocked *events.Event, queued *events.Event, liveOutput string, eventsByRun map[string][]portalEvent, deadBatches []daemon.DeadBatch) portalRun {
	issueLabel := fmt.Sprintf("#%d", issueNumber)
	// active.Dir is the live batch directory on disk (with the "+N"
	// suffix for multi-issue batches); active.BatchID is the index
	// entry id (per-row RunID for the first issue, per ADR-0036) which
	// does NOT match the on-disk directory name. Resolve LogPath via
	// active.Dir so the staleness stat hits the real per-row log file
	// even in the state-less path (issue #1715).
	logPath, logURL := v.activeRunLogPathAndURL(repoRoot, active)
	derivedRunID := perRowRunIDForActive(active, issueNumber, queued)
	run := portalRun{
		Key:         derivedRunID,
		RunID:       derivedRunID,
		Kind:        "active",
		Status:      "queued",
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		StartedAt:   active.StartedAt,
		SocketPath:  active.SocketPath,
		LogPath:     logPath,
		LogURL:      logURL,
		Log:         "Queued. Waiting to start.",
		BatchKey:    batchKeyForActive(active),
	}
	// Only surface batch membership for mixed batches. A single-issue
	// batch is not interesting to surface and would add payload noise.
	if len(active.IssueNumbers) > 1 {
		run.BatchIssues = append([]int(nil), active.IssueNumbers...)
	}
	if state != nil {
		activeWithOutput := active
		activeWithOutput.LiveOutput = liveOutput
		run = v.runFromState(repoRoot, *state, &activeWithOutput, eventsByRun, deadBatches)
		run.BatchKey = batchKeyForActive(active)
		if len(active.IssueNumbers) > 1 {
			run.BatchIssues = append([]int(nil), active.IssueNumbers...)
		}
		if state.Finished == nil {
			if live := strings.TrimSpace(stripLogLabels(liveOutput)); live != "" {
				run.Log = v.filterPortalLogByRunID(liveOutput, state.RunID)
			} else {
				run.Log = v.readPortalTextFile(run.LogPath)
			}
			return run
		}
		switch state.Status() {
		case "blocked":
			run.Log = v.portalBlockedMessage(state.Finished.Payload)
		case "aborted":
		default:
			run.Log = v.resolveRunLog(func() string { return v.readPortalTextFile(run.LogPath) }, *state, &active)
			if strings.TrimSpace(run.Log) == "" {
				run.Log = "No log file yet."
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
		run.Log = v.portalBlockedMessage(blocked.Payload)
		run.IssueTitle = v.issueTitleFromPayload(blocked.Payload)
	}
	// Fallback precedence: the state branch returns early above, the
	// blocked branch may set a title from run.blocked's payload, and only
	// when both leave IssueTitle empty do we backfill from the most recent
	// run.queued event for this issue.
	if run.IssueTitle == "" && queued != nil {
		run.IssueTitle = v.issueTitleFromPayload(queued.Payload)
	}
	// The state-less path falls through to "queued" by default so a
	// pre-run.started implementation row reads as waiting. When the
	// underlying active instance is actually a live review, the row must
	// promote to "reviewing" instead, since the linked review is what is
	// doing the work for this issue (mirrors the contract pinned by
	// runFromActiveMatch's `if prNumber > 0` branch). Without this
	// promotion, a review that started before run.started lands would
	// surface its issue row stuck on "queued" forever.
	if run.Status == "queued" && blocked == nil && active.PRNumber > 0 {
		run.Status = "reviewing"
		run.Review = true
		run.PRNumber = active.PRNumber
	}
	v.markCompletedIfSocketDead(&run, run.SocketPath)
	return run
}

func (v *portalRunsView) matchActiveRuns(instances []portalActiveRun, activeStates []events.RunState) []portalRunMatch {
	used := make([]bool, len(activeStates))
	matches := make([]portalRunMatch, 0, len(instances))

	for _, instance := range instances {
		idx := v.matchRunState(instance, activeStates, used)
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

func (v *portalRunsView) matchRunState(instance portalActiveRun, states []events.RunState, used []bool) int {
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

	// Time-proximity fallback: refuse to bind a prompt-only /
	// auto-select instance (IssueNumber == 0) to a state tied to a
	// concrete issue, even on timestamp proximity. The strict loops
	// above already filter strictly on IssueNumber; the fallback
	// below was the path that swallowed orphan issue-run states
	// into the closest unrelated auto-select / prompt-only instance
	// when the issue-run's own batch dir was missing from the
	// batches index (issue #1464). Return -1 so the state falls
	// through to the event-backed branch in compute() instead.
	if instance.IssueNumber == 0 {
		return -1
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

func (v *portalRunsView) runFromActiveMatch(repoRoot string, match portalRunMatch, eventsByRun map[string][]portalEvent, deadBatches []daemon.DeadBatch) portalRun {
	runID := match.instance.Key
	if match.state != nil {
		run := v.runFromState(repoRoot, *match.state, &match.instance, eventsByRun, deadBatches)
		run.BatchKey = batchKeyForActive(match.instance)
		if match.state.Finished != nil {
			if strings.TrimSpace(run.Log) == "" {
				run.Log = v.readPortalTextFile(run.LogPath)
			}
		}
		return run
	}

	startedAt := match.instance.ModTime
	issueLabel := "prompt-only"
	issueNumber := match.instance.IssueNumber
	prNumber := match.instance.PRNumber
	if prNumber > 0 {
		// Live orphan review row (no resolved issue): prefer the
		// explicit "Review of #<prNumber>" label, matching the
		// convention used for terminal orphan reviews (ADR-0029
		// §Review-only orphan label, issue #1526 / #1667). Falls back
		// to the raw runID, then to "PR<n>", when neither applies.
		issueLabel = reviewOrphanIssueLabel(runID, prNumber)
		if issueLabel == "" {
			issueLabel = fmt.Sprintf("PR%d", prNumber)
		}
	} else if issueNumber > 0 {
		issueLabel = fmt.Sprintf("#%d", issueNumber)
	}
	status := "running"
	review := false
	if prNumber > 0 {
		status = "reviewing"
		review = true
	}
	locator := runLocator{batchID: match.instance.BatchID, runID: match.instance.RunID}
	logPath, logURL := v.activeRunLogPathAndURL(repoRoot, match.instance)
	if logPath == "" {
		logPath = v.portalLogPathForRun(repoRoot, locator)
		logURL = v.portalLogDownloadURLForRun(repoRoot, locator)
	}
	eventKey := match.instance.Key
	if match.instance.RunID != "" {
		eventKey = match.instance.RunID
	}
	startedPayload := v.startedPayload(eventsByRun[eventKey])
	reason := chipReasonForActiveInstance(match.instance)
	if payloadReason := v.reasonFromStartedPayload(startedPayload); payloadReason != "" {
		reason = payloadReason
	}
	if reason == "auto-select" {
		status = "auto-selecting"
	}
	run := portalRun{
		Key:         activeKeyForActive(match.instance),
		RunID:       runID,
		Kind:        "active",
		Status:      status,
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		Review:      review,
		PRNumber:    prNumber,
		Reason:      reason,
		StartedAt:   startedAt,
		Duration:    time.Since(startedAt).Round(time.Second).String(),
		SocketPath:  match.instance.SocketPath,
		LogPath:     logPath,
		LogURL:      logURL,
		Log:         stripLogLabels(match.instance.LiveOutput),
		Events:      eventsByRun[eventKey],
		BatchKey:    batchKeyForActive(match.instance),
	}
	// Populate the live attempt signals from the raw event list when
	// there is no matched RunState to query (the state-absent branch of
	// runFromActiveMatch). The state-present branch above delegates to
	// runFromState, which already populates both fields.
	run.Attempts, run.LastRetryReason = attemptsAndLastRetryReasonFromEvents(eventsByRun[eventKey])
	if startedPayload != nil {
		run.IssueTitle = v.issueTitleFromPayload(startedPayload)
		if candidates := v.candidatesFromPayload(startedPayload); len(candidates) > 0 {
			run.Candidates = candidates
		}
	}
	v.markCompletedIfSocketDead(&run, run.SocketPath)
	return run
}

func (v *portalRunsView) runFromState(repoRoot string, runState events.RunState, active *portalActiveRun, eventsByRun map[string][]portalEvent, deadBatches []daemon.DeadBatch) portalRun {
	runID := runState.RunID

	// Resolve batchID from the event payload's batch_id (with "+N" on-disk
	// suffix for multi-issue batches) first, and only fall back to
	// active.BatchID when the event payload has no batch_id. The active
	// instance's BatchID comes from manifest.BatchId, which equals the
	// per-row RunID for the first issue (ADR-0036) and therefore does not
	// match the on-disk directory name; using it for the log-path locator
	// would make the staleness stat fall back to startedAt and surface a
	// stale chip equal to the run duration (issue #1715).
	batchID := runState.BatchID()
	if batchID == "" {
		if active != nil && active.BatchID != "" {
			batchID = active.BatchID
		} else {
			batchID = batchIDFromRunID(runID)
		}
	}
	activeSocket := active != nil && strings.TrimSpace(active.SocketPath) != ""
	locator := runLocator{batchID: batchID, runID: runID}

	issueNumber := runState.IssueNumber()
	branch := runState.Branch()
	review, prNumber := v.reviewContext(runState)
	issueLabel := runState.IssueLabel()
	if runState.IsReview() && issueNumber == 0 {
		// Orphan review row (no resolved issue): prefer the explicit
		// "Review of #<prNumber>" label so the table cell matches the
		// convention used by the visibleRunForIssueGroup fallback
		// (ADR-0029 §Review-only orphan label, issue #1526). When
		// even the PR number is missing, fall back to the raw runID —
		// a degraded but non-leaking display for an exotic edge case.
		issueLabel = reviewOrphanIssueLabel(runID, prNumber)
	}
	if issueLabel == "" {
		issueLabel = runID
	}

	status := runState.Status()
	if runState.IsActive() {
		status = "running"
	}
	startedAt := runState.Started.Timestamp
	var finishedAt *time.Time
	if runState.Finished != nil {
		finishedAt = &runState.Finished.Timestamp
	}

	logPath := v.portalLogPathForRun(repoRoot, locator)
	logContent := v.resolveRunLog(func() string { return v.readPortalTextFile(logPath) }, runState, active)

	batchKey := ""
	if active != nil {
		batchKey = batchKeyForActive(*active)
	} else if bid := runState.BatchID(); bid != "" {
		// Fall back to the batch id projected from the event payload
		// when no active instance is matched. Without this, ghost
		// batches (entries evicted from the batches index while the
		// on-disk daemon is still live) get an empty BatchKey and
		// collide with prior terminal rows in dedupRuns, silently
		// dropping queued members (issue #1464).
		batchKey = bid
	}
	portalRun := portalRun{
		Key:             runID,
		RunID:           runID,
		Kind:            v.kindForRun(runState),
		Status:          v.statusOrDefault(status, runState.IsActive() || (status == "" && activeSocket), runState.IsReview(), runState.IsAutoSelect()),
		IssueLabel:      issueLabel,
		IssueNumber:     issueNumber,
		IssueTitle:      v.issueTitleFromPayload(runState.Started.Payload),
		Branch:          branch,
		StartedAt:       startedAt,
		FinishedAt:      finishedAt,
		Duration:        v.durationForRun(runState),
		LogPath:         logPath,
		LogURL:          v.portalLogDownloadURLForRun(repoRoot, locator),
		Log:             logContent,
		Events:          eventsByRun[runID],
		Review:          runState.IsReview(),
		Reason:          reasonForRun(runState),
		RetriesTotal:    runState.RetriesTotal(),
		RetriesDone:     runState.RetriesDone(),
		Attempts:        v.attemptsForRun(runState),
		LastRetryReason: runState.LastRetryReason(),
		BatchKey:        batchKey,
	}
	if review {
		portalRun.PRNumber = prNumber
		if issueNum := v.reviewIssueNumber(runState.Started.Payload); issueNum > 0 {
			portalRun.IssueNumber = issueNum
			portalRun.IssueLabel = fmt.Sprintf("#%d", issueNum)
		} else if issueNum := reviewIssueNumberFromIdentity(runID, batchID); issueNum > 0 {
			// The review command stamps only pr_number on the
			// run.started payload, never issue_number. The linked
			// issue is encoded solely in the review identity
			// (`<sid>-<ts>-<issue>-PR<n>`), on both the per-row RunID
			// and the batch_id. Recover it here so a historical review
			// row groups under the canonical implementation row instead
			// of escaping as a standalone passthrough row (residual
			// #1615 — the active-socket path recovered this, the event
			// path did not).
			portalRun.IssueNumber = issueNum
			portalRun.IssueLabel = fmt.Sprintf("#%d", issueNum)
		}
	}
	if candidates := v.candidatesFromPayload(runState.Started.Payload); len(candidates) > 0 {
		portalRun.Candidates = candidates
	}
	if status == "blocked" {
		portalRun.Log = v.portalBlockedMessage(runState.Finished.Payload)
	}
	if status == "aborted" {
		portalRun.Kind = "completed"
	}
	if active != nil {
		portalRun.SocketPath = active.SocketPath
		v.markCompletedIfSocketDead(&portalRun, active.SocketPath)
		if portalRun.Kind == "completed" && runState.Finished != nil && activeSocket && (runState.IsReview() || runState.IsAutoSelect()) {
			portalRun.Kind = "active"
		}
	} else if portalRun.Kind == "active" {
		batchDir, err := v.findBatchDirForRun(repoRoot, runState.RunID, deadBatches)
		if err != nil {
			logPortalViewDegrade("batch-dir-lookup:"+runState.RunID, "find batch dir for run %q: %v", runState.RunID, err)
		}
		if batchDir != "" {
			portalRun.BatchKey = filepath.Base(batchDir)
			sockPath := daemon.RunSocketPath(batchDir, runState.RunID)
			if _, err := os.Lstat(sockPath); err == nil {
				portalRun.SocketPath = sockPath
				// In the orphan path (active==nil), the batch was not
				// confirmed alive via discoverPortalInstances. A stale
				// FindDeadRunBatches scan may have included it. Re-confirm
				// the batch is dead before demoting.
				if !daemon.IsRunActive(batchDir) {
					v.markCompletedIfSocketDead(&portalRun, sockPath)
				}
			}
		}
	}
	return portalRun
}

// attemptsForRun returns the attempt count to expose on the portalRun
// row. Finished runs prefer Finished.Payload["retries_done"] (matching
// the existing RetriesDone semantics and the slice-1 acceptance
// contract); active runs fall back to events.RunState.LiveAttempt, which
// walks the raw `run.retry` events. The finished payload wins when both
// signals are present, so a future divergence where the event-level
// highest `attempt` exceeds the orchestrator's `retries_done` is
// resolved in favor of the orchestrator's count.
func (v *portalRunsView) attemptsForRun(runState events.RunState) int {
	if runState.Finished != nil {
		if done, ok := payloadInt(runState.Finished.Payload, "retries_done"); ok {
			return done
		}
	}
	return runState.LiveAttempt()
}

// attemptsAndLastRetryReasonFromEvents computes the live attempt signals
// from a raw event list. The state-absent branch of runFromActiveMatch
// has no RunState to query, so it walks the same retry events directly.
// The returned retry count is the maximum `attempt - 1` across the
// retry events, with each candidate clamped at 0 so a malformed
// payload cannot produce a negative count. This mirrors
// events.RunState.LiveAttempt's retry-count semantic so the two
// active-row code paths (state-present vs. state-absent) agree. "Most
// recent" follows the same rule as RunState.LastRetryReason: largest
// Timestamp, ties broken by last-encountered order. Returns (0, "")
// when no retries are present or when the most recent retry payload
// omits the `reason` key.
func attemptsAndLastRetryReasonFromEvents(events []portalEvent) (int, string) {
	bestRetries := 0
	latestRetryIdx := -1
	latestTs := time.Time{}
	for i, event := range events {
		if event.Type != "run.retry" {
			continue
		}
		if attempt, ok := payloadInt(event.Payload, "attempt"); ok {
			retries := attempt - 1
			if retries < 0 {
				retries = 0
			}
			if retries > bestRetries {
				bestRetries = retries
			}
		}
		if latestRetryIdx == -1 || !event.Timestamp.Before(latestTs) {
			latestRetryIdx = i
			latestTs = event.Timestamp
		}
	}
	if latestRetryIdx == -1 {
		return 0, ""
	}
	latest := events[latestRetryIdx]
	if latest.Payload == nil {
		return bestRetries, ""
	}
	reason, _ := latest.Payload["reason"].(string)
	return bestRetries, reason
}

func (v *portalRunsView) kindForRun(runState events.RunState) string {
	// An active state (run.started / run.continued, no terminal
	// event yet) is naturally "active". Wait-state runs (queued
	// and blocked) have no live daemon, no socket, no log to
	// stream, so classifying them as "active" would surface them
	// in the "Active Batches" filter and paint them with the
	// active-row chrome. They are demoted to "completed" here;
	// the wait-state chrome (status badge, "Blocked by #…" log
	// message, row-non-expandable, aria-expanded="false") is
	// already gated on status via isRowExpandable,
	// isWaitStateRun, and portalBlockedMessage (issue #1699).
	if runState.IsActive() {
		return "active"
	}
	return "completed"
}

// reasonForRun returns the run-kind label rendered by the slice-2 chip
// column; an empty string means "no chip". Auto-select wins over review
// when both predicates match (defensive; the event log keeps them
// disjoint in practice).
func reasonForRun(runState events.RunState) string {
	if runState.IsAutoSelect() {
		return "auto-select"
	}
	if runState.IsReview() {
		return "review"
	}
	return ""
}

// chipReasonForActiveInstance derives the reason chip for a row projected
// from an active socket that has not yet been matched to a RunState in
// the event log. The only signal available is the live instance metadata;
// a PR-bearing socket is a review run, anything else has no chip.
func chipReasonForActiveInstance(instance portalActiveRun) string {
	if instance.PRNumber > 0 {
		return "review"
	}
	return ""
}

func (v *portalRunsView) startedPayload(events []portalEvent) map[string]any {
	for _, e := range events {
		if e.Type == "run.started" {
			return e.Payload
		}
	}
	return nil
}

func (v *portalRunsView) reasonFromStartedPayload(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if kind, ok := payload["run_kind"].(string); ok && strings.EqualFold(strings.TrimSpace(kind), "auto-select") {
		return "auto-select"
	}
	if review, ok := payload["review"].(bool); ok && review {
		return "review"
	}
	return ""
}

func (v *portalRunsView) statusOrDefault(status string, active bool, isReview bool, isAutoSelect bool) string {
	status = strings.TrimSpace(status)
	if active && isReview {
		return "reviewing"
	}
	if active && isAutoSelect {
		return "auto-selecting"
	}
	if active {
		return "running"
	}
	if status == "" {
		return "completed"
	}
	return status
}

func (v *portalRunsView) durationForRun(runState events.RunState) string {
	if runState.IsActive() {
		return time.Since(runState.Started.Timestamp).Round(time.Second).String()
	}
	return runState.Duration().String()
}

func (v *portalRunsView) filterPortalLogByRunID(text string, runID string) string {
	prefix := "[" + runID + "] "
	lines := strings.Split(text, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			filtered = append(filtered, stripLogLabel(line))
		}
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

// resolveRunLog is the single source of truth for the portal's saved-vs-
// live log decision. The Log field on a portalRun is rendered by either
// the saved run file (.sandman/batches/<batch-id>/runs/<run-id>/run.log,
// read via readPortalTextFile) or by the live attach stream coming off
// the still-connectable batch.sock (read via readPortalSocketOutput).
//
// Policy (issue #1730):
//   - No active batch matched (active == nil) → saved log.
//   - Active row (runState is non-terminal, i.e. IsActive() true) →
//     live wins if non-empty, else saved. The socket is the source of
//     truth while the run is still in flight.
//   - Terminal row (runState.Finished != nil, i.e. !IsActive()) →
//     saved log wins, even when the batch daemon socket is still
//     connectable. The Saved Run Log is the authoritative record of
//     a finished AgentRun per CONTEXT.md; the socket may now be
//     broadcasting a different run's content (issue #1637) and is
//     also capped at portalReadLimit (64 KiB), so the trailing
//     verdict line of a long review would otherwise be silently
//     dropped (issue #1730). This applies uniformly to review,
//     auto-select, and issue flavours. The kind=active promotion of
//     terminal review rows at lines 1593-1595 is orthogonal — it
//     affects the table-cell chip, not the Log tab content.
//   - Degraded fallback: a terminal row whose saved log is empty
//     falls back to the live socket output so the Log tab still
//     shows something meaningful when the log file has not yet been
//     flushed.
//
// `loadSaved` lazily reads the per-run run.log; the helper only invokes
// it on the saved-wins path so the live-wins branch avoids a needless
// filesystem read on every poll. `runState` carries the event-fold
// information needed to know whether the row is terminal and whether
// it is a review/auto-select. `active` is nil for the historical /
// event-only path, non-nil when an active batch is matched.
func (v *portalRunsView) resolveRunLog(loadSaved func() string, runState events.RunState, active *portalActiveRun) string {
	if active == nil {
		return loadSaved()
	}
	if runState.IsActive() {
		if live := strings.TrimSpace(stripLogLabels(active.LiveOutput)); live != "" {
			return live
		}
		return loadSaved()
	}
	if saved := loadSaved(); saved != "" {
		return saved
	}
	return strings.TrimSpace(stripLogLabels(active.LiveOutput))
}

func (v *portalRunsView) portalBlockedMessage(payload map[string]any) string {
	blockers := v.portalBlockedByIssues(payload)
	if len(blockers) == 0 {
		return "Blocked. Waiting on unresolved blockers."
	}
	parts := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		parts = append(parts, fmt.Sprintf("#%d", blocker))
	}
	return fmt.Sprintf("Blocked by %s.", strings.Join(parts, ", "))
}

func (v *portalRunsView) portalBlockedByIssues(payload map[string]any) []int {
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
			if n, ok := payloadIntValue(value); ok {
				issues = append(issues, n)
			}
		}
		return issues
	default:
		return nil
	}
}

// reviewContext reports whether the run is a review run and, if so, the PR
// number it targeted. The flag is read from the run.started payload so the
// answer matches the value the orchestrator wrote when the run began.
func (v *portalRunsView) reviewContext(runState events.RunState) (bool, int) {
	if !runState.IsReview() {
		return false, 0
	}
	return true, v.reviewPRNumber(runState.Started.Payload)
}

// reviewOrphanIssueLabel returns the main label for an orphan review run
// that has no associated issue number (i.e. the row survives as a
// passthrough, not grouped under a canonical implementation row). It
// uses the "Review of PR <N>" form, matching the orphan-with-issue
// fallback in portal.html's visibleRunForIssueGroup (ADR-0029 §Review-
// only orphan label, issue #1526 / #1667). When even the PR number is
// missing, the raw runID is returned — a degraded display for the
// exotic case of a review run with neither an issue nor a PR.
func reviewOrphanIssueLabel(runID string, prNumber int) string {
	if prNumber > 0 {
		return fmt.Sprintf("Review of PR %d", prNumber)
	}
	return runID
}

// reviewPRNumber reads the pr_number field from a payload, tolerating the
// JSON-decoded float64 representation that the event log uses.
func (v *portalRunsView) reviewPRNumber(payload map[string]any) int {
	n, _ := payloadInt(payload, "pr_number")
	return n
}

// prNumberFromEvent extracts the PR number from the run.started event
// in the given events slice. Returns 0 if no run.started event is found
// or if the payload does not contain pr_number.
func (v *portalRunsView) prNumberFromEvent(events []portalEvent) int {
	for _, e := range events {
		if e.Type == "run.started" {
			return v.reviewPRNumber(e.Payload)
		}
	}
	return 0
}

// reviewIssueNumber reads the issue_number field from a review run's
// payload. Returns 0 when the field is absent (older event logs).
func (v *portalRunsView) reviewIssueNumber(payload map[string]any) int {
	n, _ := payloadInt(payload, "issue_number")
	return n
}

// candidatesFromPayload reads the candidates field from an auto-select
// run's payload. Returns nil when the field is absent.
func (v *portalRunsView) candidatesFromPayload(payload map[string]any) []int {
	if payload == nil {
		return nil
	}
	raw, ok := payload["candidates"]
	if !ok {
		return nil
	}
	switch values := raw.(type) {
	case []int:
		return append([]int(nil), values...)
	case []any:
		candidates := make([]int, 0, len(values))
		for _, value := range values {
			if n, ok := payloadIntValue(value); ok {
				candidates = append(candidates, n)
			}
		}
		return candidates
	}
	return nil
}

// issueTitleFromPayload reads the issue_title field from a payload.
func (v *portalRunsView) issueTitleFromPayload(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	raw, ok := payload["issue_title"]
	if !ok {
		return ""
	}
	title, _ := raw.(string)
	return title
}

func (v *portalRunsView) isSocketAlive(socketPath string) bool {
	if socketPath == "" {
		return false
	}
	conn, err := net.DialTimeout("unix", socketPath, portalSocketProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (v *portalRunsView) markCompletedIfSocketDead(run *portalRun, socketPath string) {
	if run.Kind != "active" || socketPath == "" {
		return
	}
	if !v.isSocketAlive(socketPath) {
		logPortalViewDegrade("dead-socket:"+socketPath, "active run %q fell back to completed because run.sock %q is no longer live", run.Key, socketPath)
		run.Kind = "completed"
	}
}

func (v *portalRunsView) loadBatchesIndex(repoRoot string) *batchindex.Index {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	idx, err := batchindex.Load(layout.BatchesIndexPath)
	if err != nil {
		logPortalViewDegrade("batches-index-load", "load batches index: %v", err)
		return nil
	}
	return idx
}

// isRunArchived reports whether the batch's directory currently lives under
// .sandman/archive instead of .sandman/batches/<batch-id>/. A non-empty batchID that does
// not resolve to an index entry returns false; otherwise, archived-ness
// is reported from the entry's Status field, so transient or half-moved
// state never lights up the flag.
func (v *portalRunsView) isRunArchived(idx *batchindex.Index, locator runLocator) bool {
	if locator.batchID == "" || idx == nil {
		return false
	}
	entry := idx.Resolve(locator.batchID)
	if entry == nil {
		return false
	}
	return entry.Status == batchindex.StatusArchived
}

func (v *portalRunsView) sourceDirID(idx *batchindex.Index, run portalRun) runLocator {
	batchID := run.BatchKey
	runID := run.RunID
	if batchID == "" && runID != "" {
		batchID = runID
	}
	if batchID == "" && runID == "" {
		return runLocator{}
	}
	if idx != nil {
		if entry := idx.Resolve(batchID); entry != nil && entry.Path != "" {
			batchID = filepath.Base(entry.Path)
		}
	}
	return runLocator{batchID: batchID, runID: runID}
}

// unavailableRunIDsByBatchIndex returns the set of source directory IDs
// whose batch index entry is currently StatusUnavailable. The portal
// needs this to stamp Unavailable on completed historical rows that lost
// their backing directory; without this stamp the row would still render
// with a normal badge and an Archive button, inviting operator action on
// a run that no longer exists on disk.
//
// The lookup is keyed by sourceDirID (BatchKey when present, RunID
// otherwise), which matches batchindex.Entry.ID for completed historical
// rows.
func (v *portalRunsView) unavailableRunIDsByBatchIndex(idx *batchindex.Index) map[string]struct{} {
	out := map[string]struct{}{}
	if idx == nil {
		return out
	}
	for i := range idx.Entries {
		if idx.Entries[i].Status == batchindex.StatusUnavailable {
			out[idx.Entries[i].ID] = struct{}{}
		}
	}
	return out
}

func (v *portalRunsView) deadBatchDirIDsByRunID(idx *batchindex.Index) ([]daemon.DeadBatch, map[string]string, error) {
	if idx == nil || len(idx.Entries) == 0 {
		return nil, nil, nil
	}
	deadBatches := make([]daemon.DeadBatch, 0, len(idx.Entries))
	// dirIDs maps row RunID → batch dir basename, so the caller can
	// populate BatchKey for completed historical rows. We scan each
	// batch's runs/ subdirectory; unavailable batches whose directory
	// has been deleted will fail the ReadDir gracefully and contribute
	// no entries.
	dirIDs := make(map[string]string, len(idx.Entries))
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		if entry.Path == "" {
			continue
		}
		deadBatches = append(deadBatches, daemon.DeadBatch{RunDir: entry.Path})
		runsDir := filepath.Join(entry.Path, "runs")
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				dirIDs[e.Name()] = filepath.Base(entry.Path)
			}
		}
	}
	return deadBatches, dirIDs, nil
}

func (v *portalRunsView) findBatchDirForRun(repoRoot, runID string, deadBatches []daemon.DeadBatch) (string, error) {
	if len(deadBatches) == 0 {
		layout := paths.NewLayout(&config.Config{}, repoRoot)
		var err error
		deadBatches, err = daemon.FindDeadRunBatches(layout.SandmanDir)
		if err != nil {
			return "", err
		}
	}
	for _, batch := range deadBatches {
		runManifestPath := filepath.Join(batch.RunDir, "runs", runID, "run.json")
		if _, err := os.Stat(runManifestPath); err == nil {
			return batch.RunDir, nil
		}
	}
	return "", nil
}

// portalBatchEntryNotFoundError signals that no batch index entry
// resolves the supplied run id via either the fast path (idx.Resolve)
// or the on-disk fallback (per-row manifest batchId). Callers map it
// to HTTP 404 batch not found via errors.As.
type portalBatchEntryNotFoundError struct {
	runID string
}

func (e *portalBatchEntryNotFoundError) Error() string {
	return fmt.Sprintf("no batch entry resolves run %q", e.runID)
}

// resolveBatchEntryForRunID returns the batch index entry that the
// given per-row run id identifies. The fast path is idx.Resolve, which
// matches the canonical batch entry id directly. The fallback path
// reads each entry's runs/<runID>/run.json, parses the per-row
// RunManifest's BatchID field, and re-resolves that id in the index.
// This generalises the per-row pattern implemented for reviews in
// internal/review/daemon.go readReviewRowID across every batch kind.
//
// On neither-path-resolves, the helper returns a typed
// *portalBatchEntryNotFoundError so callers can errors.As-match it
// and map to http.StatusNotFound. The returned entry has Path
// populated on either success path so downstream log path resolution
// and archive moves work without a second index lookup.
func (v *portalRunsView) resolveBatchEntryForRunID(idx *batchindex.Index, runID string) (*batchindex.Entry, error) {
	if idx == nil || runID == "" {
		return nil, &portalBatchEntryNotFoundError{runID: runID}
	}
	if entry := idx.Resolve(runID); entry != nil {
		return entry, nil
	}
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		if entry.Path == "" {
			continue
		}
		manifestPath := filepath.Join(entry.Path, "runs", runID, "run.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var manifest batchindex.RunManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			continue
		}
		if manifest.BatchID == "" {
			continue
		}
		if resolved := idx.Resolve(manifest.BatchID); resolved != nil {
			return resolved, nil
		}
	}
	return nil, &portalBatchEntryNotFoundError{runID: runID}
}

func (v *portalRunsView) runDirExists(repoRoot string, locator runLocator) bool {
	if locator.runID == "" {
		return false
	}
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	info, err := os.Stat(layout.RunFolder(locator.batchID, locator.runID))
	if err == nil && info.IsDir() {
		return true
	}
	return false
}

func (v *portalRunsView) portalLogPathForRun(repoRoot string, locator runLocator) string {
	locator.runID = strings.TrimSpace(locator.runID)
	if locator.runID == "" {
		return ""
	}
	layout := paths.NewLayout(nil, repoRoot)
	return filepath.Join(layout.RunFolder(locator.batchID, locator.runID), "run.log")
}

func (v *portalRunsView) portalLogDownloadURLForRun(repoRoot string, locator runLocator) string {
	logPath := v.portalLogPathForRun(repoRoot, locator)
	if logPath == "" {
		return ""
	}
	return v.portalLogDownloadURLForPath(repoRoot, logPath)
}

// activeRunLogPathAndURL resolves the per-row log path directly from
// active.Dir (the live batch directory on disk, with "+N" suffix for
// multi-issue batches) instead of routing through Layout.RunFolder.
// The state-less active-row path (runFromActiveBatchIssue initial
// values, runFromActiveMatch prompt-only branch) cannot rely on
// active.BatchID for the locator because that id matches the per-row
// RunID for the first issue per ADR-0036 and therefore does not match
// the on-disk directory name. Falling back to Layout.RunFolder with
// active.BatchID would make the staleness stat miss the per-row log
// file and surface a stale chip equal to the run duration (issue #1715).
func (v *portalRunsView) activeRunLogPathAndURL(repoRoot string, active portalActiveRun) (string, string) {
	if active.Dir == "" || active.RunID == "" {
		return "", ""
	}
	logPath := filepath.Join(active.Dir, "runs", active.RunID, "run.log")
	logURL := v.portalLogDownloadURLForPath(repoRoot, logPath)
	return logPath, logURL
}

// portalLogDownloadURLForPath turns any sandman-relative log file path into
// the portal's raw download URL. It is the archive-aware complement to
// portalLogDownloadURLForRun: archived batches resolve through the index
// entry's Path rather than the canonical batches directory.
func (v *portalRunsView) portalLogDownloadURLForPath(repoRoot, logPath string) string {
	if logPath == "" {
		return ""
	}
	relPath, err := filepath.Rel(repoRoot, logPath)
	if err != nil {
		logPortalViewDegrade("log-rel:"+logPath, "relpath for log %q under repo %q: %v", logPath, repoRoot, err)
		return ""
	}
	return "/api/logs?path=" + url.QueryEscape(relPath)
}

// readPortalTextFile returns the contents of a saved portal log file.
// Saved log files are persisted in the same `[<label>] HH:MM:SS ` prefixed
// format as the live stream. For portal display, labels are stripped so the
// UI shows "HH:MM:SS msg" instead of "[<label>] HH:MM:SS msg". Pre-change
// log files (saved before slice 1) may be un-prefixed; the reader tolerates
// both. The raw file is unchanged; log download (/api/logs) serves it raw.
func (v *portalRunsView) readPortalTextFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logPortalViewDegrade("read-log:"+path, "read saved log %q: %v", path, err)
		}
		return ""
	}
	if len(data) > portalReadLimit {
		tail := data[len(data)-portalReadLimit:]
		return stripLogLabels(v.cleanPortalText("[truncated]\n" + string(tail)))
	}
	return stripLogLabels(v.cleanPortalText(string(data)))
}

func (v *portalRunsView) readPortalSocketOutput(sockPath string) string {
	conn, err := net.DialTimeout("unix", sockPath, portalReadTimeout)
	if err != nil {
		logPortalViewDegrade("dial-socket:"+sockPath, "dial live socket %q: %v", sockPath, err)
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
	return v.cleanPortalText(buf.String())
}

// lastOutputAt returns the staleness timestamp for an active run: the
// mtime of the saved run log at run.LogPath (the file AgentRun.Execute
// writes via O_APPEND, so its mtime tracks the last output write), with
// a StartedAt fallback for runs whose log file has not been created yet.
// A directory at the path is ignored (treated as no log) so a malformed
// path never masquerades as fresh output. Returns the zero time when
// neither source is available, which the caller skips before setting
// LastOutputAt.
func (v *portalRunsView) lastOutputAt(run portalRun) time.Time {
	return portalLastOutputAt(run.LogPath, run.StartedAt)
}

func portalLastOutputAt(logPath string, startedAt time.Time) time.Time {
	if logPath != "" {
		if info, err := os.Stat(logPath); err == nil && !info.IsDir() {
			return info.ModTime()
		}
	}
	if !startedAt.IsZero() {
		return startedAt
	}
	return time.Time{}
}

func payloadIntValue(value any) (int, bool) {
	return payloadInt(map[string]any{"value": value}, "value")
}

func (v *portalRunsView) cleanPortalText(text string) string {
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

func (v *portalRunsView) groupEventsByRun(eventsList []events.Event) map[string][]portalEvent {
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

func stripLogLabel(line string) string {
	if !strings.HasPrefix(line, "[") {
		return line
	}
	end := strings.Index(line, "]")
	if end < 0 {
		return line
	}
	rest := line[end+1:]
	return strings.TrimLeft(rest, " ")
}

func stripLogLabels(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = stripLogLabel(line)
	}
	return strings.Join(lines, "\n")
}
