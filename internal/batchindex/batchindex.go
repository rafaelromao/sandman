package batchindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"golang.org/x/sys/unix"
)

const IndexVersion = 1

const indexLockTimeout = 5 * time.Second

var indexUpdateMu sync.Mutex

type Kind string

const (
	KindIssue      Kind = "issue"
	KindReview     Kind = "review"
	KindPromptOnly Kind = "prompt-only"
)

type Status string

const (
	StatusActive      Status = "active"
	StatusArchived    Status = "archived"
	StatusUnavailable Status = "unavailable"
)

// RunManifestStatus is the lifecycle status of a single run as recorded in
// run.json. It is deliberately separate from the index Batch Status enum
// (active/archived/unavailable).
type RunManifestStatus string

const (
	RunManifestStatusActive  RunManifestStatus = "active"
	RunManifestStatusSuccess RunManifestStatus = "success"
	RunManifestStatusFailure RunManifestStatus = "failure"
	RunManifestStatusAborted RunManifestStatus = "aborted"
	RunManifestStatusBlocked RunManifestStatus = "blocked"
)

type Index struct {
	Version   int                                    `json:"version"`
	Batches   []Batch                                `json:"-"`
	StatFn    func(path string) (os.FileInfo, error) `json:"-"`
	indexPath string
}

// Batch is the canonical record in the batches index. It was
// formerly named Entry; the rename (slice 7 of #1916) makes the
// canonical slice field `Batches` reflect what it holds.
type Batch struct {
	ID         string      `json:"id"`
	Path       string      `json:"path"`
	Kind       Kind        `json:"kind"`
	Status     Status      `json:"status"`
	CreatedAt  time.Time   `json:"createdAt"`
	Issues     []int       `json:"issues,omitempty"`
	PR         int         `json:"pr,omitempty"`
	ArchivedAt *time.Time  `json:"archivedAt,omitempty"`
	Runs       []RunRecord `json:"runs,omitempty"`
}

// RunRecordStatus is the lifecycle status of a single row as recorded
// in the per-row index. It mirrors the batch-level Status enum but is
// kept independent so callers do not have to thread both into the same
// decision.
type RunRecordStatus string

const (
	RunRecordStatusActive      RunRecordStatus = "active"
	RunRecordStatusArchived    RunRecordStatus = "archived"
	RunRecordStatusUnavailable RunRecordStatus = "unavailable"
)

// RunRecord is the per-row projection of a Batch. It is appended to
// Batch.Runs so each row's lifecycle is observable independently of the
// batch-level Status (which stays active until every row is archived
// and the batch daemon is gone).
type RunRecord struct {
	RunID       string          `json:"runId"`
	Status      RunRecordStatus `json:"status"`
	ArchivePath string          `json:"archivePath,omitempty"`
}

// MarshalJSON writes the batches index with prompt-only batches carrying an
// explicit empty issues array, matching ADR-0032's index schema.
//
// The on-disk JSON key remains "entries" (NOT "batches") so existing
// operator batches.json files continue to round-trip after the
// slice-7 rename of the Go field from Entries to Batches.
func (i Index) MarshalJSON() ([]byte, error) {
	type batchJSON struct {
		ID         string      `json:"id"`
		Path       string      `json:"path"`
		Kind       Kind        `json:"kind"`
		Status     Status      `json:"status"`
		CreatedAt  time.Time   `json:"createdAt"`
		Issues     []int       `json:"issues"`
		PR         int         `json:"pr,omitempty"`
		ArchivedAt *time.Time  `json:"archivedAt,omitempty"`
		Runs       []RunRecord `json:"runs,omitempty"`
	}

	rows := make([]any, 0, len(i.Batches))
	for _, b := range i.Batches {
		issues := b.Issues
		if b.Kind == KindPromptOnly || issues == nil {
			issues = []int{}
		}
		rows = append(rows, batchJSON{
			ID:         b.ID,
			Path:       b.Path,
			Kind:       b.Kind,
			Status:     b.Status,
			CreatedAt:  b.CreatedAt,
			Issues:     issues,
			PR:         b.PR,
			ArchivedAt: b.ArchivedAt,
			Runs:       b.Runs,
		})
	}

	return json.Marshal(struct {
		Version int   `json:"version"`
		Entries []any `json:"entries"`
	}{
		Version: i.Version,
		Entries: rows,
	})
}

// UnmarshalJSON decodes a batches index, accepting the historical
// "entries" JSON key (the on-disk wire format is pinned by ADR-0032
// and by existing operator batches.json files).
func (idx *Index) UnmarshalJSON(data []byte) error {
	type rawIndex struct {
		Version int     `json:"version"`
		Entries []Batch `json:"entries"`
	}
	var raw rawIndex
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	idx.Version = raw.Version
	idx.Batches = raw.Entries
	return nil
}

type RunManifest struct {
	RunID        string            `json:"runID"`
	BatchID      string            `json:"batchId"`
	Issue        int               `json:"issue,omitempty"`
	Branch       string            `json:"branch"`
	BaseBranch   string            `json:"baseBranch"`
	WorktreePath string            `json:"worktreePath"`
	Kind         Kind              `json:"kind"`
	CreatedAt    time.Time         `json:"createdAt"`
	PR           int               `json:"pr,omitempty"`
	Status       RunManifestStatus `json:"status"`
}

type SeenComment struct {
	CommentID string    `json:"commentID"`
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	// Attempts is the launch-retry attempt count observed for this
	// comment. It survives daemon restarts and is reset to zero on a
	// terminal-success write (see review.MarkSeen). Files written
	// before this field was introduced decode with Attempts = 0.
	Attempts int `json:"attempts,omitempty"`
	// NextAttemptAt is the earliest wall-clock time at which the
	// daemon may re-launch this comment. Persisted by the
	// launch-failure path (issue #2211) so the per-trigger retry
	// budget survives daemon restarts. Nil means "no gate" —
	// processPR treats nil as "launch immediately" and the JSON
	// encoder omits the field for backward compatibility with
	// pre-#2211 files. Pointer-typed so json:"omitempty" works
	// (time.Time's zero value is not detected by encoding/json).
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
}

type Claim struct {
	Holder string    `json:"holder"`
	Since  time.Time `json:"since"`
}

type ReviewState struct {
	PR           int              `json:"pr"`
	SeenComments []SeenComment    `json:"seenComments"`
	Claims       map[string]Claim `json:"claims"`
}

func Load(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var idx Index
		if err := json.Unmarshal(data, &idx); err != nil {
			if bakIdx := loadBak(path); bakIdx != nil {
				return bakIdx, nil
			}
			return nil, fmt.Errorf("decode batches index: %w", err)
		}

		if idx.Version != IndexVersion {
			if bakIdx := loadBak(path); bakIdx != nil {
				return bakIdx, nil
			}
			return nil, fmt.Errorf("unsupported batches index version %d", idx.Version)
		}

		if idx.StatFn == nil {
			idx.StatFn = os.Stat
		}
		idx.indexPath = path
		return &idx, nil
	}

	if bakIdx := loadBak(path); bakIdx != nil {
		return bakIdx, nil
	}

	if os.IsNotExist(err) {
		return &Index{Version: IndexVersion, Batches: nil, StatFn: os.Stat, indexPath: path}, nil
	}

	return nil, fmt.Errorf("read batches index: %w", err)
}

func loadBak(path string) *Index {
	bakPath := path + ".bak"
	data, err := os.ReadFile(bakPath)
	if err != nil {
		return nil
	}

	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil
	}

	if idx.Version != IndexVersion {
		return nil
	}

	if idx.StatFn == nil {
		idx.StatFn = os.Stat
	}
	idx.indexPath = path
	return &idx
}

func (idx *Index) MarkUnavailable() bool {
	statFn := idx.StatFn
	if statFn == nil {
		statFn = os.Stat
	}
	dirty := false
	for i := range idx.Batches {
		b := &idx.Batches[i]
		if b.Status == StatusActive || b.Status == StatusArchived {
			if _, err := statFn(b.Path); err != nil {
				if os.IsNotExist(err) {
					b.Status = StatusUnavailable
					dirty = true
				}
			}
		}
	}
	return dirty
}

func (idx *Index) EnsureStatus() error {
	idx.MarkUnavailable()
	return nil
}

// EnsureStatusWithLayout reconciles both batch-level unavailable
// detection (MarkUnavailable) and per-row reconciliation
// (ReconcileRuns). The layout parameter is the repo root used to
// resolve on-disk paths for per-row reconciliation.
func (idx *Index) EnsureStatusWithLayout(repoRoot string) error {
	idx.MarkUnavailable()
	idx.ReconcileRuns(repoRoot)
	return nil
}

func (idx *Index) Save(indexPath string) error {
	for i := range idx.Batches {
		idx.Batches[i].ID = canonicalizeBatchID(idx.Batches[i].ID, idx.Batches[i].Path)
	}

	prev, err := os.ReadFile(indexPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(prev) > 0 {
		var previous Index
		if err := json.Unmarshal(prev, &previous); err != nil || previous.Version != IndexVersion {
			// A recovery update must not replace a known-good backup with the
			// corrupt primary that caused Load to fall back to it.
			prev = nil
		}
	}

	if err := atomicfs.WriteAtomicJSON(indexPath, idx, 0644); err != nil {
		return err
	}

	if prev != nil {
		bakPath := indexPath + ".bak"
		if err := atomicfs.WriteAtomic(bakPath, prev, 0644); err != nil {
			return fmt.Errorf("write index bak: %w", err)
		}
	}

	return nil
}

// Update applies mutate to the latest persisted index while holding an
// index-scoped advisory lock. The operating system releases the lock if the
// writer exits unexpectedly, so an abandoned process cannot block later work.
func Update(indexPath string, mutate func(*Index) error) error {
	indexUpdateMu.Lock()
	defer indexUpdateMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(indexPath), 0755); err != nil {
		return fmt.Errorf("create batches index directory %q: %w", filepath.Dir(indexPath), err)
	}
	lock, err := os.OpenFile(indexPath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open batches index lock %q: %w", indexPath, err)
	}
	defer lock.Close()

	deadline := time.Now().Add(indexLockTimeout)
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return fmt.Errorf("lock batches index %q: %w", indexPath, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lock batches index %q: timed out after %s", indexPath, indexLockTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	idx, err := Load(indexPath)
	if err != nil {
		return err
	}
	if err := mutate(idx); err != nil {
		return err
	}
	return idx.Save(indexPath)
}

// ResolveBatch returns the index Batch whose ID equals the supplied
// public BatchId, or nil when no batch matches. The id is the public
// BatchId (== Batch folder basename) — see ADR-0032 §"Identity
// table". Per-row RunIDs are intentionally NOT in scope here; they
// are resolved through ResolveBatchFromRowID at the portal layer
// because they need an on-disk manifest fallback.
func (idx *Index) ResolveBatch(batchID string) *Batch {
	for i := range idx.Batches {
		if idx.Batches[i].ID == batchID {
			return &idx.Batches[i]
		}
	}
	return nil
}

func (idx *Index) Resolve(batchID string) *Batch {
	return idx.ResolveBatch(batchID)
}

func (idx *Index) AddBatch(batch Batch) {
	batch.ID = canonicalizeBatchID(batch.ID, batch.Path)
	for i, b := range idx.Batches {
		if b.ID == batch.ID {
			batch.CreatedAt = b.CreatedAt
			if b.Status == StatusArchived {
				batch.Status = StatusArchived
				batch.ArchivedAt = b.ArchivedAt
			} else {
				batch.Status = StatusActive
			}
			idx.Batches[i] = batch
			return
		}
	}
	batch.Status = StatusActive
	idx.Batches = append(idx.Batches, batch)
}

// canonicalizeBatchID returns a non-empty batch id derived from the
// supplied ID, falling back to the on-disk path basename when ID is
// empty. The path-basename fallback exists to stop two distinct batches
// from silently colliding on "" in the index's id-keyed lookup, which
// would overwrite the first batch's row in place (issue #1464). When
// both the ID and the path basename are unusable (rare — only an
// empty path fits this), the literal path is returned so the batch is
// still addressable; the operator can repair the malformed id
// separately.
func canonicalizeBatchID(id, path string) string {
	if id != "" {
		return id
	}
	if path != "" {
		if base := filepath.Base(path); base != "" && base != "." && base != "/" {
			return base
		}
	}
	return path
}

func (idx *Index) SetArchived(id, archivePath string, archivedAt time.Time) error {
	for i := range idx.Batches {
		if idx.Batches[i].ID == id {
			idx.Batches[i].Status = StatusArchived
			idx.Batches[i].Path = archivePath
			idx.Batches[i].ArchivedAt = &archivedAt
			return nil
		}
	}
	return fmt.Errorf("batch not found: %s", id)
}

// AddRun records a per-row RunRecord against the named batch. If a
// record with the same RunID already exists, it is replaced (so the
// caller can re-record an updated status without duplicating rows).
func (idx *Index) AddRun(batchID string, rec RunRecord) {
	for i := range idx.Batches {
		if idx.Batches[i].ID != batchID {
			continue
		}
		for j := range idx.Batches[i].Runs {
			if idx.Batches[i].Runs[j].RunID == rec.RunID {
				idx.Batches[i].Runs[j] = rec
				return
			}
		}
		idx.Batches[i].Runs = append(idx.Batches[i].Runs, rec)
		return
	}
}

// MarkRunArchived flips the named row's record to archived and sets
// its ArchivePath. It returns an error when the batch or the row does
// not exist so callers can fail loudly instead of silently losing the
// archive marker.
func (idx *Index) MarkRunArchived(batchID, runID, archivePath string) error {
	for i := range idx.Batches {
		if idx.Batches[i].ID != batchID {
			continue
		}
		for j := range idx.Batches[i].Runs {
			if idx.Batches[i].Runs[j].RunID != runID {
				continue
			}
			idx.Batches[i].Runs[j].Status = RunRecordStatusArchived
			idx.Batches[i].Runs[j].ArchivePath = archivePath
			return nil
		}
		return fmt.Errorf("run %q not found in batch %q", runID, batchID)
	}
	return fmt.Errorf("batch not found: %s", batchID)
}

// RunRecordFor returns the per-row RunRecord for the given batch id
// and run id, or nil when neither the batch nor the row is recorded.
func (idx *Index) RunRecordFor(batchID, runID string) *RunRecord {
	for i := range idx.Batches {
		if idx.Batches[i].ID != batchID {
			continue
		}
		for j := range idx.Batches[i].Runs {
			if idx.Batches[i].Runs[j].RunID == runID {
				return &idx.Batches[i].Runs[j]
			}
		}
		return nil
	}
	return nil
}

// ReconcileRuns walks the per-row Records and reconciles each one
// against the on-disk layout. For a row whose ArchivePath is non-empty:
//
//   - live runs/<runID>/ present → leave the record as-is
//     (post-archive state, normal case)
//   - live missing, archive present → leave the record as archived
//     (lazy recovery; the archive is the source of truth)
//   - live missing, archive missing → flip the row to unavailable and
//     clear ArchivePath so subsequent renders do not echo a phantom
//     archive path
//
// ReconcileRuns is safe to call on batches that have no Runs records
// (legacy batches); it is a no-op for them. It does not flip the
// batch-level Status — that decision belongs to EnsureStatus/MarkUnavailable.
func (idx *Index) ReconcileRuns(repoRoot string) {
	statFn := idx.StatFn
	if statFn == nil {
		statFn = os.Stat
	}
	for i := range idx.Batches {
		batch := &idx.Batches[i]
		for j := range batch.Runs {
			rec := &batch.Runs[j]
			if rec.ArchivePath == "" {
				continue
			}
			livePath := filepath.Join(repoRoot, ".sandman", "batches", batch.ID, "runs", rec.RunID)
			if _, err := statFn(livePath); err == nil {
				continue
			}
			archivePath := filepath.Join(repoRoot, rec.ArchivePath)
			if _, err := statFn(archivePath); err == nil {
				continue
			}
			rec.Status = RunRecordStatusUnavailable
			rec.ArchivePath = ""
		}
	}
}

func (idx *Index) RemoveBatch(id string) error {
	if idx.indexPath != "" {
		return Update(idx.indexPath, func(current *Index) error {
			for i := range current.Batches {
				if current.Batches[i].ID == id {
					current.Batches = append(current.Batches[:i], current.Batches[i+1:]...)
					return nil
				}
			}
			return fmt.Errorf("batch not found: %s", id)
		})
	}
	for i := range idx.Batches {
		if idx.Batches[i].ID == id {
			idx.Batches = append(idx.Batches[:i], idx.Batches[i+1:]...)
			return idx.Save(idx.indexPath)
		}
	}
	return fmt.Errorf("batch not found: %s", id)
}

func WriteManifest(runDir string, manifest RunManifest) error {
	runManifestPath := filepath.Join(runDir, "run.json")
	return atomicfs.WriteAtomicJSON(runManifestPath, manifest, 0644)
}

func ReadManifest(runDir string) (RunManifest, error) {
	runManifestPath := filepath.Join(runDir, "run.json")
	data, err := os.ReadFile(runManifestPath)
	if err != nil {
		return RunManifest{}, err
	}
	var manifest RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("decode run manifest: %w", err)
	}
	return manifest, nil
}

func WriteReviewState(runDir string, state ReviewState) error {
	statePath := filepath.Join(runDir, "review-state.json")
	return atomicfs.WriteAtomicJSON(statePath, state, 0644)
}

func ReadReviewState(runDir string) (ReviewState, error) {
	statePath := filepath.Join(runDir, "review-state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		return ReviewState{}, err
	}
	var state ReviewState
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode review state: %w", err)
	}
	return state, nil
}
