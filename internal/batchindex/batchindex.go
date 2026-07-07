package batchindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rafaelromao/sandman/internal/atomicfs"
)

const IndexVersion = 1

type Kind string

const (
	KindIssue      Kind = "issue"
	KindReview     Kind = "review"
	KindAutoSelect Kind = "auto-select"
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
	Entries   []Entry                                `json:"entries"`
	Batches   []Batch                                `json:"-"`
	StatFn    func(path string) (os.FileInfo, error) `json:"-"`
	indexPath string
}

type Entry struct {
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

type Batch = Entry

// RunRecordStatus is the lifecycle status of a single row as recorded
// in the per-row index. It mirrors the entry-level Status enum but is
// kept independent so callers do not have to thread both into the same
// decision.
type RunRecordStatus string

const (
	RunRecordStatusActive      RunRecordStatus = "active"
	RunRecordStatusArchived    RunRecordStatus = "archived"
	RunRecordStatusUnavailable RunRecordStatus = "unavailable"
)

// RunRecord is the per-row projection of an Entry. It is appended to
// Entry.Runs so each row's lifecycle is observable independently of the
// entry-level Status (which stays active until every row is archived
// and the batch daemon is gone).
type RunRecord struct {
	RunID       string          `json:"runId"`
	Status      RunRecordStatus `json:"status"`
	ArchivePath string          `json:"archivePath,omitempty"`
}

// MarshalJSON writes the batches index with prompt-only batches carrying an
// explicit empty issues array, matching ADR-0032's index schema.
func (i Index) MarshalJSON() ([]byte, error) {
	type entryJSON struct {
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

	entries := i.Entries
	if len(entries) == 0 && len(i.Batches) > 0 {
		entries = i.Batches
	}
	batches := make([]any, 0, len(entries))
	for _, b := range entries {
		issues := b.Issues
		if b.Kind == KindPromptOnly || issues == nil {
			issues = []int{}
		}
		batches = append(batches, entryJSON{
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
		Entries: batches,
	})
}

func (idx *Index) UnmarshalJSON(data []byte) error {
	type rawIndex struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}
	var raw rawIndex
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	idx.Version = raw.Version
	idx.Entries = raw.Entries
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
		idx.syncSlices()
		idx.indexPath = path
		return &idx, nil
	}

	if bakIdx := loadBak(path); bakIdx != nil {
		return bakIdx, nil
	}

	if os.IsNotExist(err) {
		return &Index{Version: IndexVersion, Entries: nil, Batches: nil, StatFn: os.Stat, indexPath: path}, nil
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
	idx.syncSlices()
	idx.indexPath = path
	return &idx
}

func (idx *Index) syncSlices() {
	if idx == nil {
		return
	}
	switch {
	case len(idx.Entries) == 0 && len(idx.Batches) > 0:
		idx.Entries = idx.Batches
	case len(idx.Batches) == 0 && len(idx.Entries) > 0:
		idx.Batches = idx.Entries
	}
}

func (idx *Index) MarkUnavailable() bool {
	idx.syncSlices()
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
	idx.Entries = idx.Batches
	return dirty
}

func (idx *Index) EnsureStatus() error {
	idx.MarkUnavailable()
	return nil
}

// EnsureStatusWithLayout reconciles both entry-level unavailable
// detection (MarkUnavailable) and per-row reconciliation
// (ReconcileRuns). The layout parameter is the repo root used to
// resolve on-disk paths for per-row reconciliation.
func (idx *Index) EnsureStatusWithLayout(repoRoot string) error {
	idx.MarkUnavailable()
	idx.ReconcileRuns(repoRoot)
	return nil
}

func (idx *Index) Save(indexPath string) error {
	idx.syncSlices()
	if len(idx.Batches) > 0 {
		idx.Entries = idx.Batches
	}
	if len(idx.Entries) > 0 {
		idx.Batches = idx.Entries
	}
	for i := range idx.Entries {
		idx.Entries[i].ID = canonicalizeBatchID(idx.Entries[i].ID, idx.Entries[i].Path)
	}
	idx.Batches = idx.Entries

	prev, err := os.ReadFile(indexPath)
	if err != nil && !os.IsNotExist(err) {
		return err
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

// ResolveBatch returns the index Batch whose ID equals the supplied
// public BatchId, or nil when no batch matches. The id is the public
// BatchId (== Batch folder basename) — see ADR-0032 §"Identity
// table". Per-row RunIDs are intentionally NOT in scope here; they
// are resolved through ResolveBatchFromRowID at the portal layer
// because they need an on-disk manifest fallback.
func (idx *Index) ResolveBatch(batchID string) *Batch {
	idx.syncSlices()
	for i := range idx.Entries {
		if idx.Entries[i].ID == batchID {
			return &idx.Entries[i]
		}
	}
	return nil
}

func (idx *Index) Resolve(batchID string) *Entry {
	return idx.ResolveBatch(batchID)
}

func (idx *Index) AddBatch(batch Batch) {
	idx.syncSlices()
	batch.ID = canonicalizeBatchID(batch.ID, batch.Path)
	for i, b := range idx.Entries {
		if b.ID == batch.ID {
			batch.CreatedAt = b.CreatedAt
			if b.Status == StatusArchived {
				batch.Status = StatusArchived
				batch.ArchivedAt = b.ArchivedAt
			} else {
				batch.Status = StatusActive
			}
			idx.Entries[i] = batch
			idx.Batches = idx.Entries
			return
		}
	}
	batch.Status = StatusActive
	idx.Entries = append(idx.Entries, batch)
	idx.Batches = idx.Entries
}

func (idx *Index) Add(entry Entry) {
	idx.AddBatch(entry)
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
	idx.syncSlices()
	for i := range idx.Entries {
		if idx.Entries[i].ID == id {
			idx.Entries[i].Status = StatusArchived
			idx.Entries[i].Path = archivePath
			idx.Entries[i].ArchivedAt = &archivedAt
			idx.Batches = idx.Entries
			return nil
		}
	}
	return fmt.Errorf("batch not found: %s", id)
}

// AddRun records a per-row RunRecord against the named entry. If a
// record with the same RunID already exists, it is replaced (so the
// caller can re-record an updated status without duplicating rows).
func (idx *Index) AddRun(entryID string, rec RunRecord) {
	idx.syncSlices()
	for i := range idx.Entries {
		if idx.Entries[i].ID != entryID {
			continue
		}
		for j := range idx.Entries[i].Runs {
			if idx.Entries[i].Runs[j].RunID == rec.RunID {
				idx.Entries[i].Runs[j] = rec
				return
			}
		}
		idx.Entries[i].Runs = append(idx.Entries[i].Runs, rec)
		idx.Batches = idx.Entries
		return
	}
}

// MarkRunArchived flips the named row's record to archived and sets
// its ArchivePath. It returns an error when the entry or the row does
// not exist so callers can fail loudly instead of silently losing the
// archive marker.
func (idx *Index) MarkRunArchived(entryID, runID, archivePath string) error {
	idx.syncSlices()
	for i := range idx.Entries {
		if idx.Entries[i].ID != entryID {
			continue
		}
		for j := range idx.Entries[i].Runs {
			if idx.Entries[i].Runs[j].RunID != runID {
				continue
			}
			idx.Entries[i].Runs[j].Status = RunRecordStatusArchived
			idx.Entries[i].Runs[j].ArchivePath = archivePath
			idx.Batches = idx.Entries
			return nil
		}
		return fmt.Errorf("run %q not found in entry %q", runID, entryID)
	}
	return fmt.Errorf("batch not found: %s", entryID)
}

// RunRecordFor returns the per-row RunRecord for the given entry id
// and run id, or nil when neither the entry nor the row is recorded.
func (idx *Index) RunRecordFor(entryID, runID string) *RunRecord {
	idx.syncSlices()
	for i := range idx.Entries {
		if idx.Entries[i].ID != entryID {
			continue
		}
		for j := range idx.Entries[i].Runs {
			if idx.Entries[i].Runs[j].RunID == runID {
				return &idx.Entries[i].Runs[j]
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
// ReconcileRuns is safe to call on entries that have no Runs records
// (legacy entries); it is a no-op for them. It does not flip the
// entry-level Status — that decision belongs to EnsureStatus/MarkUnavailable.
func (idx *Index) ReconcileRuns(repoRoot string) {
	idx.syncSlices()
	statFn := idx.StatFn
	if statFn == nil {
		statFn = os.Stat
	}
	for i := range idx.Entries {
		entry := &idx.Entries[i]
		for j := range entry.Runs {
			rec := &entry.Runs[j]
			if rec.ArchivePath == "" {
				continue
			}
			livePath := filepath.Join(repoRoot, ".sandman", "batches", entry.ID, "runs", rec.RunID)
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
	idx.Batches = idx.Entries
}

func (idx *Index) RemoveBatch(id string) error {
	idx.syncSlices()
	for i := range idx.Entries {
		if idx.Entries[i].ID == id {
			idx.Entries = append(idx.Entries[:i], idx.Entries[i+1:]...)
			idx.Batches = idx.Entries
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
		return RunManifest{}, fmt.Errorf("decode run manifest: %w", err)
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
		return ReviewState{}, fmt.Errorf("decode review state: %w", err)
	}
	return state, nil
}
