package cmd

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runReproNodeScript(t *testing.T, js string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping JS-only repro test")
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	dir := filepath.Dir(currentFile)
	statePath := filepath.Join(dir, "portal_state.js")
	diffPath := filepath.Join(dir, "portal_diff.js")
	htmlPath := filepath.Join(dir, "portal.html")
	prefix := reproSharedMocks() + reproSandboxSetup(statePath, diffPath, htmlPath)
	cmd := exec.Command("node", "-e", prefix+js)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Logf("script output: %s", out)
	}
}

func reproSharedMocks() string {
	return `const escapeHTML = (v) => String(v == null ? '' : v)
  .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
const formatTime = (v) => v ? String(v) : '—';
const formatDuration = (v) => v && String(v).trim() ? String(v) : '—';
const formatBranch = (run) => run.branch && String(run.branch).trim() ? run.branch : '—';
const formatIssueTitle = (run) => run.issueTitle || '—';
const formatSource = (run) => {
  if (run.kind === 'active' && run.socketPath) return run.socketPath;
  if (run.logPath) return run.logPath;
  return '—';
};
const statusClass = (run) => {
  const s = String(run.status || '').toLowerCase();
  if (s === 'running') return 'running';
  if (s === 'active') return 'active';
  if (s === 'success') return 'success';
  if (s === 'failure' || s === 'failed' || s === 'error') return 'failure';
  if (s === 'warning' || s === 'stale' || s === 'blocked') return 'warning';
  if (s === 'aborted') return 'aborted';
  return s || 'default';
};
const renderStatusBadge = (run) => {
  const k = statusClass(run);
  const label = run.status || (run.kind === 'active' ? 'running' : 'completed');
  return '<span class="badge ' + escapeHTML(k) + '"><span class="dot"></span>' + escapeHTML(label) + '</span>';
};
const renderRunMeta = (run) => {
  const lines = [];
  const counterParts = [];
  if (run.batchKey) {
    lines.push('Batch: ' + run.batchKey);
  }
  if (Number(run.retriesDone || 0) > 0) {
    const count = Number(run.retriesDone || 0);
    counterParts.push(count + ' retr' + (count === 1 ? 'y' : 'ies'));
  }
  if (Number(run.reviewCount || 0) > 0) {
    const count = Number(run.reviewCount || 0);
    const reviewPart = count + ' review' + (count === 1 ? '' : 's');
    counterParts.push(reviewPart + ' - ' + (run.reviewVerdict || 'Unclear'));
  }
  if (run.runId && String(run.status || '').toLowerCase() !== 'queued' && String(run.status || '').toLowerCase() !== 'blocked') lines.push('Run: ' + run.runId);
  if (counterParts.length) lines.push(counterParts.join(' - '));
  return lines.length ? lines.join('\n') : 'Run';
};
const renderTerminalContent = (text) => {
  const value = String(text || '');
  if (!value) return '';
  return value.split('\n').map((line) => '<span>' + escapeHTML(line) + '</span>').join('\n');
};
const isRunAbortable = (run, abortReservations) => {
  if (!run || run.kind !== 'active') return false;
  if (run.status !== 'running' && run.status !== 'queued' && run.status !== 'blocked') return false;
  if (!run.batchKey) return false;
  const reservationKey = run.key + ':' + String(run.issueNumber != null ? run.issueNumber : '');
  if (abortReservations && abortReservations.has && abortReservations.has(reservationKey)) return false;
  return true;
};
const isRunArchivable = (run) => {
  if (!run || run.kind !== 'completed') return false;
  if (run.archived) return false;
  if (run.unavailable) return false;
  if (run.sourceExists === false) return false;
  if (!run.runId) return false;
  return true;
};
const helpers = {
  escapeHTML, formatTime, formatDuration, formatBranch, formatIssueTitle, formatSource,
  statusClass, renderStatusBadge, renderRunMeta, renderTerminalContent, isRunAbortable, isRunArchivable,
};
`
}

func reproSandboxSetup(statePath, diffPath, htmlPath string) string {
	return `const fs = require('fs');
const vm = require('vm');

// Load portal_state.js — pure state logic, no DOM needed
const stateSource = fs.readFileSync('` + statePath + `', 'utf8');
const stateSandbox = { globalThis: {}, console };
stateSandbox.globalThis = stateSandbox;
vm.runInNewContext(stateSource, stateSandbox, { filename: '` + statePath + `' });
const SandmanPortalState = stateSandbox.SandmanPortalState;
if (!SandmanPortalState) throw new Error('SandmanPortalState missing');

// Load portal_diff.js — provides logPaneCache and DOM helper logic
const diffSource = fs.readFileSync('` + diffPath + `', 'utf8');
const diffSandbox = {
  window: {}, globalThis: {}, console,
  Set, Map, WeakMap, WeakSet,
  JSON,
  setTimeout: setTimeout,
  requestAnimationFrame: function(cb) { return setTimeout(function() { cb(performance.now()); }, 0); },
  cancelAnimationFrame: function(id) { clearTimeout(id); },
  requestIdleCallback: function(cb) { return setTimeout(function() { cb({ didTimeout: false, timeRemaining: function() { return 50; } }); }, 0); },
  document: {
    createElement: function(tag) {
      return {
        tagName: tag.toUpperCase(),
        nodeType: tag === 'script' ? 1 : 1,
        children: [],
        childNodes: [],
        dataset: {},
        style: {},
        classList: { _set: new Set(), add(...cs) { for (const c of cs) this._set.add(c); }, remove(...cs) { for (const c of cs) this._set.delete(c); }, contains(c) { return this._set.has(c); }, toggle(c) { if (this._set.has(c)) this._set.delete(c); else this._set.add(c); } },
        getAttribute: function(n) { return this['_' + n] != null ? String(this['_' + n]) : null; },
        setAttribute: function(n, v) { this['_' + n] = v; this[n] = v; },
        removeAttribute: function(n) { delete this['_' + n]; delete this[n]; },
        appendChild: function(c) { c.parentNode = this; this.children.push(c); this.childNodes.push(c); return c; },
        insertBefore: function(c, ref) { c.parentNode = this; const idx = ref ? this.children.indexOf(ref) : -1; if (idx < 0) this.children.push(c); else this.children.splice(idx, 0, c); this.childNodes = this.children.slice(); return c; },
        removeChild: function(c) { const idx = this.children.indexOf(c); if (idx >= 0) this.children.splice(idx, 1); this.childNodes = this.children.slice(); c.parentNode = null; return c; },
        replaceChildren: function(...cs) { for (const c of this.children.slice()) { c.parentNode = null; } this.children = []; this.childNodes = []; for (const c of cs) this.appendChild(c); },
        querySelector: function() { return null; },
        querySelectorAll: function() { return []; },
        getElementsByTagName: function() { return []; },
        addEventListener: function() {},
        removeEventListener: function() {},
        className: '',
        innerHTML: '', innerText: '', textContent: '',
        scrollTop: 0, scrollLeft: 0,
        offsetWidth: 0, offsetHeight: 0,
        clientWidth: 0, clientHeight: 0,
        getBoundingClientRect: function() { return { left: 0, top: 0, width: 0, height: 0 }; },
        contains: function() { return false; },
        focus: function() {},
        blur: function() {},
        click: function() {},
        submit: function() {},
        reset: function() {},
      };
    },
    createTextNode: function(t) { return { nodeType: 3, textContent: t, data: t }; },
    createDocumentFragment: function() { return { nodeType: 11, children: [], childNodes: [], appendChild(c) { c.parentNode = this; this.children.push(c); this.childNodes.push(c); return c; }, replaceChildren(...cs) { this.children = cs; this.childNodes = cs.slice(); for (const c of cs) c.parentNode = this; } }; },
    getElementById: function() { return null; },
    querySelector: function() { return null; },
    querySelectorAll: function() { return []; },
  },
  HTMLElement: function() {},
  EventTarget: function() {
    return {
      addEventListener: function() {}, removeEventListener: function() {},
      dispatchEvent: function() { return true; },
    };
  },
  Node: {
    TEXT_NODE: 3, ELEMENT_NODE: 1, DOCUMENT_FRAGMENT_NODE: 11, COMMENT_NODE: 8,
    textContent: null, firstChild: null, lastChild: null, parentNode: null, childNodes: [],
  },
  performance: { now: function() { return 0; } },
};
diffSandbox.window = diffSandbox;
diffSandbox.globalThis = diffSandbox;

// Wire production renderRunMeta from portal.html into helpers
const htmlSrc = fs.readFileSync('` + htmlPath + `', 'utf8');
const renderRunMetaMatch = htmlSrc.match(/function renderRunMeta\(run\) \{[\s\S]*?^\s{4}\}/m);
if (renderRunMetaMatch) {
  const fnCtx = vm.createContext(Object.assign({}, diffSandbox, { sandbox: diffSandbox }));
  vm.runInContext(renderRunMetaMatch[0] + '\n; sandbox.renderRunMeta = renderRunMeta;', fnCtx, { filename: '` + htmlPath + `' });
  if (typeof diffSandbox.renderRunMeta === 'function') {
    helpers.renderRunMeta = diffSandbox.renderRunMeta;
  }
}

vm.runInNewContext(diffSource, diffSandbox, { filename: '` + diffPath + `' });
const SandmanPortalDiff = diffSandbox.SandmanPortalDiff;
`
}

func TestReproNormalize_DiscardsQueuedExpandedState(t *testing.T) {
	js := `
const state = { expandedRunKey: 'r1', tabs: { 'r1': 'log' }, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [{ key: 'r1', runId: 'r1', kind: 'active', status: 'queued' }];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.expandedRunKey !== null) throw new Error('expected expandedRunKey to be null, got ' + JSON.stringify(result.state.expandedRunKey));
if (!result.changed) throw new Error('expected changed=true, got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_DiscardsExpandedStateWhenRunBecomesInvisible(t *testing.T) {
	js := `
const state = { expandedRunKey: 'r1', tabs: {}, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [];
const allRuns = [{ key: 'r1', runId: 'r1', kind: 'active', status: 'running' }];
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.expandedRunKey !== null) throw new Error('expected expandedRunKey to be null for invisible run, got ' + JSON.stringify(result.state.expandedRunKey));
if (!result.changed) throw new Error('expected changed=true when run becomes invisible, got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_MigratesLegacyTabKeyToSubject(t *testing.T) {
	js := `
const state = { expandedRunKey: null, tabs: { 'legacy-key': 'log' }, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [{ key: 'legacy-key', runId: 'canonical-key', kind: 'active', status: 'running' }];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.tabs['canonical-key'] !== 'log') throw new Error('expected tab migrated to canonical key, got ' + JSON.stringify(result.state.tabs));
if (!result.changed) throw new Error('expected changed=true after legacy migration, got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_AddsDefaultTabForUnknownRunKeys(t *testing.T) {
	js := `
const state = { expandedRunKey: null, tabs: {}, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [{ key: 'r1', runId: 'r1', kind: 'active', status: 'running' }];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.tabs['r1'] !== 'log') throw new Error('expected default tab for r1, got ' + JSON.stringify(result.state.tabs));
if (!result.changed) throw new Error('expected changed=true when adding default tab, got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_BatchAndRunLines(t *testing.T) {
	js := `
const run = { batchKey: 'b1', runId: 'r1', status: 'running' };
const result = helpers.renderRunMeta(run);
if (result.indexOf('Batch: b1') === -1) throw new Error('expected Batch: b1, got ' + JSON.stringify(result));
if (result.indexOf('Run: r1') === -1) throw new Error('expected Run: r1, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_ReviewStatusWithVerdict(t *testing.T) {
	js := `
const run = { runId: 'r1', status: 'success', reviewCount: 2, reviewVerdict: 'Approved' };
const result = helpers.renderRunMeta(run);
if (result.indexOf('2 reviews - Approved') === -1) throw new Error('expected 2 reviews - Approved, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_InProgressForReviewLive(t *testing.T) {
	js := `
const run = { runId: 'r1', status: 'reviewing', reviewCount: 1, reviewLive: true };
const result = helpers.renderRunMeta(run);
if (result.indexOf('1 review - In Progress') === -1) throw new Error('expected 1 review - In Progress, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_OmitsRunLineForQueued(t *testing.T) {
	js := `
const run = { batchKey: 'b1', runId: 'r1', status: 'queued' };
const result = helpers.renderRunMeta(run);
if (result.indexOf('Batch: b1') === -1) throw new Error('expected Batch: b1, got ' + JSON.stringify(result));
if (result.indexOf('Run: r1') !== -1) throw new Error('expected no Run: r1 for queued, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_RetryCount(t *testing.T) {
	js := `
const run = { runId: 'r1', status: 'running', retriesDone: 3 };
const result = helpers.renderRunMeta(run);
if (result.indexOf('3 retries') === -1) throw new Error('expected 3 retries, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_ClearsQueuedRunTabEntries(t *testing.T) {
	js := `
const state = { expandedRunKey: null, tabs: { 'r1': 'log' }, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [{ key: 'r1', runId: 'r1', kind: 'active', status: 'queued' }];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.tabs['r1'] !== undefined) throw new Error('expected queued run tab cleared, got ' + JSON.stringify(result.state.tabs));
if (!result.changed) throw new Error('expected changed=true when clearing queued tab, got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_BlockedRunClearsExpandedState(t *testing.T) {
	js := `
const state = { expandedRunKey: 'r1', tabs: { 'r1': 'events' }, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [{ key: 'r1', runId: 'r1', kind: 'active', status: 'blocked' }];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.expandedRunKey !== null) throw new Error('expected expandedRunKey=null for blocked run, got ' + JSON.stringify(result.state.expandedRunKey));
if (!result.changed) throw new Error('expected changed=true for blocked run, got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_PreservesExpandedStateForVisibleNonQueuedRun(t *testing.T) {
	js := `
const state = { expandedRunKey: 'r1', tabs: { 'r1': 'log' }, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [{ key: 'r1', runId: 'r1', kind: 'active', status: 'running', issueNumber: 1 }];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.expandedRunKey !== 'r1') throw new Error('expected expandedRunKey=r1 for visible running run, got ' + JSON.stringify(result.state.expandedRunKey));
if (!result.changed) throw new Error('expected changed=true (tab added) for stable visible run, got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_EmptyForMinimalRun(t *testing.T) {
	js := `
const run = { key: 'r1', kind: 'active', status: 'running' };
const result = helpers.renderRunMeta(run);
if (result !== 'Run') throw new Error('expected "Run" for minimal run, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_ReviewCountWithChangesRequested(t *testing.T) {
	js := `
const run = { runId: 'r1', status: 'reviewing', reviewCount: 1, reviewVerdict: 'Changes requested' };
const result = helpers.renderRunMeta(run);
if (result.indexOf('Changes Requested') === -1) throw new Error('expected Changes Requested, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_DoesNotMigrateInvalidTabValues(t *testing.T) {
	js := `
const state = { expandedRunKey: null, tabs: { 'legacy-key': 'invalid-tab' }, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [{ key: 'canonical-key', runId: 'legacy-key', kind: 'active', status: 'running' }];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.tabs['canonical-key'] !== undefined) throw new Error('expected no migration of invalid tab value, got ' + JSON.stringify(result.state.tabs));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_ReviewCountOneReview(t *testing.T) {
	js := `
const run = { runId: 'r1', status: 'reviewing', reviewCount: 1, reviewVerdict: '' };
const result = helpers.renderRunMeta(run);
if (result.indexOf('1 review - Unclear') === -1) throw new Error('expected 1 review - Unclear for empty verdict, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_NoBatchKeyOmitsBatchLine(t *testing.T) {
	js := `
const run = { runId: 'r1', status: 'running' };
const result = helpers.renderRunMeta(run);
if (result.indexOf('Batch:') !== -1) throw new Error('expected no Batch: line without batchKey, got ' + JSON.stringify(result));
if (result.indexOf('Run: r1') === -1) throw new Error('expected Run: r1, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_CanonicalKeyNotOverwrittenByLegacyMigration(t *testing.T) {
	js := `
const state = { expandedRunKey: null, tabs: { 'legacy-key': 'events' }, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [{ key: 'legacy-key', runId: 'canonical-key', kind: 'active', status: 'running' }];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.tabs['canonical-key'] !== 'events') throw new Error('expected tab migrated to canonical key, got ' + JSON.stringify(result.state.tabs));
if (!result.changed) throw new Error('expected changed=true after migration, got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_RunWithNoIssueNumberRetainsExpanded(t *testing.T) {
	js := `
const state = { expandedRunKey: 'r1', tabs: { 'r1': 'log' }, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [{ key: 'r1', runId: 'r1', kind: 'active', status: 'running' }];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.expandedRunKey !== 'r1') throw new Error('expected expandedRunKey retained for run with no issueNumber, got ' + JSON.stringify(result.state.expandedRunKey));
if (!result.changed) throw new Error('expected changed=true (tab added) for stable run, got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproNormalize_HasVisibleSiblingPreservesExpanded(t *testing.T) {
	js := `
const state = { expandedRunKey: 'child-run', tabs: { 'child-run': 'log', 'parent-run': 'log' }, commandFormCollapsed: false, showArchived: false, activeBatches: false, sortBy: 'started', sortDir: 'desc' };
const runs = [
  { key: 'child-run', runId: 'child-run', kind: 'active', status: 'running', issueNumber: 42 },
  { key: 'parent-run', runId: 'parent-run', kind: 'completed', status: 'success', issueNumber: 42 },
];
const allRuns = runs;
const result = SandmanPortalState.normalize(state, runs, allRuns);
if (result.state.expandedRunKey !== 'child-run') throw new Error('expected expandedRunKey preserved for run with visible sibling, got ' + JSON.stringify(result.state.expandedRunKey));
if (!result.changed) throw new Error('expected changed=true (key migration runs even when key=runId), got ' + result.changed);
console.log('PASS');
`
	runReproNodeScript(t, js)
}

func TestReproRenderRunMeta_ActiveWithNoRunIdShowsOnlyCounter(t *testing.T) {
	js := `
const run = { kind: 'active', status: 'running', reviewCount: 1, reviewVerdict: 'Approved' };
const result = helpers.renderRunMeta(run);
if (result.indexOf('1 review - Approved') === -1) throw new Error('expected review line, got ' + JSON.stringify(result));
if (result.indexOf('Run:') !== -1) throw new Error('expected no Run: line without runId, got ' + JSON.stringify(result));
console.log('PASS');
`
	runReproNodeScript(t, js)
}
