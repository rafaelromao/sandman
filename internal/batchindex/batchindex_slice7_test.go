package batchindex

import (
	"encoding/json"
	"testing"
	"time"
)

// TestIndex_BatchesIsCanonicalSlice pins the #1916 contract: the canonical slice
// on Index is Batches (typed []Batch). The legacy Entries []Entry
// field is gone — only Batches remains.
func TestIndex_BatchesIsCanonicalSlice(t *testing.T) {
	idx := &Index{Version: IndexVersion}
	idx.AddBatch(Batch{ID: "abc123", Path: "/p/abc123", Kind: KindIssue, Status: StatusActive})
	if len(idx.Batches) != 1 {
		t.Fatalf("Batches len = %d, want 1", len(idx.Batches))
	}
	if idx.Batches[0].ID != "abc123" {
		t.Errorf("Batches[0].ID = %q, want %q", idx.Batches[0].ID, "abc123")
	}
}

// TestIndex_JSONKeyRemainsEntries pins the #1916 contract: the JSON wire format on
// disk MUST keep the "entries" key even though the Go field is now
// Batches. Existing operator batches.json files use "entries" and
// must continue to round-trip.
func TestIndex_JSONKeyRemainsEntries(t *testing.T) {
	idx := Index{
		Version: IndexVersion,
		Batches: []Batch{{
			ID:        "abc123",
			Path:      ".sandman/batches/abc123",
			Kind:      KindIssue,
			Status:    StatusActive,
			CreatedAt: time.Now().Truncate(time.Second),
			Issues:    []int{1213},
		}},
	}

	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to raw map: %v", err)
	}

	entries, ok := raw["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("expected on-disk key \"entries\" with one row, got %T (%v)", raw["entries"], raw["entries"])
	}
	if _, present := raw["batches"]; present {
		t.Errorf("on-disk JSON must not introduce a \"batches\" key alongside the historical \"entries\" key")
	}
}

// TestIndex_LoadLegacyEntriesKey pins the #1916 contract: an existing
// batches.json with the legacy "entries" key decodes into the new
// Index struct (Batches is populated).
func TestIndex_LoadLegacyEntriesKey(t *testing.T) {
	legacy := []byte(`{"version":1,"entries":[{"id":"legacy-1","path":"/p/legacy-1","kind":"issue","status":"active","createdAt":"2024-01-02T03:04:05Z","issues":[1213]}]}`)
	var idx Index
	if err := json.Unmarshal(legacy, &idx); err != nil {
		t.Fatalf("Unmarshal legacy: %v", err)
	}
	if len(idx.Batches) != 1 {
		t.Fatalf("Batches len = %d, want 1 (legacy \"entries\" key must decode)", len(idx.Batches))
	}
	if idx.Batches[0].ID != "legacy-1" {
		t.Errorf("Batches[0].ID = %q, want %q", idx.Batches[0].ID, "legacy-1")
	}
}
