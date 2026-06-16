package cmd

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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
	// Review flags runs whose run.started event carried payload.review = true.
	// The field is omitted from JSON when false to preserve the existing /api/runs
	// contract for implementation runs.
	Review bool `json:"review,omitempty"`
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
	// payload (added by issue #833). Empty for historical or prompt-only runs.
	IssueTitle string `json:"issueTitle,omitempty"`
	// RetriesTotal mirrors RunState.RetriesTotal (issue #976): the number of
	// retry attempts the orchestrator allowed for the run, read from the
	// run.finished payload. Omitted for active (unfinished) runs because
	// slice-2's getter returns 0 when no finished event is present.
	RetriesTotal int `json:"retriesTotal,omitempty"`
	// RetriesDone mirrors RunState.RetriesDone: the number of retry attempts
	// the run actually consumed. Omitted when the run has not finished.
	RetriesDone int `json:"retriesDone,omitempty"`
}

type portalActiveRun struct {
	Key          string
	Dir          string
	SocketPath   string
	IssueNumber  int
	IssueNumbers []int
	PRNumber     int
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
// It first strips queued and blocked rows when any other row exists, then
// applies runPriority (aborted > active > blocked > queued > other) and
// breaks ties by latest StartedAt.
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

// runPriority encodes the per-issue dedup priority order:
// aborted > active > blocked > queued > other.
//
// Note: dedupRunGroup strips queued and blocked rows when any other row
// exists, so the "queued" and "blocked" cases here are unreachable for mixed
// groups. They are kept as guards for waiting-only groups (e.g., genuinely-
// waiting runs with no terminal event) and to make the priority order
// self-documenting.
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
		prNumber, _ := v.parseRunDirPR(instance.Name)
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
	liveOutput := v.readPortalSocketOutput(active.SocketPath)
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
		LogPath:     v.portalLogPath(repoRoot, issueNumber, ""),
		LogURL:      v.portalLogDownloadURL(repoRoot, issueNumber, ""),
		Log:         "Queued. Waiting to start.",
		BatchKey:    active.Key,
	}
	// Only surface batch membership for mixed batches. A single-issue
	// batch is not interesting to surface and would add payload noise.
	if len(active.IssueNumbers) > 1 {
		run.BatchIssues = append([]int(nil), active.IssueNumbers...)
	}
	if state != nil {
		run.Key = state.RunID
		run.RunID = state.RunID
		run.Status = v.statusOrDefault(state.Status(), state.IsActive(), state.IsReview())
		if run.Status == "aborted" {
			run.Kind = "completed"
		}
		run.Branch = state.Branch()
		run.IssueTitle = v.issueTitleFromPayload(state.Started.Payload)
		run.StartedAt = state.Started.Timestamp
		run.Duration = v.durationForRun(*state)
		run.Events = eventsByRun[state.RunID]
		run.LogPath = v.portalLogPath(repoRoot, issueNumber, state.Branch())
		run.LogURL = v.portalLogDownloadURL(repoRoot, issueNumber, state.Branch())
		if state.Finished != nil {
			finishedAt := state.Finished.Timestamp
			run.FinishedAt = &finishedAt
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
		} else {
			run.Log = v.filterPortalIssueOutput(liveOutput, issueNumber)
			if strings.TrimSpace(run.Log) == "" {
				run.Log = "No live output captured yet."
			}
		}
		v.markCompletedIfSocketDead(&run, run.SocketPath)
		return run
	}
	if blocked != nil {
		run.Key = blocked.RunID
		run.RunID = blocked.RunID
		run.Status = "blocked"
		run.StartedAt = blocked.Timestamp
		run.Events = []portalEvent{{Type: blocked.Type, Timestamp: blocked.Timestamp, Payload: blocked.Payload}}
		run.Log = v.portalBlockedMessage(blocked.Payload)
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

func (v *portalRunsView) runFromActiveMatch(repoRoot string, match portalRunMatch, eventsByRun map[string][]portalEvent) portalRun {
	runID := match.instance.Key
	if match.state != nil {
		run := v.runFromState(repoRoot, *match.state, &match.instance, eventsByRun)
		run.BatchKey = match.instance.Key
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
	logPath := v.portalLogPath(repoRoot, issueNumber, "")
	run := portalRun{
		Key:         runID,
		RunID:       runID,
		Kind:        "active",
		Status:      status,
		IssueLabel:  issueLabel,
		IssueNumber: issueNumber,
		Review:      review,
		PRNumber:    prNumber,
		StartedAt:   startedAt,
		Duration:    time.Since(startedAt).Round(time.Second).String(),
		SocketPath:  match.instance.SocketPath,
		LogPath:     logPath,
		LogURL:      v.portalLogDownloadURL(repoRoot, issueNumber, ""),
		Log:         v.readPortalSocketOutput(match.instance.SocketPath),
		Events:      eventsByRun[match.instance.Key],
		BatchKey:    match.instance.Key,
	}
	v.markCompletedIfSocketDead(&run, run.SocketPath)
	return run
}

func (v *portalRunsView) runFromState(repoRoot string, runState events.RunState, active *portalActiveRun, eventsByRun map[string][]portalEvent) portalRun {
	runID := runState.RunID
	if runID == "" && active != nil {
		runID = active.Key
	}

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

	logPath := v.portalLogPath(repoRoot, issueNumber, branch)
	logContent := v.readPortalTextFile(logPath)
	if active != nil {
		logContent = v.readPortalSocketOutput(active.SocketPath)
	}

	portalRun := portalRun{
		Key:          runID,
		RunID:        runID,
		Kind:         v.kindForRun(runState),
		Status:       v.statusOrDefault(status, runState.IsActive(), runState.IsReview()),
		IssueLabel:   issueLabel,
		IssueNumber:  issueNumber,
		IssueTitle:   v.issueTitleFromPayload(runState.Started.Payload),
		Branch:       branch,
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
		Duration:     v.durationForRun(runState),
		LogPath:      logPath,
		LogURL:       v.portalLogDownloadURL(repoRoot, issueNumber, branch),
		Log:          logContent,
		Events:       eventsByRun[runID],
		Review:       runState.IsReview(),
		RetriesTotal: runState.RetriesTotal(),
		RetriesDone:  runState.RetriesDone(),
	}
	if review, pr := v.reviewContext(runState); review {
		portalRun.Review = true
		portalRun.PRNumber = pr
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
		sockPath := filepath.Join(paths.NewLayout(&config.Config{}, repoRoot).RunsDir, runState.RunID, "run.sock")
		if _, err := os.Lstat(sockPath); err == nil {
			portalRun.SocketPath = sockPath
			v.markCompletedIfSocketDead(&portalRun, sockPath)
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

func (v *portalRunsView) statusOrDefault(status string, active bool, isReview bool) string {
	status = strings.TrimSpace(status)
	if active && isReview {
		return "reviewing"
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

func (v *portalRunsView) filterPortalIssueOutput(text string, issueNumber int) string {
	prefix := fmt.Sprintf("[issue-%d] ", issueNumber)
	lines := strings.Split(text, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			filtered = append(filtered, line)
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
			switch n := value.(type) {
			case float64:
				issues = append(issues, int(n))
			case int:
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
	if payload == nil {
		return 0
	}
	raw, ok := payload["pr_number"]
	if !ok {
		return 0
	}
	switch n := raw.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
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
		run.Kind = "completed"
	}
}

func (v *portalRunsView) portalLogPath(repoRoot string, issueNumber int, branch string) string {
	layout := paths.NewLayout(&config.Config{}, repoRoot)
	if issueNumber > 0 {
		return filepath.Join(layout.LogDir, fmt.Sprintf("%d.log", issueNumber))
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	return filepath.Join(layout.LogDir, layout.SafeLogFilename(branch)+".log")
}

func (v *portalRunsView) portalLogDownloadURL(repoRoot string, issueNumber int, branch string) string {
	logPath := v.portalLogPath(repoRoot, issueNumber, branch)
	if logPath == "" {
		return ""
	}
	relPath, err := filepath.Rel(repoRoot, logPath)
	if err != nil {
		return ""
	}
	return "/api/logs?path=" + url.QueryEscape(relPath)
}

func (v *portalRunsView) parseRunDirPR(name string) (int, bool) {
	if !strings.HasPrefix(name, "PR") {
		return 0, false
	}
	rest := strings.TrimPrefix(name, "PR")
	if rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (v *portalRunsView) readPortalTextFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > portalReadLimit {
		tail := data[len(data)-portalReadLimit:]
		return v.cleanPortalText("[truncated]\n" + string(tail))
	}
	return v.cleanPortalText(string(data))
}

func (v *portalRunsView) readPortalSocketOutput(sockPath string) string {
	conn, err := net.DialTimeout("unix", sockPath, portalReadTimeout)
	if err != nil {
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
