package cmd

import "testing"

// TestPortalDiffCreateRunRow_ArchiveButtonRendersForCompletedNonArchived
// covers slice #6: the diff builder emits a <button data-action="archive-run">
// inside the actions cell for a completed run that is not yet archived.
func TestPortalDiffCreateRunRow_ArchiveButtonRendersForCompletedNonArchived(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-archive-1',
  archived: false,
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const actionsCell = row.querySelector('[data-cell="actions"]');
if (!actionsCell) throw new Error('expected actions cell');
const btn = actionsCell.querySelector('button[data-action="archive-run"]');
if (!btn) throw new Error('expected archive-run button on completed non-archived run');
if (btn.textContent.trim() !== 'Archive') throw new Error('expected label "Archive", got ' + JSON.stringify(btn.textContent));
if (btn.getAttribute('data-run-id') !== 'abcd-260618113825-archive-1') throw new Error('expected data-run-id="abcd-260618113825-archive-1", got ' + btn.getAttribute('data-run-id'));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffCreateRunRow_ArchiveButtonHiddenWhenSourceMissing ensures the
// portal does not offer an Archive action for stale rows whose source run
// directory is already gone.
func TestPortalDiffCreateRunRow_ArchiveButtonHiddenWhenSourceMissing(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-archive-stale',
  archived: false,
  sourceExists: false,
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const actionsCell = row.querySelector('[data-cell="actions"]');
if (!actionsCell) throw new Error('expected actions cell');
const btn = actionsCell.querySelector('button[data-action="archive-run"]');
if (btn) throw new Error('stale completed row must not render archive button');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffCreateRunRow_ArchiveButtonHiddenForActive covers slice #7:
// an active run never renders the archive button.
func TestPortalDiffCreateRunRow_ArchiveButtonHiddenForActive(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  kind: 'active',
  status: 'running',
  issueLabel: '#42',
  runId: 'abcd-260618113825-archive-2',
  archived: false,
  sourceExists: true,
  batchKey: 'abcd-260618113825-42',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const actionsCell = row.querySelector('[data-cell="actions"]');
if (!actionsCell) throw new Error('expected actions cell');
const btn = actionsCell.querySelector('button[data-action="archive-run"]');
if (btn) throw new Error('active run must not render archive button');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffCreateRunRow_ArchiveButtonHiddenForArchived covers slice #7
// (continued): a completed run whose Archived flag is true never renders
// the archive button.
func TestPortalDiffCreateRunRow_ArchiveButtonHiddenForArchived(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-archive-3',
  archived: true,
  sourceExists: false,
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const actionsCell = row.querySelector('[data-cell="actions"]');
if (!actionsCell) throw new Error('expected actions cell');
const btn = actionsCell.querySelector('button[data-action="archive-run"]');
if (btn) throw new Error('archived run must not render archive button');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateCells_ArchiveButtonFlipsWithArchivedFlag covers slice
// #8: when the run's archived flag changes from false to true, the diff
// update path removes the archive button from the actions cell.
func TestPortalDiffUpdateCells_ArchiveButtonFlipsWithArchivedFlag(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-archive-4',
  archived: false,
  sourceExists: true,
};
const runNew = Object.assign({}, runOld, { archived: true });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const oldBtn = created.row.querySelector('button[data-action="archive-run"]');
if (!oldBtn) throw new Error('expected initial archive button');
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true on archive flip');
const newBtn = created.row.querySelector('button[data-action="archive-run"]');
if (newBtn) throw new Error('expected archive button to be removed when archived flag flips true');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateCells_ArchiveButtonReappearsWhenArchiveRollsBack
// is a defensive counterpart to slice #8: when the archived flag flips
// back to false (e.g. a row re-appears from a stale cache) the button
// must come back so the operator can retry.
func TestPortalDiffUpdateCells_ArchiveButtonReappearsWhenArchiveRollsBack(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-archive-5',
  archived: true,
  sourceExists: true,
};
const runNew = Object.assign({}, runOld, { archived: false });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const oldBtn = created.row.querySelector('button[data-action="archive-run"]');
if (oldBtn) throw new Error('expected no initial archive button for archived run');
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true on archive flip back');
const newBtn = created.row.querySelector('button[data-action="archive-run"]');
if (!newBtn) throw new Error('expected archive button to reappear when archived flag flips false');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffCreateRunRow_ArchiveButtonAndAbortButtonCoexist covers
// the mixed case: a completed non-archived row can keep its actions cell
// independent of the abort button (which is gated to active runs only).
func TestPortalDiffCreateRunRow_ArchiveButtonAndAbortButtonCoexist(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-archive-6',
  archived: false,
  sourceExists: true,
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const actionsCell = row.querySelector('[data-cell="actions"]');
const archiveBtn = actionsCell.querySelector('button[data-action="archive-run"]');
const abortBtn = actionsCell.querySelector('button[data-action="abort-run"]');
if (!archiveBtn) throw new Error('expected archive button on completed non-archived run');
if (abortBtn) throw new Error('completed run must not render abort button');
console.log('PASS');
`
	runNodeScript(t, js)
}
