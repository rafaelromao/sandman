package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPortalCSS_NonExpandableRowNeutralizesCursorAndFocus covers the CSS
// half of the issue #1486 acceptance criteria: a queued/blocked row must
// not look clickable. The CSS rule tr.run-row.row-non-expandable must
// (a) override the default cursor: pointer and (b) neutralize the
// :focus-visible background added by the existing .run-row:focus-visible
// rule. Without (a) and (b) the row would still present as a toggle
// button even though it no longer has the toggle attrs.
func TestPortalCSS_NonExpandableRowNeutralizesCursorAndFocus(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	portalHtmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	src, err := os.ReadFile(portalHtmlPath)
	if err != nil {
		t.Fatalf("read portal.html: %v", err)
	}
	content := string(src)

	// Look for a CSS rule that targets tr.run-row.row-non-expandable and
	// overrides the cursor. The selector itself + a non-pointer cursor
	// value inside the same rule is what locks in behavior. We require
	// both tokens to appear together so a future rename of the class is
	// caught here.
	if !strings.Contains(content, "tr.run-row.row-non-expandable") {
		t.Fatalf("portal.html missing CSS selector tr.run-row.row-non-expandable; queued/blocked rows will keep the toggle cursor")
	}
	// Locate the cursor override rule. We grep for the selector and a
	// non-pointer cursor value within the following ~400 chars (covers
	// the rule body plus the next 1-2 rules).
	idx := strings.Index(content, "tr.run-row.row-non-expandable")
	if idx < 0 {
		t.Fatal("selector not found")
	}
	window := content[idx:min(len(content), idx+400)]
	if !strings.Contains(window, "cursor:") || !strings.Contains(window, "default") && !strings.Contains(window, "auto") && !strings.Contains(window, "not-allowed") && !strings.Contains(window, "inherit") {
		t.Fatalf("expected non-pointer cursor override inside tr.run-row.row-non-expandable rule, got window:\n%s", window)
	}
	// Focus neutralization: same selector must include a :focus-visible or
	// :focus override that resets the background. The existing
	// .run-row:focus-visible td rule sets an accent background on focus;
	// the new rule must override it.
	if !strings.Contains(window, ":focus-visible") && !strings.Contains(window, ":focus") {
		t.Fatalf("expected :focus-visible or :focus override inside tr.run-row.row-non-expandable rule, got window:\n%s", window)
	}
}

// TestPortalDiff_BuildDataRow_QueuedRowOmitsToggleAttrs is the issue
// #1486 build-time regression test for the queued status path. A queued
// active run must render as a non-expandable row: no data-action, no
// role, no tabindex, no aria-controls; aria-expanded must be "false";
// the row must carry the row-non-expandable class. Without this guard
// the row would still capture clicks via the tr[data-action="toggle-run"]
// selector and would expose a synthetic detail-row id whose row is
// never actually created — and any cached detail-row DOM would then
// leak unrelated logs into the queued row's panel.
func TestPortalDiff_BuildDataRow_QueuedRowOmitsToggleAttrs(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'q-1',
  kind: 'active',
  status: 'queued',
  issueLabel: '#42',
  issueNumber: 42,
  batchKey: 'b-42',
  runId: 'r1',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row) throw new Error('expected row');
if (created.row.getAttribute('data-action') !== null) throw new Error('expected no data-action on queued row, got ' + created.row.getAttribute('data-action'));
if (created.row.getAttribute('role') !== null) throw new Error('expected no role on queued row, got ' + created.row.getAttribute('role'));
if (created.row.getAttribute('tabindex') !== null) throw new Error('expected no tabindex on queued row, got ' + created.row.getAttribute('tabindex'));
if (created.row.getAttribute('aria-controls') !== null) throw new Error('expected no aria-controls on queued row, got ' + created.row.getAttribute('aria-controls'));
if (created.row.getAttribute('aria-expanded') !== 'false') throw new Error('expected aria-expanded=false on queued row, got ' + created.row.getAttribute('aria-expanded'));
if (!created.row.classList.contains('row-non-expandable')) throw new Error('expected row-non-expandable class on queued row, classes: ' + JSON.stringify(Array.from(created.row.classList._set || [])));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_BuildDataRow_BlockedRowOmitsToggleAttrs is the issue
// #1486 build-time regression test for the blocked status path. Blocked
// runs sit in the same wait-state family as queued runs: they have no
// real log to expand into, and historically a blocked row's expand
// affordance has been observed to leak unrelated sibling review logs
// through the synthetic detail-row id. The row must render without
// any toggle attrs and carry row-non-expandable, mirroring queued.
func TestPortalDiff_BuildDataRow_BlockedRowOmitsToggleAttrs(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'b-1',
  kind: 'active',
  status: 'blocked',
  issueLabel: '#43',
  issueNumber: 43,
  batchKey: 'b-43',
  runId: 'r2',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row) throw new Error('expected row');
if (created.row.getAttribute('data-action') !== null) throw new Error('expected no data-action on blocked row, got ' + created.row.getAttribute('data-action'));
if (created.row.getAttribute('role') !== null) throw new Error('expected no role on blocked row, got ' + created.row.getAttribute('role'));
if (created.row.getAttribute('tabindex') !== null) throw new Error('expected no tabindex on blocked row, got ' + created.row.getAttribute('tabindex'));
if (created.row.getAttribute('aria-controls') !== null) throw new Error('expected no aria-controls on blocked row, got ' + created.row.getAttribute('aria-controls'));
if (created.row.getAttribute('aria-expanded') !== 'false') throw new Error('expected aria-expanded=false on blocked row, got ' + created.row.getAttribute('aria-expanded'));
if (!created.row.classList.contains('row-non-expandable')) throw new Error('expected row-non-expandable class on blocked row');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_BuildDataRow_NonQueuedRowHasToggleAttrs is the issue
// #1486 negative-control test. Any status that is not queued or blocked
// must continue to render the toggle affordance: data-action, role,
// tabindex, and aria-controls all present, and the row must NOT carry
// the row-non-expandable class. Without this guard a future refactor
// could over-broadly suppress the affordance (e.g. on completed runs)
// and silently break the "user can still inspect finished runs"
// contract.
func TestPortalDiff_BuildDataRow_NonQueuedRowHasToggleAttrs(t *testing.T) {
	js := `const body = makeMockBody();
const cases = [
  { key: 'r-1', kind: 'active',   status: 'running',   issueLabel: '#1', runId: 'r-1' },
  { key: 'r-2', kind: 'active',   status: 'reviewing', issueLabel: '#2', runId: 'r-2' },
  { key: 'r-3', kind: 'completed', status: 'success',  issueLabel: '#3', runId: 'r-3' },
  { key: 'r-4', kind: 'completed', status: 'failure',  issueLabel: '#4', runId: 'r-4' },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
for (const run of cases) {
  const created = SandmanPortalDiff.insertRunRow(body, run, opts);
  if (!created.row) throw new Error('expected row for status ' + run.status);
  if (created.row.getAttribute('data-action') !== 'toggle-run') throw new Error('expected data-action=toggle-run for status ' + run.status + ', got ' + created.row.getAttribute('data-action'));
  if (created.row.getAttribute('role') !== 'button') throw new Error('expected role=button for status ' + run.status + ', got ' + created.row.getAttribute('role'));
  if (created.row.getAttribute('tabindex') !== '0') throw new Error('expected tabindex=0 for status ' + run.status + ', got ' + created.row.getAttribute('tabindex'));
  if (created.row.getAttribute('aria-controls') !== 'run-detail-' + run.key) throw new Error('expected aria-controls=run-detail-' + run.key + ' for status ' + run.status + ', got ' + created.row.getAttribute('aria-controls'));
  if (created.row.classList.contains('row-non-expandable')) throw new Error('expected no row-non-expandable for status ' + run.status);
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_InsertRunRow_QueuedRowNoDetailEvenWithExpandedKey is the
// issue #1486 detail-row guard at build time. matchesExpandedSubject can
// return true for a queued run whose runId happens to match the saved
// expandedKey (e.g. a previously expanded row that then re-queued after
// a restart). Without an additional gate, buildDetailRow would still
// run and produce a sibling <tr data-detail-for="..."> whose contents
// may surface unrelated sibling review logs. The gate must live inside
// insertRunRow so the path is closed at insertion time, not just at the
// click handler.
func TestPortalDiff_InsertRunRow_QueuedRowNoDetailEvenWithExpandedKey(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'q-x',
  kind: 'active',
  status: 'queued',
  issueLabel: '#99',
  issueNumber: 99,
  batchKey: 'b-99',
  runId: 'q-x',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'q-x', runs: [run] };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row) throw new Error('expected row');
if (created.detailRow) throw new Error('expected no detail row for queued row even with expandedKey, got ' + (created.detailRow ? created.detailRow.outerHTML : 'null'));
const sibling = body.querySelector('tr.detail-row[data-detail-for="q-x"]');
if (sibling) throw new Error('expected no sibling detail <tr> in body, got ' + sibling.outerHTML);
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_UpdateRunRowCells_RunningToQueuedRemovesToggleAttrs
// covers the issue #1486 transition: an existing running row whose new
// state is queued must lose the toggle affordance on the next poll.
// aria-expanded must be forced to "false" (even if the row was previously
// the expanded subject), and row-non-expandable must be added. The
// reconciliation must work whether or not the DOM row already has the
// class.
func TestPortalDiff_UpdateRunRowCells_RunningToQueuedRemovesToggleAttrs(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 't-1', kind: 'active', status: 'running', issueLabel: '#1', runId: 't-1',
};
const runNew = Object.assign({}, runOld, { status: 'queued' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 't-1' };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (created.row.getAttribute('data-action') !== 'toggle-run') throw new Error('expected pre-state row to have data-action=toggle-run');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (created.row.getAttribute('data-action') !== null) throw new Error('expected data-action removed after running->queued, got ' + created.row.getAttribute('data-action'));
if (created.row.getAttribute('role') !== null) throw new Error('expected role removed after running->queued, got ' + created.row.getAttribute('role'));
if (created.row.getAttribute('tabindex') !== null) throw new Error('expected tabindex removed after running->queued, got ' + created.row.getAttribute('tabindex'));
if (created.row.getAttribute('aria-controls') !== null) throw new Error('expected aria-controls removed after running->queued, got ' + created.row.getAttribute('aria-controls'));
if (created.row.getAttribute('aria-expanded') !== 'false') throw new Error('expected aria-expanded=false after running->queued, got ' + created.row.getAttribute('aria-expanded'));
if (!created.row.classList.contains('row-non-expandable')) throw new Error('expected row-non-expandable after running->queued');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_UpdateRunRowCells_RunningToBlockedRemovesToggleAttrs
// mirrors the queued transition for the blocked status. A blocked run
// has no log to expand into, so the toggle affordance must be removed
// the same way. This is the load-bearing case from the original bug
// report (sibling review runs' daemon_test.go output leaking through a
// blocked row's detail panel).
func TestPortalDiff_UpdateRunRowCells_RunningToBlockedRemovesToggleAttrs(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 't-2', kind: 'active', status: 'running', issueLabel: '#2', runId: 't-2',
};
const runNew = Object.assign({}, runOld, { status: 'blocked' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 't-2' };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (created.row.getAttribute('data-action') !== 'toggle-run') throw new Error('expected pre-state row to have data-action=toggle-run');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (created.row.getAttribute('data-action') !== null) throw new Error('expected data-action removed after running->blocked, got ' + created.row.getAttribute('data-action'));
if (created.row.getAttribute('role') !== null) throw new Error('expected role removed after running->blocked, got ' + created.row.getAttribute('role'));
if (created.row.getAttribute('tabindex') !== null) throw new Error('expected tabindex removed after running->blocked, got ' + created.row.getAttribute('tabindex'));
if (created.row.getAttribute('aria-controls') !== null) throw new Error('expected aria-controls removed after running->blocked, got ' + created.row.getAttribute('aria-controls'));
if (created.row.getAttribute('aria-expanded') !== 'false') throw new Error('expected aria-expanded=false after running->blocked, got ' + created.row.getAttribute('aria-expanded'));
if (!created.row.classList.contains('row-non-expandable')) throw new Error('expected row-non-expandable after running->blocked');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_UpdateRunRowCells_QueuedToRunningRestoresToggleAttrs
// covers the inverse transition: a queued row whose state advances to
// running must regain the expand affordance on the next poll. This
// proves the helper is symmetric — the same code path that strips
// attrs on queued transition also adds them back when the row starts
// running.
func TestPortalDiff_UpdateRunRowCells_QueuedToRunningRestoresToggleAttrs(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 't-3', kind: 'active', status: 'queued', issueLabel: '#3', runId: 't-3',
};
const runNew = Object.assign({}, runOld, { status: 'running' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (created.row.classList.contains('row-non-expandable') !== true) throw new Error('expected pre-state queued row to have row-non-expandable');
if (created.row.getAttribute('data-action') !== null) throw new Error('expected pre-state queued row to lack data-action');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (created.row.getAttribute('data-action') !== 'toggle-run') throw new Error('expected data-action=toggle-run after queued->running, got ' + created.row.getAttribute('data-action'));
if (created.row.getAttribute('role') !== 'button') throw new Error('expected role=button after queued->running, got ' + created.row.getAttribute('role'));
if (created.row.getAttribute('tabindex') !== '0') throw new Error('expected tabindex=0 after queued->running, got ' + created.row.getAttribute('tabindex'));
if (created.row.getAttribute('aria-controls') !== 'run-detail-t-3') throw new Error('expected aria-controls=run-detail-t-3 after queued->running, got ' + created.row.getAttribute('aria-controls'));
if (created.row.classList.contains('row-non-expandable')) throw new Error('expected row-non-expandable removed after queued->running');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_DiffRuns_RunningToQueuedRemovesDetailRowUnderExpandedKey
// closes the second site of the detail-row guard. A row that was
// running and had its detail panel mounted (because expandedKey matched)
// must have that sibling detail <tr> removed when the row transitions
// to queued, even though expandedKey still matches the run identity.
// Without this, the user would see a stuck detail panel under a row
// they can no longer click.
func TestPortalDiff_DiffRuns_RunningToQueuedRemovesDetailRowUnderExpandedKey(t *testing.T) {
	js := `const body = makeMockBody();
const runRunning = {
  key: 'd-1', kind: 'active', status: 'running', issueLabel: '#1', runId: 'd-1',
};
const runQueued = Object.assign({}, runRunning, { status: 'queued' });
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: 'd-1', runs: [runRunning] };
SandmanPortalDiff.insertRunRow(body, runRunning, opts1);
const insertedDetail = body.querySelector('tr.detail-row[data-detail-for="d-1"]');
if (!insertedDetail) throw new Error('expected detail row inserted for running under expandedKey');
const opts2 = { helpers, stopGroups, expandedKey: 'd-1', runs: [runQueued] };
SandmanPortalDiff.diffRuns(body, [runQueued], opts2);
const leftover = body.querySelector('tr.detail-row[data-detail-for="d-1"]');
if (leftover) throw new Error('expected detail row removed after running->queued, got ' + leftover.outerHTML);
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_UpdateRunRowCells_QueuedToCompletedRestoresExpandable
// covers the queued->terminal transition. When a queued run completes
// it transitions out of the wait-state family and the user must be
// able to expand it to inspect the logs. The reconciliation must add
// the toggle affordance back AND remove the row-non-expandable class
// AND update the kind class (active->completed). Without the
// expandability-restoration logic a queued row that completes would
// stay non-clickable and the user would lose access to its output.
func TestPortalDiff_UpdateRunRowCells_QueuedToCompletedRestoresExpandable(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 't-4', kind: 'active', status: 'queued', issueLabel: '#4', runId: 't-4',
};
const runNew = Object.assign({}, runOld, { kind: 'completed', status: 'success' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (!created.row.classList.contains('row-non-expandable')) throw new Error('expected pre-state queued row to have row-non-expandable');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (created.row.getAttribute('data-action') !== 'toggle-run') throw new Error('expected data-action=toggle-run after queued->completed, got ' + created.row.getAttribute('data-action'));
if (created.row.getAttribute('role') !== 'button') throw new Error('expected role=button after queued->completed, got ' + created.row.getAttribute('role'));
if (created.row.getAttribute('tabindex') !== '0') throw new Error('expected tabindex=0 after queued->completed, got ' + created.row.getAttribute('tabindex'));
if (created.row.getAttribute('aria-controls') !== 'run-detail-t-4') throw new Error('expected aria-controls=run-detail-t-4 after queued->completed, got ' + created.row.getAttribute('aria-controls'));
if (created.row.classList.contains('row-non-expandable')) throw new Error('expected row-non-expandable removed after queued->completed');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_BuildDataRow_QueuedRowPreservesAbortButton covers the
// issue's invariant: removing the toggle affordance must NOT remove the
// Abort button. The Abort button lives in the actions cell and is
// matched by the click handler via button[data-action="abort-run"]
// BEFORE the row click handler, so it remains actionable even when the
// row click is suppressed. The Abort button is also reachable from
// keyboard because the keydown handler bails when the target is inside
// a button element. The row's data-action/role/tabindex removal only
// affects row-level click + Enter/Space activation.
func TestPortalDiff_BuildDataRow_QueuedRowPreservesAbortButton(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'q-2',
  kind: 'active',
  status: 'queued',
  issueLabel: '#7',
  issueNumber: 7,
  batchKey: 'b-7',
  runId: 'q-2',
};
const stopGroups = new Set();
const abortReservations = new Set();
const opts = { helpers, stopGroups, abortReservations, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
const actionsCell = created.row.querySelector('[data-cell="actions"]');
if (!actionsCell) throw new Error('expected actions cell on queued row');
const abortBtn = actionsCell.querySelector('button[data-action="abort-run"]');
if (!abortBtn) throw new Error('expected Abort button on abortable queued row, got actions cell: ' + JSON.stringify(actionsCell.children.map(c => c.tagName + ':' + (c.getAttribute && c.getAttribute('data-action')))));
if (abortBtn.textContent !== 'Abort') throw new Error('expected Abort label, got ' + abortBtn.textContent);
if (abortBtn.getAttribute('data-run-key') !== 'q-2') throw new Error('expected data-run-key=q-2 on Abort button');
if (abortBtn.getAttribute('data-issue') !== '7') throw new Error('expected data-issue=7 on Abort button');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_UpdateRunRowCells_RemovesStaleAriaControlsOnTransition
// locks in the explicit removal of aria-controls on running->queued.
// aria-controls points at the synthetic detail-row id; once the row is
// non-expandable the row has no detail row to control, so the stale
// pointer must be cleared. setAttr cannot remove an attribute on its
// own (it always writes), so this test guards against a regression
// where the helper might silently leave the attr behind when the
// row only transitions in one direction.
func TestPortalDiff_UpdateRunRowCells_RemovesStaleAriaControlsOnTransition(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = {
  key: 'a-1', kind: 'active', status: 'running', issueLabel: '#10', runId: 'a-1',
};
const runNew = Object.assign({}, runOld, { status: 'queued' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (created.row.getAttribute('aria-controls') !== 'run-detail-a-1') throw new Error('expected pre-state aria-controls=run-detail-a-1');
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
const after = created.row.getAttribute('aria-controls');
if (after !== null) throw new Error('expected aria-controls cleared on running->queued, got ' + after);
// And the row must not satisfy tr[data-action="toggle-run"] either:
if (created.row.getAttribute('data-action') === 'toggle-run') throw new Error('expected data-action removed too');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_UpdateRunRowCells_QueuedToQueuedIsNoOpOnToggleAttrs
// guards against accidental always-reapply regressions. If a queued row
// receives another update that keeps status queued, the row must NOT
// cycle its toggle attrs (which would inflate the mutation counter and
// waste DOM work on every poll). The reconciliation helper must be a
// true idempotent reconciliation, not a "set toggle attrs every tick"
// loop.
func TestPortalDiff_UpdateRunRowCells_QueuedToQueuedIsNoOpOnToggleAttrs(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'n-1', kind: 'active', status: 'queued', issueLabel: '#11', runId: 'n-1',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, run, run, opts);
const counters = SandmanPortalDiff.getCounters();
// Allow other reconcilers to mutate (badge label, etc.), but the toggle
// attrs must not appear in the log. We assert by inspecting the row's
// log for setAttribute('data-action', ...), setAttribute('role', ...),
// setAttribute('tabindex', ...), setAttribute('aria-controls', ...).
const rowLog = created.row._log || [];
function countAttrCalls(name) {
  let n = 0;
  for (const entry of rowLog) {
    if (Array.isArray(entry) && entry[0] === 'setAttribute' && entry[1] === name) n += 1;
  }
  return n;
}
if (countAttrCalls('data-action') !== 0) throw new Error('expected no data-action setAttribute on queued->queued no-op, got ' + countAttrCalls('data-action'));
if (countAttrCalls('role') !== 0) throw new Error('expected no role setAttribute on queued->queued no-op, got ' + countAttrCalls('role'));
if (countAttrCalls('tabindex') !== 0) throw new Error('expected no tabindex setAttribute on queued->queued no-op, got ' + countAttrCalls('tabindex'));
if (countAttrCalls('aria-controls') !== 0) throw new Error('expected no aria-controls setAttribute on queued->queued no-op, got ' + countAttrCalls('aria-controls'));
console.log('PASS counters=' + JSON.stringify(counters));
`
	runNodeScript(t, js)
}

// TestPortalDiff_InsertRunRow_QueuedRowWithMatchingExpandedKeyHasBothGuards
// is the coexistence regression test. matchesExpandedSubject returns
// true for a queued run whose runId equals the saved expandedKey (e.g.
// the user previously expanded this row before it re-queued). The row
// must end up with BOTH (a) row-non-expandable class AND (b)
// aria-expanded="false". Without both guards the user would either
// (a) see a clickable-looking row whose click is silently swallowed,
// or (b) see a row announced as "expanded" by the screen reader with
// no associated detail panel. The two guards must coexist on every
// insertion.
func TestPortalDiff_InsertRunRow_QueuedRowWithMatchingExpandedKeyHasBothGuards(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'c-1', kind: 'active', status: 'queued', issueLabel: '#12', runId: 'c-1',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'c-1', runs: [run] };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row.classList.contains('row-non-expandable')) throw new Error('expected row-non-expandable class on queued row inserted under matching expandedKey');
if (created.row.getAttribute('aria-expanded') !== 'false') throw new Error('expected aria-expanded=false on queued row inserted under matching expandedKey, got ' + created.row.getAttribute('aria-expanded'));
if (created.detailRow) throw new Error('expected no detail row for queued row even under matching expandedKey');
console.log('PASS');
`
	runNodeScript(t, js)
}
