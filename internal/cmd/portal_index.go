package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

const portalRunsSnapshotTTL = 250 * time.Millisecond
const portalRunsIndexMaxSocketReaders = 4

type portalRunsIndex struct {
	repoRoot      string
	eventLogPath  string
	view          *portalRunsView
	mu            sync.Mutex
	snapshotAt    time.Time
	snapshotCache []portalRun
	eventsCache   []events.Event
	eventsOffset  int64
	eventsModTime time.Time
	eventsSize    int64
	manifestCache map[string]portalManifestCacheEntry
}

type portalManifestCacheEntry struct {
	size     int64
	modTime  time.Time
	manifest daemon.BatchManifest
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
		manifestCache: make(map[string]portalManifestCacheEntry),
	}
	actual, _ := portalRunsIndexes.LoadOrStore(repoRoot, idx)
	return actual.(*portalRunsIndex)
}

func (idx *portalRunsIndex) Snapshot(ctx context.Context) ([]portalRun, error) {
	idx.mu.Lock()
	if len(idx.snapshotCache) > 0 && time.Since(idx.snapshotAt) < portalRunsSnapshotTTL {
		runs := clonePortalRuns(idx.snapshotCache)
		idx.mu.Unlock()
		return runs, nil
	}
	idx.mu.Unlock()

	eventsList, err := idx.readEvents()
	if err != nil {
		return nil, err
	}
	eventsByRun := idx.view.groupEventsByRun(eventsList)
	activeInstances, err := idx.discoverActiveRuns(eventsByRun)
	if err != nil {
		return nil, err
	}
	activeInstances = idx.hydrateActiveRunOutputs(ctx, activeInstances)
	runs, err := idx.view.computeWithActiveRuns(idx.repoRoot, eventsList, eventsByRun, activeInstances)
	if err != nil {
		return nil, err
	}

	idx.mu.Lock()
	idx.snapshotAt = time.Now()
	idx.snapshotCache = clonePortalRuns(runs)
	idx.mu.Unlock()
	return clonePortalRuns(runs), nil
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

func (idx *portalRunsIndex) discoverActiveRuns(eventsByRun map[string][]portalEvent) ([]portalActiveRun, error) {
	instances, err := discoverPortalInstances(idx.repoRoot)
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
		manifest, manifestErr := idx.readManifestCached(runDir)
		prNumber := 0
		batchID := instance.Name
		runID := instance.Name
		if manifestErr == nil && manifest.BatchId != "" {
			batchID = manifest.BatchId
			prNumber = idx.view.prNumberFromEvent(eventsByRun[runID])
		}
		if manifestErr == nil && manifest.RunKind == "review" {
			// ADR-0030 (issue #1551): a review batch owns one row whose
			// canonical RunID is the per-row folder name (`runs/<rowID>/`).
			// The batchId and the row RunID are distinct — the legacy
			// literal `RunID = "review"` alias is gone — so the active
			// row must source its identity from the on-disk run.json
			// rather than from the batches-index Entry.ID.
			if canonical, ok := canonicalReviewRunID(runDir); ok {
				runID = canonical
				prNumber = idx.view.prNumberFromEvent(eventsByRun[canonical])
			}
		}
		issueNumbers := []int(nil)
		issueNumber := 0
		startedAt := info.ModTime()
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
			Key:          runID,
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

// canonicalReviewRunID returns the per-row RunID for a review batch, read
// from `runs/<rowID>/run.json` under the given batch directory. Returns
// ("", false) when the row manifest is absent or unreadable so the caller
// can fall back to its prior behavior.
//
// Per ADR-0030 (issue #1551) review batches are first-class rows, so the
// canonical row RunID lives in the same `run.json` schema every other run
// kind uses. The active discovery in `discoverActiveRuns` consults this
// helper to avoid collapsing the row RunID onto the batchId (which was the
// legacy review alias behavior).
func canonicalReviewRunID(batchDir string) (string, bool) {
	entries, err := os.ReadDir(filepath.Join(batchDir, "runs"))
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "review" {
			continue
		}
		manifest, err := daemon.ReadRunManifest(batchDir, e.Name())
		if err != nil || manifest.RunID == "" {
			continue
		}
		return manifest.RunID, true
	}
	return "", false
}

func (idx *portalRunsIndex) readManifestCached(runDir string) (daemon.BatchManifest, error) {
	path := daemon.ManifestPath(runDir)
	info, err := os.Stat(path)
	if err != nil {
		return daemon.BatchManifest{}, err
	}
	idx.mu.Lock()
	if entry, ok := idx.manifestCache[path]; ok && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
		manifest := entry.manifest
		idx.mu.Unlock()
		return manifest, nil
	}
	idx.mu.Unlock()

	manifest, err := daemon.ReadManifest(runDir)
	if err != nil {
		return daemon.BatchManifest{}, err
	}
	idx.mu.Lock()
	idx.manifestCache[path] = portalManifestCacheEntry{size: info.Size(), modTime: info.ModTime(), manifest: manifest}
	idx.mu.Unlock()
	return manifest, nil
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
	idx.snapshotCache = nil
	idx.mu.Unlock()
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
