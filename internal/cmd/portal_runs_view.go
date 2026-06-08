package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
)

type portalEvent struct {
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Payload   map[string]any `json:"payload,omitempty"`
}

type portalRun struct {
	Key         string        `json:"key"`
	RunID       string        `json:"runId"`
	Kind        string        `json:"kind"`
	Status      string        `json:"status"`
	IssueLabel  string        `json:"issueLabel"`
	IssueNumber int           `json:"issueNumber,omitempty"`
	Branch      string        `json:"branch,omitempty"`
	StartedAt   time.Time     `json:"startedAt"`
	FinishedAt  *time.Time    `json:"finishedAt,omitempty"`
	Duration    string        `json:"duration,omitempty"`
	SocketPath  string        `json:"socketPath,omitempty"`
	LogPath     string        `json:"logPath,omitempty"`
	LogURL      string        `json:"logUrl,omitempty"`
	Log         string        `json:"log,omitempty"`
	Events      []portalEvent `json:"events,omitempty"`
	// BatchKey ties a row to the batch (active runDir) that produced it.
	// Active-batch derived rows carry the active runDir's name; historical
	// rows from the event log carry "". Dedup only collapses rows that share
	// the same (IssueNumber, BatchKey) so a current active row is never hidden
	// by a historical aborted row from another batch.
	BatchKey string `json:"batchKey,omitempty"`
}

type portalActiveRun struct {
	Key          string
	Dir          string
	SocketPath   string
	IssueNumber  int
	IssueNumbers []int
	StartedAt    time.Time
	ModTime      time.Time
}

type portalRunMatch struct {
	instance portalActiveRun
	state    *events.RunState
}

type portalRunsView struct{}

// compute is the entry point for computing displayable portal runs.
func (v *portalRunsView) compute(repoRoot string, eventLog events.EventLog) ([]portalRun, error) {
	activeInstances, err := v.discoverActiveRuns(repoRoot)
	if err != nil {
		return nil, err
	}

	eventList, err := eventLog.Read()
	if err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}

	runStates := events.ProjectRunStates(eventList)
	eventsByRun := v.groupEventsByRun(eventList)
	activeStates := make([]events.RunState, 0, len(runStates))
	activeBatchStart := time.Time{}
	for _, run := range runStates {
		if run.IsActive() {
			activeStates = append(activeStates, run)
		}
	}

	runs := make([]portalRun, 0, len(runStates)+len(activeInstances))
	consumedRunIDs := make(map[string]struct{})
	promptActive := make([]portalActiveRun, 0, len(activeInstances))
	for _, active := range activeInstances {
		if activeBatchStart.IsZero() && !active.StartedAt.IsZero() {
			activeBatchStart = active.StartedAt
		}
		if len(active.IssueNumbers) == 0 {
			promptActive = append(promptActive, active)
			continue
		}
		batchRuns, usedRunIDs := v.runsFromActiveBatch(repoRoot, active, runStates, eventList, eventsByRun)
		runs = append(runs, batchRuns...)
		for runID := range usedRunIDs {
			consumedRunIDs[runID] = struct{}{}
		}
	}

	matchedActive := v.matchActiveRuns(promptActive, activeStates)
	for _, match := range matchedActive {
		run := v.runFromActiveMatch(repoRoot, match, eventsByRun)
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
		runs = append(runs, v.runFromState(repoRoot, runState, nil, eventsByRun))
	}

	runs = v.dedupRuns(runs)
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

// dedupRuns collapses duplicate rows per issue per batch. Two rows for
// the same issue only dedup when they share the same BatchKey, so a current
// active row (BatchKey=active.Key) is never hidden by a historical row
// (BatchKey="") from a different batch.
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
		byBatch := make(map[string][]portalRun)
		batchOrder := make([]string, 0)
		for _, run := range issueRuns {
			if _, ok := byBatch[run.BatchKey]; !ok {
				batchOrder = append(batchOrder, run.BatchKey)
			}
			byBatch[run.BatchKey] = append(byBatch[run.BatchKey], run)
		}
		for _, batchKey := range batchOrder {
			result = append(result, v.dedupRunGroup(byBatch[batchKey])...)
		}
	}
	return result
}

// dedupRunGroup collapses duplicate rows for one issue within one batch.
// It first strips queued rows when any non-queued row exists in the group, then
// applies runPriority (aborted > active > blocked > queued > other) and
// breaks ties by latest StartedAt.
func (v *portalRunsView) dedupRunGroup(runs []portalRun) []portalRun {
	if len(runs) <= 1 {
		return runs
	}
	// A queued row only describes the wait state of an AgentRun and is
	// superseded by any later non-queued row for the same AgentRun. When the
	// group mixes queued with non-queued rows (e.g. a queued event followed by
	// run.started + run.finished events that the same AgentRun emits with a
	// different RunID once it leaves the wait state), strip the queued rows
	// so the terminal status wins regardless of the other priorities.
	nonQueued := make([]portalRun, 0, len(runs))
	queuedOnly := make([]portalRun, 0, len(runs))
	for _, run := range runs {
		if run.Status == "queued" {
			queuedOnly = append(queuedOnly, run)
		} else {
			nonQueued = append(nonQueued, run)
		}
	}
	if len(nonQueued) == 0 {
		if len(queuedOnly) <= 1 {
			return queuedOnly
		}
		bestIdx := 0
		for i := 1; i < len(queuedOnly); i++ {
			if queuedOnly[i].StartedAt.After(queuedOnly[bestIdx].StartedAt) {
				bestIdx = i
			}
		}
		return []portalRun{queuedOnly[bestIdx]}
	}
	runs = nonQueued
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

// runPriority encodes the per-issue dedup priority order:
// aborted > active > blocked > queued > other.
//
// Note: dedupRunGroup strips queued rows when any non-queued row exists,
// so the "queued" case here is unreachable for mixed groups. It is kept as a
// guard for queued-only groups (e.g., genuinely-waiting runs with no terminal
// event) and to make the priority order self-documenting.
func (v *portalRunsView) runPriority(run portalRun) int {
	if run.Status == "aborted" {
		return 4
	}
	if run.Kind == "active" {
		return 3
	}
	switch run.Status {
	case "blocked":
		return 2
	case "queued":
		return 1
	default:
		return 0
	}
}

func (v *portalRunsView) discoverActiveRuns(repoRoot string) ([]portalActiveRun, error) {
	instances, err := discoverPortalInstances(repoRoot)
	if err != nil {
		return nil, err
	}

	active := make([]portalActiveRun, 0, len(instances))
	for _, instance := range instances {
		info, err := os.Stat(instance.SocketPath)
		if err != nil {
			continue
		}
		runDir := filepath.Dir(instance.SocketPath)
		manifest, manifestErr := daemon.ReadManifest(runDir)
		issueNumber, _ := parseRunDirIssue(instance.Name)
		issueNumbers := []int(nil)
		startedAt := info.ModTime()
		if manifestErr == nil {
			issueNumbers = append(issueNumbers, manifest.Issues...)
			if !manifest.CreatedAt.IsZero() {
				startedAt = manifest.CreatedAt
			}
		}
		if len(issueNumbers) == 0 && issueNumber > 0 {
			issueNumbers = []int{issueNumber}
		}
		active = append(active, portalActiveRun{
			Key:          instance.Name,
			Dir:          runDir,
			SocketPath:   instance.SocketPath,
			IssueNumber:  issueNumber,
			IssueNumbers: issueNumbers,
			StartedAt:    startedAt,
			ModTime:      info.ModTime(),
		})
	}
	return active, nil
}

func (v *portalRunsView) runsFromActiveBatch(repoRoot string, active portalActiveRun, runStates []events.RunState, eventList []events.Event, eventsByRun map[string][]portalEvent) ([]portalRun, map[string]struct{}) {
	batchStart := active.StartedAt
	if batchStart.IsZero() {
		batchStart = active.ModTime
	}
	liveOutput := readPortalSocketOutput(active.SocketPath)
	runs := make([]portalRun, 0, len(active.IssueNumbers))
	usedRunIDs := make(map[string]struct{})
	for _, issueNumber := range active.IssueNumbers {
		state := v.latestRunStateForIssue(runStates, issueNumber, batchStart)
		if state != nil && state.Status() == "queued" {
			state = nil
		}
		blocked := v.latestBlockedEventForIssue(eventList, issueNumber, batchStart)
		runs = append(runs, v.runFromActiveBatchIssue(repoRoot, active, issueNumber, state, blocked, liveOutput, eventsByRun))
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

func (v *portalRunsView) runFromActiveBatchIssue(repoRoot string, active portalActiveRun, issueNumber int, state *events.RunState, blocked *events.Event, liveOutput string, eventsByRun map[string][]portalEvent) portalRun {
	issueLabel := fmt.Sprintf("#%d", issueNumber)
	run := portalRun{
		Key:         fmt.Sprintf("%s-issue-%d", active.Key, issueNumber),
		Kind:        "active",
		Status:      "queued",
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		StartedAt:   active.StartedAt,
		SocketPath:  active.SocketPath,
		LogPath:     portalLogPath(repoRoot, issueNumber, ""),
		LogURL:      portalLogDownloadURL(repoRoot, issueNumber, ""),
		Log:         "Queued. Waiting to start.",
		BatchKey:    active.Key,
	}
	if state != nil {
		run.Key = state.RunID
		run.RunID = state.RunID
		run.Status = statusOrDefault(state.Status(), state.IsActive())
		if run.Status == "aborted" {
			run.Kind = "completed"
		}
		run.Branch = state.Branch()
		run.StartedAt = state.Started.Timestamp
		run.Duration = durationForRun(*state)
		run.Events = eventsByRun[state.RunID]
		run.LogPath = portalLogPath(repoRoot, issueNumber, state.Branch())
		run.LogURL = portalLogDownloadURL(repoRoot, issueNumber, state.Branch())
		if state.Finished != nil {
			finishedAt := state.Finished.Timestamp
			run.FinishedAt = &finishedAt
			switch state.Status() {
			case "blocked":
				run.Log = portalBlockedMessage(state.Finished.Payload)
			case "aborted":
				run.Log = "Aborted by operator."
			default:
				run.Log = readPortalTextFile(run.LogPath)
				if strings.TrimSpace(run.Log) == "" {
					run.Log = "No log file yet."
				}
			}
		} else {
			run.Log = filterPortalIssueOutput(liveOutput, issueNumber)
			if strings.TrimSpace(run.Log) == "" {
				run.Log = "No live output captured yet."
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
		run.Log = portalBlockedMessage(blocked.Payload)
	}
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

func (v *portalRunsView) runFromActiveMatch(repoRoot string, match portalRunMatch, eventsByRun map[string][]portalEvent) portalRun {
	if match.state != nil {
		run := v.runFromState(repoRoot, *match.state, &match.instance, eventsByRun)
		run.BatchKey = match.instance.Key
		return run
	}

	startedAt := match.instance.ModTime
	issueLabel := "prompt-only"
	issueNumber := match.instance.IssueNumber
	if issueNumber > 0 {
		issueLabel = fmt.Sprintf("#%d", issueNumber)
	}
	logPath := portalLogPath(repoRoot, issueNumber, "")
	return portalRun{
		Key:         match.instance.Key,
		RunID:       match.instance.Key,
		Kind:        "active",
		Status:      "active",
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		StartedAt:   startedAt,
		Duration:    time.Since(startedAt).Round(time.Second).String(),
		SocketPath:  match.instance.SocketPath,
		LogPath:     logPath,
		LogURL:      portalLogDownloadURL(repoRoot, issueNumber, ""),
		Log:         readPortalSocketOutput(match.instance.SocketPath),
		Events:      eventsByRun[match.instance.Key],
		BatchKey:    match.instance.Key,
	}
}

func (v *portalRunsView) runFromState(repoRoot string, runState events.RunState, active *portalActiveRun, eventsByRun map[string][]portalEvent) portalRun {
	runID := runState.RunID
	if runID == "" && active != nil {
		runID = active.Key
	}

	issueNumber := runState.IssueNumber()
	branch := runState.Branch()
	issueLabel := runState.IssueLabel()
	if issueLabel == "" {
		issueLabel = runID
	}

	status := runState.Status()
	if runState.IsActive() {
		status = "active"
	}
	startedAt := runState.Started.Timestamp
	var finishedAt *time.Time
	if runState.Finished != nil {
		finishedAt = &runState.Finished.Timestamp
	}

	logPath := portalLogPath(repoRoot, issueNumber, branch)
	logContent := readPortalTextFile(logPath)
	if active != nil {
		logContent = readPortalSocketOutput(active.SocketPath)
	}

	portalRun := portalRun{
		Key:         runID,
		RunID:       runID,
		Kind:        kindForRun(runState),
		Status:      statusOrDefault(status, runState.IsActive()),
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		Branch:      branch,
		StartedAt:   startedAt,
		FinishedAt:  finishedAt,
		Duration:    durationForRun(runState),
		LogPath:     logPath,
		LogURL:      portalLogDownloadURL(repoRoot, issueNumber, branch),
		Log:         logContent,
		Events:      eventsByRun[runID],
	}
	if status == "blocked" {
		portalRun.Log = portalBlockedMessage(runState.Finished.Payload)
	}
	if status == "aborted" {
		portalRun.Kind = "completed"
	}
	if active != nil {
		portalRun.SocketPath = active.SocketPath
	}
	return portalRun
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
