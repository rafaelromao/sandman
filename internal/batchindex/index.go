package batchindex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

var ErrNotFound = errors.New("batchindex: entry not found")

type Index struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
	path    string
	StatFn  func(path string) (os.FileInfo, error) `json:"-"`
}

type Entry struct {
	ID         string     `json:"id"`
	Path       string     `json:"path"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	Issues     []int      `json:"issues"`
	PR         *int       `json:"pr,omitempty"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
}

func Load(path string) (*Index, error) {
	idx := &Index{Version: 1, Entries: []Entry{}, path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, idx); err != nil {
		return nil, err
	}
	if idx.StatFn == nil {
		idx.StatFn = os.Stat
	}
	return idx, nil
}

func (idx *Index) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	prev, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	tmp := path + ".tmp"
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}

	if prev != nil {
		_ = os.WriteFile(path+".bak", prev, 0644)
	}
	return nil
}

func (idx *Index) Add(entry Entry) {
	entry.Status = "active"
	for i, e := range idx.Entries {
		if e.ID == entry.ID {
			entry.CreatedAt = e.CreatedAt
			idx.Entries[i] = entry
			return
		}
	}
	idx.Entries = append(idx.Entries, entry)
}

func (idx *Index) Resolve(id string) *Entry {
	for _, e := range idx.Entries {
		if e.ID == id {
			return &e
		}
	}
	return nil
}

func (idx *Index) EnsureStatus() error {
	if idx.StatFn == nil {
		idx.StatFn = os.Stat
	}
	for i := range idx.Entries {
		if idx.Entries[i].Status == "unavailable" {
			continue
		}
		_, err := idx.StatFn(idx.Entries[i].Path)
		if err != nil {
			if os.IsNotExist(err) {
				idx.Entries[i].Status = "unavailable"
			}
		}
	}
	return nil
}

func (idx *Index) ArchiveBatch(id string, archivedAt time.Time) error {
	for i := range idx.Entries {
		if idx.Entries[i].ID == id && idx.Entries[i].Status == "active" {
			idx.Entries[i].Status = "archived"
			idx.Entries[i].ArchivedAt = &archivedAt
			return idx.Save(idx.path)
		}
	}
	return ErrNotFound
}

func (idx *Index) RemoveBatch(id string) error {
	for i := range idx.Entries {
		if idx.Entries[i].ID == id {
			idx.Entries = append(idx.Entries[:i], idx.Entries[i+1:]...)
			return idx.Save(idx.path)
		}
	}
	return ErrNotFound
}
