// Package-internal helper tests for issue #1756's per-PR scoping.

package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSelfPostStore_RecordAndLookup_PerPR pins the contract that the
// SelfPostStore dedup key is (prNumber, sha256(body)) — the body
// hash alone is not sufficient. Recording a body on PR 1 must NOT
// mark the same body on PR 2 as self-posted. This is the regression
// pin for issue #1756's failure mode (cross-PR poisoning).
func TestSelfPostStore_RecordAndLookup_PerPR(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := store.Record(1, "body", ""); err != nil {
		t.Fatalf("Record(1, body): %v", err)
	}
	if !store.IsSelfPosted(1, "body") {
		t.Error("IsSelfPosted(1, body) should return true after Record(1, body)")
	}
	if store.IsSelfPosted(2, "body") {
		t.Error("IsSelfPosted(2, body) must return false; cross-PR scope is the new contract (#1756)")
	}
}

// TestSelfPostStore_Normalization_PerPR pins the post-#1756
// normalization contract: a body recorded on PR 1 normalizes the
// same way on PR 2, and each PR has its own independent normalized
// hash scope. A body that normalizes the same on multiple PRs
// remains scoped per-PR.
func TestSelfPostStore_Normalization_PerPR(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	body := "/sandman review"
	if err := store.Record(11, body, ""); err != nil {
		t.Fatalf("Record(11, body): %v", err)
	}
	if err := store.Record(22, body, ""); err != nil {
		t.Fatalf("Record(22, body): %v", err)
	}
	if !store.IsSelfPosted(11, "/sandman review   ") {
		t.Error("PR 11: trailing whitespace should be normalized on check")
	}
	if !store.IsSelfPosted(22, "/SANDMAN REVIEW") {
		t.Error("PR 22: case should be normalized on check")
	}
	if store.IsSelfPosted(33, body) {
		t.Error("PR 33: should report false; bodies cross scopes per PR (#1756)")
	}
	if store.IsSelfPosted(33, "/sandman review\n\n") {
		t.Error("PR 33: should report false for normalized variants; bodies cross scopes per PR (#1756)")
	}
}

// TestSelfPostStore_GreenfieldLoad_OldFileBackedUpAndIgnored pins
// the issue #1756 greenfield loader invariant: a pre-existing
// self-posted.json is renamed to self-posted.json.ignore-<ts>.bak
// at startup; the in-memory store starts empty; and the old SHA
// key shape is ignored.
//
// This sweep breaks any stale state from prior per-body-only
// deployments: a trigger hash recorded against PR A in the old
// single-key file is no longer present (it has been moved to the
// .bak file and the store starts empty), so the trigger can be
// posted on PR B without being silently dropped.
func TestSelfPostStore_GreenfieldLoad_OldFileBackedUpAndIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")

	body := "/sandman review"
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimRight(body, " \t\n"))))
	sha := hex.EncodeToString(sum[:])
	oldShape := map[string]selfPostEntry{
		sha: {
			Hash:     sha,
			PRNumber: 9999,
			RunID:    "stale",
			PostedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	data, err := json.MarshalIndent(oldShape, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile seed: %v", err)
	}

	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("original self-posted.json should be renamed away, stat err: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "self-posted.json.ignore-*.bak"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 .bak file, got %d (matches=%v)", len(matches), matches)
	}
	if got := len(store.Hashes()); got != 0 {
		t.Errorf("greenfield store should have 0 entries, got %d", got)
	}
	if store.IsSelfPosted(42, body) {
		t.Error("greenfield store MUST NOT report the pre-existing old-shape body as self-posted; that is the cross-PR poisoning failure (#1756)")
	}
}

// TestSelfPostStore_Record_WritesNewKeyFormat pins the post-#1756
// on-disk shape: the JSON object's keys are composite
// "pr-<N>-<sha>", and the value is the same selfPostEntry
// diagnostic metadata.
func TestSelfPostStore_Record_WritesNewKeyFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := store.Record(1, "x", "run-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	sum := sha256.Sum256([]byte("x"))
	sha := hex.EncodeToString(sum[:])
	wantKey := "pr-1-" + sha
	if _, ok := raw[wantKey]; !ok {
		t.Errorf("disk entry should be keyed by %q, got keys %v", wantKey, raw)
	}
	if _, ok := raw[sha]; ok {
		t.Errorf("disk file must NOT contain a legacy %q key (cross-PR poisoning risk)", sha)
	}
	if len(raw) != 1 {
		t.Errorf("expected exactly 1 key on disk, got %d (%v)", len(raw), raw)
	}
}

// TestSelfPostStore_NoCrossContamination_OnRealisticPoisonShape is
// the regression test for PR 1752's specific failure: the trigger
// hash recorded on PR 1745 silently drops the `/sandman review`
// trigger on PR 1752 because both PRs share the same body hash.
// Under the new (prNumber, sha256) key shape, the same body on a
// different PR is NOT self-posted and the trigger fires.
//
// This is the eponymous regression pin for issue #1756.
func TestSelfPostStore_NoCrossContamination_OnRealisticPoisonShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-posted.json")
	store, err := NewSelfPostStore(path)
	if err != nil {
		t.Fatalf("NewSelfPostStore: %v", err)
	}
	if err := store.Record(1745, "/sandman review", ""); err != nil {
		t.Fatalf("Record(1745, /sandman review): %v", err)
	}
	if !store.IsSelfPosted(1745, "/sandman review") {
		t.Error("PR 1745: recorded body should be self-posted on the recording PR")
	}
	if store.IsSelfPosted(1752, "/sandman review") {
		t.Error("PR 1752: the trigger recorded on PR 1745 MUST NOT drop the same body on PR 1752 (cross-PR poisoning, #1756, the PR 1752 failure)")
	}
	if store.IsSelfPosted(2000, "/sandman review") {
		t.Error("PR 2000: same body on a third PR must also be fresh (#1756)")
	}
}
