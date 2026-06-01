package cmd

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

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
  localStorage: {
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
  tabs: { gone: 'events', keep: 'bogus', orphan: 'details' },
}));
const loaded = api.load();
const normalized = api.normalize(loaded, [{ key: 'keep' }]);
api.save(normalized);
const persisted = JSON.parse(storage.get(api.storageKey));
if (persisted.expandedRunKey !== null) throw new Error('expected missing run to clear, got ' + JSON.stringify(persisted));
if (persisted.tabs.keep !== 'log') throw new Error('expected invalid tab to fall back to log, got ' + JSON.stringify(persisted));
if (Object.prototype.hasOwnProperty.call(persisted.tabs, 'gone')) throw new Error('expected missing run tab to be removed, got ' + JSON.stringify(persisted));
if (Object.prototype.hasOwnProperty.call(persisted.tabs, 'orphan')) throw new Error('expected orphan tab entry to be removed, got ' + JSON.stringify(persisted));
`
	cmd := exec.Command("node", "-e", script, portalStatePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal state helper failed: %v\n%s", err, out)
	}
}
