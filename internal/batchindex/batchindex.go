package batchindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
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

type Index struct {
	Version   int                                    `json:"version"`
	Entries   []Entry                                `json:"entries"`
	StatFn    func(path string) (os.FileInfo, error) `json:"-"`
	indexPath string
}

type Entry struct {
	ID         string     `json:"id"`
	Path       string     `json:"path"`
	Kind       Kind       `json:"kind"`
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	Issues     []int      `json:"issues,omitempty"`
	PR         int        `json:"pr,omitempty"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
}

// MarshalJSON writes the batches index with prompt-only entries carrying an
// explicit empty issues array, matching ADR-0032's index schema.
func (i Index) MarshalJSON() ([]byte, error) {
	type entryJSON struct {
		ID         string     `json:"id"`
		Path       string     `json:"path"`
		Kind       Kind       `json:"kind"`
		Status     Status     `json:"status"`
		CreatedAt  time.Time  `json:"createdAt"`
		Issues     []int      `json:"issues,omitempty"`
		PR         int        `json:"pr,omitempty"`
		ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	}

	entries := make([]any, 0, len(i.Entries))
	for _, e := range i.Entries {
		if e.Kind == KindPromptOnly {
			entries = append(entries, struct {
				ID         string     `json:"id"`
				Path       string     `json:"path"`
				Kind       Kind       `json:"kind"`
				Status     Status     `json:"status"`
				CreatedAt  time.Time  `json:"createdAt"`
				Issues     []int      `json:"issues"`
				PR         int        `json:"pr,omitempty"`
				ArchivedAt *time.Time `json:"archivedAt,omitempty"`
			}{
				ID:         e.ID,
				Path:       e.Path,
				Kind:       e.Kind,
				Status:     e.Status,
				CreatedAt:  e.CreatedAt,
				Issues:     []int{},
				PR:         e.PR,
				ArchivedAt: e.ArchivedAt,
			})
			continue
		}
		entries = append(entries, entryJSON{
			ID:         e.ID,
			Path:       e.Path,
			Kind:       e.Kind,
			Status:     e.Status,
			CreatedAt:  e.CreatedAt,
			Issues:     e.Issues,
			PR:         e.PR,
			ArchivedAt: e.ArchivedAt,
		})
	}

	return json.Marshal(struct {
		Version int   `json:"version"`
		Entries []any `json:"entries"`
	}{
		Version: i.Version,
		Entries: entries,
	})
}

type RunManifest struct {
	RunID        string    `json:"runID"`
	BatchID      string    `json:"batchId"`
	Issue        int       `json:"issue,omitempty"`
	Branch       string    `json:"branch"`
	BaseBranch   string    `json:"baseBranch"`
	WorktreePath string    `json:"worktreePath"`
	Kind         Kind      `json:"kind"`
	CreatedAt    time.Time `json:"createdAt"`
	PR           int       `json:"pr,omitempty"`
	Status       Status    `json:"status"`
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
		return &Index{Version: IndexVersion, Entries: nil, StatFn: os.Stat, indexPath: path}, nil
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
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if e.Status == StatusActive || e.Status == StatusArchived {
			if _, err := statFn(e.Path); err != nil {
				if os.IsNotExist(err) {
					e.Status = StatusUnavailable
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
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}

	dir := filepath.Dir(indexPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	prev, err := os.ReadFile(indexPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	tmpFile, err := os.CreateTemp(dir, filepath.Base(indexPath)+".tmp.")
	if err != nil {
		return fmt.Errorf("create index tmp: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write index tmp: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close index tmp: %w", err)
	}

	if err := os.Rename(tmpPath, indexPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename index tmp: %w", err)
	}

	if prev != nil {
		bakPath := indexPath + ".bak"
		if err := os.WriteFile(bakPath, prev, 0644); err != nil {
			return fmt.Errorf("write index bak: %w", err)
		}
	}

	return nil
}

func (idx *Index) Resolve(id string) *Entry {
	for i := range idx.Entries {
		if idx.Entries[i].ID == id {
			return &idx.Entries[i]
		}
	}
	return nil
}

func (idx *Index) Add(entry Entry) {
	for i, e := range idx.Entries {
		if e.ID == entry.ID {
			entry.CreatedAt = e.CreatedAt
			if e.Status == StatusArchived {
				entry.Status = StatusArchived
				entry.ArchivedAt = e.ArchivedAt
			} else {
				entry.Status = StatusActive
			}
			idx.Entries[i] = entry
			return
		}
	}
	entry.Status = StatusActive
	idx.Entries = append(idx.Entries, entry)
}

func (idx *Index) ArchiveBatch(id string, archivedAt time.Time) error {
	for i := range idx.Entries {
		if idx.Entries[i].ID == id {
			idx.Entries[i].Status = StatusArchived
			idx.Entries[i].ArchivedAt = &archivedAt
			return idx.Save(idx.indexPath)
		}
	}
	return fmt.Errorf("batch not found: %s", id)
}

func (idx *Index) SetArchived(id, archivePath string, archivedAt time.Time) error {
	for i := range idx.Entries {
		if idx.Entries[i].ID == id {
			idx.Entries[i].Status = StatusArchived
			idx.Entries[i].Path = archivePath
			idx.Entries[i].ArchivedAt = &archivedAt
			return nil
		}
	}
	return fmt.Errorf("batch not found: %s", id)
}

func (idx *Index) RemoveBatch(id string) error {
	for i := range idx.Entries {
		if idx.Entries[i].ID == id {
			idx.Entries = append(idx.Entries[:i], idx.Entries[i+1:]...)
			return idx.Save(idx.indexPath)
		}
	}
	return fmt.Errorf("batch not found: %s", id)
}

func WriteManifest(runDir string, manifest RunManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run manifest: %w", err)
	}
	runManifestPath := filepath.Join(runDir, "run.json")
	if err := os.WriteFile(runManifestPath, data, 0644); err != nil {
		return fmt.Errorf("write run manifest: %w", err)
	}
	return nil
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
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal review state: %w", err)
	}
	statePath := filepath.Join(runDir, "review-state.json")
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		return fmt.Errorf("write review state: %w", err)
	}
	return nil
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
