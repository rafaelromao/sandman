package batchindex

import (
	"encoding/json"
	"errors"
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
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
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
	Status       string    `json:"status"`
}

type SeenComment struct {
	CommentID string    `json:"commentID"`
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

func Load(repoRoot string) (*Index, error) {
	indexPath := filepath.Join(repoRoot, ".sandman", "batches.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &Index{Version: IndexVersion, Entries: nil}, nil
		}
		return nil, fmt.Errorf("read batches index: %w", err)
	}

	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("decode batches index: %w", err)
	}

	if idx.Version != IndexVersion {
		return nil, fmt.Errorf("unsupported batches index version %d", idx.Version)
	}

	if err := idx.probeStatus(); err != nil {
		return nil, err
	}

	return &idx, nil
}

func (idx *Index) probeStatus() error {
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if e.Status == StatusActive || e.Status == StatusArchived {
			if _, err := os.Stat(e.Path); err != nil {
				if os.IsNotExist(err) {
					e.Status = StatusUnavailable
				}
			}
		}
	}
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

	tmpPath := indexPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write index tmp: %w", err)
	}

	if err := os.Rename(tmpPath, indexPath); err != nil {
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

func (idx *Index) Resolve(id string) (*Entry, bool) {
	for i := range idx.Entries {
		if idx.Entries[i].ID == id {
			return &idx.Entries[i], true
		}
	}
	return nil, false
}

var ErrNotFound = errors.New("entry not found")

func (idx *Index) AddEntry(entry Entry) {
	for i, e := range idx.Entries {
		if e.ID == entry.ID {
			idx.Entries[i] = entry
			return
		}
	}
	idx.Entries = append(idx.Entries, entry)
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
