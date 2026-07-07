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
	Batches   []Batch                                `json:"entries"`
	StatFn    func(path string) (os.FileInfo, error) `json:"-"`
	indexPath string
}

// Batch is one row of the batches index. Each Batch corresponds to a
// folder under .sandman/batches/<id>/; its ID is the public BatchId —
// the batch folder basename — used by portal/archive/clean endpoints
// to address the row. Per-row identities are scoped inside a Batch
// under runs/<runID>/ and are NOT represented in the index.
type Batch struct {
	ID         string     `json:"id"`
	Path       string     `json:"path"`
	Kind       Kind       `json:"kind"`
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	Issues     []int      `json:"issues,omitempty"`
	PR         int        `json:"pr,omitempty"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
}

// MarshalJSON writes the batches index with prompt-only batches carrying an
// explicit empty issues array, matching ADR-0032's index schema.
func (i Index) MarshalJSON() ([]byte, error) {
	type batchJSON struct {
		ID         string     `json:"id"`
		Path       string     `json:"path"`
		Kind       Kind       `json:"kind"`
		Status     Status     `json:"status"`
		CreatedAt  time.Time  `json:"createdAt"`
		Issues     []int      `json:"issues"`
		PR         int        `json:"pr,omitempty"`
		ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	}

	batches := make([]any, 0, len(i.Batches))
	for _, b := range i.Batches {
		issues := b.Issues
		if b.Kind == KindPromptOnly || issues == nil {
			issues = []int{}
		}
		batches = append(batches, batchJSON{
			ID:         b.ID,
			Path:       b.Path,
			Kind:       b.Kind,
			Status:     b.Status,
			CreatedAt:  b.CreatedAt,
			Issues:     issues,
			PR:         b.PR,
			ArchivedAt: b.ArchivedAt,
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

func (idx *Index) Save(indexPath string) error {
	for i := range idx.Batches {
		idx.Batches[i].ID = canonicalizeBatchID(idx.Batches[i].ID, idx.Batches[i].Path)
	}

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
	for i := range idx.Batches {
		if idx.Batches[i].ID == batchID {
			return &idx.Batches[i]
		}
	}
	return nil
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

func (idx *Index) RemoveBatch(id string) error {
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
