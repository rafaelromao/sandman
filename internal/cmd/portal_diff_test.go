package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestPortalDiffUpdateCells_StatusChangeUpdatesOnlyBadge(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1', branch: 'main' };
const runNew = Object.assign({}, runOld, { status: 'success' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const startedCell = created.row.querySelector('[data-cell="started"]');
const durationCell = created.row.querySelector('[data-cell="duration"]');
const branchCell = created.row.querySelector('[data-cell="branch"]');
const sourceCell = created.row.querySelector('[data-cell="source"]');
const titleCell = created.row.querySelector('[data-cell="title"]');
clearLog(startedCell); clearLog(durationCell); clearLog(branchCell); clearLog(sourceCell); clearLog(titleCell);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true');
if (result.cells !== 2) throw new Error('expected 2 cell mutations on status change, got ' + JSON.stringify(result));
if (countLog(startedCell) !== 0) throw new Error('started cell was touched');
if (countLog(durationCell) !== 0) throw new Error('duration cell was touched');
if (countLog(branchCell) !== 0) throw new Error('branch cell was touched');
if (countLog(sourceCell) !== 0) throw new Error('source cell was touched');
if (countLog(titleCell) !== 0) throw new Error('title cell was touched');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_DurationChangeUpdatesOnlyDuration(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1', branch: 'main', duration: '5s' };
const runNew = Object.assign({}, runOld, { duration: '12s' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const badgeCell = created.row.querySelector('[data-cell="badge"]');
const startedCell = created.row.querySelector('[data-cell="started"]');
const branchCell = created.row.querySelector('[data-cell="branch"]');
clearLog(badgeCell); clearLog(startedCell); clearLog(branchCell);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true');
if (result.cells !== 1) throw new Error('expected 1 cell mutation on duration change, got ' + JSON.stringify(result));
if (countLog(badgeCell) !== 0) throw new Error('badge cell was touched');
if (countLog(startedCell) !== 0) throw new Error('started cell was touched');
if (countLog(branchCell) !== 0) throw new Error('branch cell was touched');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_TitleChangeUpdatesOnlyTitle(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1', branch: 'main' };
const runNew = Object.assign({}, runOld, { issueLabel: 'Issue 1 updated' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const badgeCell = created.row.querySelector('[data-cell="badge"]');
const durationCell = created.row.querySelector('[data-cell="duration"]');
const branchCell = created.row.querySelector('[data-cell="branch"]');
clearLog(badgeCell); clearLog(durationCell); clearLog(branchCell);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true');
if (result.cells !== 2) throw new Error('expected 2 cell mutations on title change, got ' + JSON.stringify(result));
if (countLog(badgeCell) !== 0) throw new Error('badge cell was touched');
if (countLog(durationCell) !== 0) throw new Error('duration cell was touched');
if (countLog(branchCell) !== 0) throw new Error('branch cell was touched');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_KindChangeUpdatesRowClass(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1' };
const runNew = Object.assign({}, runOld, { kind: 'completed' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true');
if (result.cells !== 2) throw new Error('expected 2 row class mutations on kind change, got ' + JSON.stringify(result));
if (!created.row.classList.contains('completed')) throw new Error('row should have completed class');
if (created.row.classList.contains('active')) throw new Error('row should not have active class');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_AriaExpandedChangeUpdatesRowAttr(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1' };
const runNew = runOld;
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts1);
if (created.row.getAttribute('aria-expanded') !== 'false') throw new Error('expected aria-expanded=false');
SandmanPortalDiff.resetCounters();
const opts2 = { helpers, stopGroups, expandedKey: 'a' };
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts2);
if (!result.mutated) throw new Error('expected mutated=true');
if (result.cells !== 1) throw new Error('expected 1 cell mutation for aria-expanded, got ' + JSON.stringify(result));
if (created.row.getAttribute('aria-expanded') !== 'true') throw new Error('expected aria-expanded=true, got ' + created.row.getAttribute('aria-expanded'));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_ZeroMutationsOnUnchangedRun(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1', branch: 'main' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row) throw new Error('insertRunRow returned no row');
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, run, run, opts);
if (result.cells !== 0) throw new Error('expected 0 cell mutations on unchanged run, got ' + JSON.stringify(result));
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations !== 0) throw new Error('expected 0 mutations on unchanged run, got ' + JSON.stringify(counters));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_DetailRowWhenExpanded(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a' };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row) throw new Error('expected data row');
if (!created.detailRow) throw new Error('expected detail row when expandedKey matches');
if (created.detailRow.getAttribute('data-detail-for') !== 'a') throw new Error('detail row has wrong data-detail-for');
if (body.children.length !== 2) throw new Error('expected body to have 2 children, got ' + body.children.length);
const tabButtons = created.detailRow.querySelectorAll('button[data-action="set-tab"]');
if (tabButtons.length !== 3) throw new Error('expected 3 tab buttons, got ' + tabButtons.length);
const tabs = tabButtons.map(b => b.getAttribute('data-tab'));
if (!tabs.includes('log') || !tabs.includes('events') || !tabs.includes('details')) throw new Error('missing tab buttons, got ' + JSON.stringify(tabs));
const content = created.detailRow.querySelector('.detail-content');
if (!content) throw new Error('expected .detail-content inside detail row');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_NoDetailRowWhenNotExpanded(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'b' };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row) throw new Error('expected data row');
if (created.detailRow) throw new Error('expected NO detail row when expandedKey does not match');
if (body.children.length !== 1) throw new Error('expected body to have 1 child, got ' + body.children.length);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_NoInnerHTMLOnBody(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a' };
SandmanPortalDiff.insertRunRow(body, run, opts);
const innerHTMLEvents = body._log.filter(e => e[0] === 'innerHTML=');
if (innerHTMLEvents.length > 0) throw new Error('innerHTML was set on body, log: ' + JSON.stringify(innerHTMLEvents));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffRemoveRunRow_RemovesDataAndDetail(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a' };
SandmanPortalDiff.insertRunRow(body, run, opts);
if (body.children.length !== 2) throw new Error('expected 2 children before remove');
SandmanPortalDiff.resetCounters();
const removed = SandmanPortalDiff.removeRunRow(body, 'a');
if (removed !== 2) throw new Error('expected 2 rows removed, got ' + removed);
if (body.children.length !== 0) throw new Error('expected body empty after remove, got ' + body.children.length);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffRemoveRunRow_RemovesDataRowOnlyWhenNoDetail(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
if (body.children.length !== 1) throw new Error('expected 1 child before remove');
const removed = SandmanPortalDiff.removeRunRow(body, 'a');
if (removed !== 1) throw new Error('expected 1 row removed, got ' + removed);
if (body.children.length !== 0) throw new Error('expected body empty after remove');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_MatchesExistingRow(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1', branch: 'main' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const originalRow = body.children[0];
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.diffRuns(body, [run], opts);
if (result.inserted !== 0) throw new Error('expected 0 inserted, got ' + result.inserted);
if (result.removed !== 0) throw new Error('expected 0 removed, got ' + result.removed);
if (result.updated !== 0) throw new Error('expected 0 updated, got ' + result.updated);
if (result.mutations !== 0) throw new Error('expected 0 mutations, got ' + result.mutations);
if (body.children[0] !== originalRow) throw new Error('row identity should be preserved');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_InsertsNewRun(t *testing.T) {
	js := `const body = makeMockBody();
const runA = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1' };
const runB = { key: 'b', kind: 'active', status: 'active', issueLabel: 'B', runId: 'r2' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, runA, opts);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.diffRuns(body, [runA, runB], opts);
if (result.inserted !== 1) throw new Error('expected 1 inserted, got ' + result.inserted);
if (result.removed !== 0) throw new Error('expected 0 removed, got ' + result.removed);
if (body.children.length !== 2) throw new Error('expected 2 rows, got ' + body.children.length);
const b = body.children[1];
if (b.getAttribute('data-run-key') !== 'b') throw new Error('expected b as second child');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_RemovesGoneRun(t *testing.T) {
	js := `const body = makeMockBody();
const runA = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1' };
const runB = { key: 'b', kind: 'active', status: 'active', issueLabel: 'B', runId: 'r2' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a' };
SandmanPortalDiff.insertRunRow(body, runA, opts);
SandmanPortalDiff.insertRunRow(body, runB, opts);
if (body.children.length !== 3) throw new Error('expected 3 children (a-data + a-detail + b-data), got ' + body.children.length);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.diffRuns(body, [runA], opts);
if (result.removed !== 1) throw new Error('expected 1 removed (b), got ' + result.removed);
if (body.children.length !== 2) throw new Error('expected 2 children after remove, got ' + body.children.length);
const remaining = body.children.map(c => c.getAttribute('data-run-key') || c.getAttribute('data-detail-for'));
if (!remaining.includes('a') || !remaining.includes('a')) throw new Error('expected a + a-detail to remain, got ' + JSON.stringify(remaining));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_PreservesRowIdentityForUnchanged(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1' },
  { key: 'b', kind: 'active', status: 'active', issueLabel: 'B', runId: 'r2' },
  { key: 'c', kind: 'active', status: 'active', issueLabel: 'C', runId: 'r3' },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const aRow = body.children[0];
const bRow = body.children[1];
const cRow = body.children[2];
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.diffRuns(body, runs, opts);
if (result.mutations !== 0) throw new Error('expected 0 mutations on idempotent render, got ' + result.mutations);
if (body.children[0] !== aRow) throw new Error('a row identity changed');
if (body.children[1] !== bRow) throw new Error('b row identity changed');
if (body.children[2] !== cRow) throw new Error('c row identity changed');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_ReordersRows(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1' },
  { key: 'b', kind: 'active', status: 'active', issueLabel: 'B', runId: 'r2' },
  { key: 'c', kind: 'active', status: 'active', issueLabel: 'C', runId: 'r3' },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const aRow = body.children[0];
const bRow = body.children[1];
const cRow = body.children[2];
SandmanPortalDiff.resetCounters();
const reordered = [runs[2], runs[0], runs[1]];
const result = SandmanPortalDiff.diffRuns(body, reordered, opts);
if (result.mutations !== 0) throw new Error('reorder should be 0 mutations, got ' + result.mutations);
if (body.children[0] !== cRow) throw new Error('c should be first');
if (body.children[1] !== aRow) throw new Error('a should be second');
if (body.children[2] !== bRow) throw new Error('b should be third');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_InsertsDetailRowWhenExpanded(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, [run], opts1);
if (body.children.length !== 1) throw new Error('expected 1 child initially, got ' + body.children.length);
const opts2 = { helpers, stopGroups, expandedKey: 'a' };
const result = SandmanPortalDiff.diffRuns(body, [run], opts2);
if (result.inserted !== 1) throw new Error('expected 1 inserted (detail row), got ' + result.inserted);
if (body.children.length !== 2) throw new Error('expected 2 children after expand, got ' + body.children.length);
const second = body.children[1];
if (second.getAttribute('data-detail-for') !== 'a') throw new Error('expected detail row as second child');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_RemovesDetailRowWhenCollapsed(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: 'a' };
SandmanPortalDiff.diffRuns(body, [run], opts1);
if (body.children.length !== 2) throw new Error('expected 2 children (data + detail), got ' + body.children.length);
const detailRow = body.children[1];
const opts2 = { helpers, stopGroups, expandedKey: null };
const result = SandmanPortalDiff.diffRuns(body, [run], opts2);
if (result.removed !== 1) throw new Error('expected 1 removed (detail), got ' + result.removed);
if (body.children.length !== 1) throw new Error('expected 1 child after collapse, got ' + body.children.length);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_StopGroupsDedupeAcrossCall(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', socketPath: '/tmp/sock' },
  { key: 'b', kind: 'active', status: 'active', issueLabel: 'B', runId: 'r2', socketPath: '/tmp/sock' },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const aRow = body.children[0];
const bRow = body.children[1];
const aBtn = aRow.querySelector('button[data-action="stop-batch"]');
const bBtn = bRow.querySelector('button[data-action="stop-batch"]');
if (!aBtn) throw new Error('a should have stop button');
if (bBtn) throw new Error('b should NOT have stop button (deduped)');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_StopButtonsHiddenWhenPlatformUnsupported(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', socketPath: '/tmp/sock' },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, stopSupported: false, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const aRow = body.children[0];
const aBtn = aRow.querySelector('button[data-action="stop-batch"]');
if (aBtn) throw new Error('a should NOT have stop button when stopSupported is false');
if (stopGroups.size !== 0) throw new Error('stopGroups should not be touched when stopSupported is false, got size ' + stopGroups.size);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_LogPreUpdatedInPlace(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'line 1\nline 2' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1) throw new Error('expected log pre');
if (pre1.textContent !== 'line 1\nline 2') throw new Error('expected initial log text, got ' + JSON.stringify(pre1.textContent));
const hasSpans = pre1.children.length > 0;
if (!hasSpans) throw new Error('expected log pre to contain highlighted span children');
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { log: 'line 1\nline 2\nline 3' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 !== pre1) throw new Error('log pre should be the same DOM node across polls (selection preservation)');
if (pre2.textContent !== 'line 1\nline 2\nline 3') throw new Error('expected updated log text, got ' + pre2.textContent);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_UnchangedLogSkipsRefill(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'line 1\nline 2' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1) throw new Error('expected log pre');
const childrenBefore = pre1.children.length;
if (!childrenBefore) throw new Error('expected span children initially');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations !== 0) throw new Error('unchanged log should not mutate the pre, got mutations=' + counters.mutations);
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 !== pre1) throw new Error('pre identity must be preserved when log is unchanged');
if (pre2.children.length !== childrenBefore) throw new Error('pre children should not be replaced when log is unchanged');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffFillTerminalPre_PreservesTextNodes(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'first\nsecond' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
if (pre.textContent !== 'first\nsecond') throw new Error('expected log text with newline preserved, got ' + JSON.stringify(pre.textContent));
if (pre.getAttribute('data-rendered-log') !== 'first\nsecond') throw new Error('pre should be tagged with the rendered log');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_AppendPreservesExistingNodes(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'line 1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1) throw new Error('expected log pre');
const firstChildren = pre1.children.slice();
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { log: 'line 1\nline 2' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 !== pre1) throw new Error('pre identity must be preserved on append');
if (pre2.children.length <= firstChildren.length) throw new Error('append should grow the children list, got ' + pre2.children.length + ' vs ' + firstChildren.length);
for (let i = 0; i < firstChildren.length; i += 1) {
  if (pre2.children[i] !== firstChildren[i]) throw new Error('original child node ' + i + ' was replaced during append');
}
if (pre2.textContent !== 'line 1\nline 2') throw new Error('appended text should appear in pre, got ' + JSON.stringify(pre2.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_AppendRehighlightsTrailingToken(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'http://example/' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1) throw new Error('expected log pre');
const lastChildBefore = pre1.children[pre1.children.length - 1];
const beforeText = lastChildBefore._textContent;
if (beforeText !== 'http://example/') throw new Error('expected last span to hold the URL, got ' + JSON.stringify(beforeText));
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { log: 'http://example/running' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 !== pre1) throw new Error('pre identity should be preserved');
if (pre2.textContent !== 'http://example/running') throw new Error('expected pre text to include the suffix, got ' + JSON.stringify(pre2.textContent));
const lastBefore = pre2.children[pre2.children.length - 2];
if (!lastBefore || lastBefore._textContent !== 'http://example/') throw new Error('expected the original URL span to be preserved, got ' + JSON.stringify(lastBefore && lastBefore._textContent));
const lastAfter = pre2.children[pre2.children.length - 1];
if (!lastAfter || lastAfter._textContent !== 'running') throw new Error('expected the suffix to be appended as a new node, got ' + JSON.stringify(lastAfter && lastAfter._textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_AppendAfterNewlineIsTreatedAsBoundary(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'line 1\n' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1) throw new Error('expected log pre');
const firstChildren = pre1.children.slice();
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { log: 'line 1\nline 2\n' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 !== pre1) throw new Error('pre identity must be preserved');
if (pre2.textContent !== 'line 1\nline 2\n') throw new Error('expected pre to include the new line, got ' + JSON.stringify(pre2.textContent));
for (let i = 0; i < firstChildren.length; i += 1) {
  if (pre2.children[i] !== firstChildren[i]) throw new Error('original child node ' + i + ' was replaced during append');
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_AppendRebuildsOnInlineTokenExtension(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'foo running' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1) throw new Error('expected log pre');
const firstChildren = pre1.children.slice();
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { log: 'foo runningx' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 !== pre1) throw new Error('pre identity should be preserved');
if (pre2.textContent !== 'foo runningx') throw new Error('expected pre to contain the combined text, got ' + JSON.stringify(pre2.textContent));
const allText = pre2.children.map(c => c._textContent != null ? c._textContent : (c.textContent != null ? c.textContent : '')).join('');
if (allText.indexOf('foo ') !== allText.lastIndexOf('foo ')) throw new Error('foo prefix should not be duplicated, got ' + allText);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_RewriteReplacesNodes(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'line 1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1) throw new Error('expected log pre');
const firstChildren = pre1.children.slice();
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { log: 'replacement' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 !== pre1) throw new Error('pre identity must be preserved on rewrite');
let reused = 0;
for (const c of firstChildren) {
  if (pre2.children.indexOf(c) !== -1) reused += 1;
}
if (reused !== 0) throw new Error('rewrite should not reuse old child nodes, got ' + reused + ' reused');
if (pre2.textContent !== 'replacement') throw new Error('replacement log should appear in pre, got ' + JSON.stringify(pre2.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailEvents_SkipsRebuildWhenUnchanged(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'log text', events: [{ type: 'start', timestamp: 1700000000000, payload: { ok: true } }] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const content1 = detailRow.querySelector('.detail-content');
if (!content1) throw new Error('expected detail-content');
const firstChildren = content1.children.slice();
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations !== 0) throw new Error('unchanged events tab should not mutate, got ' + counters.mutations);
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('content identity should be preserved');
if (content2.children.length !== firstChildren.length) throw new Error('events children should not be replaced, got ' + content2.children.length + ' vs ' + firstChildren.length);
for (let i = 0; i < firstChildren.length; i += 1) {
  if (content2.children[i] !== firstChildren[i]) throw new Error('events child node ' + i + ' was replaced');
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailDetails_SkipsRebuildWhenUnchanged(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logUrl: '/log' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const content1 = detailRow.querySelector('.detail-content');
if (!content1) throw new Error('expected detail-content');
const firstChildren = content1.children.slice();
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations !== 0) throw new Error('unchanged details tab should not mutate, got ' + counters.mutations);
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('content identity should be preserved');
if (content2.children.length !== firstChildren.length) throw new Error('details children should not be replaced');
for (let i = 0; i < firstChildren.length; i += 1) {
  if (content2.children[i] !== firstChildren[i]) throw new Error('details child node ' + i + ' was replaced');
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailEvents_RebuildsWhenEventsChange(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', events: [{ type: 'start', timestamp: 1, payload: { ok: true } }] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const content1 = detailRow.querySelector('.detail-content');
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { events: [{ type: 'start', timestamp: 1, payload: { ok: true } }, { type: 'progress', timestamp: 2, payload: { ok: true } }] });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('changed events should mutate, got 0');
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('content identity should be preserved across rebuilds');
let rows = 0;
function countEventRows(n) {
  if (!n) return;
  if (n.classList && n.classList.contains && n.classList.contains('event-row')) rows += 1;
  if (n.children) for (const c of n.children) countEventRows(c);
}
countEventRows(content2);
if (rows !== 2) throw new Error('expected 2 event rows after rebuild, got ' + rows);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_RebuildsAfterTabRoundTrip(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'log text', events: [{ type: 'start', timestamp: 1, payload: { ok: true } }] };
const stopGroups = new Set();
const optsLog = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
const optsEvents = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], optsEvents);
const detailRow = body.children[1];
let content = detailRow.querySelector('.detail-content');
let eventRows = 0;
function countEventRows(n) {
  if (!n) return;
  if (n.classList && n.classList.contains && n.classList.contains('event-row')) eventRows += 1;
  if (n.children) for (const c of n.children) countEventRows(c);
}
countEventRows(content);
if (eventRows !== 1) throw new Error('expected 1 event row initially, got ' + eventRows);
SandmanPortalDiff.diffRuns(body, [run], optsLog);
content = detailRow.querySelector('.detail-content');
const logPre = content.querySelector('pre[data-scroll-key]');
if (!logPre) throw new Error('expected log pre after switch to log');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], optsEvents);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('returning to events tab should rebuild the pane, got 0 mutations');
content = detailRow.querySelector('.detail-content');
eventRows = 0;
countEventRows(content);
if (eventRows !== 1) throw new Error('expected 1 event row after returning to events, got ' + eventRows);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_TabChangeRebuildsContent(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1', log: 'log text' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts1);
const detailRow = body.children[1];
const logBtn1 = detailRow.querySelector('button[data-tab="log"]');
const eventsBtn1 = detailRow.querySelector('button[data-tab="events"]');
if (logBtn1.getAttribute('aria-pressed') !== 'true') throw new Error('log button should be pressed initially');
if (eventsBtn1.getAttribute('aria-pressed') !== 'false') throw new Error('events button should not be pressed initially');
const opts2 = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], opts2);
const logBtn2 = detailRow.querySelector('button[data-tab="log"]');
const eventsBtn2 = detailRow.querySelector('button[data-tab="events"]');
if (logBtn2.getAttribute('aria-pressed') !== 'false') throw new Error('log button should not be pressed after switch');
if (eventsBtn2.getAttribute('aria-pressed') !== 'true') throw new Error('events button should be pressed after switch');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffSetEmpty_UsesReplaceChildren(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'Issue 1', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
if (body.children.length !== 1) throw new Error('expected 1 child before setEmpty');
SandmanPortalDiff.setEmpty(body, '<div class="empty-state">No runs.</div>');
if (body.children.length !== 1) throw new Error('expected 1 child after setEmpty, got ' + body.children.length);
const empty = body.children[0];
if (empty.tagName !== 'TR') throw new Error('expected empty row, got ' + empty.tagName);
const innerHTMLEvents = body._log.filter(e => e[0] === 'innerHTML=');
if (innerHTMLEvents.length > 0) throw new Error('innerHTML was set on body, log: ' + JSON.stringify(innerHTMLEvents));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_RemovesStalePlaceholder(t *testing.T) {
	js := `const body = makeMockBody();
const placeholder = makeMockRow();
placeholder.tagName = 'TR';
placeholder.setAttribute('data-empty', 'true');
body.appendChild(placeholder);
const run = { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const result = SandmanPortalDiff.diffRuns(body, [run], opts);
if (body.children.length !== 1) throw new Error('expected 1 child after diff (placeholder removed), got ' + body.children.length);
if (body.children[0].getAttribute('data-run-key') !== 'a') throw new Error('expected run a to be the only child');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_ReorderAccountsForDetailRows(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', kind: 'active', status: 'active', issueLabel: 'A', runId: 'r1' },
  { key: 'b', kind: 'active', status: 'active', issueLabel: 'B', runId: 'r2' },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a' };
SandmanPortalDiff.diffRuns(body, runs, opts);
if (body.children.length !== 3) throw new Error('expected 3 children (a, detail, b), got ' + body.children.length);
if (body.children[0].getAttribute('data-run-key') !== 'a') throw new Error('a should be first');
if (body.children[1].getAttribute('data-detail-for') !== 'a') throw new Error('a-detail should be second');
if (body.children[2].getAttribute('data-run-key') !== 'b') throw new Error('b should be third');
const reordered = [runs[1], runs[0]];
const opts2 = { helpers, stopGroups, expandedKey: 'a' };
SandmanPortalDiff.diffRuns(body, reordered, opts2);
if (body.children.length !== 3) throw new Error('expected 3 children after reorder');
if (body.children[0].getAttribute('data-run-key') !== 'b') throw new Error('b should be first after reorder');
if (body.children[1].getAttribute('data-run-key') !== 'a') throw new Error('a should be second after reorder');
if (body.children[2].getAttribute('data-detail-for') !== 'a') throw new Error('a-detail should be third after reorder');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalHTMLRenderPath_NoRunsBodyInnerHTML(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	source, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read portal.html: %v", err)
	}
	pattern := regexp.MustCompile(`runsBody\.innerHTML\s*=`)
	if pattern.Match(source) {
		t.Fatalf("portal.html must not assign runsBody.innerHTML in the render path; found match")
	}
}

func TestPortalDiffHelperExists(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal diff helper test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	helperPath := filepath.Join(filepath.Dir(currentFile), "portal_diff.js")

	script := `const fs = require('fs');
const vm = require('vm');
const helperPath = process.argv[1];
const source = fs.readFileSync(helperPath, 'utf8');
const sandbox = { window: {}, globalThis: {}, Set, Map, WeakMap, JSON, console };
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: helperPath });
const api = sandbox.SandmanPortalDiff;
if (!api) throw new Error('missing SandmanPortalDiff');
const required = ['insertRunRow', 'updateRunRowCells', 'removeRunRow', 'setEmpty', 'diffRuns', 'getRowData', 'resetCounters', 'getCounters'];
for (const name of required) {
  if (typeof api[name] !== 'function') throw new Error('missing SandmanPortalDiff.' + name);
}
`
	cmd := exec.Command("node", "-e", script, helperPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal diff helper missing API: %v\n%s", err, out)
	}
}

func sharedMockHelpers() string {
	return `const escapeHTML = (v) => String(v == null ? '' : v)
  .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
const formatTime = (v) => v ? String(v) : '—';
const formatDuration = (v) => v && String(v).trim() ? String(v) : '—';
const formatBranch = (run) => run.branch && String(run.branch).trim() ? run.branch : '—';
const formatSource = (run) => {
  if (run.kind === 'active' && run.socketPath) return run.socketPath;
  if (run.logPath) return run.logPath;
  return '—';
};
const statusClass = (run) => {
  const s = String(run.status || '').toLowerCase();
  if (s === 'active') return 'active';
  if (s === 'success') return 'success';
  if (s === 'failure' || s === 'failed' || s === 'error') return 'failure';
  if (s === 'warning' || s === 'stale' || s === 'blocked') return 'warning';
  return s || 'default';
};
const renderStatusBadge = (run) => {
  const k = statusClass(run);
  const label = run.status || (run.kind === 'active' ? 'active' : 'completed');
  return '<span class="badge ' + escapeHTML(k) + '"><span class="dot"></span>' + escapeHTML(label) + '</span>';
};
const renderRunMeta = (run) => {
  const parts = [];
  if (run.runId) parts.push('ID ' + run.runId);
  if (run.issueLabel) parts.push(run.issueLabel);
  return parts.length ? parts.join(' · ') : 'Run';
};
const renderTerminalContent = (text) => {
  const value = String(text || '');
  if (!value) return '';
  return value.split('\n').map((line) => '<span>' + escapeHTML(line) + '</span>').join('\n');
};
const isRunStoppable = (run, stopGroups) => {
  if (!run || run.kind !== 'active') return false;
  if (run.status !== 'active' && run.status !== 'queued') return false;
  if (!run.socketPath) return false;
  if (stopGroups && stopGroups.has && stopGroups.has(run.socketPath)) return false;
  return true;
};
const helpers = {
  escapeHTML, formatTime, formatDuration, formatBranch, formatSource,
  statusClass, renderStatusBadge, renderRunMeta, renderTerminalContent, isRunStoppable,
};
`
}

func sharedMockBody() string {
	return `function makeMockBody() {
  const log = [];
  const body = {
    tagName: 'TBODY',
    children: [],
    innerHTML: '',
    dataset: {},
    setAttribute(name, value) { this[name] = value; log.push(['setAttribute', name, value]); },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; log.push(['removeAttribute', name]); },
    appendChild(child) {
      const parent = child.parentNode;
      if (parent) {
        const idx = parent.children.indexOf(child);
        if (idx >= 0) parent.children.splice(idx, 1);
      }
      child.parentNode = this;
      this.children.push(child);
      log.push(['appendChild', child.__id || '?']);
      return child;
    },
    insertBefore(child, ref) {
      const parent = child.parentNode;
      if (parent) {
        const idx = parent.children.indexOf(child);
        if (idx >= 0) parent.children.splice(idx, 1);
      }
      child.parentNode = this;
      const idx = ref ? this.children.indexOf(ref) : -1;
      if (idx < 0) this.children.push(child);
      else this.children.splice(idx, 0, child);
      log.push(['insertBefore', child.__id || '?', ref ? (ref.__id || '?') : null]);
      return child;
    },
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx < 0) return child;
      this.children.splice(idx, 1);
      child.parentNode = null;
      log.push(['removeChild', child.__id || '?']);
      return child;
    },
    replaceChildren(...nodes) {
      for (const child of this.children.slice()) this.removeChild(child);
      for (const node of nodes) this.appendChild(node);
      log.push(['replaceChildren', nodes.length]);
    },
    querySelectorAll(sel) {
      const m = sel.match(/^(\[|tr\.detail-row\[|tr\[)data-(run-key|detail-for)="([^"]+)"\]$/);
      if (!m) return [];
      const attr = 'data-' + m[2];
      const value = m[3];
      return this.children.filter((c) => c.getAttribute && c.getAttribute(attr) === value);
    },
    querySelector(sel) {
      const all = this.querySelectorAll(sel);
      return all[0] || null;
    },
    insertRow(idx) {
      const tr = makeMockRow();
      const at = (idx == null || idx < 0 || idx > this.children.length) ? this.children.length : idx;
      this.children.splice(at, 0, tr);
      tr.parentNode = this;
      log.push(['insertRow', at]);
      return tr;
    },
    _log: log,
  };
  Object.defineProperty(body, 'innerHTML', {
    get() { return this._innerHTML || ''; },
    set(v) {
      this._innerHTML = v;
      log.push(['innerHTML=', v === '' ? '' : '<set>']);
      for (const c of this.children.slice()) {
        c.parentNode = null;
      }
      this.children = [];
    },
  });
  Object.defineProperty(body, 'childNodes', {
    get() { return this.children; },
  });
  Object.defineProperty(body, 'firstChild', {
    get() { return this.children.length ? this.children[0] : null; },
  });
  Object.defineProperty(body, 'lastChild', {
    get() { return this.children.length ? this.children[this.children.length - 1] : null; },
  });
  return body;
}
function makeMockRow() {
  const log = [];
  const row = {
    tagName: 'TR',
    children: [],
    dataset: {},
    classList: { _set: new Set(), add(c) { this._set.add(c); log.push(['class+', c]); }, remove(c) { this._set.delete(c); log.push(['class-', c]); }, contains(c) { return this._set.has(c); }, toggle(c, force) { if (force === true) this._set.add(c); else if (force === false) this._set.delete(c); else if (this._set.has(c)) this._set.delete(c); else this._set.add(c); log.push(['class^', c]); } },
    parentNode: null,
    __id: 'r' + Math.random().toString(36).slice(2, 8),
    _log: log,
    setAttribute(name, value) { this[name] = value; log.push(['setAttribute', name, value]); },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; log.push(['removeAttribute', name]); },
    appendChild(child) {
      const parent = child.parentNode;
      if (parent) { const i = parent.children.indexOf(child); if (i >= 0) parent.children.splice(i, 1); }
      child.parentNode = this;
      this.children.push(child);
      log.push(['appendChild', child.__id || '?']);
      return child;
    },
    insertBefore(child, ref) {
      const parent = child.parentNode;
      if (parent) { const i = parent.children.indexOf(child); if (i >= 0) parent.children.splice(i, 1); }
      child.parentNode = this;
      const idx = ref ? this.children.indexOf(ref) : -1;
      if (idx < 0) this.children.push(child);
      else this.children.splice(idx, 0, child);
      log.push(['insertBefore', child.__id || '?', ref ? (ref.__id || '?') : null]);
      return child;
    },
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx < 0) return child;
      this.children.splice(idx, 1);
      child.parentNode = null;
      log.push(['removeChild', child.__id || '?']);
      return child;
    },
    replaceChildren(...nodes) {
      for (const c of this.children.slice()) this.removeChild(c);
      for (const node of nodes) this.appendChild(node);
      log.push(['replaceChildren', nodes.length]);
    },
    querySelector(sel) {
      function visit(node) {
        if (!node) return null;
        if (node.getAttribute && matchSelector(node, sel)) return node;
        if (!node.children) return null;
        for (const c of node.children) {
          const hit = visit(c);
          if (hit) return hit;
        }
        return null;
      }
      return visit(this);
    },
    querySelectorAll(sel) {
      const out = [];
      const root = this;
      function visit(node) {
        if (!node) return;
        if (node !== root && node.getAttribute && matchSelector(node, sel)) out.push(node);
        if (!node.children) return;
        for (const c of node.children) visit(c);
      }
      visit(root);
      return out;
    },
  };
  Object.defineProperty(row, 'textContent', {
    get() {
      if (this._textContent != null) return this._textContent;
      const parts = [];
      function walk(n) {
        if (!n) return;
        if (n.nodeType === 3) { parts.push(n._textContent != null ? n._textContent : (n.textContent || '')); return; }
        if (n._textContent != null) { parts.push(n._textContent); return; }
        const list = n.childNodes || n.children;
        if (list) for (const c of list) walk(c);
      }
      walk(this);
      return parts.join('');
    },
    set(v) { this._textContent = v; log.push(['textContent=', '<set>']); },
  });
  Object.defineProperty(row, 'innerHTML', {
    get() { return this._innerHTML || ''; },
    set(v) {
      this._innerHTML = v;
      log.push(['innerHTML=', '<set>']);
      for (const c of this.children.slice()) c.parentNode = null;
      this.children = [];
      parseHtmlInto(this, String(v || ''), log);
    },
  });
  Object.defineProperty(row, 'childNodes', {
    get() { return this.children; },
  });
  Object.defineProperty(row, 'firstChild', {
    get() { return this.children.length ? this.children[0] : null; },
  });
  Object.defineProperty(row, 'lastChild', {
    get() { return this.children.length ? this.children[this.children.length - 1] : null; },
  });
  return row;
}
function parseHtmlInto(parent, html, log) {
  let pos = 0;
  let guard = 0;
  while (pos < html.length) {
    if (++guard > html.length * 4 + 10) break;
    if (html.charCodeAt(pos) === 10) {
      const tn = { nodeType: 3, textContent: '\n', parentNode: null, _log: [] };
      parent.appendChild(tn);
      pos += 1;
      continue;
    }
    if (html.startsWith('<span', pos)) {
      const openEnd = html.indexOf('>', pos);
      if (openEnd < 0) { pos = html.length; break; }
      const closeStart = html.indexOf('</span>', openEnd);
      if (closeStart < 0) { pos = html.length; break; }
      const text = html.slice(openEnd + 1, closeStart);
      const span = makeMockRow();
      span.tagName = 'SPAN';
      span._textContent = text;
      parent.appendChild(span);
      pos = closeStart + 7;
      continue;
    }
    if (html.charCodeAt(pos) === 60) {
      const closeAngle = html.indexOf('>', pos);
      if (closeAngle < 0) { pos = html.length; break; }
      pos = closeAngle + 1;
      continue;
    }
    const next = html.indexOf('<', pos + 1);
    const end = next < 0 ? html.length : next;
    const text = html.slice(pos, end);
    if (text) {
      const tn = { nodeType: 3, textContent: text, parentNode: null, _log: [] };
      parent.appendChild(tn);
    }
    pos = end;
  }
}
function matchSelector(node, sel) {
  const cls = sel.match(/^([a-zA-Z]+)?\.([a-zA-Z-]+)$/);
  if (cls) {
    const tag = cls[1];
    const klass = cls[2];
    if (tag && node.tagName !== tag.toUpperCase()) return false;
    return node.classList && node.classList.contains(klass);
  }
  const m = sel.match(/^([a-zA-Z]+)?\[([a-zA-Z-]+)(?:="([^"]+)")?\]$/);
  if (m) {
    const tag = m[1];
    const attr = m[2];
    const value = m[3];
    if (tag && node.tagName !== tag.toUpperCase()) return false;
    const current = node.getAttribute && node.getAttribute(attr);
    if (value === undefined) return current !== null;
    return current === value;
  }
  return false;
}
function makeMockDocument() {
  return {
    createElement(tag) {
      if (tag === 'tr' || tag === 'TR') return makeMockRow();
      const el = makeMockRow();
      el.tagName = String(tag).toUpperCase();
      return el;
    },
    createTextNode(text) {
      return { nodeType: 3, textContent: String(text), parentNode: null, _log: [] };
    },
  };
}
function clearLog(node) {
  if (!node) return;
  if (node._log) node._log.length = 0;
  if (node.children) for (const c of node.children) clearLog(c);
  if (node.childNodes) for (const c of node.childNodes) clearLog(c);
}
function countLog(node) {
  if (!node) return 0;
  let n = node._log ? node._log.length : 0;
  if (node.children) for (const c of node.children) n += countLog(c);
  if (node.childNodes) for (const c of node.childNodes) n += countLog(c);
  return n;
}
const documentRef = makeMockDocument();
`
}

func runNodeScript(t *testing.T, js string) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	helperPath := filepath.Join(filepath.Dir(currentFile), "portal_diff.js")
	prefix := sharedMockHelpers() + sharedMockBody() + `
const fs = require('fs');
const vm = require('vm');
const helperPath = ` + "`" + helperPath + "`" + `;
const source = fs.readFileSync(helperPath, 'utf8');
const sandbox = { window: {}, globalThis: {}, Set, Map, WeakMap, JSON, console };
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
sandbox.document = documentRef;
sandbox.HTMLElement = function() {};
vm.runInNewContext(source, sandbox, { filename: helperPath });
const SandmanPortalDiff = sandbox.SandmanPortalDiff;
if (!SandmanPortalDiff) throw new Error('SandmanPortalDiff missing');
`
	cmd := exec.Command("node", "-e", prefix+js)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Logf("script output: %s", out)
	}
}
