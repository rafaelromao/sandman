package cmd

import "testing"

// TestPortalDiffUpdateCells_UnavailableFlipAddsRowClass covers slice
// #4 of #1312: when the unavailable flag flips false→true between
// polls, the diff update path adds the row-unavailable CSS class to
// the <tr>. This mirrors the existing archived-flip handling at
// portal_diff.js:962-964.
func TestPortalDiffUpdateCells_UnavailableFlipAddsRowClass(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-unavail-1',
  archived: false,
  unavailable: false,
  sourceExists: true,
};
const runNew = Object.assign({}, runOld, { unavailable: true });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (created.row.classList.contains('row-unavailable')) {
  throw new Error('pre-flip row must not carry row-unavailable');
}
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true on unavailable flip');
if (!created.row.classList.contains('row-unavailable')) {
  throw new Error('expected row-unavailable after flip; classes=' + created.row.className);
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateCells_UnavailableFlipRemovesArchiveButton covers
// slice #4 (continued): a flip to unavailable must also remove the
// archive-run button via the isRunArchivable helper (now gating on
// the unavailable flag in portal.html).
func TestPortalDiffUpdateCells_UnavailableFlipRemovesArchiveButton(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-unavail-2',
  archived: false,
  unavailable: false,
  sourceExists: true,
};
const runNew = Object.assign({}, runOld, { unavailable: true });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const oldActions = created.row.querySelector('[data-cell="actions"]');
const oldBtn = oldActions.querySelector('button[data-action="archive-run"]');
if (!oldBtn) throw new Error('expected initial archive button on non-archived non-unavailable row');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
const newActions = created.row.querySelector('[data-cell="actions"]');
const newBtn = newActions.querySelector('button[data-action="archive-run"]');
if (newBtn) throw new Error('expected archive button to be removed on unavailable flip');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateCells_UnavailableFlipRollsBack removes the
// row-unavailable class when the unavailable flag flips back to false
// (e.g. a stale snapshot that briefly marked a row as unavailable).
func TestPortalDiffUpdateCells_UnavailableFlipRollsBack(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-unavail-3',
  archived: false,
  unavailable: true,
  sourceExists: false,
};
const runNew = Object.assign({}, runOld, { unavailable: false, sourceExists: true });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (!created.row.classList.contains('row-unavailable')) {
  throw new Error('expected initial row-unavailable');
}
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true on unavailable rollback');
if (created.row.classList.contains('row-unavailable')) {
  throw new Error('expected row-unavailable to be removed after rollback; classes=' + created.row.className);
}
const actions = created.row.querySelector('[data-cell="actions"]');
const btn = actions.querySelector('button[data-action="archive-run"]');
if (!btn) throw new Error('expected archive button to come back on rollback');
console.log('PASS');
`
	runNodeScript(t, js)
}
