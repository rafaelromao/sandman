package cmd

import (
	"encoding/json"
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
const runOld = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1', branch: 'main' };
const runNew = Object.assign({}, runOld, { status: 'success' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const startedCell = created.row.querySelector('[data-cell="started"]');
const durationCell = created.row.querySelector('[data-cell="duration"]');
const issueTitleCell = created.row.querySelector('[data-cell="issue-title"]');
const titleCell = created.row.querySelector('[data-cell="title"]');
clearLog(startedCell); clearLog(durationCell); clearLog(issueTitleCell); clearLog(titleCell);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true');
if (result.cells !== 3) throw new Error('expected 3 cell mutations on status change (remove old class, add new class, update label), got ' + JSON.stringify(result));
if (countLog(startedCell) !== 0) throw new Error('started cell was touched');
if (countLog(durationCell) !== 0) throw new Error('duration cell was touched');
if (countLog(issueTitleCell) !== 0) throw new Error('issue-title cell was touched');
if (countLog(titleCell) !== 0) throw new Error('title cell was touched');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_DoesNotRenderReviewBadgeInTitle(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'reviewing', review: true, issueLabel: 'Issue 1', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
const titleCell = created.row.querySelector('[data-cell="title"]');
if (!titleCell) throw new Error('expected title cell');
const titleWrap = titleCell.children[0];
if (!titleWrap) throw new Error('expected title wrap');
const chip = titleWrap.querySelector('.kind-chip');
if (chip) throw new Error('expected no kind chip when run.reason is empty, got chip with data-reason=' + chip.getAttribute('data-reason'));
if (titleWrap.children.length !== 2) throw new Error('expected title wrap to have only issue label and meta, got ' + titleWrap.children.length);
const statusCell = created.row.querySelector('[data-cell="badge"]');
const statusLabel = statusCell.querySelector('.badge-label');
if (!statusLabel || statusLabel.textContent !== 'reviewing') throw new Error('expected reviewing status badge, got ' + (statusLabel && statusLabel.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_AddsAutoSelectContextRowWhenReasonAppears(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
const runNew = Object.assign({}, runOld, { issueLabel: 'Issue 1 updated', reason: 'auto-select', candidates: [42] });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (body.querySelector('tr.context-row[data-context-for="a"]')) throw new Error('expected no context row before reason appears');
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true');
const ctxRow = body.querySelector('tr.context-row[data-context-for="a"]');
if (!ctxRow) throw new Error('expected context row when reason becomes auto-select');
const chip = ctxRow.querySelector('.context-chip');
if (!chip) throw new Error('expected context chip');
if (!chip.textContent.includes('Auto-select candidates: #42')) throw new Error('expected auto-select chip text');
const titleWrap = created.row.querySelector('[data-cell="title"]').children[0];
if (titleWrap.children[0].textContent !== 'Issue 1 updated') throw new Error('expected updated issue label');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_RendersRetryAndReviewSummaryAndDropsGroupedChips(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'issue-1', issueNumber: 1, retriesDone: 3, retriesTotal: 3, reviewCount: 2, reviewVerdict: 'Approved', batchIssues: [1, 2, 3] };
const groupedReview = { key: 'PR42', kind: 'active', status: 'reviewing', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, reason: 'review', groupedReview: true };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const createdParent = SandmanPortalDiff.insertRunRow(body, parentRun, opts);
const parentMeta = createdParent.row.querySelector('[data-cell="title"]').children[0].children[1];
if (!parentMeta.textContent.includes('3 retries')) throw new Error('expected retry count in meta, got ' + parentMeta.textContent);
if (!parentMeta.textContent.includes('2 reviews')) throw new Error('expected review count in meta, got ' + parentMeta.textContent);
if (!parentMeta.textContent.includes('Approved')) throw new Error('expected latest verdict in meta, got ' + parentMeta.textContent);
if (parentMeta.textContent.includes('RunID')) throw new Error('expected RunID label removed from meta, got ' + parentMeta.textContent);
if (!parentMeta.textContent.includes('\n3 retries - 2 reviews - Approved')) throw new Error('expected retry summary before reviews on new line, got ' + JSON.stringify(parentMeta.textContent));
const createdGrouped = SandmanPortalDiff.insertRunRow(body, groupedReview, opts);
if (body.querySelector('tr.context-row[data-context-for="PR42"]')) throw new Error('expected grouped review chip removed');
const groupedTitle = createdGrouped.row.querySelector('[data-cell="title"]').children[0];
if (groupedTitle.children.length !== 2) throw new Error('expected grouped review title to keep label and meta only');

const singularRun = { key: 'issue-2', kind: 'active', status: 'reviewing', issueLabel: '#2', runId: 'issue-2', issueNumber: 2, retriesDone: 1, retriesTotal: 1 };
SandmanPortalDiff.insertRunRow(body, singularRun, opts);
const singularMeta = body.querySelector('tr[data-run-key="issue-2"]').querySelector('[data-cell="title"]').children[0].children[1];
if (!singularMeta.textContent.includes('1 retry')) throw new Error('expected singular retry label, got ' + singularMeta.textContent);
if (singularMeta.textContent.includes('retriy')) throw new Error('misspelling "retriy" must not appear, got ' + singularMeta.textContent);
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestRenderRunMeta_StackedBatchWithRetrySummary is the legacy #1348 layout
// test, updated for issue #1483 to assert the counter line lands below Run:
// rather than on the Batch: line.
func TestRenderRunMeta_StackedBatchWithRetrySummary(t *testing.T) {
	js := `const run = { key: 'run-2', kind: 'active', status: 'reviewing', issueLabel: '#43', runId: 'b1c2d', issueNumber: 43, batchKey: 'batch-xyz', retriesDone: 2, retriesTotal: 2, reviewCount: 1, reviewVerdict: 'Approved' };
const meta = helpers.renderRunMeta(run);
const lines = meta.split(String.fromCharCode(10));
if (lines.length !== 3) throw new Error('expected exactly 3 lines (Batch, Run, counter line) for fully-loaded, got: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: batch-xyz') throw new Error('expected batch line first, got: ' + JSON.stringify(lines));
if (lines[1] !== 'Run: b1c2d') throw new Error('expected Run line second, got: ' + JSON.stringify(lines));
if (lines[2] !== '2 retries - 1 review - Approved') throw new Error('expected joined counters on trailing line, got: ' + JSON.stringify(lines));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_StackedBatchOnly is the legacy #1348 batch-only test,
// updated for issue #1483 to confirm the two-line layout when there are no
// counters (Batch above Run, no trailing blank line).
func TestRenderRunMeta_StackedBatchOnly(t *testing.T) {
	js := `const run = { key: 'run-1', kind: 'active', status: 'running', issueLabel: '#42', runId: 'a0c19', issueNumber: 42, batchKey: 'batch-abc' };
const meta = helpers.renderRunMeta(run);
const lines = meta.split(String.fromCharCode(10));
if (lines.length !== 2) throw new Error('expected exactly 2 lines (Batch, Run) for batch-only row, got: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: batch-abc') throw new Error('expected Batch line first, got: ' + JSON.stringify(lines));
if (lines[1] !== 'Run: a0c19') throw new Error('expected Run line second, got: ' + JSON.stringify(lines));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_BatchOnlyNoCounters_OmitsTrailingBlankLine is the
// regression guard for issue #1483: when a row has a batch key but neither
// retriesDone nor reviewCount > 0, the meta must be exactly two lines
// (Batch: and Run:) with no trailing blank line and no extra text.
func TestRenderRunMeta_BatchOnlyNoCounters_OmitsTrailingBlankLine(t *testing.T) {
	js := `const run = { key: 'run-no-counters', kind: 'active', status: 'running', issueLabel: '#99', runId: 'no-ctr', issueNumber: 99, batchKey: 'batch-empty', retriesDone: 0, reviewCount: 0 };
const meta = helpers.renderRunMeta(run);
const lines = meta.split('\\n');
if (lines.length !== 2) throw new Error('expected exactly 2 lines when no counters present, got ' + lines.length + ' lines: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: batch-empty') throw new Error('expected Batch line first, got: ' + JSON.stringify(lines));
if (lines[1] !== 'Run: no-ctr') throw new Error('expected Run line second, got: ' + JSON.stringify(lines));
if (/^\s*$/.test(meta)) throw new Error('meta must not be blank, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_RetryOnlyMovesToTrailingLine asserts that a row with
// only retriesDone > 0 (no batch key, no review) renders two lines:
// Run: ... and the retry counter on the trailing line.
func TestRenderRunMeta_RetryOnlyMovesToTrailingLine(t *testing.T) {
	js := `const run = { key: 'no-batch', kind: 'active', status: 'running', issueLabel: '#1', runId: 'r-only', issueNumber: 1, retriesDone: 3 };
const meta = helpers.renderRunMeta(run);
const lines = meta.split('\\n');
if (lines.length !== 2) throw new Error('expected exactly 2 lines for no-batch + retry, got: ' + JSON.stringify(lines));
if (lines[0] !== 'Run: r-only') throw new Error('expected Run line first, got: ' + JSON.stringify(lines));
if (lines[1] !== '3 retries') throw new Error('expected "3 retries" on trailing line, got: ' + JSON.stringify(lines));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_BatchPlusRetry_RendersThreeLines asserts the new
// three-line layout: Batch:, Run:, and the retry counter on a trailing line.
func TestRenderRunMeta_BatchPlusRetry_RendersThreeLines(t *testing.T) {
	js := `const run = { key: 'b-retry', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'br1', issueNumber: 1, batchKey: 'batch-br', retriesDone: 2 };
const meta = helpers.renderRunMeta(run);
const lines = meta.split('\\n');
if (lines.length !== 3) throw new Error('expected exactly 3 lines (Batch, Run, retry counter), got: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: batch-br') throw new Error('expected Batch line first, got: ' + JSON.stringify(lines));
if (lines[1] !== 'Run: br1') throw new Error('expected Run line second, got: ' + JSON.stringify(lines));
if (lines[2] !== '2 retries') throw new Error('expected "2 retries" on trailing line, got: ' + JSON.stringify(lines));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_BatchPlusReviewWithVerdict_RendersThreeLines asserts the
// review counter plus verdict live on the trailing counter line (joined by
// " - "), not on the Batch: line.
func TestRenderRunMeta_BatchPlusReviewWithVerdict_RendersThreeLines(t *testing.T) {
	js := `const run = { key: 'b-review', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'brev', issueNumber: 1, batchKey: 'batch-bv', reviewCount: 1, reviewVerdict: 'Approved' };
const meta = helpers.renderRunMeta(run);
const lines = meta.split('\\n');
if (lines.length !== 3) throw new Error('expected exactly 3 lines (Batch, Run, review+verdict), got: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: batch-bv') throw new Error('expected Batch line first, got: ' + JSON.stringify(lines));
if (lines[1] !== 'Run: brev') throw new Error('expected Run line second, got: ' + JSON.stringify(lines));
if (lines[2] !== '1 review - Approved') throw new Error('expected "1 review - Approved" on trailing line, got: ' + JSON.stringify(lines));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_FullyLoaded_TrailingLineJoinsAllCounters asserts the
// canonical issue example: Batch:, Run:, then "2 retries - 1 review - Approved"
// joined with " - ".
func TestRenderRunMeta_FullyLoaded_TrailingLineJoinsAllCounters(t *testing.T) {
	js := `const run = { key: 'full', kind: 'active', status: 'reviewing', issueLabel: '#43', runId: 'b1c2d', issueNumber: 43, batchKey: 'batch-xyz', retriesDone: 2, retriesTotal: 2, reviewCount: 1, reviewVerdict: 'Approved' };
const meta = helpers.renderRunMeta(run);
const lines = meta.split('\\n');
if (lines.length !== 3) throw new Error('expected exactly 3 lines for fully-loaded, got: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: batch-xyz') throw new Error('expected Batch line first, got: ' + JSON.stringify(lines));
if (lines[1] !== 'Run: b1c2d') throw new Error('expected Run line second, got: ' + JSON.stringify(lines));
if (lines[2] !== '2 retries - 1 review - Approved') throw new Error('expected joined counters on trailing line, got: ' + JSON.stringify(lines));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_SingularPluralLabelsPreserved locks in the
// singular/plural wording for retry (1 retry vs 2 retries) and review
// (1 review vs 2 reviews).
func TestRenderRunMeta_SingularPluralLabelsPreserved(t *testing.T) {
	js := `const oneRetry = { key: 'r1', kind: 'active', status: 'running', issueLabel: '#1', runId: 'r1id', issueNumber: 1, retriesDone: 1 };
const m1 = helpers.renderRunMeta(oneRetry);
if (!m1.includes('1 retry')) throw new Error('expected "1 retry" for count=1, got: ' + JSON.stringify(m1));
if (m1.includes('1 retries')) throw new Error('must not show "1 retries", got: ' + JSON.stringify(m1));

const twoRetries = { key: 'r2', kind: 'active', status: 'running', issueLabel: '#2', runId: 'r2id', issueNumber: 2, retriesDone: 2 };
const m2 = helpers.renderRunMeta(twoRetries);
if (!m2.includes('2 retries')) throw new Error('expected "2 retries" for count=2, got: ' + JSON.stringify(m2));

const oneReview = { key: 'v1', kind: 'active', status: 'reviewing', issueLabel: '#3', runId: 'v1id', issueNumber: 3, reviewCount: 1 };
const m3 = helpers.renderRunMeta(oneReview);
if (!m3.includes('1 review')) throw new Error('expected "1 review" for count=1, got: ' + JSON.stringify(m3));
if (m3.includes('1 reviews')) throw new Error('must not show "1 reviews", got: ' + JSON.stringify(m3));

const twoReviews = { key: 'v2', kind: 'active', status: 'reviewing', issueLabel: '#4', runId: 'v2id', issueNumber: 4, reviewCount: 2 };
const m4 = helpers.renderRunMeta(twoReviews);
if (!m4.includes('2 reviews')) throw new Error('expected "2 reviews" for count=2, got: ' + JSON.stringify(m4));

console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalDiffCreateRunRow_BatchAndRetryOnSeparateLines asserts the DOM
// row produced by SandmanPortalDiff.insertRunRow renders the new three-line
// meta: Batch:, Run:, then the trailing counter line.
func TestPortalDiffCreateRunRow_BatchAndRetryOnSeparateLines(t *testing.T) {
	js := `const body = makeMockBody();
const batchOnlyRun = { key: 'run-1', kind: 'active', status: 'running', issueLabel: '#42', runId: 'a0c19', issueNumber: 42, batchKey: 'batch-abc' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, batchOnlyRun, opts);
const meta = created.row.querySelector('[data-cell="title"]').children[0].children[1];
const lines = meta.textContent.split(String.fromCharCode(10));
if (lines.length !== 2) throw new Error('expected exactly 2 lines (Batch, Run) for batch-only row, got: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: batch-abc') throw new Error('expected Batch line first, got: ' + JSON.stringify(meta.textContent));
if (lines[1] !== 'Run: a0c19') throw new Error('expected Run line second, got: ' + JSON.stringify(meta.textContent));

const batchWithRetry = { key: 'run-2', kind: 'active', status: 'reviewing', issueLabel: '#43', runId: 'b1c2d', issueNumber: 43, batchKey: 'batch-xyz', retriesDone: 2, retriesTotal: 2, reviewCount: 1, reviewVerdict: 'Approved' };
SandmanPortalDiff.insertRunRow(body, batchWithRetry, opts);
const metaWithRetry = body.querySelector('tr[data-run-key="run-2"]').querySelector('[data-cell="title"]').children[0].children[1];
const retryLines = metaWithRetry.textContent.split(String.fromCharCode(10));
if (retryLines.length !== 3) throw new Error('expected exactly 3 lines for fully-loaded row, got: ' + JSON.stringify(retryLines));
if (retryLines[0] !== 'Batch: batch-xyz') throw new Error('expected Batch line first, got: ' + JSON.stringify(metaWithRetry.textContent));
if (retryLines[1] !== 'Run: b1c2d') throw new Error('expected Run line second, got: ' + JSON.stringify(metaWithRetry.textContent));
if (retryLines[2] !== '2 retries - 1 review - Approved') throw new Error('expected joined counters on trailing line, got: ' + JSON.stringify(metaWithRetry.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_NoArchivedColumnOrBadge(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'archived-1', kind: 'completed', status: 'success', archived: true, issueLabel: '#1', runId: 'archived-1', issueNumber: 1 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (created.row.querySelector('[data-cell="archived"]')) throw new Error('archived column must not exist');
for (const cell of created.row.querySelectorAll('td, th')) {
  if (cell.textContent.includes('Archived')) throw new Error('archived badge text must not appear anywhere');
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_DurationChangeUpdatesOnlyDuration(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1', branch: 'main', duration: '5s' };
const runNew = Object.assign({}, runOld, { duration: '12s' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const badgeCell = created.row.querySelector('[data-cell="badge"]');
const startedCell = created.row.querySelector('[data-cell="started"]');
clearLog(badgeCell); clearLog(startedCell);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true');
if (result.cells !== 1) throw new Error('expected 1 cell mutation on duration change, got ' + JSON.stringify(result));
if (countLog(badgeCell) !== 0) throw new Error('badge cell was touched');
if (countLog(startedCell) !== 0) throw new Error('started cell was touched');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_TitleChangeUpdatesOnlyTitle(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1', branch: 'main' };
const runNew = Object.assign({}, runOld, { issueLabel: 'Issue 1 updated' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const badgeCell = created.row.querySelector('[data-cell="badge"]');
const durationCell = created.row.querySelector('[data-cell="duration"]');
clearLog(badgeCell); clearLog(durationCell);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true');
if (result.cells !== 1) throw new Error('expected 1 cell mutation on title change, got ' + JSON.stringify(result));
if (countLog(badgeCell) !== 0) throw new Error('badge cell was touched');
if (countLog(durationCell) !== 0) throw new Error('duration cell was touched');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_KindChangeUpdatesRowClass(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
const runNew = Object.assign({}, runOld, { kind: 'completed' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true');
if (!created.row.classList.contains('completed')) throw new Error('row should have completed class');
if (created.row.classList.contains('active')) throw new Error('row should not have active class');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_AriaExpandedChangeUpdatesRowAttr(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1', branch: 'main' };
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
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1 };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const retryRun = { key: 'retry-1', kind: 'active', status: 'running', issueLabel: 'Issue 1 retry', runId: 'retry-1', issueNumber: 1 };
const run = parentRun;
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'issue-1', runs: [parentRun, retryRun, childReview] };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row) throw new Error('expected data row');
if (!created.detailRow) throw new Error('expected detail row when expandedKey matches');
if (created.detailRow.getAttribute('data-detail-for') !== 'issue-1') throw new Error('detail row has wrong data-detail-for');
if (created.row.getAttribute('id') !== 'run-row-issue-1') throw new Error('expected stable row id, got ' + created.row.getAttribute('id'));
if (created.row.getAttribute('aria-controls') !== 'run-detail-issue-1') throw new Error('expected aria-controls=run-detail-issue-1, got ' + created.row.getAttribute('aria-controls'));
if (created.detailRow.getAttribute('id') !== 'run-detail-issue-1') throw new Error('expected stable detail id, got ' + created.detailRow.getAttribute('id'));
if (created.detailRow.getAttribute('role') !== 'region') throw new Error('expected detail row role=region');
if (created.detailRow.getAttribute('aria-labelledby') !== 'run-row-issue-1') throw new Error('expected detail row aria-labelledby run-row-issue-1, got ' + created.detailRow.getAttribute('aria-labelledby'));
if (body.children.length !== 2) throw new Error('expected body to have 2 children, got ' + body.children.length);
const subjectSelect = created.detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect) throw new Error('expected subject selector in detail row');
if (subjectSelect.children.length !== 2) throw new Error('expected selector for parent and one child review, got ' + subjectSelect.children.length);
if (subjectSelect.children[0].getAttribute('value') !== 'issue-1') throw new Error('expected parent option value to use run ID, got ' + subjectSelect.children[0].getAttribute('value'));
if (subjectSelect.children[1].getAttribute('value') !== 'PR42') throw new Error('expected child review option value to use run ID, got ' + subjectSelect.children[1].getAttribute('value'));
if (subjectSelect.children[0].textContent !== 'issue-1') throw new Error('expected parent label to be run ID only, got ' + subjectSelect.children[0].textContent);
if (subjectSelect.children[1].textContent !== 'PR42') throw new Error('expected review label to be run ID only, got ' + subjectSelect.children[1].textContent);
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

func TestPortalDiffCreateRunRow_ExpandedChildReviewKeepsVisibleParentRow(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1 };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR42', runs: [parentRun, childReview], visibleRuns: [parentRun] };
const created = SandmanPortalDiff.insertRunRow(body, parentRun, opts);
if (!created.detailRow) throw new Error('expected detail row when child review is expanded');
if (body.querySelector('tr[data-run-key="PR42"]')) throw new Error('expected hidden child review row not to render');
if (body.querySelector('tr[data-run-key="issue-1"]') !== created.row) throw new Error('expected visible parent row to remain rendered');
const subjectSelect = created.detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect || subjectSelect.children.length !== 2) throw new Error('expected parent row detail selector to include child review');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffDiffRuns_OrphanReviewUsesRealRunIDForVisibleRow replaces
// TestPortalDiffDiffRuns_HidesReviewRowsAndShowsStubForOrphanReviews after
// issue #1489 removed the synthetic review stub.
func TestPortalDiffDiffRuns_OrphanReviewUsesRealRunIDForVisibleRow(t *testing.T) {
	js := `const body = makeMockBody();
const review = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR42', runs: [review], visibleRuns: [review] };
const result = SandmanPortalDiff.diffRuns(body, [review], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
if (body.querySelector('tr[data-run-key="review-stub-1"]')) throw new Error('expected no synthetic stub row with placeholder key');
const visibleRow = body.querySelector('tr[data-run-key="PR42"]');
if (!visibleRow) throw new Error('expected orphan review visible under its real RunID, got ' + body.innerHTML);
const detailRow = body.querySelector('tr.detail-row[data-detail-for="PR42"]');
if (!detailRow) throw new Error('expected detail row keyed by the orphan review RunID');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect || subjectSelect.children.length !== 1) throw new Error('expected exactly one option for single orphan review, got ' + (subjectSelect && subjectSelect.children.length));
if (subjectSelect.children[0].getAttribute('value') !== 'PR42') throw new Error('expected picker option to match review RunID, got ' + subjectSelect.children[0].getAttribute('value'));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_SubjectSelectorIncludesDistinctParentAndReview(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', review: false, issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1 };
const review = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'issue-1', runs: [parentRun, review], visibleRuns: [parentRun] };
const result = SandmanPortalDiff.diffRuns(body, [parentRun, review], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
const detailRow = body.querySelector('tr.detail-row[data-detail-for="issue-1"]');
if (!detailRow) throw new Error('expected detail row for parent run');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect) throw new Error('expected subject selector for parent run');
if (subjectSelect.children.length !== 2) throw new Error('expected parent and review options, got ' + subjectSelect.children.length);
if (subjectSelect.children[0].getAttribute('value') !== 'issue-1') throw new Error('expected parent option to use the parent run id, got ' + subjectSelect.children[0].getAttribute('value'));
if (subjectSelect.children[1].getAttribute('value') !== 'PR42') throw new Error('expected review option to use the review run id, got ' + subjectSelect.children[1].getAttribute('value'));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_SubjectSelectorSkipsQueuedAndBlockedPlaceholders(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', review: false, issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1 };
const review = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const queued = { key: 'queued-1', kind: 'active', status: 'queued', review: false, issueLabel: '#1 queued', runId: 'queued-1', issueNumber: 1 };
const blocked = { key: 'blocked-1', kind: 'active', status: 'blocked', review: false, issueLabel: '#1 blocked', runId: 'blocked-1', issueNumber: 1 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'issue-1', runs: [parentRun, review, queued, blocked], visibleRuns: [parentRun] };
const result = SandmanPortalDiff.diffRuns(body, [parentRun], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
const detailRow = body.querySelector('tr.detail-row[data-detail-for="issue-1"]');
if (!detailRow) throw new Error('expected detail row for parent run');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect) throw new Error('expected subject selector for parent run');
if (subjectSelect.children.length !== 2) throw new Error('expected parent and review options only, got ' + subjectSelect.children.length);
const values = Array.from(subjectSelect.children).map((opt) => opt.getAttribute('value'));
if (values.indexOf('issue-1') < 0 || values.indexOf('PR42') < 0) throw new Error('expected parent and review subjects, got ' + JSON.stringify(values));
if (values.indexOf('queued-1') >= 0 || values.indexOf('blocked-1') >= 0) throw new Error('expected queued/blocked placeholders to be filtered, got ' + JSON.stringify(values));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_RefreshesSubjectSelectorWhenChildReviewAppears(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1 };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: 'issue-1', runs: [parentRun] };
const created = SandmanPortalDiff.insertRunRow(body, parentRun, opts1);
const subjectSelectBefore = created.detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelectBefore) throw new Error('expected subject selector before refresh');
if (subjectSelectBefore.children.length !== 1) throw new Error('expected single parent option before child review appears, got ' + subjectSelectBefore.children.length);
SandmanPortalDiff.resetCounters();
const opts2 = { helpers, stopGroups, expandedKey: 'issue-1', runs: [parentRun, childReview] };
SandmanPortalDiff.diffRuns(body, [parentRun], opts2);
const subjectSelectAfter = created.detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelectAfter) throw new Error('expected subject selector after refresh');
if (subjectSelectAfter.children.length !== 2) throw new Error('expected parent plus child review options after refresh, got ' + subjectSelectAfter.children.length);
if (subjectSelectAfter.children[1].getAttribute('value') !== 'PR42') throw new Error('expected child review option to use RunID PR42 after refresh, got ' + subjectSelectAfter.children[1].getAttribute('value'));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffDiffRuns_OrphanReviewRendersSingleSubjectOption is the
// single-review regression for issue #1489.
func TestPortalDiffDiffRuns_OrphanReviewRendersSingleSubjectOption(t *testing.T) {
	js := `const body = makeMockBody();
const review = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR42', runs: [review], visibleRuns: [review] };
const result = SandmanPortalDiff.diffRuns(body, [review], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
const visibleRow = body.querySelector('tr[data-run-key="PR42"]');
if (!visibleRow) throw new Error('expected review row visible under its real RunID, got ' + body.innerHTML);
const detailRow = body.querySelector('tr.detail-row[data-detail-for="PR42"]');
if (!detailRow) throw new Error('expected detail row for the orphan review');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect) throw new Error('expected subject selector on orphan review detail row');
if (subjectSelect.children.length !== 1) throw new Error('expected exactly one option on orphan review picker, got ' + subjectSelect.children.length);
if (subjectSelect.children[0].getAttribute('value') !== 'PR42') throw new Error('expected orphan review option to use the review RunID, got ' + subjectSelect.children[0].getAttribute('value'));
if (subjectSelect.value !== 'PR42') throw new Error('expected picker default value to match the review RunID, got ' + subjectSelect.value);
if (subjectSelect.children[0].getAttribute('selected') !== 'selected') throw new Error('expected orphan review option to be selected by default');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffDiffRuns_MultipleOrphanReviewsRenderOneOptionPerRun is the
// multi-review regression for issue #1489.
func TestPortalDiffDiffRuns_MultipleOrphanReviewsRenderOneOptionPerRun(t *testing.T) {
	js := `const body = makeMockBody();
const terminal = { key: 'PR41', kind: 'completed', status: 'success', review: true, issueLabel: 'PR41', runId: 'PR41', issueNumber: 1, prNumber: 41, startedAt: '2026-06-29T10:00:00Z' };
const live = { key: 'PR42', kind: 'active', status: 'reviewing', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, startedAt: '2026-06-30T10:00:00Z' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR42', runs: [terminal, live], visibleRuns: [live] };
const result = SandmanPortalDiff.diffRuns(body, [terminal, live], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
const visibleRow = body.querySelector('tr[data-run-key="PR42"]');
if (!visibleRow) throw new Error('expected most-recent review row visible, got ' + body.innerHTML);
if (body.querySelector('tr[data-run-key="PR41"]')) throw new Error('expected older review row hidden under grouped review');
const detailRow = body.querySelector('tr.detail-row[data-detail-for="PR42"]');
if (!detailRow) throw new Error('expected detail row for most-recent review');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect) throw new Error('expected subject selector on multi-review detail row');
if (subjectSelect.children.length !== 2) throw new Error('expected exactly one option per review, got ' + subjectSelect.children.length);
const values = Array.from(subjectSelect.children).map((opt) => opt.getAttribute('value'));
if (values.indexOf('PR41') < 0 || values.indexOf('PR42') < 0) throw new Error('expected both review RunIDs as picker options, got ' + JSON.stringify(values));
if (subjectSelect.value !== 'PR42') throw new Error('expected picker to default to the visible review RunID, got ' + subjectSelect.value);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_NoDetailRowWhenNotExpanded(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1', branch: 'main' };
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
const runA = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1' };
const runB = { key: 'b', kind: 'active', status: 'running', issueLabel: 'B', runId: 'r2' };
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
const runA = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1' };
const runB = { key: 'b', kind: 'active', status: 'running', issueLabel: 'B', runId: 'r2' };
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
  { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1' },
  { key: 'b', kind: 'active', status: 'running', issueLabel: 'B', runId: 'r2' },
  { key: 'c', kind: 'active', status: 'running', issueLabel: 'C', runId: 'r3' },
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
  { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1' },
  { key: 'b', kind: 'active', status: 'running', issueLabel: 'B', runId: 'r2' },
  { key: 'c', kind: 'active', status: 'running', issueLabel: 'C', runId: 'r3' },
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1' };
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1' };
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

func TestPortalDiffDiffRuns_AbortButtonsAllowedOnSharedSocketRows(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', issueNumber: 41, socketPath: '/tmp/sock', batchKey: 'run-41-1' },
  { key: 'b', kind: 'active', status: 'queued', issueLabel: 'B', runId: 'r2', issueNumber: 42, socketPath: '/tmp/sock', batchKey: 'run-42-1' },
];
const abortReservations = new Set();
const opts = { helpers, abortReservations, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const aRow = body.children[0];
const bRow = body.children[1];
const aBtn = aRow.querySelector('button[data-action="abort-run"]');
const bBtn = bRow.querySelector('button[data-action="abort-run"]');
if (!aBtn) throw new Error('a (active) should have abort button');
if (!bBtn) throw new Error('b (queued) should also have abort button even on same socket');
const bBadge = bRow.querySelector('[data-cell="badge"]').children[0];
if (!bBadge.classList.contains('queued')) throw new Error('b (queued) should have queued badge class');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffStatusClass_AbortedReturnsAborted(t *testing.T) {
	js := `const result = helpers.statusClass({ status: 'aborted' });
if (result !== 'aborted') throw new Error('expected statusClass to return aborted, got ' + result);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildBadgeCell_AbortedHasBadgeAbortedClasses(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'aborted', issueLabel: 'A', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const badgeCell = row.querySelector('[data-cell="badge"]');
if (!badgeCell) throw new Error('expected badge cell');
const badge = badgeCell.children[0];
if (!badge) throw new Error('expected badge span');
if (!badge.classList.contains('badge')) throw new Error('expected badge class');
if (!badge.classList.contains('aborted')) throw new Error('expected aborted class');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildBadgeCell_RetryChipRemovedWhenRetriesDoneGreaterThanZero(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: '#42', runId: 'r1', retriesTotal: 3, retriesDone: 2 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const badgeCell = row.querySelector('[data-cell="badge"]');
if (!badgeCell) throw new Error('expected badge cell');
const chip = badgeCell.querySelector('.retry-chip');
if (chip) throw new Error('expected NO .retry-chip in badge cell when retriesDone=2, got ' + JSON.stringify(chip.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildBadgeCell_RetryChipRemovedWhenRetriesDoneIsOne(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: '#42', runId: 'r1', retriesTotal: 1, retriesDone: 1 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const badgeCell = row.querySelector('[data-cell="badge"]');
const chip = badgeCell.querySelector('.retry-chip');
if (chip) throw new Error('expected NO .retry-chip in badge cell when retriesDone=1, got ' + JSON.stringify(chip.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildBadgeCell_RetryChipAbsentWhenRetriesDoneZero(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: '#42', runId: 'r1', retriesTotal: 0, retriesDone: 0 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const badgeCell = row.querySelector('[data-cell="badge"]');
const chip = badgeCell.querySelector('.retry-chip');
if (chip) throw new Error('expected NO .retry-chip in badge cell when retriesDone=0, got text: ' + JSON.stringify(chip.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateDetailContent_StreamedLogIsShieldedFromPoll pins the
// seam that lets a live SSE stream own a Log <pre>: while a run's key is in
// opts.streamingKeys, the poll path (updateDetailContent) must not touch
// the streamed pre, even as run.log changes. Once the key is removed, the
// poll resumes authority and reconciles the log normally.
func TestPortalDiffUpdateDetailContent_StreamedLogIsShieldedFromPoll(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r1', log: 'first line' };
const streamingKeys = new Set();
const opts = { helpers, stopGroups: new Set(), expandedKey: 'a', tabs: { a: 'log' }, streamingKeys };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.querySelector('tr.detail-row[data-detail-for="a"]');
if (!detailRow) throw new Error('expected a detail row for the expanded run');
const pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected a log pre for the expanded run');
const before = pre.getAttribute('data-rendered-log') || '';

// Stream takes ownership; a poll with a longer log must NOT clobber the pre.
streamingKeys.add('a');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [Object.assign({}, run, { log: 'first line\nsecond line' })], opts);
const guarded = pre.getAttribute('data-rendered-log') || '';
if (guarded !== before) throw new Error('streamed log pre was clobbered by poll (before=' + JSON.stringify(before) + ', after=' + JSON.stringify(guarded) + ')');

// Stream releases; the poll resumes and appends the new line.
streamingKeys.delete('a');
SandmanPortalDiff.diffRuns(body, [Object.assign({}, run, { log: 'first line\nsecond line' })], opts);
const resumed = pre.getAttribute('data-rendered-log') || '';
if (resumed === before) throw new Error('poll did not resume updating the log after the stream released it');
if (!/second line/.test(resumed)) throw new Error('resumed log missing the new line, got ' + JSON.stringify(resumed));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildDurationCell_StaleLineWarnPast180s(t *testing.T) {
	js := `const body = makeMockBody();
const lastOutputAt = new Date(Date.now() - 200*1000).toISOString();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r1', lastOutputAt, duration: 1200 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const durationCell = row.querySelector('[data-cell="duration"]');
if (!durationCell) throw new Error('expected duration cell');
const line = durationCell.querySelector('.stale-line');
if (!line) throw new Error('expected .stale-line.warn for an active run idle 200s');
if (!line.classList.contains('warn')) throw new Error('expected warn tier (>=180s) to add .warn class');
if (!/^stale \u00b7 /.test(line.textContent)) throw new Error('expected "stale \u00b7 ..." text, got ' + JSON.stringify(line.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildDurationCell_StaleLineMutedBetween60sAnd180s(t *testing.T) {
	js := `const body = makeMockBody();
const lastOutputAt = new Date(Date.now() - 90*1000).toISOString();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r1', lastOutputAt, duration: 1200 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const durationCell = row.querySelector('[data-cell="duration"]');
const line = durationCell.querySelector('.stale-line');
if (!line) throw new Error('expected .stale-line for an active run idle 90s');
if (line.classList.contains('warn')) throw new Error('90s must NOT be warn tier (only >=180s)');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildDurationCell_NoStaleLineUnder60sOrForCompletedRows(t *testing.T) {
	js := `const body = makeMockBody();
const fresh = new Date(Date.now() - 10*1000).toISOString();
// active but fresh → no line
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, { key: 'a', kind: 'active', status: 'running', issueLabel: '#1', runId: 'r1', lastOutputAt: fresh, duration: 1200 }, opts);
let line = body.children[0].querySelector('[data-cell="duration"]').querySelector('.stale-line');
if (line) throw new Error('expected NO .stale-line for an active run idle only 10s');
// completed run even with lastOutputAt → no line (staleness is active-only)
const old = new Date(Date.now() - 999*1000).toISOString();
SandmanPortalDiff.insertRunRow(body, { key: 'b', kind: 'completed', status: 'success', issueLabel: '#2', runId: 'r2', lastOutputAt: old, duration: 1200 }, opts);
line = body.children[1].querySelector('[data-cell="duration"]').querySelector('.stale-line');
if (line) throw new Error('expected NO .stale-line for a completed row even when lastOutputAt present');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateCells_StaleLineTransitionsOnLastOutputAtChange pins
// the reconciliation path in updateDurationCell: a fresh→stale transition
// adds the line, and a stale→fresh transition (new output resumed) removes
// it, without rebuilding the whole duration cell.
func TestPortalDiffUpdateCells_StaleLineTransitionsOnLastOutputAtChange(t *testing.T) {
	js := `const body = makeMockBody();
const fresh = new Date(Date.now() - 5*1000).toISOString();
const stale = new Date(Date.now() - 300*1000).toISOString();
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const runFresh = { key: 'a', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r1', lastOutputAt: fresh, duration: 1200 };
const runStale = Object.assign({}, runFresh, { lastOutputAt: stale });
const created = SandmanPortalDiff.insertRunRow(body, runFresh, opts);
const durationCell = created.row.querySelector('[data-cell="duration"]');
if (durationCell.querySelector('.stale-line')) throw new Error('fresh run must not start with a stale line');

// fresh → stale: line (warn) appears
SandmanPortalDiff.updateRunRowCells(created.row, runFresh, runStale, opts);
let line = durationCell.querySelector('.stale-line');
if (!line || !line.classList.contains('warn')) throw new Error('fresh->stale transition must add a warn stale line');

// stale → fresh: line removed (run produced output again)
SandmanPortalDiff.updateRunRowCells(created.row, runStale, runFresh, opts);
line = durationCell.querySelector('.stale-line');
if (line) throw new Error('stale->fresh transition must remove the stale line');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildDurationCell_NoStaleLineForQueuedStatus(t *testing.T) {
	js := `const body = makeMockBody();
const stale = new Date(Date.now() - 90*1000).toISOString();
const run = { key: 'a', kind: 'active', status: 'queued', issueLabel: '#42', runId: 'r1', lastOutputAt: stale, duration: 1200 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const durationCell = row.querySelector('[data-cell="duration"]');
const line = durationCell.querySelector('.stale-line');
if (line) throw new Error('expected NO .stale-line for queued status even when lastOutputAt is stale');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildDurationCell_NoStaleLineForBlockedStatus(t *testing.T) {
	js := `const body = makeMockBody();
const stale = new Date(Date.now() - 90*1000).toISOString();
const run = { key: 'a', kind: 'active', status: 'blocked', issueLabel: '#42', runId: 'r1', lastOutputAt: stale, duration: 1200 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const durationCell = row.querySelector('[data-cell="duration"]');
const line = durationCell.querySelector('.stale-line');
if (line) throw new Error('expected NO .stale-line for blocked status even when lastOutputAt is stale');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildDurationCell_NoStaleLineForFailedStatus(t *testing.T) {
	js := `const body = makeMockBody();
const stale = new Date(Date.now() - 90*1000).toISOString();
const run = { key: 'a', kind: 'active', status: 'failure', issueLabel: '#42', runId: 'r1', lastOutputAt: stale, duration: 1200 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const durationCell = row.querySelector('[data-cell="duration"]');
const line = durationCell.querySelector('.stale-line');
if (line) throw new Error('expected NO .stale-line for failed status even when lastOutputAt is stale');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildDurationCell_StaleLineStillAppearsForAutoSelectingStatus(t *testing.T) {
	js := `const body = makeMockBody();
const stale = new Date(Date.now() - 90*1000).toISOString();
const run = { key: 'a', kind: 'active', status: 'auto-selecting', issueLabel: '#42', runId: 'r1', lastOutputAt: stale, duration: 1200 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, run, opts);
const row = body.children[0];
const durationCell = row.querySelector('[data-cell="duration"]');
const line = durationCell.querySelector('.stale-line');
if (!line) throw new Error('expected .stale-line for auto-selecting status when lastOutputAt is stale');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildEventsContent_RunRetryRendersJSONDocument(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: '#42', runId: 'r1', events: [
  { type: 'run.retry', timestamp: 1700000000000, payload: { attempt: 2, max_attempts: 3, previous_status: 'failure', last_log_lines: ['[orchestrator] sandbox error', '[orchestrator] retrying'] } }
] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
if (!detailRow) throw new Error('expected detail row');
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected pre[data-rendered-json] in events tab');
if (pre.textContent.indexOf('run.retry') === -1) throw new Error('expected run.retry in json');
if (pre.textContent.indexOf('previous_status') === -1) throw new Error('expected previous_status in json');
if (pre.textContent.indexOf('[orchestrator] sandbox error') === -1) throw new Error('expected last_log_lines in json');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildEventsContent_RunIdleTimeoutRendersJSONDocument(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'aborted', issueLabel: '#42', runId: 'r1', events: [
  { type: 'run.idle_timeout', timestamp: 1700000000000, payload: { attempt: 1, max_attempts: 3, idle_seconds: 300, last_log_lines: ['[orchestrator] idle for 5m'] } }
] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
if (!detailRow) throw new Error('expected detail row');
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected pre[data-rendered-json] in events tab');
if (pre.textContent.indexOf('run.idle_timeout') === -1) throw new Error('expected run.idle_timeout in json');
if (pre.textContent.indexOf('[orchestrator] idle for 5m') === -1) throw new Error('expected last_log_lines in json');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildEventsContent_OtherEventTypesRenderJSONArray(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: '#42', runId: 'r1', events: [
  { type: 'run.started', timestamp: 1700000000000, payload: { branch: 'main' } }
] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
if (!detailRow) throw new Error('expected detail row');
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected pre[data-rendered-json] for events json render');
if (pre.textContent.indexOf('run.started') === -1) throw new Error('expected run.started in json');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalJSONToHTMLRender_RetryChipPresentWhenRetriesDoneGreaterThanZero(t *testing.T) {
	// Simulate the server-emitted /api/runs row JSON: a finished run with
	// retriesTotal: 3, retriesDone: 2. Feed that JSON-shaped object through
	// the full portal diff render path and assert the rendered HTML carries
	// the retry chip with the correct text and title.
	serverJSON := []byte(`[{"key":"a","runId":"a","kind":"completed","status":"success","issueLabel":"#42","issueNumber":42,"retriesTotal":3,"retriesDone":2,"startedAt":"2025-01-01T12:00:00Z"}]`)
	var runs []map[string]any
	if err := json.Unmarshal(serverJSON, &runs); err != nil {
		t.Fatalf("unmarshal server JSON: %v", err)
	}
	js := `const body = makeMockBody();
const runs = ` + string(serverJSON) + `;
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const row = body.children[0];
if (!row) throw new Error('expected data row');
const chip = row.querySelector('.retry-chip');
if (chip) throw new Error('expected NO .retry-chip in row when server JSON has retriesDone=2, got ' + JSON.stringify(chip.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalJSONToHTMLRender_RetryChipAbsentWhenRetriesDoneZero(t *testing.T) {
	// Clean run, retriesDone=0: chip must not appear in the rendered row.
	serverJSON := []byte(`[{"key":"a","runId":"a","kind":"completed","status":"success","issueLabel":"#42","issueNumber":42,"startedAt":"2025-01-01T12:00:00Z"}]`)
	if err := json.Unmarshal(serverJSON, new([]interface{})); err != nil {
		t.Fatalf("unmarshal server JSON: %v", err)
	}
	js := `const body = makeMockBody();
const runs = ` + string(serverJSON) + `;
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const row = body.children[0];
if (!row) throw new Error('expected data row');
const chip = row.querySelector('.retry-chip');
if (chip) throw new Error('expected NO .retry-chip in row when server JSON has no retriesDone, got: ' + JSON.stringify(chip.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalJSONToHTMLRender_RetryChipUsesSingularWhenRetriesDoneIsOne(t *testing.T) {
	// retriesDone: 1 should still not render retry chip.
	serverJSON := []byte(`[{"key":"a","runId":"a","kind":"completed","status":"success","issueLabel":"#42","issueNumber":42,"retriesTotal":1,"retriesDone":1,"startedAt":"2025-01-01T12:00:00Z"}]`)
	var runs []map[string]any
	if err := json.Unmarshal(serverJSON, &runs); err != nil {
		t.Fatalf("unmarshal server JSON: %v", err)
	}
	js := `const body = makeMockBody();
const runs = ` + string(serverJSON) + `;
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const row = body.children[0];
if (!row) throw new Error('expected data row');
const chip = row.querySelector('.retry-chip');
if (chip) throw new Error('expected NO .retry-chip in row when server JSON has retriesDone=1, got ' + JSON.stringify(chip.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_StopButtonsHiddenWhenPlatformUnsupported(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', socketPath: '/tmp/sock', batchKey: 'run-1' },
];
const abortReservations = new Set();
const opts = { helpers, abortReservations, abortSupported: false, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const aRow = body.children[0];
const aBtn = aRow.querySelector('button[data-action="abort-run"]');
if (aBtn) throw new Error('a should NOT have abort button when abortSupported is false');
if (abortReservations.size !== 0) throw new Error('abortReservations should not be touched when abortSupported is false, got size ' + abortReservations.size);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_AbortButtonHiddenForRowsFromFinishedBatch(t *testing.T) {
	js := `const body = makeMockBody();
const abortReservations = new Set();
const activeRun = { key: 'run-42-1-issue-42', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r1', issueNumber: 42, socketPath: '/tmp/sock', batchKey: 'run-42-1' };
const historicalBlocked = { key: 'run-old-1', kind: 'active', status: 'blocked', issueLabel: '#42', runId: 'r-old', issueNumber: 42, socketPath: '/tmp/sock', batchKey: '' };
if (!helpers.isRunAbortable(activeRun, abortReservations)) throw new Error('active run with batchKey should be abortable');
if (helpers.isRunAbortable(historicalBlocked, abortReservations)) throw new Error('historical blocked row with empty batchKey should NOT be abortable');

const opts = { helpers, abortReservations, expandedKey: null };
SandmanPortalDiff.diffRuns(body, [activeRun, historicalBlocked], opts);
const activeRow = body.children[0];
const historicalRow = body.children[1];
const activeBtn = activeRow.querySelector('button[data-action="abort-run"]');
const historicalBtn = historicalRow.querySelector('button[data-action="abort-run"]');
if (!activeBtn) throw new Error('active run should have abort button');
if (historicalBtn) throw new Error('historical blocked row should NOT have abort button');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_AbortButtonHiddenWhenRunDirSocketVanishes(t *testing.T) {
	js := `const body = makeMockBody();
const abortReservations = new Set();
const deadDaemonRun = { key: 'run-42-1-issue-42', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r1', issueNumber: 42, socketPath: '/tmp/sock', batchKey: '' };
if (helpers.isRunAbortable(deadDaemonRun, abortReservations)) throw new Error('run with empty batchKey (vanished socket) should NOT be abortable');

const opts = { helpers, abortReservations, expandedKey: null };
SandmanPortalDiff.diffRuns(body, [deadDaemonRun], opts);
const row = body.children[0];
const btn = row.querySelector('button[data-action="abort-run"]');
if (btn) throw new Error('run with vanished socket should NOT have abort button');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_LogPreUpdatedInPlace(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1\nline 2' };
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1\nline 2' };
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'first\nsecond' };
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

func TestPortalDiffFillTerminalPre_ApostropheNotMishandled(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: "don't worry\nI'll fix it" };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
if (pre.textContent !== "don't worry\nI'll fix it" && pre.textContent !== "don&#39;t worry\nI&#39;ll fix it") throw new Error('expected apostrophes preserved in log text, got ' + JSON.stringify(pre.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_AppendPreservesExistingNodes(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1' };
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
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'http://example/' };
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
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1\n' };
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
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'foo running' };
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

func TestPortalDiffUpdateDetailLog_InlineAppendPreservesEarlierLines(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1\nfoo running' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1) throw new Error('expected log pre');
const firstChild = pre1.children[0];
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { log: 'line 1\nfoo runningx' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 !== pre1) throw new Error('pre identity should be preserved');
if (pre2.textContent !== 'line 1\nfoo runningx') throw new Error('expected pre to contain the combined text, got ' + JSON.stringify(pre2.textContent));
if (pre2.children[0] !== firstChild) throw new Error('earlier-line span should be preserved across an inline append');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_RewriteReplacesNodes(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1' };
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

func TestPortalDiffUpdateDetailPanelLog_AppendExtendsLog(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1\nline 2' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
if (pre.getAttribute('data-rendered-log') !== 'line 1\nline 2') throw new Error('expected initial log');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateDetailPanelLog(body, 'a', 'line 1\nline 2\nline 3', helpers);
if (pre.getAttribute('data-rendered-log') !== 'line 1\nline 2\nline 3') throw new Error('expected updated log attr, got ' + pre.getAttribute('data-rendered-log'));
if (pre.textContent !== 'line 1\nline 2\nline 3') throw new Error('expected appended text in pre, got ' + JSON.stringify(pre.textContent));
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('expected mutations on append');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailPanelLog_TruncationFallsBackToReload(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1\nline 2\nline 3' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
const firstChildren = pre.children.slice();
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateDetailPanelLog(body, 'a', 'line 1', helpers);
if (pre.getAttribute('data-rendered-log') !== 'line 1') throw new Error('expected truncated log attr');
if (pre.textContent !== 'line 1') throw new Error('expected truncated text in pre, got ' + JSON.stringify(pre.textContent));
let reused = 0;
for (const c of firstChildren) {
  if (pre.children.indexOf(c) !== -1) reused += 1;
}
if (reused > 0) throw new Error('truncation should fall back to full reload, not append');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailPanelLog_NoOpWhenDetailNotOpen(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1\nline 2' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, [run1], opts);
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateDetailPanelLog(body, 'a', '', helpers);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations !== 0) throw new Error('no mutations expected when log is empty and no detail row, got ' + counters.mutations);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailPanelLog_PlaceholderFollowedByRealLog(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: '' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
	if (pre.getAttribute('data-rendered-log') !== '') throw new Error('expected empty');
if (pre.textContent !== '') throw new Error('expected empty text, got ' + JSON.stringify(pre.textContent));
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateDetailPanelLog(body, 'a', 'real log line', helpers);
if (pre.getAttribute('data-rendered-log') !== 'real log line') throw new Error('expected real log attr');
if (pre.textContent !== 'real log line') throw new Error('expected real log text, got ' + JSON.stringify(pre.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_SwitchingSubjectRestoresPlaceholder(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1, log: '' };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, log: 'review log' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: 'issue-1', tabs: { 'issue-1': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
SandmanPortalDiff.diffRuns(body, [parentRun], opts1);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1) throw new Error('expected parent log pre initially');
if (pre1.textContent !== '') throw new Error('expected empty initially, got ' + JSON.stringify(pre1.textContent));
const opts2 = { helpers, stopGroups, expandedKey: 'PR42', tabs: { 'PR42': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [parentRun], opts2);
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre2 || pre2 === pre1) throw new Error('expected a fresh child pane after first subject switch');
if (pre2.textContent !== 'review log') throw new Error('expected review log after subject switch, got ' + JSON.stringify(pre2.textContent));
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [parentRun], opts1);
const pre3 = detailRow.querySelector('pre[data-scroll-key]');
if (pre3 !== pre1) throw new Error('expected placeholder pane to be reused after switching back');
if (pre3.textContent !== '') throw new Error('expected empty restored after switching back, got ' + JSON.stringify(pre3.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_RemovesPlaceholderAfterRealLog(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: '' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
if (pre.textContent !== '') throw new Error('expected empty text, got ' + JSON.stringify(pre.textContent));
const run2 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'real log line' };
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run2], opts);
if (pre.textContent !== 'real log line') throw new Error('expected real log text, got ' + JSON.stringify(pre.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailLog_PreservesLiveReviewLogAfterCompletion(t *testing.T) {
	js := `const body = makeMockBody();
const activeRun = { key: 'PR17', kind: 'active', status: 'reviewing', review: true, issueLabel: 'PR17', runId: 'PR17', log: 'review output line 1\nreview output line 2' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR17', tabs: { PR17: 'log' } };
SandmanPortalDiff.diffRuns(body, [activeRun], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
if (pre.textContent !== 'review output line 1\nreview output line 2') throw new Error('expected live review log, got ' + JSON.stringify(pre.textContent));
const completedRun = { key: 'PR17', kind: 'completed', status: 'success', review: true, issueLabel: 'PR17', runId: 'PR17', log: 'saved review log\nmore review output' };
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [completedRun], opts);
if (pre.textContent === 'No log file yet.') throw new Error('completion should not wipe live review log');
if (pre.textContent !== 'review output line 1\nreview output line 2') throw new Error('expected live review log to remain visible, got ' + JSON.stringify(pre.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailPanelLog_PreservesScrollPosition(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'line 1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
pre.scrollTop = 50;
pre.scrollHeight = 200;
pre.clientHeight = 100;
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateDetailPanelLog(body, 'a', 'line 1\nline 2', helpers);
if (pre.scrollTop !== 50) throw new Error('scroll position not preserved, got ' + pre.scrollTop);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailPanelLog_ExportedFunction(t *testing.T) {
	js := `if (typeof SandmanPortalDiff.updateDetailPanelLog !== 'function') throw new Error('updateDetailPanelLog not exported');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailEvents_SkipsRebuildWhenUnchanged(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'log text', events: [{ type: 'start', timestamp: 1700000000000, payload: { ok: true } }] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const content1 = detailRow.querySelector('.detail-content');
if (!content1) throw new Error('expected detail-content');
const pre1 = detailRow.querySelector('pre[data-rendered-json]');
if (!pre1) throw new Error('expected events json pre');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations > 1) throw new Error('unchanged events tab should stay stable, got ' + counters.mutations + ' mutations');
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('content identity should be preserved');
const pre2 = detailRow.querySelector('pre[data-rendered-json]');
if (!pre2) throw new Error('expected events json pre after second diff');
if (pre2.getAttribute('data-rendered-json') !== pre1.getAttribute('data-rendered-json')) throw new Error('events json should stay stable when unchanged');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailDetails_RendersPrettyJSON(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log', logUrl: '/log' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected details pre');
if (!pre.getAttribute('data-rendered-json') || pre.getAttribute('data-rendered-json').indexOf('"runId": "r1"') === -1) throw new Error('expected raw json fingerprint, got ' + pre.getAttribute('data-rendered-json'));
if (!pre.querySelector('.json-key')) throw new Error('expected highlighted json key');
if (!pre.querySelector('.json-string')) throw new Error('expected highlighted json string');
if (!pre.querySelector('.json-punctuation')) throw new Error('expected highlighted json punctuation');
if (pre.textContent.indexOf('runId') === -1) throw new Error('expected runId in json, got ' + pre.textContent);
if (pre.textContent.indexOf('logPath') === -1) throw new Error('expected logPath in json, got ' + pre.textContent);
if (detailRow.querySelector('.detail-meta')) throw new Error('old detail-meta layout should be gone');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailDetails_SkipsRebuildWhenUnchanged(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log', logUrl: '/log' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const content1 = detailRow.querySelector('.detail-content');
if (!content1) throw new Error('expected detail-content');
const pre1 = detailRow.querySelector('pre[data-rendered-json]');
const run2 = Object.assign({}, run1);
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run2], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations !== 0) throw new Error('unchanged details tab should not mutate, got ' + counters.mutations);
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('content identity should be preserved');
const pre2 = detailRow.querySelector('pre[data-rendered-json]');
if (pre2 !== pre1) throw new Error('details pre should not be replaced');
if (!pre2.getAttribute('data-rendered-json').includes('"logPath": "/tmp/run.log"')) throw new Error('expected logPath to remain in json');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailDetails_RebuildsWhenChanged(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log', logUrl: '/log' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const content1 = detailRow.querySelector('.detail-content');
const pre1 = detailRow.querySelector('pre[data-rendered-json]');
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { logPath: '/tmp/run-2.log' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('changed details should mutate, got 0');
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('content identity should be preserved across rebuilds');
const pre2 = detailRow.querySelector('pre[data-rendered-json]');
if (pre2.getAttribute('data-rendered-json') === pre1.getAttribute('data-rendered-json')) throw new Error('details json should change when data changes');
if (!pre2.getAttribute('data-rendered-json').includes('"logPath": "/tmp/run-2.log"')) throw new Error('expected updated logPath in json, got ' + pre2.getAttribute('data-rendered-json'));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailDetails_ActiveRunSkipsRebuildBeforeFirstPayload(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: null, duration: '1s', branch: 'main', log: '', events: [] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const content1 = detailRow.querySelector('.detail-content');
const pre1 = detailRow.querySelector('pre[data-rendered-json]');
if (!content1 || !pre1) throw new Error('expected initial details content');
const run2 = Object.assign({}, run1, { duration: '2s' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const content2 = detailRow.querySelector('.detail-content');
const pre2 = detailRow.querySelector('pre[data-rendered-json]');
if (content2 !== content1) throw new Error('details content identity should be preserved');
if (pre2 !== pre1) throw new Error('details pre should not be replaced');
if (pre2.getAttribute('data-rendered-json') !== pre1.getAttribute('data-rendered-json')) throw new Error('details json fingerprint should stay stable before first payload');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateDetailDetails_RendersIssueNumberAndTitle (issue #1506)
// locks AC #2 directly: the Details tab JSON text must include the literal
// keys `issueNumber` and `issueTitle` so operators can see the GitHub issue
// context from the details pane without switching back to the summary row.
func TestPortalDiffUpdateDetailDetails_RendersIssueNumberAndTitle(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log', issueNumber: 42, issueTitle: 'Fix the frobnicator' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected details pre');
const text = pre.textContent || '';
if (text.indexOf('issueNumber') === -1) throw new Error('expected issueNumber key in details json, got ' + text);
if (text.indexOf('issueTitle') === -1) throw new Error('expected issueTitle key in details json, got ' + text);
if (text.indexOf('42') === -1) throw new Error('expected issueNumber value 42 in details json, got ' + text);
if (text.indexOf('Fix the frobnicator') === -1) throw new Error('expected issueTitle value in details json, got ' + text);
const raw = pre.getAttribute('data-rendered-json') || '';
if (raw.indexOf('"issueNumber": 42') === -1) throw new Error('expected "issueNumber": 42 in raw fingerprint, got ' + raw);
if (raw.indexOf('"issueTitle": "Fix the frobnicator"') === -1) throw new Error('expected "issueTitle" value in raw fingerprint, got ' + raw);
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateDetailDetails_RebuildsWhenIssueTitleChanges (issue
// #1506) locks AC #4: the new fields participate in the fingerprint
// comparison, so a change in issueTitle (or issueNumber) between polls
// forces the details pane to rebuild with the new value.
func TestPortalDiffUpdateDetailDetails_RebuildsWhenIssueTitleChanges(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log', issueNumber: 42, issueTitle: 'Fix the frobnicator' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-rendered-json]');
if (!pre1) throw new Error('expected initial details pre');
if (!(pre1.getAttribute('data-rendered-json') || '').includes('"issueTitle": "Fix the frobnicator"')) throw new Error('expected initial issueTitle in json');
SandmanPortalDiff.resetCounters();
const run2 = Object.assign({}, run1, { issueTitle: 'Calibrate the frobnicator' });
SandmanPortalDiff.diffRuns(body, [run2], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('changed issueTitle should mutate details pane, got 0');
const pre2 = detailRow.querySelector('pre[data-rendered-json]');
if (!pre2) throw new Error('expected details pre after update');
if (pre2 === pre1) throw new Error('details pre should be replaced when issueTitle changes');
const raw2 = pre2.getAttribute('data-rendered-json') || '';
if (raw2.indexOf('"issueTitle": "Calibrate the frobnicator"') === -1) throw new Error('expected updated issueTitle in raw fingerprint, got ' + raw2);
if (raw2.indexOf('Fix the frobnicator') !== -1) throw new Error('expected stale issueTitle gone from raw fingerprint, got ' + raw2);
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateDetailDetails_SkipsRebuildWithNewFieldsUnchanged
// (issue #1506) locks AC #4 from the stability side: when the new
// fields are present on the run object but unchanged between polls,
// the fingerprint comparison sees them, the rendered JSON matches
// byte-for-byte (full-string comparison, not substring), and the
// details pane stays untouched (0 mutations, same <pre> element).
// The new-field assertions guarantee this isn't a degenerate pass
// where the change accidentally dropped the fields from detailsData.
func TestPortalDiffUpdateDetailDetails_SkipsRebuildWithNewFieldsUnchanged(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log', issueNumber: 42, issueTitle: 'Fix the frobnicator' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-rendered-json]');
const raw1 = pre1.getAttribute('data-rendered-json');
if (!raw1.includes('"issueNumber": 42')) throw new Error('expected issueNumber in initial fingerprint, got ' + raw1);
if (!raw1.includes('"issueTitle": "Fix the frobnicator"')) throw new Error('expected issueTitle in initial fingerprint, got ' + raw1);
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations !== 0) throw new Error('unchanged details (incl. new fields) should not mutate, got ' + counters.mutations);
const pre2 = detailRow.querySelector('pre[data-rendered-json]');
if (pre2 !== pre1) throw new Error('details pre should not be replaced');
const raw2 = pre2.getAttribute('data-rendered-json');
if (raw2 !== raw1) throw new Error('details json fingerprint should be byte-identical across polls, got ' + raw2);
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateDetailDetails_MissingIssueRendersNull (issue #1506)
// pins the sentinel behavior: a run with no linked issue must render
// `issueNumber: null`, not `0` (which would mislabel a missing issue
// as issue #0). This matches portalRun's `omitempty` contract — absent
// issue is distinct from a valid integer — and locks down the coercion
// so a future change can't silently re-introduce `Number(...) || 0`.
func TestPortalDiffUpdateDetailDetails_MissingIssueRendersNull(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected details pre');
const raw = pre.getAttribute('data-rendered-json') || '';
if (!raw.includes('"issueNumber": null')) throw new Error('expected issueNumber: null when run has no linked issue, got ' + raw);
if (raw.includes('"issueNumber": 0')) throw new Error('issueNumber: 0 would mislabel a missing issue as issue #0, got ' + raw);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailEvents_RebuildsWhenEventsChange(t *testing.T) {
	js := `const body = makeMockBody();
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: [{ type: 'start', timestamp: 1, payload: { ok: true } }] };
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
const pre = content2.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected events json pre after rebuild');
if (pre.textContent.indexOf('progress') === -1) throw new Error('expected added event in json');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_SwitchingSubjectPreservesContent(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1, log: 'parent log' };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, log: 'review log' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: 'issue-1', tabs: { 'issue-1': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
SandmanPortalDiff.diffRuns(body, [parentRun], opts1);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1 || pre1.textContent.indexOf('parent log') === -1) throw new Error('expected parent log initially');
const content1 = detailRow.querySelector('.detail-content');
const opts2 = { helpers, stopGroups, expandedKey: 'PR42', tabs: { 'PR42': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [parentRun], opts2);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('switching subject should mutate the detail row, got 0');
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('expected detail content to be preserved across subject switch');
const subjectPicker = detailRow.querySelector('.detail-subject-picker');
if (!subjectPicker) throw new Error('expected subject picker');
if (subjectPicker.querySelector('label')) throw new Error('subject label should not be visible');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (subjectSelect.getAttribute('aria-label') !== 'Subject') throw new Error('subject select should keep aria-label');
if (!subjectSelect || subjectSelect.value !== 'PR42') throw new Error('expected selected review subject after switch, got ' + (subjectSelect && subjectSelect.value));
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 === pre1) throw new Error('first subject switch should mount a fresh pane for the new subject');
if (!pre2 || pre2.textContent.indexOf('review log') === -1) throw new Error('expected review log after subject switch, got ' + (pre2 && pre2.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_SwitchingSubjectMutationsAndContent(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1, log: 'parent log', socketPath: '/tmp/sock' };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, log: 'review log' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: 'issue-1', tabs: { 'issue-1': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
SandmanPortalDiff.diffRuns(body, [parentRun], opts1);
const detailRow = body.children[1];
const pre1 = detailRow.querySelector('pre[data-scroll-key]');
if (!pre1 || pre1.textContent.indexOf('parent log') === -1) throw new Error('expected parent log initially');
const content1 = detailRow.querySelector('.detail-content');
const opts2 = { helpers, stopGroups, expandedKey: 'PR42', tabs: { 'PR42': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [parentRun], opts2);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('switching subject should mutate the detail row, got 0');
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('expected detail content to be preserved across subject switch');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect || subjectSelect.value !== 'PR42') throw new Error('expected selected review subject after switch, got ' + (subjectSelect && subjectSelect.value));
const pre2 = detailRow.querySelector('pre[data-scroll-key]');
if (pre2 === pre1) throw new Error('first subject switch should mount a fresh pane for the new subject');
if (!pre2 || pre2.textContent.indexOf('review log') === -1) throw new Error('expected review log after subject switch, got ' + (pre2 && pre2.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_SwitchingSubjectRoundTripReusesCachedPane(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: 'issue-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1, log: 'parent log' };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, log: 'review log' };
const stopGroups = new Set();
const optsParent = { helpers, stopGroups, expandedKey: 'issue-1', tabs: { 'issue-1': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
const optsChild = { helpers, stopGroups, expandedKey: 'PR42', tabs: { 'PR42': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
SandmanPortalDiff.diffRuns(body, [parentRun], optsParent);
const detailRow = body.children[1];
const parentPre1 = detailRow.querySelector('pre[data-scroll-key]');
const parentFirstChild = parentPre1.firstChild;
if (!parentPre1 || parentPre1.textContent.indexOf('parent log') === -1) throw new Error('expected parent log initially');
SandmanPortalDiff.diffRuns(body, [parentRun], optsChild);
const childPre = detailRow.querySelector('pre[data-scroll-key]');
if (!childPre || childPre === parentPre1 || childPre.textContent.indexOf('review log') === -1) throw new Error('expected fresh child pane on first subject switch');
SandmanPortalDiff.diffRuns(body, [parentRun], optsParent);
const parentPre2 = detailRow.querySelector('pre[data-scroll-key]');
if (parentPre2 !== parentPre1) throw new Error('expected parent pane node to be reused on subject round-trip');
if (parentPre2.firstChild !== parentFirstChild) throw new Error('expected parent pane children to be reused on subject round-trip');
if (parentPre2.textContent.indexOf('parent log') === -1) throw new Error('expected parent log after returning to parent subject');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_RebuildsAfterTabRoundTrip(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', log: 'log text', events: [{ type: 'start', timestamp: 1, payload: { ok: true } }] };
const stopGroups = new Set();
const optsLog = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
const optsEvents = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], optsEvents);
const detailRow = body.children[1];
let content = detailRow.querySelector('.detail-content');
let pre = content.querySelector('pre[data-rendered-json]');
if (!pre || pre.textContent.indexOf('start') === -1) throw new Error('expected events json initially');
SandmanPortalDiff.diffRuns(body, [run], optsLog);
content = detailRow.querySelector('.detail-content');
const logPre = content.querySelector('pre[data-scroll-key]');
if (!logPre) throw new Error('expected log pre after switch to log');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], optsEvents);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('returning to events tab should rebuild the pane, got 0 mutations');
content = detailRow.querySelector('.detail-content');
pre = content.querySelector('pre[data-rendered-json]');
if (!pre || pre.textContent.indexOf('start') === -1) throw new Error('expected events json after returning to events');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_RebuildsAfterTabRoundTrip_Details(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log', logUrl: '/log' };
const stopGroups = new Set();
const optsDetails = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
const optsLog = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], optsDetails);
const detailRow = body.children[1];
let pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre || !pre.getAttribute('data-rendered-json').includes('"logPath": "/tmp/run.log"')) throw new Error('expected details json initially');
SandmanPortalDiff.diffRuns(body, [run], optsLog);
pre = detailRow.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre after switch to log');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], optsDetails);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('returning to details tab should rebuild the pane, got 0 mutations');
pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre || !pre.getAttribute('data-rendered-json').includes('"logPath": "/tmp/run.log"')) throw new Error('expected details json after returning to details');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_TabChangeRebuildsContent(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', log: 'log text', logPath: '/tmp/run.log' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts1);
const detailRow = body.children[1];
const logBtn1 = detailRow.querySelector('button[data-tab="log"]');
const detailsBtn1 = detailRow.querySelector('button[data-tab="details"]');
if (logBtn1.getAttribute('aria-pressed') !== 'true') throw new Error('log button should be pressed initially');
if (detailsBtn1.getAttribute('aria-pressed') !== 'false') throw new Error('details button should not be pressed initially');
const opts2 = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], opts2);
const logBtn2 = detailRow.querySelector('button[data-tab="log"]');
const detailsBtn2 = detailRow.querySelector('button[data-tab="details"]');
if (logBtn2.getAttribute('aria-pressed') !== 'false') throw new Error('log button should not be pressed after switch');
if (detailsBtn2.getAttribute('aria-pressed') !== 'true') throw new Error('details button should be pressed after switch');
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre || !pre.getAttribute('data-rendered-json').includes('"logPath": "/tmp/run.log"')) throw new Error('expected details json after tab switch');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffSetEmpty_UsesReplaceChildren(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
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
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1' };
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
  { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1' },
  { key: 'b', kind: 'active', status: 'running', issueLabel: 'B', runId: 'r2' },
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

func TestPortalDiffHighlightJSON_Exists(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"key": "val"}');
if (typeof result !== 'string') throw new Error('highlightJSON should return string');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_KeyToken(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"key": "val"}');
if (result.indexOf('json-key') === -1) throw new Error('expected json-key span');
if (result.indexOf('&quot;key&quot;') === -1) throw new Error('expected escaped key in span');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_StringValue(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"k": "hello world"}');
if (result.indexOf('json-string') === -1) throw new Error('expected json-string span');
if (result.indexOf('&quot;hello world&quot;') === -1) throw new Error('expected escaped string value');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_NumberValue(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"n": 42}');
if (result.indexOf('json-number') === -1) throw new Error('expected json-number span');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_NegativeNumber(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"n": -7}');
if (result.indexOf('json-number') === -1) throw new Error('expected json-number span for negative');
if (result.indexOf('>-7<') === -1) throw new Error('expected -7 in number span');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_Zero(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"n": 0}');
if (result.indexOf('json-number') === -1) throw new Error('expected json-number span for zero');
if (result.indexOf('>0<') === -1) throw new Error('expected 0 in number span');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_ScientificNotation(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"a": 1e10, "b": 1.5e-3}');
const count = (result.match(/json-number/g) || []).length;
if (count !== 2) throw new Error('expected 2 json-number spans for scientific notation, got ' + count);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_BooleanValue(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"f": true, "g": false}');
const bools = (result.match(/json-boolean/g) || []).length;
if (bools !== 2) throw new Error('expected 2 json-boolean spans, got ' + bools);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_NullValue(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"n": null}');
if (result.indexOf('json-null') === -1) throw new Error('expected json-null span');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_Punctuation(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"a": 1, "b": 2}');
const puncts = (result.match(/json-punctuation/g) || []).length;
if (puncts < 2) throw new Error('expected json-punctuation spans for colon and comma, got ' + puncts);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_EmptyInput(t *testing.T) {
	js := `if (SandmanPortalDiff.highlightJSON('') !== '') throw new Error('empty should return empty');
if (SandmanPortalDiff.highlightJSON(null) !== '') throw new Error('null should return empty');
if (SandmanPortalDiff.highlightJSON(undefined) !== '') throw new Error('undefined should return empty');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_NestedObject(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"outer": {"inner": "deep"}}');
if (result.indexOf('json-key') === -1) throw new Error('expected json-key');
if (result.indexOf('json-string') === -1) throw new Error('expected json-string');
if (result.indexOf('json-punctuation') === -1) throw new Error('expected json-punctuation');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_Array(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"items": [1, "two", true]}');
if (result.indexOf('json-number') === -1) throw new Error('expected json-number');
if (result.indexOf('json-string') === -1) throw new Error('expected json-string');
if (result.indexOf('json-boolean') === -1) throw new Error('expected json-boolean');
if (result.indexOf('json-punctuation') === -1) throw new Error('expected json-punctuation for brackets');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_ColonInKey(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"time:out": true}');
if (result.indexOf('json-key') === -1) throw new Error('expected json-key span');
if (result.indexOf('&quot;time:out&quot;') === -1) throw new Error('expected full key with colon');
if (result.indexOf('json-boolean') === -1) throw new Error('expected json-boolean span for value');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightJSON_EscapedChars(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightJSON('{"url": "a&b"}');
if (result.indexOf('&amp;') === -1) throw new Error('expected &amp; for & in string value');
const result2 = SandmanPortalDiff.highlightJSON('{"html": "<b>bold</b>"}');
if (result2.indexOf('&lt;b&gt;') === -1) throw new Error('expected &lt; and &gt; for HTML chars');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffBuildEventsContent_RendersHighlightedPayload(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', events: [{ type: 'check', timestamp: 1700000000000, payload: { ok: true, count: 42, msg: "done", items: [1, null] } }] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
if (!detailRow) throw new Error('expected detail row');
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected pre[data-rendered-json] in events tab');
if (!pre.querySelector('.json-key')) throw new Error('expected json-key in highlighted output');
if (!pre.querySelector('.json-boolean')) throw new Error('expected json-boolean in highlighted output');
if (!pre.querySelector('.json-number')) throw new Error('expected json-number in highlighted output');
if (!pre.querySelector('.json-string')) throw new Error('expected json-string in highlighted output');
if (!pre.querySelector('.json-punctuation')) throw new Error('expected json-punctuation in highlighted output');
if (!pre.querySelector('.json-null')) throw new Error('expected json-null in highlighted output');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_TimestampHighlighted(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('14:32:15 running agent task');
if (result.indexOf('term-time') === -1) throw new Error('expected term-time span');
if (result.indexOf('>14:32:15<') === -1) throw new Error('expected timestamp wrapped');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_FencedLanguageLabelHighlightsFence(t *testing.T) {
	js := "const result = SandmanPortalDiff.highlightTerminalLog('```go\\nconst msg = \"hello\" // note\\n```');\nif (result.indexOf('term-heading') === -1) throw new Error('expected fenced language label span');\nif (result.indexOf('const msg') === -1) throw new Error('expected fenced code text preserved');\nconsole.log('PASS');\n"
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_LangLabelHighlightsHintLine(t *testing.T) {
	js := "const result = SandmanPortalDiff.highlightTerminalLog('lang=ruby');\nif (result.indexOf('term-heading') === -1) throw new Error('expected explicit lang label span');\nif (result.indexOf('ruby') === -1) throw new Error('expected lang label text preserved');\nconsole.log('PASS');\n"
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_FenceHintSharpening(t *testing.T) {
	js := "const result = SandmanPortalDiff.highlightTerminalLog('```ruby\\n# note\\n```');\nif (result.indexOf('term-heading') === -1) throw new Error('expected fenced fence/label span');\nif (result.indexOf('# note') === -1) throw new Error('expected fenced code text preserved');\nconsole.log('PASS');\n"
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_LangHintSharpening(t *testing.T) {
	js := "const result = SandmanPortalDiff.highlightTerminalLog('lang=ruby\\n# note');\nif (result.indexOf('term-heading') === -1) throw new Error('expected lang label span');\nif (result.indexOf('# note') === -1) throw new Error('expected hint-mode code text preserved');\nconsole.log('PASS');\n"
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_StripsANSISequences(t *testing.T) {
	js := "const result = SandmanPortalDiff.highlightTerminalLog('\\u001b[31m--- FAIL: TestFoo\\u001b[0m');\nif (result.indexOf('\\u001b[') !== -1) throw new Error('expected ANSI escape codes removed');\nif (result.indexOf('term-fail') === -1) throw new Error('expected fail token preserved after ANSI stripping');\nconsole.log('PASS');\n"
	runNodeScript(t, js)
}

func TestPortalHTMLAppendStreamLine_RendersHighlightedHTML(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	data, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read %s: %v", htmlPath, err)
	}
	content := string(data)
	for _, want := range []string{"function appendStreamLine(runKey, line)", "SandmanPortalDiff.highlightTerminalLog(line + '\\n')"} {
		if !strings.Contains(content, want) {
			t.Fatalf("page missing SSE highlight marker %q\n%s", want, content[:min(1200, len(content))])
		}
	}
	for _, forbidden := range []string{"highlightTerminalText(", "term-protected"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("page must no longer contain %q after the Prism-backed SSE change\n%s", forbidden, content[:min(1200, len(content))])
		}
	}
}

func TestPortalDiffHighlightTerminalLog_StringPreserved(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('msg = "hello world"');
// Strings are NOT highlighted to avoid over-coloring prose and HTML breakage
// The output should be valid HTML without broken spans
const stripped = result.replace(/<[^>]+>/g, '');
if (stripped.indexOf('hello world') === -1) throw new Error('expected hello world preserved in output');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_ExistingPatternsPreserved(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('$ git branch --show-current');
if (result.indexOf('term-prompt') === -1) throw new Error('expected shell prompt still highlighted');
const result2 = SandmanPortalDiff.highlightTerminalLog('https://github.com/user/repo');
if (result2.indexOf('term-url') === -1) throw new Error('expected URL still highlighted');
const result3 = SandmanPortalDiff.highlightTerminalLog('--- PASS: TestFoo');
if (result3.indexOf('term-pass') === -1) throw new Error('expected test pass still highlighted');
const result4 = SandmanPortalDiff.highlightTerminalLog('test result: ok 3.2.1');
if (result4.indexOf('term-pass') === -1) throw new Error('expected Rust ok result still highlighted');
const result5 = SandmanPortalDiff.highlightTerminalLog('test result: FAILED. 0.1.0');
if (result5.indexOf('term-fail') === -1) throw new Error('expected Rust FAILED result still highlighted');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_CommandAfterPrompt(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('$ git status');
if (result.indexOf('term-command') === -1) throw new Error('expected term-command span');
if (result.indexOf('term-prompt') === -1) throw new Error('expected term-prompt span');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_OpencodeSessionTokens(t *testing.T) {
	js := `const r1 = SandmanPortalDiff.highlightTerminalLog('--- run 2/5 ---');
if (r1.indexOf('term-mark') === -1) throw new Error('expected term-mark span');
const r2 = SandmanPortalDiff.highlightTerminalLog('→ Read file.go');
if (r2.indexOf('term-action') === -1) throw new Error('expected term-action span');
const r3 = SandmanPortalDiff.highlightTerminalLog('Fix #1443 and PR1419');
if (r3.indexOf('term-issue') === -1) throw new Error('expected term-issue span');
const r4 = SandmanPortalDiff.highlightTerminalLog('commit 14cb4c83a');
if (r4.indexOf('term-hash') === -1) throw new Error('expected term-hash span');
const r5 = SandmanPortalDiff.highlightTerminalLog('delegating to sub-agent');
if (r5.indexOf('term-subagent') === -1) throw new Error('expected term-subagent span');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_NoProgrammingTokens(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('const msg = "hello"; // note\nfoo.bar()\nif (x == 5) { return 42; }');
const removed = ['term-string','term-comment','term-keyword','term-number','term-operator','term-func'];
for (const cls of removed) {
  if (result.indexOf(cls) !== -1) throw new Error('unexpected ' + cls + ' span');
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_MixedCodeLineNoProgrammingTokens(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('const msg = "hello world"; // note');
const removed = ['term-keyword','term-string','term-comment'];
for (const cls of removed) {
  if (result.indexOf(cls) !== -1) throw new Error('unexpected ' + cls + ' span');
}
if (result.replace(/<[^>]+>/g, '').indexOf('hello world') === -1) throw new Error('expected text preserved');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_FuncCallNotHighlighted(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('foo.bar()');
if (result.indexOf('term-func') !== -1) throw new Error('unexpected term-func span');
if (result.replace(/<[^>]+>/g, '').indexOf('foo.bar()') === -1) throw new Error('expected text preserved');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_KeywordNotHighlighted(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('if (x == 5) { return; }');
if (result.indexOf('term-keyword') !== -1) throw new Error('unexpected term-keyword span');
if (result.replace(/<[^>]+>/g, '').indexOf('if (x == 5) { return; }') === -1) throw new Error('expected text preserved');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_OperatorNotHighlighted(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('x == 5 && y != 0');
if (result.indexOf('term-operator') !== -1) throw new Error('unexpected term-operator span');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_NumberNotHighlighted(t *testing.T) {
	js := `const result = SandmanPortalDiff.highlightTerminalLog('count = 42');
if (result.indexOf('term-number') !== -1) throw new Error('unexpected term-number span');
if (result.replace(/<[^>]+>/g, '').indexOf('count = 42') === -1) throw new Error('expected text preserved');
console.log('PASS');
`
	runNodeScript(t, js)
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

func TestPortalDiffUpdateCells_ZeroMutationsWhenBatchIssuesUnchanged(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'active', status: 'running', issueLabel: 'auto-select', runId: 'r1', reason: 'auto-select', batchIssues: [42, 43], candidates: [42, 43] };
const runNew = Object.assign({}, runOld);
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (result.cells !== 0) throw new Error('expected 0 cell mutations on unchanged run with reason and batchIssues, got ' + JSON.stringify(result));
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations !== 0) throw new Error('expected 0 mutations on unchanged run with reason and batchIssues, got ' + JSON.stringify(counters));
const titleCell = created.row.querySelector('[data-cell="title"]');
if (titleCell.querySelector('.batch-membership')) throw new Error('batch-membership should not be in title cell for auto-select runs');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_KindChipRendersBeforeBatchMembershipOnUpdate(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'auto-select', runId: 'r1' };
const runNew = Object.assign({}, runOld, { reason: 'auto-select', batchIssues: [42, 43], candidates: [42, 43] });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
const titleCell = created.row.querySelector('[data-cell="title"]');
if (titleCell.querySelector('.batch-membership')) throw new Error('batch-membership should not be in title cell for auto-select runs');
const ctxRow = body.querySelector('tr.context-row[data-context-for="a"]');
if (!ctxRow) throw new Error('expected context row for auto-select');
const chip = ctxRow.querySelector('.context-chip');
if (!chip) throw new Error('expected context chip');
if (!chip.textContent.includes('Auto-select candidates:')) throw new Error('expected auto-select candidates text');
console.log('PASS');
`
	runNodeScript(t, js)
}

func sharedMockHelpers() string {
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
    if (run.reviewVerdict) {
      counterParts.push(reviewPart + ' - ' + run.reviewVerdict);
    } else {
      counterParts.push(reviewPart);
    }
  }
  if (run.runId) lines.push('Run: ' + run.runId);
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
      // DocumentFragment (nodeType === 11) is inserted as its children, not itself.
      if (child && child.nodeType === 11) {
        const fragChildren = child.children.slice();
        child.children.length = 0;
        for (const c of fragChildren) {
          c.parentNode = null;
          this.appendChild(c);
        }
        child.firstChild = null;
        child.lastChild = null;
        return child;
      }
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
      if (child && child.nodeType === 11) {
        const fragChildren = child.children.slice();
        child.children.length = 0;
        for (const c of fragChildren) this.appendChild(c);
        return child;
      }
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
      const m = sel.match(/^(\[|tr\.detail-row\[|tr\.context-row\[|tr[^\[]+\[|tr\[)data-(run-key|detail-for|context-for|batch-for)="([^"]+)"\]$/);
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
    nodeType: 1,
    children: [],
    dataset: {},
    classList: { _set: new Set(), add(...cs) { for (const c of cs) { this._set.add(c); log.push(['class+', c]); } }, remove(...cs) { for (const c of cs) { this._set.delete(c); log.push(['class-', c]); } }, contains(c) { return this._set.has(c); }, toggle(c, force) { if (force === true) this._set.add(c); else if (force === false) this._set.delete(c); else if (this._set.has(c)) this._set.delete(c); else this._set.add(c); log.push(['class^', c]); } },
    parentNode: null,
    __id: 'r' + Math.random().toString(36).slice(2, 8),
    _log: log,
    setAttribute(name, value) { this[name] = value; log.push(['setAttribute', name, value]); },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; log.push(['removeAttribute', name]); },
    appendChild(child) {
      if (child && child.nodeType === 11) {
        const fragChildren = child.children.slice();
        child.children.length = 0;
        for (const c of fragChildren) this.appendChild(c);
        return child;
      }
      const parent = child.parentNode;
      if (parent) { const i = parent.children.indexOf(child); if (i >= 0) parent.children.splice(i, 1); }
      child.parentNode = this;
      this.children.push(child);
      log.push(['appendChild', child.__id || '?']);
      return child;
    },
    insertBefore(child, ref) {
      if (child && child.nodeType === 11) {
        const fragChildren = child.children.slice();
        child.children.length = 0;
        for (const c of fragChildren) this.appendChild(c);
        return child;
      }
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
  Object.defineProperty(row, 'nextElementSibling', {
    get() {
      const p = this.parentNode;
      if (!p || !p.children) return null;
      const idx = p.children.indexOf(this);
      for (let i = idx + 1; i < p.children.length; i += 1) {
        if (p.children[i].nodeType === 1) return p.children[i];
      }
      return null;
    },
  });
  Object.defineProperty(row, 'previousElementSibling', {
    get() {
      const p = this.parentNode;
      if (!p || !p.children) return null;
      const idx = p.children.indexOf(this);
      for (let i = idx - 1; i >= 0; i -= 1) {
        if (p.children[i].nodeType === 1) return p.children[i];
      }
      return null;
    },
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
      const openTag = html.slice(pos, openEnd + 1);
      const text = html.slice(openEnd + 1, closeStart);
      const span = makeMockRow();
      span.tagName = 'SPAN';
      const classMatch = openTag.match(/class="([^"]+)"/);
      if (classMatch) {
        for (const cls of classMatch[1].split(/\s+/)) {
          if (cls) span.classList.add(cls);
        }
      }
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
    createDocumentFragment() {
      return {
        nodeType: 11,
        firstChild: null,
        lastChild: null,
        children: [],
        childNodes: [],
        appendChild(child) {
          const parent = child.parentNode;
          if (parent) {
            const idx = parent.children ? parent.children.indexOf(child) : -1;
            if (idx >= 0) parent.children.splice(idx, 1);
          }
          child.parentNode = this;
          this.children.push(child);
          this.childNodes.push(child);
          if (this.firstChild === null) this.firstChild = child;
          this.lastChild = child;
          return child;
        },
      };
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
	const sandbox = { window: {}, globalThis: {}, Set, Map, WeakMap, JSON, console, setTimeout: setTimeout, requestIdleCallback: function(cb) { var start = Date.now(); setTimeout(function() { cb({ didTimeout: false, timeRemaining: function() { return Math.max(0, 50 - (Date.now() - start)); } }); }, 0); } };
	sandbox.window = sandbox;
	sandbox.globalThis = sandbox;
	sandbox.document = documentRef;
	sandbox.HTMLElement = function() {};
	// Wire the production renderRunMeta from portal.html into the sandbox so
	// tests that pass helpers into SandmanPortalDiff.insertRunRow /
	// diffRuns actually exercise the live function instead of the
	// sharedMockHelpers() mock.
	const portalHtmlPath = helperPath.replace(/portal_diff\.js$/, 'portal.html');
	const portalHtmlSrc = fs.readFileSync(portalHtmlPath, 'utf8');
	const renderRunMetaMatch = portalHtmlSrc.match(/function renderRunMeta\(run\) \{[\s\S]*?^\s{4}\}/m);
	if (renderRunMetaMatch) {
		const fnCtx = vm.createContext(Object.assign({}, sandbox, { sandbox: sandbox }));
		vm.runInContext(renderRunMetaMatch[0] + '\n; sandbox.renderRunMeta = renderRunMeta;', fnCtx, { filename: portalHtmlPath });
		if (typeof sandbox.renderRunMeta === 'function') {
			helpers.renderRunMeta = sandbox.renderRunMeta;
		}
	}
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

func TestPortalDiffCreateRunRow_SingleIssueOmitsBatchMembership(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  kind: 'active',
  status: 'running',
  issueLabel: '#42',
  issueNumber: 42,
  batchKey: 'run-42-1',
  batchIssues: [42],
  runId: 'r1',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
const titleCell = created.row.querySelector('[data-cell="title"]');
if (!titleCell) throw new Error('expected title cell');
const wrap = titleCell.children[0];
const marker = wrap.querySelector('.batch-membership');
if (marker) throw new Error('expected no batch-membership for single issue, got ' + marker.outerHTML);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_RendersAutoSelectChipInTitle(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'auto-select-1700000000000',
  runId: 'auto-select-1700000000000',
  kind: 'completed',
  status: 'success',
  issueLabel: 'auto-select',
  reason: 'auto-select',
  candidates: [1, 2, 3],
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
const ctxRow = body.querySelector('tr.context-row[data-context-for="auto-select-1700000000000"]');
if (!ctxRow) throw new Error('expected context row for auto-select run');
const chip = ctxRow.querySelector('.context-chip');
if (!chip) throw new Error('expected .context-chip in context row');
const text = chip.textContent || '';
if (!text.includes('Auto-select candidates: #1, #2, #3')) {
  throw new Error('expected chip text "Auto-select candidates: #1, #2, #3", got ' + JSON.stringify(text));
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_OmitsReviewBatchContextRow(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'PR42',
  runId: 'PR42',
  kind: 'active',
  status: 'reviewing',
  issueLabel: 'PR42',
  review: true,
  prNumber: 42,
  issueNumber: 1,
  reason: 'review',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (body.querySelector('tr.context-row[data-context-for="PR42"]')) throw new Error('expected no context row for review run');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_OmitsChipWhenReasonEmpty(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  runId: 'r1',
  kind: 'active',
  status: 'running',
  issueLabel: '#42',
  issueNumber: 42,
  batchKey: 'run-42-1',
  batchIssues: [42],
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
const wrap = created.row.querySelector('[data-cell="title"]').children[0];
const autoChip = wrap.querySelector('.auto-select');
const reviewChip = wrap.querySelector('.review');
if (autoChip) throw new Error('expected no auto-select chip for empty reason');
if (reviewChip) throw new Error('expected no review chip for empty reason');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_ReasonChangeAddsAndRemovesChipInPlace(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: '#42' };
const runNew = Object.assign({}, runOld, { reason: 'auto-select', issueLabel: 'auto-select', candidates: [42] });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const wrap = created.row.querySelector('[data-cell="title"]').children[0];
if (wrap.querySelector('.kind-chip')) throw new Error('expected no chip on initial run with no reason');
if (body.querySelector('tr.context-row[data-context-for="a"]')) throw new Error('expected no context row on initial run');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
const ctxRow = body.querySelector('tr.context-row[data-context-for="a"]');
if (!ctxRow) throw new Error('expected context row added after reason change to auto-select');
const chip = ctxRow.querySelector('.context-chip');
if (!chip) throw new Error('expected context chip in context row');
if (!chip.textContent.includes('Auto-select candidates: #42')) throw new Error('expected chip text auto-select candidates, got ' + chip.textContent);
if (wrap.children[0].textContent !== 'auto-select') throw new Error('expected name updated to auto-select');
if (wrap.children[1].textContent.indexOf('r1') < 0) throw new Error('expected meta-line to retain run id');
const runNew2 = Object.assign({}, runNew, { reason: '' });
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runNew, runNew2, opts);
if (body.querySelector('tr.context-row[data-context-for="a"]')) throw new Error('expected context row removed when reason clears');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_KindChipRendersBeforeBatchMembership(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a',
  runId: 'r1',
  kind: 'active',
  status: 'running',
  issueLabel: '#42',
  issueNumber: 42,
  batchKey: 'run-42-1',
  batchIssues: [42, 43],
  reason: 'auto-select',
  candidates: [42, 43],
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (created.row.querySelector('[data-cell="title"]').querySelector('.batch-membership')) {
  throw new Error('batch-membership must not live inside the title cell for auto-select runs');
}
const ctxRow = body.querySelector('tr.context-row[data-context-for="a"]');
if (!ctxRow) throw new Error('expected context row for auto-select');
const chip = ctxRow.querySelector('.context-chip');
if (!chip) throw new Error('expected context chip');
if (!chip.textContent.includes('Auto-select candidates:')) throw new Error('expected auto-select candidates text');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_ReasonChangeAddsAutoSelectContextRow(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: 'PR42' };
const runNew = Object.assign({}, runOld, { reason: 'auto-select', candidates: [1, 2] });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (body.querySelector('tr.context-row[data-context-for="a"]')) throw new Error('expected no context row before reason appears');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
const after = body.querySelector('tr.context-row[data-context-for="a"]');
if (!after) throw new Error('expected context row after update');
const afterChip = after.querySelector('.context-chip');
if (!afterChip) throw new Error('expected context chip after update');
if (!afterChip.textContent.includes('Auto-select candidates:')) throw new Error('expected chip to switch to auto-select');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_RendersAutoSelectChipFromPortalRunJSON(t *testing.T) {
	// This test exercises the full JSON->DOM path the portal page uses
	// for /api/runs: a portalRun-shaped JSON object (the wire format
	// produced by the Go portalRunsView.compute pipeline and serialized
	// to /api/runs) is fed into SandmanPortalDiff.diffRuns, which is the
	// same entry point the page calls. We then assert on the rendered
	// DOM the JS produces. The Go-side /api/runs JSON shape is
	// independently covered by TestPortal_RunsEndpoint_RoundTrips*
	// in portal_server_test.go.
	js := `const body = makeMockBody();
const runs = [
  { key: 'auto-select-1700000000000', runId: 'auto-select-1700000000000', kind: 'completed', status: 'success', issueLabel: 'auto-select', reason: 'auto-select', candidates: [1, 2, 3] },
  { key: 'PR42', runId: 'PR42', kind: 'active', status: 'reviewing', issueLabel: 'PR42', reason: 'review', review: true, prNumber: 42, issueNumber: 1 },
  { key: 'a', runId: 'r1', kind: 'active', status: 'running', issueLabel: '#42', issueNumber: 42, batchKey: 'run-42-1', batchIssues: [42] },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);

if (body.children.length !== 4) throw new Error('expected 4 rows (3 data + 1 context), got ' + body.children.length);

const autoCtx = body.querySelector('tr.context-row[data-context-for="auto-select-1700000000000"]');
if (!autoCtx) throw new Error('expected auto-select context row');
const autoChip = autoCtx.querySelector('.context-chip');
if (!autoChip) throw new Error('expected auto-select context chip');
if (!autoChip.textContent.includes('Auto-select candidates: #1, #2, #3')) throw new Error('expected auto-select candidates text, got ' + autoChip.textContent);

  if (body.querySelector('tr.context-row[data-context-for="PR42"]')) throw new Error('expected no review context row');

const issueCtx = body.querySelector('tr.context-row[data-context-for="a"]');
if (issueCtx) throw new Error('issue row should not have context row');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_ArchivedAddsRowClass(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: '#42', archived: true };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row.classList.contains('row-archived')) {
  throw new Error('expected row-archived class on archived row');
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_ArchivedRunHasNoBadge(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: '#42', archived: true };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (created.row.querySelector('[data-cell="archived"]')) throw new Error('archived column must not exist');
if (!created.row.classList.contains('row-archived')) throw new Error('expected row to have row-archived class');
for (const cell of created.row.querySelectorAll('td, th')) {
  if (cell.textContent.includes('Archived')) throw new Error('archived badge text must not appear anywhere');
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_NonArchivedRowClassAndStatusBadge(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: '#42', archived: false };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (created.row.classList.contains('row-archived')) {
  throw new Error('expected no row-archived class on non-archived row');
}
const badgeCell = created.row.querySelector('[data-cell="badge"]');
if (!badgeCell) throw new Error('expected badge cell');
const badges = badgeCell.querySelectorAll('.badge');
let statusFound = false;
for (const b of badges) {
  if (b.classList.contains('success')) { statusFound = true; break; }
}
if (!statusFound) throw new Error('expected normal .badge.success to still render, got: ' + badgeCell.outerHTML);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_ArchivedToggleUpdatesRowClass(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: '#42', archived: false };
const runNew = Object.assign({}, runOld, { archived: true });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const beforeRow = created.row;
SandmanPortalDiff.resetCounters();
const result = SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
if (!result.mutated) throw new Error('expected mutated=true on archived false->true');
if (body.children.indexOf(beforeRow) < 0) throw new Error('expected row identity preserved on toggle (row replaced)');
if (!created.row.classList.contains('row-archived')) {
  throw new Error('expected row-archived class added after toggle');
}
if (created.row.querySelector('[data-cell="archived"]')) throw new Error('archived column must not exist');

// Toggle back to non-archived.
const runNew2 = Object.assign({}, runNew, { archived: false });
SandmanPortalDiff.resetCounters();
const result2 = SandmanPortalDiff.updateRunRowCells(created.row, runNew, runNew2, opts);
if (!result2.mutated) throw new Error('expected mutated=true on archived true->false');
if (created.row.classList.contains('row-archived')) {
  throw new Error('expected row-archived class removed after toggle back');
}
if (created.row.querySelector('[data-cell="archived"]')) throw new Error('archived column must not exist after toggle back');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_ArchivedPreservesRowClassAcrossPolls(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: '#42', archived: true },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const firstRow = body.querySelector('tr[data-run-key="a"]');
if (!firstRow) throw new Error('expected first row');
if (!firstRow.classList.contains('row-archived')) throw new Error('expected row-archived on first render');
if (firstRow.querySelector('[data-cell="archived"]')) throw new Error('archived column must not exist');

// Second poll: same data, expect the same DOM node to be reused.
SandmanPortalDiff.diffRuns(body, runs, opts);
const secondRow = body.querySelector('tr[data-run-key="a"]');
if (!secondRow) throw new Error('expected second row');
if (secondRow !== firstRow) throw new Error('expected same DOM node identity across polls (row was rebuilt)');
if (!secondRow.classList.contains('row-archived')) throw new Error('expected row-archived preserved on second render');
if (secondRow.querySelector('[data-cell="archived"]')) throw new Error('archived column must not exist');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_OmitsReviewContextRow(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', runId: 'r1', kind: 'active', status: 'running', issueLabel: '#42', issueNumber: 42, batchKey: 'run-42-1', batchIssues: [1, 2, 3], reason: 'review', prNumber: 42 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (body.querySelector('tr.batch-row[data-batch-for="a"]')) throw new Error('expected no batch-row for review run with batchIssues');
if (body.querySelector('tr.context-row[data-context-for="a"]')) throw new Error('expected no context-row for review run');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffCreateRunRow_SuppressesBatchRowWhenReasonIsAutoSelect(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: 'auto-select', candidates: [1, 2, 3], batchIssues: [1, 2, 3], reason: 'auto-select' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (body.querySelector('tr.batch-row[data-batch-for="a"]')) throw new Error('expected no batch-row for auto-select run with batchIssues');
const ctx = body.querySelector('tr.context-row[data-context-for="a"]');
if (!ctx) throw new Error('expected context-row for auto-select run');
const chip = ctx.querySelector('.context-chip');
if (!chip) throw new Error('expected context chip');
if (!chip.textContent.includes('Auto-select candidates:')) throw new Error('expected auto-select text');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_NoInsertBeforeForStableRuns(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1' },
  { key: 'b', kind: 'active', status: 'running', issueLabel: 'B', runId: 'r2' },
  { key: 'c', kind: 'active', status: 'running', issueLabel: 'C', runId: 'r3' },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const aRow = body.children[0];
const bRow = body.children[1];
const cRow = body.children[2];
clearLog(body);
SandmanPortalDiff.resetCounters();
const modifiedRuns = [
  Object.assign({}, runs[0], { status: 'success' }),
  Object.assign({}, runs[1], { status: 'success' }),
  Object.assign({}, runs[2], { status: 'success' }),
];
SandmanPortalDiff.diffRuns(body, modifiedRuns, opts);
const insertBeforeCalls = body._log.filter(e => e[0] === 'insertBefore');
if (insertBeforeCalls.length !== 0) {
  throw new Error('expected 0 insertBefore calls for stable runs, got ' + insertBeforeCalls.length + ': ' + JSON.stringify(insertBeforeCalls));
}
if (body.children[0] !== aRow) throw new Error('a row identity changed');
if (body.children[1] !== bRow) throw new Error('b row identity changed');
if (body.children[2] !== cRow) throw new Error('c row identity changed');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_NoInsertBeforeForStableRunsWithContextRows(t *testing.T) {
	js := `const body = makeMockBody();
const runs = [
  { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', reason: 'review', prNumber: 1, issueNumber: 1 },
  { key: 'b', kind: 'active', status: 'running', issueLabel: 'B', runId: 'r2', reason: 'review', prNumber: 2, issueNumber: 2 },
  { key: 'c', kind: 'active', status: 'running', issueLabel: 'C', runId: 'r3', reason: 'review', prNumber: 3, issueNumber: 3 },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);
const aRow = body.querySelector('tr[data-run-key="a"]');
const bRow = body.querySelector('tr[data-run-key="b"]');
const cRow = body.querySelector('tr[data-run-key="c"]');
if (!aRow || !bRow || !cRow) throw new Error('expected all data rows');
if (body.querySelector('tr.context-row[data-context-for="a"]')) throw new Error('expected no review context row for a');
if (body.querySelector('tr.context-row[data-context-for="b"]')) throw new Error('expected no review context row for b');
if (body.querySelector('tr.context-row[data-context-for="c"]')) throw new Error('expected no review context row for c');
clearLog(body);
SandmanPortalDiff.resetCounters();
const modifiedRuns = [
  Object.assign({}, runs[0], { status: 'success' }),
  Object.assign({}, runs[1], { status: 'success' }),
  Object.assign({}, runs[2], { status: 'success' }),
];
SandmanPortalDiff.diffRuns(body, modifiedRuns, opts);
const insertBeforeCalls = body._log.filter(e => e[0] === 'insertBefore');
if (insertBeforeCalls.length !== 0) {
  throw new Error('expected 0 insertBefore calls for stable runs with context rows, got ' + insertBeforeCalls.length + ': ' + JSON.stringify(insertBeforeCalls));
}
const aRowNow = body.querySelector('tr[data-run-key="a"]');
const bRowNow = body.querySelector('tr[data-run-key="b"]');
const cRowNow = body.querySelector('tr[data-run-key="c"]');
if (aRowNow !== aRow) throw new Error('a row identity changed');
if (bRowNow !== bRow) throw new Error('b row identity changed');
if (cRowNow !== cRow) throw new Error('c row identity changed');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffDiffRuns_OrphanReviewExpandedUsesRealRunID replaces
// TestPortalDiffDiffRuns_ExpandedRealRunIDRendersDetailRow after issue
// #1489 removed the synthetic review stub.
func TestPortalDiffDiffRuns_OrphanReviewExpandedUsesRealRunID(t *testing.T) {
	js := `const body = makeMockBody();
const review = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR42', runs: [review], visibleRuns: [review] };
const result = SandmanPortalDiff.diffRuns(body, [review], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
if (body.querySelector('tr[data-run-key="review-stub-1"]')) throw new Error('expected no synthetic stub row with placeholder key');
const expandedRow = body.querySelector('tr[data-run-key="PR42"]');
if (!expandedRow) throw new Error('expected orphan review row visible under its real RunID');
const expandedAria = expandedRow.getAttribute('aria-expanded');
if (expandedAria !== 'true') throw new Error('expected aria-expanded=true on expanded orphan review row, got ' + expandedAria);
const detailRow = body.querySelector('tr.detail-row[data-detail-for="PR42"]');
if (!detailRow) throw new Error('expected detail row keyed by the orphan review RunID');
console.log('PASS');
`
	runNodeScript(t, js)
}

// runPortalHTMLScript extracts the inline runtime script from portal.html
// (the body inside <script>...</script> at line 1612) and runs it inside a
// sandbox with a minimal DOM/document mock so the pure-data helpers such as
// visibleRunsForTable are reachable from a Node test. The Go template
// placeholders inside the script are stripped before evaluation, and the
// resulting body is escaped so embedding it in a JS template literal does
// not reinterpret "\n" / "\t" escapes that would split the script's
// source lines.
func runPortalHTMLScript(t *testing.T, js string) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	data, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read %s: %v", htmlPath, err)
	}
	src := string(data)
	const startMarker = "<script>\n    const apiPath"
	const endMarker = "</script>"
	start := strings.Index(src, startMarker)
	if start < 0 {
		t.Fatalf("could not find runtime inline <script> block in %s", htmlPath)
	}
	end := strings.Index(src[start:], endMarker)
	if end < 0 {
		t.Fatalf("could not find closing </script> for runtime inline block in %s", htmlPath)
	}
	scriptBody := src[start+len("<script>") : start+end]
	tmplRe := regexp.MustCompile(`\{\{[^}]*\}\}`)
	scriptBody = tmplRe.ReplaceAllString(scriptBody, "undefined")
	escapedBody := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		`${`, `\${`,
	).Replace(scriptBody)
	prefix := sharedMockHelpers() + `
const fs = require('fs');
const vm = require('vm');
const htmlPath = ` + "`" + htmlPath + "`" + `;
const scriptBody = ` + "`" + escapedBody + "`" + `;
const sandbox = { window: {}, globalThis: {}, Set, Map, WeakMap, JSON, Date, Math, console, fs, vm, localStorage: { getItem() { return null; } }, helpers };
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
sandbox.localStorage = { getItem() { return null; } };
const fakeEl = {
  children: [],
  classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
  dataset: {},
  style: {},
  setAttribute() {}, getAttribute() { return null; }, removeAttribute() {},
  appendChild(child) { this.children.push(child); return child; },
  insertBefore(child) { this.children.push(child); return child; },
  addEventListener() {}, removeEventListener() {},
  querySelector() { return null; }, querySelectorAll() { return []; },
  cloneNode() { return Object.assign({}, this); },
  scrollIntoView() {},
	};
const fakeDocument = {
  getElementById() { return fakeEl; },
  querySelector() { return fakeEl; },
  querySelectorAll() { return []; },
  documentElement: { dataset: { theme: '' } },
  createElement() { return Object.assign({}, fakeEl); },
  createTextNode(text) { return { nodeType: 3, textContent: String(text) }; },
  addEventListener() {}, removeEventListener() {},
};
sandbox.document = fakeDocument;
sandbox.window.addEventListener = function () {};
sandbox.window.removeEventListener = function () {};
sandbox.fetch = function () { return Promise.reject(new Error('fetch unavailable in test')); };
sandbox.window.SandmanPortalState = null;
sandbox.window.SandmanPortalScroll = null;
sandbox.window.SandmanPortalDiff = null;
sandbox.setInterval = function () { return 0; };
sandbox.clearInterval = function () {};
sandbox.setTimeout = function () { return 0; };
sandbox.clearTimeout = function () {};
sandbox.window.setInterval = sandbox.setInterval;
sandbox.window.clearInterval = sandbox.clearInterval;
sandbox.requestAnimationFrame = function () {};
sandbox.window.requestAnimationFrame = sandbox.requestAnimationFrame;
sandbox.Intl = Intl;
sandbox.window.Intl = Intl;
// Wire the production renderRunMeta from portal.html into the test sandbox
// so tests that call helpers.renderRunMeta actually exercise the live
// function instead of the sharedMockHelpers() mock. The scriptBody for
// portal.html cannot be evaluated directly (it touches the DOM at top
// level), so we extract just the renderRunMeta function source and eval
// it into the sandbox before patching helpers.
const portalHtmlSrc = fs.readFileSync(htmlPath, 'utf8');
const renderRunMetaMatch = portalHtmlSrc.match(/function renderRunMeta\(run\) \{[\s\S]*?^\s{4}\}/m);
if (renderRunMetaMatch) {
  const fnCtx = vm.createContext(Object.assign({}, sandbox, { sandbox: sandbox }));
  vm.runInContext(renderRunMetaMatch[0] + '\n; sandbox.renderRunMeta = renderRunMeta;', fnCtx, { filename: htmlPath });
  if (typeof sandbox.renderRunMeta === 'function') {
    helpers.renderRunMeta = sandbox.renderRunMeta;
  }
}
vm.runInNewContext(scriptBody + '\n' + ` + "`" + js + "`" + `, Object.assign({}, sandbox, { helpers: sandbox.helpers }), { filename: htmlPath });
`
	cmd := exec.Command("node", "-e", prefix)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Logf("script output: %s", out)
	}
}

// TestPortalRunsView_VisibleRunForIssueGroup_PreservesReviewRunID is the
// regression test for the bug where a review-only daemon (no parent issue
// row in the same group) loses its real RunID in the synthesized "issue-N"
// stub row, which breaks row expansion because the DOM row key no longer
// matches any state.runs entry. The visibleRunForIssueGroup helper must
// preserve the source row's runId so renderRunMeta continues to surface
// the real run identifier in the meta line, and so findRunByIdentity can
// locate the row from its data-run-key.
func TestPortalDiffBuildLogPre_HasTerminalLogClass(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', log: 'some log output' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const content = detailRow.querySelector('.detail-content');
const pre = content.querySelector('pre[data-scroll-key]');
if (!pre) throw new Error('expected log pre');
const hasTerminalLog = pre.classList.contains('terminal-log');
if (!hasTerminalLog) throw new Error('expected terminal-log class');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalRunsView_VisibleRunForIssueGroup_PreservesReviewRunID(t *testing.T) {
	js := `const review = { key: 'a0c19-260622193226-1227', kind: 'active', status: 'reviewing', review: true, issueLabel: '#1223', runId: 'a0c19-260622193226-1227', issueNumber: 1223, prNumber: 5 };
const stub = visibleRunForIssueGroup(1223, [review]);
if (!stub) throw new Error('expected stub row for review-only issue group');
if (stub.runId !== 'a0c19-260622193226-1227') throw new Error('expected stub.runId to preserve source RunID, got ' + JSON.stringify(stub.runId));
if (stub.key !== 'a0c19-260622193226-1227') throw new Error('expected stub.key to preserve source RunID, got ' + JSON.stringify(stub.key));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_ReviewOnlyReturnsSourceRowWithReviewTrue
// is the tracer bullet for issue #1489.
func TestPortalRunsView_VisibleRunForIssueGroup_ReviewOnlyReturnsSourceRowWithReviewTrue(t *testing.T) {
	js := `const review = { key: 'a0c19-260622193226-1227', kind: 'active', status: 'reviewing', review: true, issueLabel: '#1223', runId: 'a0c19-260622193226-1227', issueNumber: 1223, prNumber: 5 };
const stub = visibleRunForIssueGroup(1223, [review]);
if (!stub) throw new Error('expected visible row for review-only issue group');
if (stub.review !== true) throw new Error('expected visible row to keep review=true for review-only issue group, got ' + JSON.stringify(stub.review));
if (stub.groupedReview !== false) throw new Error('expected groupedReview=false on review-only visible row, got ' + JSON.stringify(stub.groupedReview));
if (stub.runId !== 'a0c19-260622193226-1227') throw new Error('expected visible row runId to match source, got ' + JSON.stringify(stub.runId));
if (stub.key !== 'a0c19-260622193226-1227') throw new Error('expected visible row key to match source, got ' + JSON.stringify(stub.key));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestPortalRunsView_VisibleRunForIssueGroup_TerminalReviewWinsOverActiveKind(t *testing.T) {
	js := `const review = { key: 'a0c19-260622193226-1227', kind: 'active', status: 'success', review: true, issueLabel: '#1223', runId: 'a0c19-260622193226-1227', issueNumber: 1223, prNumber: 5 };
const stub = visibleRunForIssueGroup(1223, [review]);
if (!stub) throw new Error('expected stub row for terminal review group');
if (stub.kind !== 'completed') throw new Error('expected completed kind once terminal status is present, got ' + JSON.stringify(stub.kind));
if (stub.status !== 'success') throw new Error('expected terminal status to win over live kind, got ' + JSON.stringify(stub.status));
if (stub.reviewVerdict !== 'Approved') throw new Error('expected Approved verdict, got ' + JSON.stringify(stub.reviewVerdict));
if (stub.review !== true) throw new Error('expected review=true on visible row for terminal review-only group, got ' + JSON.stringify(stub.review));
if (stub.groupedReview !== false) throw new Error('expected groupedReview=false on visible row for review-only group, got ' + JSON.stringify(stub.groupedReview));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestPortalRunsView_VisibleRunForIssueGroup_PrefersLiveActiveOverCompletedParent(t *testing.T) {
	js := `const parent = { key: 'issue-1-parent', kind: 'completed', status: 'success', review: false, issueLabel: '#1', runId: 'issue-1-parent', issueNumber: 1, batchKey: 'batch-1' };
const active = { key: 'issue-1-active', kind: 'active', status: 'running', review: false, issueLabel: '#1', runId: 'issue-1-active', issueNumber: 1, batchKey: 'batch-1' };
const result = visibleRunForIssueGroup(1, [parent, active]);
if (!result) throw new Error('expected visible row');
if (result.key !== 'issue-1-active') throw new Error('expected live active child as visible row, got ' + JSON.stringify(result.key));
if (result.kind !== 'active') throw new Error('expected active kind for visible row, got ' + JSON.stringify(result.kind));
if (!helpers.isRunAbortable(result, new Set())) throw new Error('expected live active visible row to keep Abort control');
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestPortalRunsView_VisibleRunForIssueGroup_CompletedOnlyStillUsesCompletedRow(t *testing.T) {
	js := `const parent = { key: 'issue-2-parent', kind: 'completed', status: 'success', review: false, issueLabel: '#2', runId: 'issue-2-parent', issueNumber: 2, reviewCount: 3, reviewVerdict: 'Approved' };
const result = visibleRunForIssueGroup(2, [parent]);
if (!result) throw new Error('expected visible row');
if (result.key !== 'issue-2-parent') throw new Error('expected completed row to stay visible, got ' + JSON.stringify(result.key));
if (result.kind !== 'completed') throw new Error('expected completed kind to stay visible, got ' + JSON.stringify(result.kind));
if (result.status !== 'success') throw new Error('expected completed status to stay visible, got ' + JSON.stringify(result.status));
if (result.reviewVerdict !== 'Approved') throw new Error('expected existing completed summary verdict to stay visible, got ' + JSON.stringify(result.reviewVerdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_LiveReviewReprojectsKindOntoSourceRow
// covers the kind/status re-projection rule from issue #1489.
func TestPortalRunsView_VisibleRunForIssueGroup_LiveReviewReprojectsKindOntoSourceRow(t *testing.T) {
	js := `const terminal = { key: 'review-1', kind: 'completed', status: 'success', review: true, issueLabel: '#7', runId: 'review-1', issueNumber: 7, prNumber: 11, startedAt: '2026-06-29T10:00:00Z' };
const live = { key: 'review-2', kind: 'active', status: 'reviewing', review: true, issueLabel: '#7', runId: 'review-2', issueNumber: 7, prNumber: 12, startedAt: '2026-06-30T10:00:00Z' };
const stub = visibleRunForIssueGroup(7, [terminal, live]);
if (!stub) throw new Error('expected visible row for live review group');
if (stub.kind !== 'active') throw new Error('expected kind=active for live group, got ' + JSON.stringify(stub.kind));
if (stub.status !== 'reviewing') throw new Error('expected status=reviewing for live group, got ' + JSON.stringify(stub.status));
if (stub.review !== true) throw new Error('expected review=true on visible row, got ' + JSON.stringify(stub.review));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_TerminalParentWinsOverLiveChild
// is the regression test for issue #1362: when an issue has both a terminal
// parent run (status=success) and a live review child (status=reviewing,
// kind=active), the visible row must be the terminal parent, not the live
// child. The review child must remain accessible in the expanded selector.
func TestPortalRunsView_VisibleRunForIssueGroup_TerminalParentWinsOverLiveChild(t *testing.T) {
	js := `const parent = { key: 'issue-1', kind: 'completed', status: 'success', review: false, issueLabel: '#1', runId: 'issue-1', issueNumber: 1, reviewCount: 1 };
const liveChild = { key: 'PR42', kind: 'active', status: 'reviewing', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const result = visibleRunForIssueGroup(1, [parent, liveChild]);
if (!result) throw new Error('expected visible row');
if (result.key !== 'issue-1') throw new Error('expected parent as visible row, got ' + JSON.stringify(result.key));
if (result.status !== 'success') throw new Error('expected terminal status preserved, got ' + JSON.stringify(result.status));
if (result.kind !== 'completed') throw new Error('expected completed kind, got ' + JSON.stringify(result.kind));
if (result.review) throw new Error('expected review flag false for parent row, got ' + JSON.stringify(result.review));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestPortalRunsView_VisibleRunsForTable_SortsByStartedDesc(t *testing.T) {
	js := `const runs = [
  { key: 'issue-1014', kind: 'completed', status: 'failure', issueLabel: '#1014', runId: 'issue-1014', issueNumber: 1014, startedAt: '2026-06-29T04:34:39Z' },
  { key: 'issue-1401', kind: 'completed', status: 'failure', issueLabel: '#1401', runId: 'issue-1401', issueNumber: 1401, startedAt: '2026-06-26T19:47:13Z' },
  { key: 'issue-1402', kind: 'completed', status: 'failure', issueLabel: '#1402', runId: 'issue-1402', issueNumber: 1402, startedAt: '2026-06-27T00:12:51Z' },
  { key: 'issue-1467', kind: 'active', status: 'running', issueLabel: '#1467', runId: 'issue-1467', issueNumber: 1467, startedAt: '2026-06-29T14:29:12Z' },
];
const visible = visibleRunsForTable(runs);
const order = visible.map((run) => run.key).join(',');
if (order !== 'issue-1467,issue-1014,issue-1402,issue-1401') throw new Error('expected newest started rows first, got ' + JSON.stringify(order));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunsForTable_ReviewMetaLineShowsRealRunID
// covers the user-visible symptom: the meta-line under the title cell is
// fed by renderRunMeta(run), which reads run.runId. When
// visibleRunsForTable clobbers runId to "issue-N", the meta-line shows
// "issue-N" instead of the real run identifier. This test asserts on the
// post-table run shape the renderer actually receives.
func TestPortalRunsView_VisibleRunsForTable_ReviewMetaLineShowsRealRunID(t *testing.T) {
	js := `const review = { key: 'a0c19-260622193226-1227', kind: 'active', status: 'reviewing', review: true, issueLabel: '#1223', runId: 'a0c19-260622193226-1227', issueNumber: 1223, prNumber: 5 };
const visible = visibleRunsForTable([review]);
if (visible.length !== 1) throw new Error('expected 1 visible row, got ' + visible.length);
const rendered = visible[0];
if (rendered.runId !== 'a0c19-260622193226-1227') throw new Error('expected visible[0].runId to preserve source RunID for meta-line rendering, got ' + JSON.stringify(rendered.runId));
const meta = helpers.renderRunMeta(rendered);
if (!meta.includes('a0c19-260622193226-1227')) throw new Error('expected renderRunMeta to surface real run identifier, got ' + JSON.stringify(meta));
if (meta.startsWith('issue-')) throw new Error('expected renderRunMeta not to start with synthetic "issue-" stub, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestRenderRunMeta_BatchAboveRunID(t *testing.T) {
	js := `const runWithBatch = { key: 'a', runId: 'r1', batchKey: 'batch-42', kind: 'active', status: 'running' };
const metaWithBatch = helpers.renderRunMeta(runWithBatch);
if (metaWithBatch.indexOf('Batch:') < 0) throw new Error('expected Batch: in meta for run with batch, got ' + JSON.stringify(metaWithBatch));
if (metaWithBatch.indexOf('Run:') < 0) throw new Error('expected Run: in meta for run with batch, got ' + JSON.stringify(metaWithBatch));
const batchPos = metaWithBatch.indexOf('Batch:');
const runPos = metaWithBatch.indexOf('Run:');
if (batchPos > runPos) throw new Error('expected Batch: to appear before Run:, got Batch: at ' + batchPos + ', Run: at ' + runPos + ' in ' + JSON.stringify(metaWithBatch));

const runWithoutBatch = { key: 'b', runId: 'r2', kind: 'active', status: 'running' };
const metaWithoutBatch = helpers.renderRunMeta(runWithoutBatch);
if (metaWithoutBatch.indexOf('Run:') < 0) throw new Error('expected Run: in meta for run without batch, got ' + JSON.stringify(metaWithoutBatch));
if (metaWithoutBatch.indexOf('Batch:') >= 0) throw new Error('expected no Batch: in meta for run without batch, got ' + JSON.stringify(metaWithoutBatch));

console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestRenderRunMeta_ActiveFreshBatchRow_RendersBothBatchAndRunLabels(t *testing.T) {
	js := `const run = { key: 'abcd-260618113825-issue-42', runId: 'abcd-260618113825-issue-42', batchKey: 'abcd-260618113825', kind: 'active', status: 'running' };
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('Batch:') < 0) throw new Error('expected Batch: in meta for active fresh batch row, got ' + JSON.stringify(meta));
if (meta.indexOf('Run:') < 0) throw new Error('expected Run: in meta for active fresh batch row, got ' + JSON.stringify(meta));
if (meta.indexOf('abcd-260618113825') < 0) throw new Error('expected batchKey value in Batch: label, got ' + JSON.stringify(meta));
if (meta.indexOf('abcd-260618113825-issue-42') < 0) throw new Error('expected runId value in Run: label, got ' + JSON.stringify(meta));
const batchPos = meta.indexOf('Batch:');
const runPos = meta.indexOf('Run:');
if (batchPos > runPos) throw new Error('expected Batch: to appear before Run:, got Batch: at ' + batchPos + ', Run: at ' + runPos + ' in ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestRenderRunMeta_QueuedRow_SuppressesRunLabel(t *testing.T) {
	js := `const run = { key: 'abcd-260618113825-issue-42', runId: '', batchKey: 'abcd-260618113825', kind: 'active', status: 'queued' };
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('Batch:') < 0) throw new Error('expected Batch: in meta for queued row, got ' + JSON.stringify(meta));
if (meta.indexOf('abcd-260618113825') < 0) throw new Error('expected batchKey value in Batch: label, got ' + JSON.stringify(meta));
if (meta.indexOf('Run:') >= 0) throw new Error('expected no Run: in meta for queued row, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}
