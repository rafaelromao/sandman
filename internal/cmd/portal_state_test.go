package cmd

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPortalStatePersistsCommandFormCollapsed(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal state helper test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	portalStatePath := filepath.Join(filepath.Dir(currentFile), "portal_state.js")

	script := `const fs = require('fs');
const vm = require('vm');
const helperPath = process.argv[1];
const source = fs.readFileSync(helperPath, 'utf8');
const storage = new Map();
const sandbox = {
  window: {},
  globalThis: {},
  sessionStorage: {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  },
  Set, Map, JSON, console,
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: helperPath });
const api = sandbox.SandmanPortalState;

// Test 1: defaultState includes commandFormCollapsed: false
const defaults = api.load();
if (defaults.commandFormCollapsed !== false) {
  throw new Error('expected default commandFormCollapsed to be false, got ' + JSON.stringify(defaults.commandFormCollapsed));
}

// Test 2: save and load commandFormCollapsed: true
storage.set(api.storageKey, JSON.stringify({
  expandedRunKey: null,
  tabs: {},
  commandFormCollapsed: true,
}));
const loadedTrue = api.load();
if (loadedTrue.commandFormCollapsed !== true) {
  throw new Error('expected commandFormCollapsed true to survive load, got ' + JSON.stringify(loadedTrue.commandFormCollapsed));
}

// Test 3: save and load commandFormCollapsed: false
storage.set(api.storageKey, JSON.stringify({
  expandedRunKey: null,
  tabs: {},
  commandFormCollapsed: false,
}));
const loadedFalse = api.load();
if (loadedFalse.commandFormCollapsed !== false) {
  throw new Error('expected commandFormCollapsed false to survive load, got ' + JSON.stringify(loadedFalse.commandFormCollapsed));
}

// Test 4: missing field defaults to false (migration from old state)
storage.set(api.storageKey, JSON.stringify({
  expandedRunKey: null,
  tabs: {},
}));
const loadedMissing = api.load();
if (loadedMissing.commandFormCollapsed !== false) {
  throw new Error('expected missing commandFormCollapsed to default to false, got ' + JSON.stringify(loadedMissing.commandFormCollapsed));
}

// Test 5: save includes commandFormCollapsed
api.save({ expandedRunKey: null, tabs: {}, commandFormCollapsed: true });
const persisted = JSON.parse(storage.get(api.storageKey));
if (persisted.commandFormCollapsed !== true) {
  throw new Error('expected saved state to include commandFormCollapsed, got ' + JSON.stringify(persisted));
}
`
	cmd := exec.Command("node", "-e", script, portalStatePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal state helper failed: %v\n%s", err, out)
	}
}

func TestPortalStateNormalizesMissingRunAndTab(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal state helper test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	portalStatePath := filepath.Join(filepath.Dir(currentFile), "portal_state.js")

	script := `const fs = require('fs');
const vm = require('vm');
const helperPath = process.argv[1];
const source = fs.readFileSync(helperPath, 'utf8');
const storage = new Map();
const sandbox = {
  window: {},
  globalThis: {},
  sessionStorage: {
    getItem(key) {
      return storage.has(key) ? storage.get(key) : null;
    },
    setItem(key, value) {
      storage.set(key, String(value));
    },
    removeItem(key) {
      storage.delete(key);
    },
  },
  Set,
  Map,
  JSON,
  console,
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: helperPath });
const api = sandbox.SandmanPortalState;
if (!api) throw new Error('missing SandmanPortalState');
storage.set(api.storageKey, JSON.stringify({
  expandedRunKey: 'gone',
  tabs: { gone: 'events', keep: 'bogus' },
}));
const loaded = api.load();
const normalized = api.normalize(loaded, [{ key: 'keep' }], [{ key: 'keep' }]);
api.save(normalized);
const persisted = JSON.parse(storage.get(api.storageKey));
if (persisted.expandedRunKey !== null) throw new Error('expected missing expanded run to become null, got ' + JSON.stringify(persisted));
if (persisted.tabs.gone !== 'events') throw new Error('expected saved run tab to survive round trip, got ' + JSON.stringify(persisted));
if (persisted.tabs.keep !== 'log') throw new Error('expected visible run tab to normalize to log, got ' + JSON.stringify(persisted));
if (api.getSelectedTab(persisted, 'keep') !== 'log') throw new Error('expected invalid tab to fall back to log, got ' + JSON.stringify(persisted));
`
	cmd := exec.Command("node", "-e", script, portalStatePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal state helper failed: %v\n%s", err, out)
	}
}

func TestPortalStatePersistsShowArchived(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal state helper test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	portalStatePath := filepath.Join(filepath.Dir(currentFile), "portal_state.js")

	script := `const fs = require('fs');
const vm = require('vm');
const helperPath = process.argv[1];
const source = fs.readFileSync(helperPath, 'utf8');
const storage = new Map();
const sandbox = {
  window: {},
  globalThis: {},
  sessionStorage: {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  },
  Set, Map, JSON, console,
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: helperPath });
const api = sandbox.SandmanPortalState;

const defaults = api.load();
if (defaults.showArchived !== false) {
  throw new Error('expected default showArchived to be false, got ' + JSON.stringify(defaults.showArchived));
}

storage.set(api.storageKey, JSON.stringify({
  expandedRunKey: null,
  tabs: {},
  commandFormCollapsed: false,
  showArchived: true,
}));
const loadedTrue = api.load();
if (loadedTrue.showArchived !== true) {
  throw new Error('expected showArchived true to survive load, got ' + JSON.stringify(loadedTrue.showArchived));
}

storage.set(api.storageKey, JSON.stringify({
  expandedRunKey: null,
  tabs: {},
  commandFormCollapsed: false,
  showArchived: false,
}));
const loadedFalse = api.load();
if (loadedFalse.showArchived !== false) {
  throw new Error('expected showArchived false to survive load, got ' + JSON.stringify(loadedFalse.showArchived));
}

storage.set(api.storageKey, JSON.stringify({
  expandedRunKey: null,
  tabs: {},
  commandFormCollapsed: false,
}));
const loadedMissing = api.load();
if (loadedMissing.showArchived !== false) {
  throw new Error('expected missing showArchived to default to false, got ' + JSON.stringify(loadedMissing.showArchived));
}

api.save({
  expandedRunKey: null,
  tabs: {},
  commandFormCollapsed: false,
  showArchived: true,
});
const persisted = JSON.parse(storage.get(api.storageKey));
if (persisted.showArchived !== true) {
  throw new Error('expected saved state to include showArchived, got ' + JSON.stringify(persisted));
}

storage.delete(api.storageKey);
api.save({ expandedRunKey: null, tabs: {}, commandFormCollapsed: false, showArchived: true });
const afterRefresh = api.load();
if (afterRefresh.showArchived !== true) {
  throw new Error('expected showArchived true to survive page refresh, got ' + JSON.stringify(afterRefresh.showArchived));
}
`
	cmd := exec.Command("node", "-e", script, portalStatePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal state helper failed: %v\n%s", err, out)
	}
}

func TestPortalStatePersistsActiveBatches(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal state helper test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	portalStatePath := filepath.Join(filepath.Dir(currentFile), "portal_state.js")

	script := `const fs = require('fs');
const vm = require('vm');
const helperPath = process.argv[1];
const source = fs.readFileSync(helperPath, 'utf8');
const storage = new Map();
const sandbox = {
  window: {},
  globalThis: {},
  sessionStorage: {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  },
  Set, Map, JSON, console,
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: helperPath });
const api = sandbox.SandmanPortalState;

const defaults = api.load();
if (defaults.activeBatches !== false) {
  throw new Error('expected default activeBatches to be false, got ' + JSON.stringify(defaults.activeBatches));
}

storage.set(api.storageKey, JSON.stringify({
  expandedRunKey: null,
  tabs: {},
  commandFormCollapsed: false,
  activeBatches: true,
}));
const loadedTrue = api.load();
if (loadedTrue.activeBatches !== true) {
  throw new Error('expected activeBatches true to survive load, got ' + JSON.stringify(loadedTrue.activeBatches));
}

api.save({ expandedRunKey: null, tabs: {}, commandFormCollapsed: false, activeBatches: true });
const persisted = JSON.parse(storage.get(api.storageKey));
if (persisted.activeBatches !== true) {
  throw new Error('expected saved state to include activeBatches, got ' + JSON.stringify(persisted));
}
`
	cmd := exec.Command("node", "-e", script, portalStatePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal state helper failed: %v\n%s", err, out)
	}
}

func TestPortalStatePersistsSortPreference(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal state helper test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	portalStatePath := filepath.Join(filepath.Dir(currentFile), "portal_state.js")

	script := `const fs = require('fs');
const vm = require('vm');
const helperPath = process.argv[1];
const source = fs.readFileSync(helperPath, 'utf8');
const storage = new Map();
const sandbox = {
  window: {},
  globalThis: {},
  sessionStorage: {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  },
  Set, Map, JSON, console,
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: helperPath });
const api = sandbox.SandmanPortalState;

const defaults = api.load();
if (defaults.sortBy !== 'started' || defaults.sortDir !== 'desc') {
  throw new Error('expected default sort state {started,desc}, got ' + JSON.stringify({ sortBy: defaults.sortBy, sortDir: defaults.sortDir }));
}

storage.set(api.storageKey, JSON.stringify({
  expandedRunKey: null,
  tabs: {},
  commandFormCollapsed: false,
  sortBy: 'status',
  sortDir: 'asc',
}));
const loaded = api.load();
if (loaded.sortBy !== 'status' || loaded.sortDir !== 'asc') {
  throw new Error('expected persisted sort state {status,asc}, got ' + JSON.stringify({ sortBy: loaded.sortBy, sortDir: loaded.sortDir }));
}

storage.set(api.storageKey, JSON.stringify({ sortBy: 'bogus', sortDir: 'bogus' }));
const migrated = api.load();
if (migrated.sortBy !== 'started' || migrated.sortDir !== 'desc') {
  throw new Error('expected invalid sort state to normalize to {started,desc}, got ' + JSON.stringify({ sortBy: migrated.sortBy, sortDir: migrated.sortDir }));
}

api.save({ expandedRunKey: null, tabs: {}, commandFormCollapsed: false, sortBy: 'duration', sortDir: 'asc' });
const persisted = JSON.parse(storage.get(api.storageKey));
if (persisted.sortBy !== 'duration' || persisted.sortDir !== 'asc') {
  throw new Error('expected save() to persist sort fields, got ' + JSON.stringify(persisted));
}
`
	cmd := exec.Command("node", "-e", script, portalStatePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal state helper failed: %v\n%s", err, out)
	}
}

func TestPortalStateNormalizesExpandedKeyAgainstRawRuns(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal state helper test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	portalStatePath := filepath.Join(filepath.Dir(currentFile), "portal_state.js")

	script := `const fs = require('fs');
const vm = require('vm');
const helperPath = process.argv[1];
const source = fs.readFileSync(helperPath, 'utf8');
const storage = new Map();
const sandbox = {
  window: {},
  globalThis: {},
  sessionStorage: {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(String(key), String(value)); },
    removeItem(key) { storage.delete(key); },
  },
  Set, Map, JSON, console,
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: helperPath });
const api = sandbox.SandmanPortalState;

const rawRuns = [
  { key: 'abc1-issue-42', runId: 'abc1-issue-42', issueNumber: 42 },
  { key: 'abc2-PR42', runId: 'abc2-PR42', issueNumber: 42, review: true },
];

storage.set(api.storageKey, JSON.stringify({ expandedRunKey: 'issue-42', tabs: {} }));
const loaded1 = api.load();
const norm1 = api.normalize(loaded1, rawRuns, rawRuns);
if (norm1.state.expandedRunKey !== null) {
  throw new Error('expected synthetic key issue-42 to be nulled, got ' + JSON.stringify(norm1.state.expandedRunKey));
}

storage.set(api.storageKey, JSON.stringify({ expandedRunKey: 'review-stub-42', tabs: {} }));
const loaded2 = api.load();
const norm2 = api.normalize(loaded2, rawRuns, rawRuns);
if (norm2.state.expandedRunKey !== null) {
  throw new Error('expected synthetic key review-stub-42 to be nulled, got ' + JSON.stringify(norm2.state.expandedRunKey));
}

storage.set(api.storageKey, JSON.stringify({ expandedRunKey: 'abc1-issue-42', tabs: {} }));
const loaded3 = api.load();
const norm3 = api.normalize(loaded3, rawRuns, rawRuns);
if (norm3.state.expandedRunKey !== 'abc1-issue-42') {
  throw new Error('expected real RunID abc1-issue-42 to survive normalization, got ' + JSON.stringify(norm3.state.expandedRunKey));
}

storage.set(api.storageKey, JSON.stringify({ expandedRunKey: 'abc2-PR42', tabs: {} }));
const loaded4 = api.load();
const norm4 = api.normalize(loaded4, rawRuns, rawRuns);
if (norm4.state.expandedRunKey !== 'abc2-PR42') {
  throw new Error('expected real review RunID abc2-PR42 to survive normalization, got ' + JSON.stringify(norm4.state.expandedRunKey));
}

console.log('PASS');
`
	cmd := exec.Command("node", "-e", script, portalStatePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal state helper failed: %v\n%s", err, out)
	}
}

func TestPortalStateKeepsExpandedHiddenSiblingSubject(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal state helper test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	portalStatePath := filepath.Join(filepath.Dir(currentFile), "portal_state.js")

	script := `const fs = require('fs');
const vm = require('vm');
const helperPath = process.argv[1];
const source = fs.readFileSync(helperPath, 'utf8');
const storage = new Map();
const sandbox = {
  window: {},
  globalThis: {},
  sessionStorage: {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(String(key), String(value)); },
    removeItem(key) { storage.delete(key); },
  },
  Set, Map, JSON, console,
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: helperPath });
const api = sandbox.SandmanPortalState;

const visibleRuns = [
  { key: 'abc1-issue-42', runId: 'abc1-issue-42', issueNumber: 42 },
];
const allRuns = [
  { key: 'abc1-issue-42', runId: 'abc1-issue-42', issueNumber: 42 },
  { key: 'abc2-PR42', runId: 'abc2-PR42', issueNumber: 42, review: true },
];

storage.set(api.storageKey, JSON.stringify({ expandedRunKey: 'abc2-PR42', tabs: {} }));
const loaded = api.load();
const normalized = api.normalize(loaded, visibleRuns, allRuns);
if (normalized.state.expandedRunKey !== 'abc2-PR42') {
  throw new Error('expected hidden sibling subject to stay expanded, got ' + JSON.stringify(normalized.state.expandedRunKey));
}

console.log('PASS');
`
	cmd := exec.Command("node", "-e", script, portalStatePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal state helper failed: %v\n%s", err, out)
	}
}
