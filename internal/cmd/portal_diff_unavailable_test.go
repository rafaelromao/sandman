package cmd

import "testing"

// TestPortalDiffCreateRunRow_UnavailableRowGetsRowClassAndBadge covers
// slice #2 of #1312: a row whose Unavailable flag is true renders with
// the `row-unavailable` CSS class on the <tr> and the `unavailable`
// badge class on the badge cell. The snapshot returned from
// snapshotCellState must report badgeClass: 'unavailable' so the diff
// update path can re-stamp the badge class on a flip.
func TestPortalDiffCreateRunRow_UnavailableRowGetsRowClassAndBadge(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-unavail-1',
  archived: false,
  unavailable: true,
  sourceExists: false,
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
if (!row.classList.contains('row-unavailable')) {
  throw new Error('expected row-unavailable class on <tr>, got ' + row.className);
}
const badgeCell = row.querySelector('[data-cell="badge"]');
const badge = badgeCell.children[0];
if (!badge.classList.contains('unavailable')) {
  throw new Error('expected badge class unavailable, got ' + badge.className);
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffCreateRunRow_ArchivedRowKeepsExistingBehavior is a
// regression guard for slice #2: the new row-unavailable behaviour must
// not regress the existing row-archived class. Archived rows continue
// to render with row-archived + badge class 'archived'.
func TestPortalDiffCreateRunRow_ArchivedRowKeepsExistingBehavior(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  kind: 'completed',
  status: 'success',
  issueLabel: '#42',
  runId: 'abcd-260618113825-unavail-2',
  archived: true,
  unavailable: false,
  sourceExists: false,
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
if (!row.classList.contains('row-archived')) {
  throw new Error('expected row-archived class on <tr>, got ' + row.className);
}
const badgeCell = row.querySelector('[data-cell="badge"]');
const badge = badgeCell.children[0];
if (!badge.classList.contains('archived')) {
  throw new Error('expected badge class archived, got ' + badge.className);
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffCreateRunRow_ActiveRowHasNoReadOnlyClass covers slice #2
// (continued): an active row must not pick up either row-archived or
// row-unavailable, since the read-only flags apply only to completed
// historical rows in the current contract.
func TestPortalDiffCreateRunRow_ActiveRowHasNoReadOnlyClass(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  kind: 'active',
  status: 'running',
  issueLabel: '#42',
  runId: 'abcd-260618113825-unavail-3',
  batchKey: 'abcd-260618113825-42',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
if (row.classList.contains('row-archived')) {
  throw new Error('active row must not carry row-archived');
}
if (row.classList.contains('row-unavailable')) {
  throw new Error('active row must not carry row-unavailable');
}
console.log('PASS');
`
	runNodeScript(t, js)
}
