package cmd

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
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
	// It is populated only on issue-owner rows and omitted from JSON when zero.
	ReviewCount int `json:"reviewCount,omitempty"`
	// ReviewVerdict carries latest terminal child-review status for canonical
	// issue rows. It is omitted when no terminal child verdict exists yet.
	ReviewVerdict string `json:"reviewVerdict,omitempty"`
	// GroupedReview marks review rows that are owned by an issue-parent row.
	// Grouped review rows suppress legacy row chrome because canonical parent
	// already shows review summary.
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
	// Archived is true when a completed run's directory has been
	// relocated from .sandman/runs/<run-id> to .sandman/archive/<run-id>
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
	IssueNumber  int
	IssueNumbers []int
	PRNumber     int
	BatchID      string
	RunID        string
	StartedAt    time.Time
	ModTime      time.Time
}

type portalRunMatch struct {
	instance portalActiveRun
	state    *events.RunState
}

type portalRunsView struct{}

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

const portalViewDegradeLogInterval = 30 * time.Second

var (
	portalViewDegradeLogMu   sync.Mutex
	portalViewDegradeLogSeen = make(map[string]time.Time)
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

	idx := v.loadBatchesIndex(repoRoot)

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
	runs = append(runs, v.synthesizedDeadBatchRows(deadBatches, runStates, dirIDs)...)

	runs = v.dedupRuns(runs)
	runs = v.demoteOrphanedActiveRunsFromDeadBatches(repoRoot, runs)
	runs = v.aggregateReviewChildren(runs)
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

func seenIssuesForBatch(runStates []events.RunState, batchStart, nextBatchStart time.Time) map[int]struct{} {
	seen := make(map[int]struct{})
	for _, runState := range runStates {
		issue := runState.IssueNumber()
		if issue <= 0 || runState.RunID == "" {
			continue
		}
		if !batchStart.IsZero() && !(&portalRunsView{}).eventBelongsToBatch(runState.Started.Timestamp, batchStart) {
			continue
		}
		if !nextBatchStart.IsZero() && !runState.Started.Timestamp.Before(nextBatchStart) {
			continue
		}
		seen[issue] = struct{}{}
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

func (v *portalRunsView) synthesizedDeadBatchRows(deadBatches []daemon.DeadBatch, runStates []events.RunState, dirIDs map[string]string) []portalRun {
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
	for i, batch := range sortedBatches {
		nextBatchStart := time.Time{}
		if i+1 < len(sortedBatches) {
			nextBatchStart = sortedBatches[i+1].RunTimestamp()
			if nextBatchStart.IsZero() {
				nextBatchStart = sortedBatches[i+1].Manifest.CreatedAt
			}
		}
		missing := missingManifestIssues(batch.Manifest, seenIssuesForBatch(runStates, batch.RunTimestamp(), nextBatchStart))
		if len(missing) == 0 {
			continue
		}
		batchKey := filepath.Base(batch.RunDir)
		startedAt := batch.RunTimestamp()
		if startedAt.IsZero() {
			startedAt = batch.Manifest.CreatedAt
		}
		for _, issueNumber := range missing {
			runID := fmt.Sprintf("%s-issue-%d", batchKey, issueNumber)
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

func (v *portalRunsView) aggregateReviewChildren(runs []portalRun) []portalRun {
	if len(runs) == 0 {
		return runs
	}
	type reviewSummary struct {
		count      int
		live       bool
		verdict    string
		finishedAt time.Time
		startedAt  time.Time
	}
	parents := make(map[int]int)
	summaries := make(map[int]*reviewSummary)
	for i := range runs {
		run := runs[i]
		if run.IssueNumber <= 0 {
			continue
		}
		if run.Review {
			summary := summaries[run.IssueNumber]
			if summary == nil {
				summary = &reviewSummary{}
				summaries[run.IssueNumber] = summary
			}
			summary.count++
			if run.Status == "reviewing" {
				summary.live = true
			}
			if verdict := reviewVerdictForStatus(run.Status); verdict != "" {
				finishedAt := run.StartedAt
				if run.FinishedAt != nil {
					finishedAt = *run.FinishedAt
				}
				if summary.verdict == "" || finishedAt.After(summary.finishedAt) || (finishedAt.Equal(summary.finishedAt) && run.StartedAt.After(summary.startedAt)) {
					summary.verdict = verdict
					summary.finishedAt = finishedAt
					summary.startedAt = run.StartedAt
				}
			}
			continue
		}
		if idx, ok := parents[run.IssueNumber]; !ok || run.StartedAt.Before(runs[idx].StartedAt) {
			parents[run.IssueNumber] = i
		}
	}
	for issueNumber, summary := range summaries {
		idx, ok := parents[issueNumber]
		if !ok || summary.count == 0 {
			continue
		}
		runs[idx].ReviewCount = summary.count
		runs[idx].ReviewVerdict = summary.verdict
		if summary.live {
			currentStatus := runs[idx].Status
			if currentStatus != "success" && currentStatus != "failure" && currentStatus != "aborted" {
				runs[idx].Status = "reviewing"
			}
		}
	}
	for i := range runs {
		if runs[i].Review {
			runs[i].GroupedReview = true
		}
	}
	return runs
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

func reviewVerdictForStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "success":
		return "Approved"
	case "failure":
		return "Changes requested"
	default:
		return ""
	}
}

// dedupRunGroup collapses duplicate rows for one issue within one batch.
// It first strips queued and blocked rows when any other row exists, then
// applies runPriority (aborted > active > blocked > queued > other) and
// breaks ties by latest StartedAt. The active vs terminal reconciliation
// for the same RunID happens in compute() before this pass; this helper
// stays untouched so unrelated terminal rows for the same issue (e.g.,
// a recovered failure plus a fresh success) continue to surface as two
// rows.
func (v *portalRunsView) dedupRunGroup(runs []portalRun) []portalRun {
	if len(runs) <= 1 {
		return runs
	}
	// A queued or blocked row only describes the wait state of an AgentRun
	// and is superseded by any later non-waiting row for the same AgentRun.
	// When the group mixes waiting rows with terminal/active rows (e.g. a
	// queued or blocked event followed by run.started + run.finished events
	// that the same AgentRun emits with a different RunID once it leaves the
	// wait state), strip the waiting rows so the terminal status wins
	// regardless of the other priorities.
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
	runs = terminal
	bestIdx := 0
	bestPriority := v.runPriority(runs[0])
	for i := 1; i < len(runs); i++ {
		priority := v.runPriority(runs[i])
		if priority > bestPriority {
			bestIdx = i
			bestPriority = priority
			continue
		}
		if priority == bestPriority && runs[i].StartedAt.After(runs[bestIdx].StartedAt) {
			bestIdx = i
		}
	}
	if bestPriority == 0 {
		return runs
	}
	return []portalRun{runs[bestIdx]}
}

// runPriority encodes the reachable per-issue dedup priority order:
// aborted > active > other.
//
// queued/blocked rows never reach this function: dedupRunGroup strips them
// into the waiting slice first and either returns the latest waiting row
// directly (when there are no non-waiting rows) or drops them entirely when
// a later active/terminal row exists.
func (v *portalRunsView) runPriority(run portalRun) int {
	if run.Status == "aborted" {
		return 4
	}
	if run.Kind == "active" {
		return 3
	}
	return 0
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
		manifest, manifestErr := daemon.ReadManifest(runDir)
		prNumber := 0
		batchID := instance.Name
		runID := filepath.Base(runDir)
		if manifestErr == nil && manifest.BatchId != "" {
			batchID = manifest.BatchId
			prNumber = v.prNumberFromEvent(eventsByRun[runID])
		}
		issueNumbers := []int(nil)
		issueNumber := 0
		startedAt := info.ModTime()
		// Issue identity is taken from the batch manifest only. The
		// directory name (e.g. run-NNN-<ts>) is not consulted, so a
		// mixed batch can no longer be mistaken for a private
		// single-issue run by reading the sibling issue's dir name.
		if manifestErr == nil {
			issueNumbers = append(issueNumbers, manifest.Issues...)
			if !manifest.CreatedAt.IsZero() {
				startedAt = manifest.CreatedAt
			}
		}
		if len(issueNumbers) > 0 {
			issueNumber = issueNumbers[0]
		}
		active = append(active, portalActiveRun{
			Key:          instance.Name,
			Dir:          runDir,
			SocketPath:   instance.SocketPath,
			IssueNumber:  issueNumber,
			IssueNumbers: issueNumbers,
			PRNumber:     prNumber,
			BatchID:      batchID,
			RunID:        runID,
			StartedAt:    startedAt,
			ModTime:      info.ModTime(),
		})
	}
	return active, nil
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
		runs = append(runs, v.runFromActiveBatchIssue(repoRoot, active, issueNumber, state, blocked, queued, active.LiveOutput, eventsByRun, deadBatches))
		if state != nil && state.RunID != "" {
			usedRunIDs[state.RunID] = struct{}{}
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

func (v *portalRunsView) runFromActiveBatchIssue(repoRoot string, active portalActiveRun, issueNumber int, state *events.RunState, blocked *events.Event, queued *events.Event, liveOutput string, eventsByRun map[string][]portalEvent, deadBatches []daemon.DeadBatch) portalRun {
	issueLabel := fmt.Sprintf("#%d", issueNumber)
	run := portalRun{
		Key:         fmt.Sprintf("%s-issue-%d", active.Key, issueNumber),
		Kind:        "active",
		Status:      "queued",
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		StartedAt:   active.StartedAt,
		SocketPath:  active.SocketPath,
		LogPath:     v.portalLogPathForRun(repoRoot, runLocator{batchID: active.BatchID, runID: active.RunID}),
		LogURL:      v.portalLogDownloadURLForRun(repoRoot, runLocator{batchID: active.BatchID, runID: active.RunID}),
		Log:         "Queued. Waiting to start.",
		BatchKey:    active.Key,
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
		run.BatchKey = active.Key
		if len(active.IssueNumbers) > 1 {
			run.BatchIssues = append([]int(nil), active.IssueNumbers...)
		}
		if state.Finished == nil {
			run.Log = v.filterPortalLogByRunID(liveOutput, state.RunID)
			if strings.TrimSpace(run.Log) == "" {
				run.Log = "No live output captured yet."
			}
		} else {
			switch state.Status() {
			case "blocked":
				run.Log = v.portalBlockedMessage(state.Finished.Payload)
			case "aborted":
				run.Log = "Aborted by operator."
			default:
				run.Log = v.readPortalTextFile(run.LogPath)
				if strings.TrimSpace(run.Log) == "" {
					run.Log = "No log file yet."
				}
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
		run.BatchKey = match.instance.Key
		if match.state.Finished != nil {
			if strings.TrimSpace(run.Log) == "" {
				if saved := v.readPortalTextFile(run.LogPath); strings.TrimSpace(saved) != "" {
					run.Log = saved
				} else {
					run.Log = "No log file yet."
				}
			}
		}
		return run
	}

	startedAt := match.instance.ModTime
	issueLabel := "prompt-only"
	issueNumber := match.instance.IssueNumber
	prNumber := match.instance.PRNumber
	if prNumber > 0 {
		issueLabel = runID
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
	logPath := v.portalLogPathForRun(repoRoot, locator)
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
		Key:         runID,
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
		LogURL:      v.portalLogDownloadURLForRun(repoRoot, locator),
		Log:         stripLogLabels(match.instance.LiveOutput),
		Events:      eventsByRun[eventKey],
		BatchKey:    match.instance.Key,
	}
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
	if runID == "" && active != nil {
		if active.IssueNumber > 0 {
			runID = fmt.Sprintf("%s-issue-%d", active.Key, active.IssueNumber)
		} else {
			runID = active.Key
		}
	}

	batchID := batchIDFromRunID(runID)
	if active != nil && active.BatchID != "" {
		batchID = active.BatchID
	}
	locator := runLocator{batchID: batchID, runID: runID}

	issueNumber := runState.IssueNumber()
	branch := runState.Branch()
	issueLabel := runState.IssueLabel()
	if runState.IsReview() && issueNumber == 0 && runID != "" {
		issueLabel = runID
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
	review, prNumber := v.reviewContext(runState)

	logPath := v.portalLogPathForRun(repoRoot, locator)
	logContent := v.readPortalTextFile(logPath)
	if active != nil {
		logContent = stripLogLabels(active.LiveOutput)
	}

	batchKey := ""
	if active != nil {
		batchKey = active.Key
	}
	portalRun := portalRun{
		Key:          runID,
		RunID:        runID,
		Kind:         v.kindForRun(runState),
		Status:       v.statusOrDefault(status, runState.IsActive(), runState.IsReview(), runState.IsAutoSelect()),
		IssueLabel:   issueLabel,
		IssueNumber:  issueNumber,
		IssueTitle:   v.issueTitleFromPayload(runState.Started.Payload),
		Branch:       branch,
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
		Duration:     v.durationForRun(runState),
		LogPath:      logPath,
		LogURL:       v.portalLogDownloadURLForRun(repoRoot, locator),
		Log:          logContent,
		Events:       eventsByRun[runID],
		Review:       runState.IsReview(),
		Reason:       reasonForRun(runState),
		RetriesTotal: runState.RetriesTotal(),
		RetriesDone:  runState.RetriesDone(),
		BatchKey:     batchKey,
	}
	if review {
		portalRun.PRNumber = prNumber
		if issueNum := v.reviewIssueNumber(runState.Started.Payload); issueNum > 0 {
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
				v.markCompletedIfSocketDead(&portalRun, sockPath)
			}
		}
	}
	return portalRun
}

func (v *portalRunsView) kindForRun(runState events.RunState) string {
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
// .sandman/archive instead of .sandman/runs. A non-empty batchID that does
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
	if run.LogPath != "" {
		if info, err := os.Stat(run.LogPath); err == nil && !info.IsDir() {
			return info.ModTime()
		}
	}
	if !run.StartedAt.IsZero() {
		return run.StartedAt
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
