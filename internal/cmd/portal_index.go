package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

const portalRunsSnapshotTTL = 250 * time.Millisecond
const portalRunsIndexMaxSocketReaders = 4

type portalRunsIndex struct {
	repoRoot          string
	eventLogPath      string
	view              *portalRunsView
	mu                sync.Mutex
	snapshotAt        time.Time
	snapshotReady     bool
	snapshotCache     []portalRun
	snapshotSourceKey string
	snapshotETag      string
	eventsCache       []events.Event
	eventsOffset      int64
	eventsModTime     time.Time
	eventsSize        int64
}

type portalSummaryState struct {
	eventsList      []events.Event
	runStates       []events.RunState
	eventsByRun     map[string][]portalEvent
	activeInstances []portalActiveRun
	batchesIndex    *batchindex.Index
	sourceKey       string
}

type portalSummaryResponse struct {
	Runs        []portalRun
	ETag        string
	NotModified bool
}

var portalRunsIndexes sync.Map // map[repoRoot]*portalRunsIndex

func getPortalRunsIndex(repoRoot string) *portalRunsIndex {
	if idx, ok := portalRunsIndexes.Load(repoRoot); ok {
		return idx.(*portalRunsIndex)
	}
	layout := paths.NewLayout(nil, repoRoot)
	idx := &portalRunsIndex{
		repoRoot:      repoRoot,
		eventLogPath:  layout.EventsLogPath,
		view:          &portalRunsView{},
		snapshotAt:    time.Time{},
		snapshotReady: false,
	}
	actual, _ := portalRunsIndexes.LoadOrStore(repoRoot, idx)
	return actual.(*portalRunsIndex)
}

func (idx *portalRunsIndex) Snapshot(ctx context.Context) ([]portalRun, error) {
	idx.mu.Lock()
	if (idx.snapshotReady || len(idx.snapshotCache) > 0) && time.Since(idx.snapshotAt) < portalRunsSnapshotTTL {
		runs := clonePortalRuns(idx.snapshotCache)
		idx.mu.Unlock()
		return runs, nil
	}
	idx.mu.Unlock()

	state, err := idx.loadSummaryState(ctx)
	if err != nil {
		return nil, err
	}
	runs, err := idx.view.computeWithActiveRunsAndIndex(idx.repoRoot, state.eventsList, state.eventsByRun, state.activeInstances, state.batchesIndex)
	if err != nil {
		return nil, err
	}
	etag, err := portalSummaryETag(idx.repoRoot, runs)
	if err != nil {
		return nil, err
	}

	idx.mu.Lock()
	idx.snapshotAt = time.Now()
	idx.snapshotReady = true
	idx.snapshotCache = clonePortalRuns(runs)
	idx.snapshotSourceKey = state.sourceKey
	idx.snapshotETag = etag
	idx.mu.Unlock()
	return clonePortalRuns(runs), nil
}

func (idx *portalRunsIndex) SummarySnapshot(ctx context.Context, ifNoneMatch string) (portalSummaryResponse, error) {
	idx.mu.Lock()
	if (idx.snapshotReady || len(idx.snapshotCache) > 0) && time.Since(idx.snapshotAt) < portalRunsSnapshotTTL {
		eTag := idx.snapshotETag
		idx.mu.Unlock()
		if eTag != "" && etagMatches(ifNoneMatch, eTag) {
			return portalSummaryResponse{ETag: eTag, NotModified: true}, nil
		}
	} else {
		idx.mu.Unlock()
	}

	state, err := idx.loadSummaryProbe(ctx)
	if err != nil {
		return portalSummaryResponse{}, err
	}

	idx.mu.Lock()
	if (idx.snapshotReady || len(idx.snapshotCache) > 0) && state.sourceKey == idx.snapshotSourceKey {
		runs := portalSummaryRuns(idx.snapshotCache)
		etag := idx.snapshotETag
		idx.mu.Unlock()
		if etag != "" && etagMatches(ifNoneMatch, etag) {
			return portalSummaryResponse{ETag: etag, NotModified: true}, nil
		}
		return portalSummaryResponse{Runs: runs, ETag: etag}, nil
	}
	idx.mu.Unlock()

	state.activeInstances = idx.hydrateActiveRunOutputs(ctx, state.activeInstances)
	runs, err := idx.view.computeWithActiveRunsAndIndex(idx.repoRoot, state.eventsList, state.eventsByRun, state.activeInstances, state.batchesIndex)
	if err != nil {
		return portalSummaryResponse{}, err
	}
	etag, err := portalSummaryETag(idx.repoRoot, runs)
	if err != nil {
		return portalSummaryResponse{}, err
	}
	if etag != "" && etagMatches(ifNoneMatch, etag) {
		idx.mu.Lock()
		idx.snapshotAt = time.Now()
		idx.snapshotReady = true
		idx.snapshotCache = clonePortalRuns(runs)
		idx.snapshotSourceKey = state.sourceKey
		idx.snapshotETag = etag
		idx.mu.Unlock()
		return portalSummaryResponse{ETag: etag, NotModified: true}, nil
	}

	idx.mu.Lock()
	idx.snapshotAt = time.Now()
	idx.snapshotReady = true
	idx.snapshotCache = clonePortalRuns(runs)
	idx.snapshotSourceKey = state.sourceKey
	idx.snapshotETag = etag
	idx.mu.Unlock()
	return portalSummaryResponse{Runs: portalSummaryRuns(runs), ETag: etag}, nil
}

func (idx *portalRunsIndex) loadSummaryProbe(ctx context.Context) (portalSummaryState, error) {
	eventsList, err := idx.readEvents()
	if err != nil {
		return portalSummaryState{}, err
	}
	eventsByRun := idx.view.groupEventsByRun(eventsList)
	runStates := events.ProjectRunStates(eventsList)
	activeInstances, err := idx.view.discoverActiveRuns(idx.repoRoot, eventsByRun)
	if err != nil {
		return portalSummaryState{}, err
	}
	batchesIndex := idx.view.loadBatchesIndex(idx.repoRoot)
	sourceKey, err := portalSummarySourceKey(idx.repoRoot, runStates, activeInstances, batchesIndex)
	if err != nil {
		return portalSummaryState{}, err
	}
	return portalSummaryState{
		eventsList:      eventsList,
		runStates:       runStates,
		eventsByRun:     eventsByRun,
		activeInstances: activeInstances,
		batchesIndex:    batchesIndex,
		sourceKey:       sourceKey,
	}, nil
}

func (idx *portalRunsIndex) loadSummaryState(ctx context.Context) (portalSummaryState, error) {
	state, err := idx.loadSummaryProbe(ctx)
	if err != nil {
		return portalSummaryState{}, err
	}
	state.activeInstances = idx.hydrateActiveRunOutputs(ctx, state.activeInstances)
	return state, nil
}

func (idx *portalRunsIndex) hydrateActiveRunOutputs(ctx context.Context, instances []portalActiveRun) []portalActiveRun {
	sem := make(chan struct{}, portalRunsIndexMaxSocketReaders)
	var wg sync.WaitGroup
	for i := range instances {
		if instances[i].SocketPath == "" {
			continue
		}
		select {
		case <-ctx.Done():
			return instances
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			instances[i].LiveOutput = idx.view.readPortalSocketOutput(instances[i].SocketPath)
		}(i)
	}
	wg.Wait()
	return instances
}

func (idx *portalRunsIndex) FindByKey(ctx context.Context, runKey string) (portalRun, error) {
	runs, err := idx.Snapshot(ctx)
	if err != nil {
		return portalRun{}, err
	}
	for _, run := range runs {
		if run.Key == runKey {
			return run, nil
		}
	}
	return portalRun{}, &portalAbortError{status: 404, message: fmt.Sprintf("run %q not found", runKey)}
}

func (idx *portalRunsIndex) Invalidate() {
	idx.mu.Lock()
	idx.snapshotAt = time.Time{}
	idx.snapshotReady = false
	idx.snapshotCache = nil
	idx.mu.Unlock()
}

func portalSummarySourceKey(repoRoot string, runStates []events.RunState, activeInstances []portalActiveRun, batchesIndex *batchindex.Index) (string, error) {
	type summaryActiveFingerprint struct {
		portalActiveRun
		LastOutputAt time.Time `json:"lastOutputAt,omitempty"`
	}
	activeStates := make([]events.RunState, 0, len(runStates))
	for _, runState := range runStates {
		if runState.IsActive() {
			activeStates = append(activeStates, runState)
		}
	}
	matches := (&portalRunsView{}).matchActiveRuns(activeInstances, activeStates)
	activeFingerprint := make([]summaryActiveFingerprint, len(matches))
	for i := range matches {
		match := matches[i]
		active := match.instance
		active.LiveOutput = ""
		runID := active.RunID
		batchID := active.BatchID
		startedAt := active.ModTime
		if startedAt.IsZero() {
			startedAt = active.StartedAt
		}
		if match.state != nil {
			runID = match.state.RunID
			startedAt = match.state.Started.Timestamp
			// Resolve batchID from the event payload's batch_id (with
			// "+N" on-disk suffix for multi-issue batches) first, and
			// only fall back to active.BatchID when the payload has no
			// batch_id. The active instance's BatchID comes from
			// manifest.BatchId, which equals the per-row RunID for the
			// first issue (ADR-0036) and does not match the on-disk
			// directory name; using it for the log-path locator makes
			// the summary ETag stat miss the real per-row log file
			// (issue #1715).
			batchID = match.state.BatchID()
			if batchID == "" {
				if active.BatchID != "" {
					batchID = active.BatchID
				} else {
					batchID = batchIDFromRunID(runID)
				}
			}
		}
		activeFingerprint[i] = summaryActiveFingerprint{
			portalActiveRun: active,
			LastOutputAt:    portalLastOutputAt((&portalRunsView{}).portalLogPathForRun(repoRoot, runLocator{batchID: batchID, runID: runID}), startedAt),
		}
	}
	payload, err := json.Marshal(struct {
		RunStates       []events.RunState          `json:"runStates"`
		ActiveInstances []summaryActiveFingerprint `json:"activeInstances"`
		BatchesIndex    *batchindex.Index          `json:"batchesIndex,omitempty"`
	}{
		RunStates:       runStates,
		ActiveInstances: activeFingerprint,
		BatchesIndex:    batchesIndex,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func portalSummaryETag(repoRoot string, runs []portalRun) (string, error) {
	summaryRuns := portalSummaryRuns(runs)
	payload, err := json.Marshal(map[string]any{"repoRoot": repoRoot, "runs": summaryRuns})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return `"` + hex.EncodeToString(sum[:]) + `"`, nil
}

func portalSummaryRuns(runs []portalRun) []portalRun {
	summaryRuns := clonePortalRuns(runs)
	for i := range summaryRuns {
		summaryRuns[i].Log = ""
		summaryRuns[i].LogURL = ""
	}
	return summaryRuns
}

func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" || etag == "" {
		return false
	}
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		value := strings.TrimSpace(candidate)
		if value == etag {
			return true
		}
		if strings.HasPrefix(value, "W/") && strings.TrimPrefix(value, "W/") == etag {
			return true
		}
	}
	return false
}

func (idx *portalRunsIndex) readEvents() ([]events.Event, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	info, err := os.Stat(idx.eventLogPath)
	if err != nil {
		if os.IsNotExist(err) {
			idx.eventsCache = nil
			idx.eventsOffset = 0
			idx.eventsSize = 0
			idx.eventsModTime = time.Time{}
			return nil, nil
		}
		return nil, fmt.Errorf("stat event log: %w", err)
	}

	if len(idx.eventsCache) > 0 && info.Size() == idx.eventsSize && info.ModTime().Equal(idx.eventsModTime) {
		return cloneEvents(idx.eventsCache), nil
	}

	if len(idx.eventsCache) > 0 && info.Size() >= idx.eventsOffset {
		if appended, ok, err := idx.readAppendedEventsLocked(info.Size()); err != nil {
			return nil, err
		} else if ok {
			idx.eventsCache = append(idx.eventsCache, appended...)
			idx.eventsOffset = info.Size()
			idx.eventsSize = info.Size()
			idx.eventsModTime = info.ModTime()
			return cloneEvents(idx.eventsCache), nil
		}
	}

	log := &events.JSONLLogger{Path: idx.eventLogPath}
	eventsList, err := log.Read()
	if err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}
	idx.eventsCache = cloneEvents(eventsList)
	idx.eventsOffset = info.Size()
	idx.eventsSize = info.Size()
	idx.eventsModTime = info.ModTime()
	return cloneEvents(eventsList), nil
}

func (idx *portalRunsIndex) readAppendedEventsLocked(size int64) ([]events.Event, bool, error) {
	if idx.eventsOffset == 0 || size == idx.eventsOffset {
		return nil, false, nil
	}
	f, err := os.Open(idx.eventLogPath)
	if err != nil {
		return nil, false, fmt.Errorf("open event log: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(idx.eventsOffset, 0); err != nil {
		return nil, false, fmt.Errorf("seek event log: %w", err)
	}
	appendBytes, err := io.ReadAll(f)
	if err != nil {
		return nil, false, fmt.Errorf("read appended event log: %w", err)
	}
	decoded, ok, err := decodeAppendedPortalEvents(appendBytes)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return decoded, true, nil
}

func decodeAppendedPortalEvents(data []byte) ([]events.Event, bool, error) {
	if len(data) == 0 {
		return nil, true, nil
	}
	if data[len(data)-1] != '\n' {
		return nil, false, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	out := make([]events.Event, 0, 8)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev events.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, false, nil
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func clonePortalRuns(in []portalRun) []portalRun {
	return append([]portalRun(nil), in...)
}

func cloneEvents(in []events.Event) []events.Event {
	return append([]events.Event(nil), in...)
}
