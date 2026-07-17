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

func TestPortalDiffRefreshLiveTimeCells_UpdatesDurationAndStalenessWithoutFullRender(t *testing.T) {
	js := `const body = makeMockBody();
const realNow = Date.now;
try {
  Date.now = () => 1700000050000;
  const run = {
    key: 'active', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1',
    startedAt: new Date(1700000000000).toISOString(),
    lastOutputAt: new Date(1700000000000).toISOString(),
    duration: '50s',
  };
  const created = SandmanPortalDiff.insertRunRow(body, run, { helpers, expandedKey: null });
  const durationCell = created.row.querySelector('[data-cell="duration"]');
  if (durationCell.querySelector('.duration-value').textContent !== '50s') throw new Error('expected initial server duration');
  if (durationCell.querySelector('.stale-line')) throw new Error('run should not be stale at 50s');

  Date.now = () => 1700000130000;
  SandmanPortalDiff.refreshLiveTimeCells(body, [run]);

  if (durationCell.querySelector('.duration-value').textContent !== '2m10s') throw new Error('expected duration to advance on an unchanged summary poll');
  const stale = durationCell.querySelector('.stale-line');
  if (!stale || stale.textContent !== 'stale · 2m 10s') throw new Error('expected stale time to advance on an unchanged summary poll');
  console.log('PASS');
} finally {
  Date.now = realNow;
}
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

func TestPortalDiffCreateRunRow_RendersRetryAndReviewSummaryAndDropsGroupedChips(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, retriesDone: 3, retriesTotal: 3, reviewCount: 2, reviewVerdict: 'Approved', batchIssues: [1, 2, 3] };
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

const singularRun = { key: '260618113825-abcd-2', kind: 'active', status: 'reviewing', issueLabel: '#2', runId: '260618113825-abcd-2', issueNumber: 2, retriesDone: 1, retriesTotal: 1 };
SandmanPortalDiff.insertRunRow(body, singularRun, opts);
const singularMeta = body.querySelector('tr[data-run-key="260618113825-abcd-2"]').querySelector('[data-cell="title"]').children[0].children[1];
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

// TestRenderRunMeta_ActiveWithAttempts_RendersChipOnCounterLine is the
// tracer bullet for slice 4 of #1499: an active row backed by
// run.started + run.retry (no run.finished) must surface the live
// attempt count on the trailing counter line in the same "N retries"
// shape the finished #1483 path uses, and stay within the 3-line
// Batch/Run/counter cap.
func TestRenderRunMeta_ActiveWithAttempts_RendersChipOnCounterLine(t *testing.T) {
	js := `const run = { key: 'active-2', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r-2', issueNumber: 42, batchKey: 'b-2', attempts: 2, lastRetryReason: 'agent-stalled' };
const meta = helpers.renderRunMeta(run);
const NL = String.fromCharCode(10);
const lines = meta.split(NL);
if (lines.length !== 3) throw new Error('expected exactly 3 lines (Batch, Run, counter) for active-with-attempts row, got: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: b-2') throw new Error('expected Batch line first, got: ' + JSON.stringify(lines));
if (lines[1] !== 'Run: r-2') throw new Error('expected Run line second, got: ' + JSON.stringify(lines));
if (lines[2] !== '2 retries') throw new Error('expected "2 retries" on trailing line, got: ' + JSON.stringify(lines));
if (meta !== 'Batch: b-2' + NL + 'Run: r-2' + NL + '2 retries') throw new Error('expected exact meta text, got: ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_ActiveWithSingleAttempt_UsesSingularLabel locks in
// the singular/plural wording for the new active-path chip (1 retry,
// not 1 retries).
func TestRenderRunMeta_ActiveWithSingleAttempt_UsesSingularLabel(t *testing.T) {
	js := `const run = { key: 'active-1', kind: 'active', status: 'running', issueLabel: '#1', runId: 'r-1', issueNumber: 1, attempts: 1, lastRetryReason: 'agent-stalled' };
const meta = helpers.renderRunMeta(run);
if (!meta.includes('1 retry')) throw new Error('expected "1 retry" for attempts=1, got: ' + JSON.stringify(meta));
if (meta.includes('1 retries')) throw new Error('must not show "1 retries", got: ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_ActiveRowFromServerJSONWithOneRetryEvent_RendersOneRetry
// is the end-to-end HTML regression test for issue #1879: when a server
// JSON row is backed by exactly one run.retry event with attempt=2
// (i.e., one actual retry), the live attempts field is the retry count
// (1), and the rendered meta line must read "1 retry" — not "2 retries".
// The bug report ("1 retry + initial run shows '2 retries'") goes away
// here. The run object mirrors the portalRun wire format emitted by
// portalRunsView.compute (the same shape /api/runs serializes), with
// retriesDone omitted because the run has not finished. The test drives
// the same runPortalHTMLScript seam as the adjacent slice tests.
func TestRenderRunMeta_ActiveRowFromServerJSONWithOneRetryEvent_RendersOneRetry(t *testing.T) {
	js := `const serverRow = { key: 'r-active-1retry', runId: 'r-active-1retry', kind: 'active', status: 'running', issueLabel: '#42', issueNumber: 42, attempts: 1, lastRetryReason: 'agent-stalled' };
const meta = helpers.renderRunMeta(serverRow);
if (!meta.includes('1 retry')) throw new Error('expected meta to include "1 retry" for one retry event (attempt=2 maps to retry count 1), got: ' + JSON.stringify(meta));
if (meta.includes('2 retries')) throw new Error('must not show "2 retries" for one retry event, got: ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_ActiveZeroAttempts_OmitsCounterLine is the
// regression guard for the no-attempts active case: when attempts is
// zero (or omitted) the trailing counter line must not appear, even
// though the run is active. The active row stays at exactly the
// 2-line Batch/Run layout from #1483.
func TestRenderRunMeta_ActiveZeroAttempts_OmitsCounterLine(t *testing.T) {
	js := `const run = { key: 'clean-active', kind: 'active', status: 'running', issueLabel: '#7', runId: 'r-7', issueNumber: 7, batchKey: 'b-7', attempts: 0 };
const meta = helpers.renderRunMeta(run);
const lines = meta.split(String.fromCharCode(10));
if (lines.length !== 2) throw new Error('expected exactly 2 lines (Batch, Run) for active-with-zero-attempts row, got: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: b-7') throw new Error('expected Batch line first, got: ' + JSON.stringify(lines));
if (lines[1] !== 'Run: r-7') throw new Error('expected Run line second, got: ' + JSON.stringify(lines));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_ActiveAttemptsJoinsReviewCounter asserts the chip
// joins the existing review counter on the trailing counter line, with
// the same " - " separator #1483 uses. The 3-line Batch/Run/counter
// cap is preserved.
func TestRenderRunMeta_ActiveAttemptsJoinsReviewCounter(t *testing.T) {
	js := `const run = { key: 'active-r', kind: 'active', status: 'reviewing', issueLabel: '#9', runId: 'r-9', issueNumber: 9, batchKey: 'b-9', attempts: 2, lastRetryReason: 'agent-stalled', reviewCount: 1, reviewVerdict: 'Approved' };
const meta = helpers.renderRunMeta(run);
const lines = meta.split(String.fromCharCode(10));
if (lines.length !== 3) throw new Error('expected exactly 3 lines (Batch, Run, joined counter), got: ' + JSON.stringify(lines));
if (lines[0] !== 'Batch: b-9') throw new Error('expected Batch line first, got: ' + JSON.stringify(lines));
if (lines[1] !== 'Run: r-9') throw new Error('expected Run line second, got: ' + JSON.stringify(lines));
if (lines[2] !== '2 retries - 1 review - Approved') throw new Error('expected joined counter line, got: ' + JSON.stringify(lines));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_FinishedRunWithAttemptsAndRetriesDone_DoesNotDuplicate
// is the duplication-guard regression: a finished row carries both
// attempts (sourced from retries_done by slice 1's attemptsForRun) and
// retriesDone in the JSON, but the chip must emit only once. The
// attempts branch is gated on retriesDone === 0 so the chip is the
// active-only path; finished rows use the existing retriesDone branch.
func TestRenderRunMeta_FinishedRunWithAttemptsAndRetriesDone_DoesNotDuplicate(t *testing.T) {
	js := `const run = { key: 'fin-2', kind: 'completed', status: 'success', issueLabel: '#42', runId: 'fr-2', issueNumber: 42, batchKey: 'fb-2', retriesDone: 2, attempts: 2, reviewCount: 1, reviewVerdict: 'Approved' };
const meta = helpers.renderRunMeta(run);
const lines = meta.split(String.fromCharCode(10));
if (lines.length !== 3) throw new Error('expected exactly 3 lines (Batch, Run, counter) for finished run, got: ' + JSON.stringify(lines));
if (lines[2] !== '2 retries - 1 review - Approved') throw new Error('expected single retry counter, no duplication, got: ' + JSON.stringify(lines[2]));
if (lines[2].includes('2 retries - 2 retries')) throw new Error('finished row duplicated the retry counter, got: ' + JSON.stringify(lines[2]));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalDiffCreateRunRow_ActiveAttemptsChipPersistsTitle is the
// client-path coverage for slice 4: when SandmanPortalDiff.insertRunRow
// renders an active row backed by run.started + run.retry, the
// meta-line element must carry the "2 retries" text on its textContent
// AND a title attribute naming the most recent retry reason. The
// tooltip is set on first render in buildTitleCell and persists across
// reconciliation because setText only touches textContent.
func TestPortalDiffCreateRunRow_ActiveAttemptsChipPersistsTitle(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'r-active', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r-42', issueNumber: 42, batchKey: 'b-42', attempts: 2, lastRetryReason: 'agent-stalled' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
const meta = created.row.querySelector('[data-cell="title"]').children[0].children[1];
if (!meta.textContent.includes('2 retries')) throw new Error('expected meta text to include "2 retries", got ' + JSON.stringify(meta.textContent));
const title = meta.getAttribute('title');
if (!title || !title.includes('agent-stalled')) throw new Error('expected title attribute to include reason, got ' + JSON.stringify(title));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffCreateRunRow_ActiveAttemptsEmptyReason_OmitsTitle pins
// the empty-reason branch: the chip is still shown but the meta-line
// has no title attribute (no dangling tooltip).
func TestPortalDiffCreateRunRow_ActiveAttemptsEmptyReason_OmitsTitle(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'r-active-nr', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r-42nr', issueNumber: 42, batchKey: 'b-42', attempts: 2, lastRetryReason: '' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
const meta = created.row.querySelector('[data-cell="title"]').children[0].children[1];
if (!meta.textContent.includes('2 retries')) throw new Error('expected meta text to include "2 retries", got ' + JSON.stringify(meta.textContent));
const title = meta.getAttribute('title');
if (title) throw new Error('expected no title attribute when reason is empty, got ' + JSON.stringify(title));
console.log('PASS');
`
	runNodeScript(t, js)
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
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1 };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const retryRun = { key: 'retry-1', kind: 'active', status: 'running', issueLabel: 'Issue 1 retry', runId: 'retry-1', issueNumber: 1 };
const run = parentRun;
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', runs: [parentRun, retryRun, childReview] };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row) throw new Error('expected data row');
if (!created.detailRow) throw new Error('expected detail row when expandedKey matches');
if (created.detailRow.getAttribute('data-detail-for') !== '260618113825-abcd-1') throw new Error('detail row has wrong data-detail-for');
if (created.row.getAttribute('id') !== 'run-row-260618113825-abcd-1') throw new Error('expected stable row id, got ' + created.row.getAttribute('id'));
if (created.row.getAttribute('aria-controls') !== 'run-detail-260618113825-abcd-1') throw new Error('expected aria-controls=run-detail-issue-1, got ' + created.row.getAttribute('aria-controls'));
if (created.detailRow.getAttribute('id') !== 'run-detail-260618113825-abcd-1') throw new Error('expected stable detail id, got ' + created.detailRow.getAttribute('id'));
if (created.detailRow.getAttribute('role') !== 'region') throw new Error('expected detail row role=region');
if (created.detailRow.getAttribute('aria-labelledby') !== 'run-row-260618113825-abcd-1') throw new Error('expected detail row aria-labelledby run-row-issue-1, got ' + created.detailRow.getAttribute('aria-labelledby'));
if (body.children.length !== 2) throw new Error('expected body to have 2 children, got ' + body.children.length);
const subjectSelect = created.detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect) throw new Error('expected subject selector in detail row');
if (subjectSelect.children.length !== 2) throw new Error('expected selector for parent and one child review, got ' + subjectSelect.children.length);
if (subjectSelect.children[0].getAttribute('value') !== '260618113825-abcd-1') throw new Error('expected parent option value to use run ID, got ' + subjectSelect.children[0].getAttribute('value'));
if (subjectSelect.children[1].getAttribute('value') !== 'PR42') throw new Error('expected child review option value to use run ID, got ' + subjectSelect.children[1].getAttribute('value'));
if (subjectSelect.children[0].textContent !== '260618113825-abcd-1') throw new Error('expected parent label to be run ID only, got ' + subjectSelect.children[0].textContent);
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
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1 };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR42', runs: [parentRun, childReview], visibleRuns: [parentRun] };
const created = SandmanPortalDiff.insertRunRow(body, parentRun, opts);
if (!created.detailRow) throw new Error('expected detail row when child review is expanded');
if (body.querySelector('tr[data-run-key="PR42"]')) throw new Error('expected hidden child review row not to render');
if (body.querySelector('tr[data-run-key="260618113825-abcd-1"]') !== created.row) throw new Error('expected visible parent row to remain rendered');
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
	// A review-only orphan row is always alone (single subject), so there is
	// nothing to switch between and no subject selector should render. Its
	// detail panel shows the review's own content directly.
	const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
	if (subjectSelect) throw new Error('expected no subject selector for a single orphan review, got ' + subjectSelect.children.length + ' options');
	console.log('PASS');
	`
	runNodeScript(t, js)
}

// TestPortalDiffDiffRuns_SubjectSelectorExcludesUndefinedIssueRuns is the
// #1615 regression for the loose issueNumber guard: runs with a missing or
// non-integer issueNumber must never be grouped together (the old
// `issueNumber <= 0` check let `undefined` through, grouping unrelated
// no-issue rows by undefined and producing a giant spurious selector on a
// review-only orphan).
func TestPortalDiffDiffRuns_SubjectSelectorExcludesUndefinedIssueRuns(t *testing.T) {
	js := `const body = makeMockBody();
// Orphan review with no issueNumber at all — the expanded row.
const orphan = { key: 'PR42', runId: 'PR42', review: true, issueLabel: 'Review of PR 42', prNumber: 42, kind: 'active', status: 'reviewing', startedAt: '2026-06-30T12:00:00Z' };
// Unrelated runs that carry NO issueNumber (prompt-only rows).
// These must be ignored entirely, never grouped onto the orphan.
const noIssue1 = { key: 'prompt-a', runId: 'prompt-a', issueLabel: 'Prompt', kind: 'active', status: 'running' };
const noIssue2 = { key: 'auto-b', runId: 'auto-b', issueLabel: 'Auto', kind: 'active', status: 'running' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR42', runs: [orphan, noIssue1, noIssue2], visibleRuns: [orphan] };
const result = SandmanPortalDiff.diffRuns(body, [orphan], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
const detailRow = body.querySelector('tr.detail-row[data-detail-for="PR42"]');
if (!detailRow) throw new Error('expected detail row for the orphan review');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (subjectSelect) throw new Error('expected no subject selector on a single-subject orphan, and undefined-issue runs must never join it, got ' + subjectSelect.children.length + ' options');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_SubjectSelectorIncludesDistinctParentAndReview(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', review: false, issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1 };
const review = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', runs: [parentRun, review], visibleRuns: [parentRun] };
const result = SandmanPortalDiff.diffRuns(body, [parentRun, review], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
const detailRow = body.querySelector('tr.detail-row[data-detail-for="260618113825-abcd-1"]');
if (!detailRow) throw new Error('expected detail row for parent run');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect) throw new Error('expected subject selector for parent run');
if (subjectSelect.children.length !== 2) throw new Error('expected parent and review options, got ' + subjectSelect.children.length);
if (subjectSelect.children[0].getAttribute('value') !== '260618113825-abcd-1') throw new Error('expected parent option to use the parent run id, got ' + subjectSelect.children[0].getAttribute('value'));
if (subjectSelect.children[1].getAttribute('value') !== 'PR42') throw new Error('expected review option to use the review run id, got ' + subjectSelect.children[1].getAttribute('value'));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_SubjectSelectorSkipsQueuedAndBlockedPlaceholders(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', review: false, issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1 };
const review = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const queued = { key: 'queued-1', kind: 'active', status: 'queued', review: false, issueLabel: '#1 queued', runId: 'queued-1', issueNumber: 1 };
const blocked = { key: 'blocked-1', kind: 'active', status: 'blocked', review: false, issueLabel: '#1 blocked', runId: 'blocked-1', issueNumber: 1 };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', runs: [parentRun, review, queued, blocked], visibleRuns: [parentRun] };
const result = SandmanPortalDiff.diffRuns(body, [parentRun], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
const detailRow = body.querySelector('tr.detail-row[data-detail-for="260618113825-abcd-1"]');
if (!detailRow) throw new Error('expected detail row for parent run');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect) throw new Error('expected subject selector for parent run');
if (subjectSelect.children.length !== 2) throw new Error('expected parent and review options only, got ' + subjectSelect.children.length);
const values = Array.from(subjectSelect.children).map((opt) => opt.getAttribute('value'));
if (values.indexOf('260618113825-abcd-1') < 0 || values.indexOf('PR42') < 0) throw new Error('expected parent and review subjects, got ' + JSON.stringify(values));
if (values.indexOf('queued-1') >= 0 || values.indexOf('blocked-1') >= 0) throw new Error('expected queued/blocked placeholders to be filtered, got ' + JSON.stringify(values));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_RefreshesSubjectSelectorWhenChildReviewAppears(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1 };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', runs: [parentRun] };
const created = SandmanPortalDiff.insertRunRow(body, parentRun, opts1);
const subjectSelectBefore = created.detailRow.querySelector('select[data-action="set-subject"]');
if (subjectSelectBefore) throw new Error('expected no subject selector before a child review appears (nothing to switch between), got ' + subjectSelectBefore.children.length + ' options');
SandmanPortalDiff.resetCounters();
const opts2 = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', runs: [parentRun, childReview] };
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
	// A single review-only orphan is always alone: nothing to switch between,
	// so no subject selector should render on its detail row.
	const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
	if (subjectSelect) throw new Error('expected no subject selector for a single orphan review, got ' + subjectSelect.children.length + ' options');
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
if (removed.count !== 2) throw new Error('expected 2 rows removed, got ' + removed.count);
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
if (removed.count !== 1) throw new Error('expected 1 row removed, got ' + removed.count);
if (body.children.length !== 0) throw new Error('expected body empty after remove');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffInsertRunRow_AddsRowAddedClass(t *testing.T) {
	js := `const body = makeMockBody();
// Wait-state row (no kind) keeps row-added as the "just inserted" cue.
const run = { key: 'a', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (!created.row) throw new Error('expected data row');
if (!created.row.classList.contains('row-added')) throw new Error('expected row-added class on inserted wait-state row, got ' + JSON.stringify(Array.from(created.row.classList)));
const cellTags = created.row.children.filter((c) => c.tagName === 'TD' || c.tagName === 'TH');
if (cellTags.length !== 6) throw new Error('expected 6 cells in inserted row, got ' + cellTags.length);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffRemoveRunRow_AddsRowRemovedClassBeforeDetach(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'Issue 1', runId: 'r1' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a' };
SandmanPortalDiff.insertRunRow(body, run, opts);
const result = SandmanPortalDiff.removeRunRow(body, 'a');
if (result.count !== 2) throw new Error('expected 2 rows removed, got ' + result.count);
if (!Array.isArray(result.rows) || result.rows.length !== 2) throw new Error('expected 2 detached rows in result.rows, got ' + (result.rows && result.rows.length));
const dataRow = result.rows.find((r) => r.getAttribute && r.getAttribute('data-run-key') === 'a');
if (!dataRow) throw new Error('expected detached data row in result.rows');
if (!dataRow.classList.contains('row-removed')) throw new Error('expected row-removed class on detached row, got ' + JSON.stringify(Array.from(dataRow.classList)));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffInsertRunRow_ActiveKindOmitsRowAdded pins the contract
// from PR #1677 follow-up: an active row (kind === 'active') must NOT
// carry the sticky row-added highlight either. The green --success tint
// on row-added otherwise wins over the purple --reviewing-accent active
// background at equal CSS specificity (both (0,0,2,3) on the td), so
// the active row would render green instead of purple. Skipping row-added
// for active rows makes the purple highlight win by absence.
func TestPortalDiffInsertRunRow_ActiveKindOmitsRowAdded(t *testing.T) {
	js := `const body = makeMockBody();
const active = { key: 'live', kind: 'active', status: 'running', issueLabel: 'Live', runId: 'r1' };
const reviewing = { key: 'rev', kind: 'active', status: 'reviewing', issueLabel: 'Rev', runId: 'r2' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, active, opts);
SandmanPortalDiff.insertRunRow(body, reviewing, opts);
const liveRow = body.querySelector('tr[data-run-key="live"]');
const revRow = body.querySelector('tr[data-run-key="rev"]');
if (!liveRow) throw new Error('expected active row to be inserted');
if (!revRow) throw new Error('expected reviewing row to be inserted');
if (!liveRow.classList.contains('active')) throw new Error('expected active kind class on active row');
if (liveRow.classList.contains('row-added')) throw new Error('active row must not carry row-added (PR #1677 follow-up: row-added green would override the purple active highlight at equal CSS specificity), got ' + JSON.stringify(Array.from(liveRow.classList)));
if (revRow.classList.contains('row-added')) throw new Error('reviewing row must not carry row-added (same reason), got ' + JSON.stringify(Array.from(revRow.classList)));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffInsertRunRow_TerminalKindOmitsRowAdded pins the contract
// from issue #1669: a terminal row (kind === 'completed') must NOT carry
// the sticky row-added highlight, so completed/failed rows render in the
// neutral background instead of the green success tint. The Sandman theme
// expects inactive rows to be muted against active rows.
func TestPortalDiffInsertRunRow_TerminalKindOmitsRowAdded(t *testing.T) {
	js := `const body = makeMockBody();
const completed = { key: 'done', kind: 'completed', status: 'success', issueLabel: 'Done', runId: 'r1' };
const failed = { key: 'fail', kind: 'completed', status: 'failure', issueLabel: 'Fail', runId: 'r2' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.insertRunRow(body, completed, opts);
SandmanPortalDiff.insertRunRow(body, failed, opts);
const doneRow = body.querySelector('tr[data-run-key="done"]');
const failRow = body.querySelector('tr[data-run-key="fail"]');
if (!doneRow) throw new Error('expected completed row to be inserted');
if (!failRow) throw new Error('expected failed row to be inserted');
if (!doneRow.classList.contains('run-row')) throw new Error('expected run-row class on terminal row');
if (!doneRow.classList.contains('completed')) throw new Error('expected completed kind class on terminal row');
if (doneRow.classList.contains('row-added')) throw new Error('terminal row must not carry row-added (issue #1669: completed rows render with neutral background, not success tint), got ' + JSON.stringify(Array.from(doneRow.classList)));
if (failRow.classList.contains('row-added')) throw new Error('terminal row must not carry row-added (issue #1669: failed rows render with neutral background, not success tint), got ' + JSON.stringify(Array.from(failRow.classList)));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffDiffRuns_InsertedAndRemovedRowsCarryHighlightClasses is the
// end-to-end coverage of the diff render path (issue #1548): when diffRuns
// inserts a new run, the new row carries row-added; when diffRuns removes
// a run via its inline removal branch, the detached row carried row-removed
// at the moment of removal (asserted via the returned dataRow, which still
// has its class list set even though the node is detached from body).
func TestPortalDiffDiffRuns_InsertedAndRemovedRowsCarryHighlightClasses(t *testing.T) {
	js := `const body = makeMockBody();
// Wait-state rows (no kind) keep row-added as the "just inserted" cue.
const runA = { key: 'a', status: 'running', issueLabel: 'A', runId: 'r1' };
const runB = { key: 'b', status: 'running', issueLabel: 'B', runId: 'r2' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, [runA], opts1);
const aRow = body.querySelector('tr[data-run-key="a"]');
if (!aRow) throw new Error('expected row a after first diff');
if (!aRow.classList.contains('row-added')) throw new Error('expected row-added on first-inserted row a');
// Now diff with a + b. Row a should still carry row-added (kept on the
// existing row, not re-added), and row b should be inserted with row-added.
const opts2 = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, [runA, runB], opts2);
const bRow = body.querySelector('tr[data-run-key="b"]');
if (!bRow) throw new Error('expected row b after second diff');
if (!bRow.classList.contains('row-added')) throw new Error('expected row-added on newly-inserted row b');
// Now diff with only b. Row a is removed via the inline branch; assert
// it carried row-removed at the moment of removal (the dataRow reference
// is still readable after detach).
const opts3 = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, [runB], opts3);
if (!aRow.classList.contains('row-removed')) throw new Error('expected row-removed on detached row a, got ' + JSON.stringify(Array.from(aRow.classList)));
if (body.querySelector('tr[data-run-key="a"]')) throw new Error('expected row a detached from body');
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

// TestPortalDiffUpdateDetailContent_ReviewSubjectStreamShieldsPane extends the
// contract pinned by TestPortalDiffUpdateDetailContent_StreamedLogIsShieldedFromPoll
// to the grouped-review subject case. While a review run is the streaming subject
// (streamingKeys holds the review key), the poll path must not overwrite its
// <pre>. Once the key is removed, the poll resumes and reconciles normally.
func TestPortalDiffUpdateDetailContent_ReviewSubjectStreamShieldsPane(t *testing.T) {
	js := `const body = makeMockBody();
const parent = { key: 'impl-1', runId: 'impl-1', kind: 'active', status: 'running', issueLabel: '#1', issueNumber: 1, log: 'impl line 1' };
const review = { key: 'PR42', runId: 'PR42', kind: 'active', status: 'reviewing', issueLabel: 'PR42', issueNumber: 1, prNumber: 42, review: true, log: 'review line 1' };
const streamingKeys = new Set();
const opts = { helpers, stopGroups: new Set(), runs: [parent, review], visibleRuns: [parent], expandedKey: 'PR42', tabs: { PR42: 'log' }, streamingKeys };
SandmanPortalDiff.diffRuns(body, [parent], opts);
const parentDetail = body.querySelector('tr.detail-row[data-detail-for="impl-1"]');
if (!parentDetail) throw new Error('expected detail row keyed by parent impl-1');
const reviewPre = parentDetail.querySelector('pre[data-scroll-key="PR42"]');
if (!reviewPre) throw new Error('expected review pane keyed by data-scroll-key=PR42');
const before = reviewPre.getAttribute('data-rendered-log') || '';
if (before !== 'review line 1') throw new Error('initial review pane wrong, got: ' + JSON.stringify(before));

// Stream takes ownership; a poll with a longer review.log must NOT clobber the pre.
streamingKeys.add('PR42');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [parent], Object.assign({}, opts, { runs: [parent, Object.assign({}, review, { log: 'review line 1\nreview line 2' })] }));
const guarded = reviewPre.getAttribute('data-rendered-log') || '';
if (guarded !== before) throw new Error('streamed review pane was clobbered by poll (before=' + JSON.stringify(before) + ', after=' + JSON.stringify(guarded) + ')');

// Stream releases; the poll resumes and appends the new line.
streamingKeys.delete('PR42');
SandmanPortalDiff.diffRuns(body, [parent], Object.assign({}, opts, { runs: [parent, Object.assign({}, review, { log: 'review line 1\nreview line 2' })] }));
const resumed = reviewPre.getAttribute('data-rendered-log') || '';
if (resumed === before) throw new Error('poll did not resume updating the review log after the stream released it');
if (!/review line 2/.test(resumed)) throw new Error('resumed log missing the new line, got ' + JSON.stringify(resumed));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_DetailRowKeying_SubjectScrollKeyVsParentDetailFor pins the
// DOM shape that the streamPreFor fix relies on: for a grouped review subject,
// the detail row is keyed by the parent (data-detail-for=parent key) but the
// <pre> inside it is keyed by the subject (data-scroll-key=review key). This
// means a lookup via data-detail-for=<reviewKey> finds nothing, which is why
// the old streamPreFor returned null and dropped every streamed line.
func TestPortalDiff_DetailRowKeying_SubjectScrollKeyVsParentDetailFor(t *testing.T) {
	js := `const body = makeMockBody();
const parent = { key: 'impl-1', runId: 'impl-1', kind: 'active', status: 'running', issueLabel: '#1', issueNumber: 1, log: 'p' };
const review = { key: 'PR42', runId: 'PR42', kind: 'active', status: 'reviewing', issueLabel: 'PR42', issueNumber: 1, prNumber: 42, review: true, log: 'r' };
const opts = { helpers, stopGroups: new Set(), runs: [parent, review], visibleRuns: [parent], expandedKey: 'PR42', tabs: { PR42: 'log' } };
SandmanPortalDiff.diffRuns(body, [parent], opts);

// The detail row is keyed by the PARENT, not the review subject.
const parentDetail = body.querySelector('tr.detail-row[data-detail-for="impl-1"]');
if (!parentDetail) throw new Error('expected detail row keyed by parent impl-1');
// There is no detail row keyed by the review — this is why the old streamPreFor
// returned null for review subjects and the SSE coalescer dropped every line.
const reviewDetail = body.querySelector('tr.detail-row[data-detail-for="PR42"]');
if (reviewDetail) throw new Error('review must NOT own a detail row (it borrows the parent row)');

// The review pane is mounted inside the parent detail row, keyed by the SUBJECT's
// data-scroll-key. The fixed streamPreFor resolves it via
// runsBody.querySelector('pre[data-scroll-key="PR42"]').
const reviewPane = parentDetail.querySelector('pre[data-scroll-key="PR42"]');
if (!reviewPane) throw new Error('expected review pane keyed by data-scroll-key=PR42 inside parent detail');
if (reviewPane.getAttribute('data-rendered-log') !== 'r') {
  throw new Error('review pane did not render the review log, got: ' + JSON.stringify(reviewPane.getAttribute('data-rendered-log')));
}
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
const activeRun = { key: 'run-42-1-42', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r1', issueNumber: 42, socketPath: '/tmp/sock', batchKey: 'run-42-1' };
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
const deadDaemonRun = { key: 'run-42-1-42', kind: 'active', status: 'running', issueLabel: '#42', runId: 'r1', issueNumber: 42, socketPath: '/tmp/sock', batchKey: '' };
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
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1, log: '' };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, log: 'review log' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', tabs: { '260618113825-abcd-1': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
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

func TestPortalDiffUpdateDetailEvents_IgnoresSubjectPickerChurn(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1, events: [{ type: 'start', timestamp: 1700000000000, payload: { ok: true } }] };
const reviewRun = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const reviewRun2 = { key: 'PR43', kind: 'completed', status: 'success', review: true, issueLabel: 'PR43', runId: 'PR43', issueNumber: 1, prNumber: 43 };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', tabs: { '260618113825-abcd-1': 'events' }, runs: [parentRun, reviewRun], visibleRuns: [parentRun] };
SandmanPortalDiff.diffRuns(body, [parentRun], opts1);
const detailRow = body.children[1];
const content1 = detailRow.querySelector('.detail-content');
const pre1 = detailRow.querySelector('pre[data-rendered-json]');
if (!content1 || !pre1) throw new Error('expected initial events content');
const opts2 = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', tabs: { '260618113825-abcd-1': 'events' }, runs: [parentRun, reviewRun, reviewRun2], visibleRuns: [parentRun] };
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [parentRun], opts2);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('subject-picker churn should still update the panel chrome, got 0 mutations');
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('events content identity should stay stable when only subject-picker options change');
const pre2 = detailRow.querySelector('pre[data-rendered-json]');
if (pre2 !== pre1) throw new Error('events pre should not be replaced when payload is unchanged');
if (pre2.getAttribute('data-rendered-json') !== pre1.getAttribute('data-rendered-json')) throw new Error('events payload should stay byte-identical when unchanged');
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

func TestPortalDiffUpdateDetailDetails_IgnoresSubjectPickerChurn(t *testing.T) {
	js := `const body = makeMockBody();
const parentRun = { key: '260618113825-abcd-1', kind: 'completed', status: 'success', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1, startedAt: 1000, finishedAt: 2000, duration: 1, branch: 'main', logPath: '/tmp/run.log' };
const reviewRun = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const reviewRun2 = { key: 'PR43', kind: 'completed', status: 'success', review: true, issueLabel: 'PR43', runId: 'PR43', issueNumber: 1, prNumber: 43 };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', tabs: { '260618113825-abcd-1': 'details' }, runs: [parentRun, reviewRun], visibleRuns: [parentRun] };
SandmanPortalDiff.diffRuns(body, [parentRun], opts1);
const detailRow = body.children[1];
const content1 = detailRow.querySelector('.detail-content');
const pre1 = detailRow.querySelector('pre[data-rendered-json]');
if (!content1 || !pre1) throw new Error('expected initial details content');
const opts2 = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', tabs: { '260618113825-abcd-1': 'details' }, runs: [parentRun, reviewRun, reviewRun2], visibleRuns: [parentRun] };
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [parentRun], opts2);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('subject-picker churn should still update the panel chrome, got 0 mutations');
const content2 = detailRow.querySelector('.detail-content');
if (content2 !== content1) throw new Error('details content identity should stay stable when only subject-picker options change');
const pre2 = detailRow.querySelector('pre[data-rendered-json]');
if (pre2 !== pre1) throw new Error('details pre should not be replaced when payload is unchanged');
if (pre2.getAttribute('data-rendered-json') !== pre1.getAttribute('data-rendered-json')) throw new Error('details payload should stay byte-identical when unchanged');
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
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1, log: 'parent log' };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, log: 'review log' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', tabs: { '260618113825-abcd-1': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
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
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1, log: 'parent log', socketPath: '/tmp/sock' };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, log: 'review log' };
const stopGroups = new Set();
const opts1 = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', tabs: { '260618113825-abcd-1': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
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
const parentRun = { key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1, reviewCount: 1, log: 'parent log' };
const childReview = { key: 'PR42', kind: 'completed', status: 'success', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42, log: 'review log' };
const stopGroups = new Set();
const optsParent = { helpers, stopGroups, expandedKey: '260618113825-abcd-1', tabs: { '260618113825-abcd-1': 'log' }, runs: [parentRun, childReview], visibleRuns: [parentRun] };
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

func TestPortalDiffUpdateDetail_LoadingMarkerAddsBusyState(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', log: '', logPath: '/tmp/run.log' };
const stopGroups = new Set();
const loadingDetailKeys = new Set(['r1']);
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' }, loadingDetailKeys };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
if (!detailRow) throw new Error('expected detail row');
const panel = detailRow.querySelector('.detail-panel');
if (!panel) throw new Error('expected detail panel');
if (panel.getAttribute('aria-busy') !== 'true') throw new Error('expected detail panel aria-busy while loading');
if (!panel.classList.contains('is-loading')) throw new Error('expected detail panel loading class while loading');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetail_LoadingMarkerPersistsThenClears(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1', log: '', logPath: '/tmp/run.log' };
const stopGroups = new Set();
const loadingDetailKeys = new Set(['r1']);
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'log' }, loadingDetailKeys };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
if (!detailRow) throw new Error('expected detail row');
const panel = detailRow.querySelector('.detail-panel');
if (!panel) throw new Error('expected detail panel');
if (!panel.classList.contains('is-loading')) throw new Error('expected initial loading class');
SandmanPortalDiff.diffRuns(body, [run], opts);
if (!panel.classList.contains('is-loading')) throw new Error('loading class should persist across rerender while fetch is pending');
loadingDetailKeys.delete('r1');
SandmanPortalDiff.diffRuns(body, [run], opts);
if (panel.classList.contains('is-loading')) throw new Error('loading class should clear after fetch settles');
if (panel.getAttribute('aria-busy') !== null) throw new Error('expected aria-busy to clear after fetch settles');
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

func TestPortalDiffHighlightTerminalLog_BareCommandWordsStayPlain(t *testing.T) {
	js := `const cases = [
  'git command not found',
  'npm install failed',
  'make: *** [build] Error 1',
  'error: cannot find gh CLI'
];
for (const line of cases) {
  const result = SandmanPortalDiff.highlightTerminalLog(line);
  if (result.indexOf('term-command') !== -1) throw new Error('unexpected term-command span for ' + line + ': ' + result);
  const plain = result.replace(/<[^>]+>/g, '');
  if (plain.indexOf(line) === -1) throw new Error('expected plain text preserved for ' + line + ': ' + plain);
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffHighlightTerminalLog_FallbackNoLongerHighlightsBareCommands(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	jsPath := filepath.Join(filepath.Dir(currentFile), "portal_diff.js")
	data, err := os.ReadFile(jsPath)
	if err != nil {
		t.Fatalf("read %s: %v", jsPath, err)
	}
	content := string(data)
	forbidden := "regex: /^(?:gh|git|go|npm|yarn|node|npx|ls|echo|cat|make|mkdir|rm|cp|mv|find|grep|sed|awk|curl|wget|pwd|cd|printf|tar|unzip|jq|chmod|ln|whoami|sort|head|tail|less|more|touch|ssh|scp)\\b/, render: (m) => wrapToken('term-command', m[0])"
	if strings.Contains(content, forbidden) {
		t.Fatalf("portal_diff.js still contains bare-command fallback rule")
	}
}

func TestPortalDiffHighlightTerminalLog_PromptAndToolMarkersStillHighlight(t *testing.T) {
	js := `const prompt = SandmanPortalDiff.highlightTerminalLog('$ git status');
if (prompt.indexOf('term-prompt') === -1) throw new Error('expected term-prompt span');
if (prompt.indexOf('term-command') === -1) throw new Error('expected term-command span');
const bashTool = SandmanPortalDiff.highlightTerminalLog('→ Bash install');
if (bashTool.indexOf('term-action') === -1) throw new Error('expected term-action span for Bash');
if (bashTool.indexOf('Bash') === -1) throw new Error('expected Bash label preserved');
const readTool = SandmanPortalDiff.highlightTerminalLog('→ Read file.go');
if (readTool.indexOf('term-action') === -1) throw new Error('expected term-action span for Read');
if (readTool.indexOf('Read') === -1) throw new Error('expected Read label preserved');
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
	// Per #1563 the SSE path now coalesces pending lines and feeds the
	// joined batch into highlightTerminalLog once per rAF, instead of
	// tokenizing line-by-line. The old per-line marker
	// `SandmanPortalDiff.highlightTerminalLog(line + '\n')` is no longer
	// the source-of-truth for SSE rendering — the coalescer's flush is.
	wantMarkers := []string{
		"function appendStreamLine(runKey, line)",
		"SandmanPortalDiff.highlightTerminalLog(joined)",
		"createStreamCoalescer(",
	}
	for _, want := range wantMarkers {
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
	const sandbox = { window: {}, globalThis: {}, Date, Set, Map, WeakMap, JSON, console, setTimeout: setTimeout, requestIdleCallback: function(cb) { var impl = sandbox.requestIdleCallbackImpl || sandbox.__requestIdleCallbackDefault; return impl.call(sandbox, cb); } };
	// Tests can replace sandbox.requestIdleCallbackImpl before any code
	// pulls window.requestIdleCallback (portal.html's prewarm path uses
	// the browser API directly). The shim above consults the override on
	// every call so per-test stubs (e.g. { didTimeout: true } for the
	// prewarm skip-work test) take effect without rebuilding the
	// sandbox.
	sandbox.__requestIdleCallbackDefault = function(cb) { var start = Date.now(); setTimeout(function() { cb({ didTimeout: false, timeRemaining: function() { return Math.max(0, 50 - (Date.now() - start)); } }); }, 0); };
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
	const chip = wrap.querySelector('.kind-chip');
	if (chip) throw new Error('expected no kind chip for empty reason');
	console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_ReasonChangeAddsAndRemovesChipInPlace(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: '#42' };
const runNew = Object.assign({}, runOld, { reason: 'review', issueLabel: 'Review of PR 42' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
const wrap = created.row.querySelector('[data-cell="title"]').children[0];
if (wrap.querySelector('.kind-chip')) throw new Error('expected no chip on initial run with no reason');
if (body.querySelector('tr.context-row[data-context-for="a"]')) throw new Error('expected no context row on initial run');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
const ctxRow = body.querySelector('tr.context-row[data-context-for="a"]');
if (ctxRow) throw new Error('review rows must not render a context row');
if (wrap.children[0].textContent !== 'Review of PR 42') throw new Error('expected name updated to review label');
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
  reason: 'review',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, run, opts);
if (created.row.querySelector('[data-cell="title"]').querySelector('.batch-membership')) {
  throw new Error('batch-membership must not live inside the title cell for review runs');
}
const ctxRow = body.querySelector('tr.context-row[data-context-for="a"]');
if (ctxRow) throw new Error('review context row must not render');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateCells_ReasonChangeOmitsReviewContextRow(t *testing.T) {
	js := `const body = makeMockBody();
const runOld = { key: 'a', runId: 'r1', kind: 'completed', status: 'success', issueLabel: 'PR42' };
const runNew = Object.assign({}, runOld, { reason: 'review' });
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, runOld, opts);
if (body.querySelector('tr.context-row[data-context-for="a"]')) throw new Error('expected no context row before reason appears');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateRunRowCells(created.row, runOld, runNew, opts);
const after = body.querySelector('tr.context-row[data-context-for="a"]');
if (after) throw new Error('review context row must not render');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffDiffRuns_OmitsReviewContextRowsFromPortalRunJSON(t *testing.T) {
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
	  { key: 'PR42', runId: 'PR42', kind: 'active', status: 'reviewing', issueLabel: 'PR42', reason: 'review', review: true, prNumber: 42, issueNumber: 1 },
	  { key: 'a', runId: 'r1', kind: 'active', status: 'running', issueLabel: '#42', issueNumber: 42, batchKey: 'run-42-1', batchIssues: [42] },
];
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
SandmanPortalDiff.diffRuns(body, runs, opts);

if (body.children.length !== 2) throw new Error('expected 2 data rows (no context row), got ' + body.children.length);

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

// runStreamCoalescerScript extracts the production
// `function createStreamCoalescer(opts) { ... }` body from
// `internal/cmd/portal.html` and runs the supplied JS test fragment in a
// vm sandbox where the production function is bound to `createStreamCoalescer`.
// This keeps the streaming-coalescer tests anchored to the real production
// coalescer (mirroring how `runPortalHTMLScript` extracts `renderRunMeta`)
// instead of duplicating the production logic into the test source. The
// sandbox wires a controllable `requestAnimationFrame` and stubs
// `SandmanPortalDiff.highlightTerminalLog` so tests can drive flushes
// deterministically. The `_debug.flushCount` counter on the coalescer is
// what tests assert against for coalescing behavior.
//
// The user-supplied `js` is concatenated directly to the prefix
// (matching the established `runNodeScript` pattern) without
// template-literal wrapping or escaping. Tests must avoid writing a
// stray backtick in their source — the established convention across
// the rest of this file.
func runStreamCoalescerScript(t *testing.T, js string) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	prefix := sharedMockHelpers() + sharedMockBody() + `
const fs = require('fs');
const vm = require('vm');
const htmlPath = ` + "`" + htmlPath + "`" + `;
const src = fs.readFileSync(htmlPath, 'utf8');
const factoryMatch = src.match(/function createStreamCoalescer\(opts\) \{[\s\S]*?^\s{4}\}/m);
if (!factoryMatch) {
  process.stderr.write('createStreamCoalescer body not found in portal.html\\n');
  process.exit(2);
}
const factoryCtx = vm.createContext({});
vm.runInContext(factoryMatch[0] + '\nthis.createStreamCoalescer = createStreamCoalescer;', factoryCtx, { filename: htmlPath });
const createStreamCoalescer = factoryCtx.createStreamCoalescer;
if (typeof createStreamCoalescer !== 'function') {
  process.stderr.write('createStreamCoalescer not a function after vm.runInContext\\n');
  process.exit(2);
}
globalThis.createStreamCoalescer = createStreamCoalescer;
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

// TestPortalDiffBuildDetailsContent_HasTerminalJSONClass asserts the
// `<pre data-rendered-json>` produced by buildDetailsContent (the Details
// tab) carries the `terminal-json` class in addition to `terminal-text`.
// The companion CSS-rule parse test
// (TestPortal_TerminalJSONCSS_HorizontalScrollbar) pins the rule that
// gives the class its `white-space: pre` / `overflow-x: auto` behaviour;
// together they prove the user-visible contract from issue #1751.
func TestPortalDiffBuildDetailsContent_HasTerminalJSONClass(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: '#42', runId: 'r1', logPath: '/tmp/run.log' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
if (!detailRow) throw new Error('expected detail row');
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected pre[data-rendered-json] in details tab');
if (!pre.classList.contains('terminal-text')) throw new Error('expected terminal-text class to remain on details pre');
if (!pre.classList.contains('terminal-json')) throw new Error('expected terminal-json class on details pre');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffBuildEventsContent_HasTerminalJSONClass asserts the
// `<pre data-rendered-json>` produced by buildEventsContent (the Events
// tab) carries the `terminal-json` class in addition to `terminal-text`,
// mirroring the Details-tab test. The CSS-rule parse test
// (TestPortal_TerminalJSONCSS_HorizontalScrollbar) pins the rule that
// gives the class its `white-space: pre` / `overflow-x: auto` behaviour.
func TestPortalDiffBuildEventsContent_HasTerminalJSONClass(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'completed', status: 'success', issueLabel: '#42', runId: 'r1', events: [{ type: 'check', timestamp: 1700000000000, payload: { ok: true } }] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
if (!detailRow) throw new Error('expected detail row');
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected pre[data-rendered-json] in events tab');
if (!pre.classList.contains('terminal-text')) throw new Error('expected terminal-text class to remain on events pre');
if (!pre.classList.contains('terminal-json')) throw new Error('expected terminal-json class on events pre');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalRunsView_VisibleRunForIssueGroup_PreservesReviewRunID(t *testing.T) {
	js := `const review = { key: '260622193226-a0c19-1227', kind: 'active', status: 'reviewing', review: true, issueLabel: '#1223', runId: '260622193226-a0c19-1227', issueNumber: 1223, prNumber: 5 };
const stub = visibleRunForIssueGroup(1223, [review]);
if (!stub) throw new Error('expected stub row for review-only issue group');
if (stub.runId !== '260622193226-a0c19-1227') throw new Error('expected stub.runId to preserve source RunID, got ' + JSON.stringify(stub.runId));
if (stub.key !== '260622193226-a0c19-1227') throw new Error('expected stub.key to preserve source RunID, got ' + JSON.stringify(stub.key));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_ReviewOnlyReturnsSourceRowWithReviewTrue
// is the tracer bullet for issue #1489.
func TestPortalRunsView_VisibleRunForIssueGroup_ReviewOnlyReturnsSourceRowWithReviewTrue(t *testing.T) {
	js := `const review = { key: '260622193226-a0c19-1227', kind: 'active', status: 'reviewing', review: true, issueLabel: '#1223', runId: '260622193226-a0c19-1227', issueNumber: 1223, prNumber: 5 };
const stub = visibleRunForIssueGroup(1223, [review]);
if (!stub) throw new Error('expected visible row for review-only issue group');
if (stub.review !== true) throw new Error('expected visible row to keep review=true for review-only issue group, got ' + JSON.stringify(stub.review));
if (stub.groupedReview !== false) throw new Error('expected groupedReview=false on review-only visible row, got ' + JSON.stringify(stub.groupedReview));
if (stub.runId !== '260622193226-a0c19-1227') throw new Error('expected visible row runId to match source, got ' + JSON.stringify(stub.runId));
if (stub.key !== '260622193226-a0c19-1227') throw new Error('expected visible row key to match source, got ' + JSON.stringify(stub.key));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_ReviewOnlyLabelsIssueWithPrefix
// is the tracer bullet for issue #1526: a review-only issue group must
// produce a visible row whose issueLabel explicitly signals review-only
// (e.g. "Review of PR 1508 (#1472)") rather than reusing the source PR
// label or fabricating a bare issue label. The PR the review targeted
// is surfaced first, the linked issue is shown as a parenthesised
// reference.
func TestPortalRunsView_VisibleRunForIssueGroup_ReviewOnlyLabelsIssueWithPrefix(t *testing.T) {
	js := `const review = { key: 'PR1508', kind: 'active', status: 'reviewing', review: true, issueLabel: 'PR1508', runId: 'PR1508', issueNumber: 1472, prNumber: 1508 };
const stub = visibleRunForIssueGroup(1472, [review]);
if (!stub) throw new Error('expected visible row for review-only issue group');
const label = String(stub.issueLabel || '');
if (label === 'PR1508') throw new Error('expected review-only label to differ from source PR label, got bare PR label ' + JSON.stringify(label));
if (label === '#1472') throw new Error('expected review-only label to differ from bare issue label, got bare issue label ' + JSON.stringify(label));
if (label.indexOf('Review') < 0) throw new Error('expected review-only label to mention Review, got ' + JSON.stringify(label));
if (label.indexOf('PR 1508') < 0) throw new Error('expected review-only label to reference the PR number, got ' + JSON.stringify(label));
if (label.indexOf('#1472') < 0) throw new Error('expected review-only label to reference the issue number, got ' + JSON.stringify(label));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_ReviewOnlyIdentityMatchesSourceRun
// covers behaviour #2 of issue #1526: the visible row's identity fields
// must come from the review run, not be fabricated from missing
// implementation-run metadata. batchKey on a non-review source must NOT
// leak into the review-only visible row.
func TestPortalRunsView_VisibleRunForIssueGroup_ReviewOnlyIdentityMatchesSourceRun(t *testing.T) {
	js := `const review = { key: 'PR1508', kind: 'active', status: 'reviewing', review: true, issueLabel: 'PR1508', runId: 'PR1508', issueNumber: 1472, prNumber: 1508, startedAt: '2026-06-30T12:00:00Z' };
const stub = visibleRunForIssueGroup(1472, [review]);
if (!stub) throw new Error('expected visible row for review-only issue group');
if (stub.key !== 'PR1508') throw new Error('expected visible row key to match source runId, got ' + JSON.stringify(stub.key));
if (stub.runId !== 'PR1508') throw new Error('expected visible row runId to match source runId, got ' + JSON.stringify(stub.runId));
if (stub.issueNumber !== 1472) throw new Error('expected visible row issueNumber to match source issueNumber, got ' + JSON.stringify(stub.issueNumber));
if (stub.prNumber !== 1508) throw new Error('expected visible row prNumber to match source prNumber, got ' + JSON.stringify(stub.prNumber));
if (stub.startedAt !== '2026-06-30T12:00:00Z') throw new Error('expected visible row startedAt to match source, got ' + JSON.stringify(stub.startedAt));
if ('batchKey' in stub && stub.batchKey) throw new Error('expected review-only visible row not to carry a batchKey, got ' + JSON.stringify(stub.batchKey));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunsForTable_OrphanReviewRowCarriesReviewOnlyLabel
// covers behaviour #3 of issue #1526 at the table-projection level:
// visibleRunsForTable must surface the orphan review's issueLabel as
// "Review of PR <N> (#<issue>)", so the table renders the explicit
// review-only label with both the PR and the linked issue visible.
func TestPortalRunsView_VisibleRunsForTable_OrphanReviewRowCarriesReviewOnlyLabel(t *testing.T) {
	js := `const review = { key: 'PR1508', kind: 'active', status: 'reviewing', review: true, issueLabel: 'PR1508', runId: 'PR1508', issueNumber: 1472, prNumber: 1508, startedAt: '2026-06-30T12:00:00Z' };
const visible = visibleRunsForTable([review]);
if (visible.length !== 1) throw new Error('expected exactly one visible row, got ' + JSON.stringify(visible.length));
if (visible[0].key !== 'PR1508') throw new Error('expected visible row key to match source runId, got ' + JSON.stringify(visible[0].key));
if (visible[0].issueLabel !== 'Review of PR 1508 (#1472)') throw new Error('expected visible row label to be explicit review-only with PR+issue, got ' + JSON.stringify(visible[0].issueLabel));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunsForTable_OrphanReviewWithoutIssueNumberPassesThrough
// pins the no-issue-number projection of issue #1667: an orphan review
// row (review=true, no issueNumber) is routed to the passthrough bucket
// in visibleRunsForTable, and the row survives with whatever issueLabel
// the Go projection set — here the explicit "Review of PR <N>" we now
// construct server-side for this case.
func TestPortalRunsView_VisibleRunsForTable_OrphanReviewWithoutIssueNumberPassesThrough(t *testing.T) {
	js := `const review = { key: '260622193226-a0c19-PR1508', kind: 'active', status: 'reviewing', review: true, issueLabel: 'Review of PR 1508', runId: '260622193226-a0c19-PR1508', prNumber: 1508, startedAt: '2026-06-30T12:00:00Z' };
const visible = visibleRunsForTable([review]);
if (visible.length !== 1) throw new Error('expected exactly one visible row, got ' + JSON.stringify(visible.length));
if (visible[0].key !== '260622193226-a0c19-PR1508') throw new Error('expected visible row key to match source runId, got ' + JSON.stringify(visible[0].key));
if (visible[0].issueLabel !== 'Review of PR 1508') throw new Error('expected visible row label to use the Review of PR <n> form, got ' + JSON.stringify(visible[0].issueLabel));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalDiffCreateRunRow_OrphanReviewWithoutIssueNumberRendersReviewOfPRLabel
// asserts the DOM end of issue #1667: when a passthrough orphan review
// row carries an explicit "Review of PR <n>" issueLabel, the rendered
// name cell surfaces that label verbatim (matching the convention used
// for orphan reviews WITH an issue number,
// ADR-0029 §Review-only orphan label).
func TestPortalDiffCreateRunRow_OrphanReviewWithoutIssueNumberRendersReviewOfPRLabel(t *testing.T) {
	js := `const body = makeMockBody();
const review = { key: '260622193226-a0c19-PR1508', kind: 'active', status: 'reviewing', review: true, issueLabel: 'Review of PR 1508', runId: '260622193226-a0c19-PR1508', prNumber: 1508, startedAt: '2026-06-30T12:00:00Z' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: null };
const created = SandmanPortalDiff.insertRunRow(body, review, opts);
const cells = Array.from(created.row.children || []).map((c) => c.getAttribute('data-cell'));
if (cells.length === 0) throw new Error('expected at least one cell on the row, got ' + JSON.stringify(cells));
const titleCell = created.row.children.find((c) => c.getAttribute && c.getAttribute('data-cell') === 'title');
if (!titleCell) throw new Error('expected title cell, got cells=' + JSON.stringify(cells));
const wrap = titleCell.children[0];
if (!wrap) throw new Error('expected wrap div in title cell');
const name = Array.from(wrap.children || []).find((c) => c.classList && c.classList.contains('name'));
if (!name) throw new Error('expected name span in title wrap, got ' + JSON.stringify(Array.from(wrap.children || []).map((c) => c.className || c.tagName)));
if (name.textContent !== 'Review of PR 1508') throw new Error('expected name text "Review of PR 1508", got ' + JSON.stringify(name.textContent));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffDiffRuns_OrphanReviewWithReviewOnlyLabelExpandsToDetailPanel
// covers behaviour #4 of issue #1526 at the DOM level: the review-only
// orphan row must expand to a detail panel keyed by its own runId, surfacing
// the review run's content directly. A review-only orphan is always alone, so
// no subject selector should render. The visible row here mirrors what
// visibleRunForIssueGroup produces: identity fields from the review run
// with an explicit "Review of PR <N> (#<issue>)" issueLabel.
func TestPortalDiffDiffRuns_OrphanReviewWithReviewOnlyLabelExpandsToDetailPanel(t *testing.T) {
	js := `const body = makeMockBody();
const orphanRow = { key: 'PR1508', runId: 'PR1508', review: true, groupedReview: false, issueLabel: 'Review of PR 1508 (#1472)', issueNumber: 1472, prNumber: 1508, kind: 'active', status: 'reviewing', startedAt: '2026-06-30T12:00:00Z' };
const review = { key: 'PR1508', runId: 'PR1508', review: true, issueLabel: 'PR1508', issueNumber: 1472, prNumber: 1508, kind: 'active', status: 'reviewing', startedAt: '2026-06-30T12:00:00Z' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR1508', runs: [review], visibleRuns: [orphanRow] };
const result = SandmanPortalDiff.diffRuns(body, [orphanRow], opts);
if (result.inserted < 1) throw new Error('expected rows to be inserted, got ' + JSON.stringify(result));
const visibleRow = body.querySelector('tr[data-run-key="PR1508"]');
if (!visibleRow) throw new Error('expected orphan review visible under its real RunID');
const aria = visibleRow.getAttribute('aria-expanded');
if (aria !== 'true') throw new Error('expected review-only orphan row to be expanded, got aria-expanded=' + aria);
const detailRow = body.querySelector('tr.detail-row[data-detail-for="PR1508"]');
if (!detailRow) throw new Error('expected detail row keyed by the orphan review RunID');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (subjectSelect) throw new Error('expected no subject selector on a review-only orphan (always alone), got ' + subjectSelect.children.length + ' options');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffDiffRuns_OrphanReviewSubjectSwitchPreservesRowIdentity
// covers behaviour #5 of issue #1526 at the DOM level: when the subject
// picker on a review-only orphan row is rebuilt with multiple review
// runs for the same issue, the data row identity (data-run-key) must
// stay anchored to the visible review's runId, and the picker must list
// every real review run as an option (no fake parent option).
func TestPortalDiffDiffRuns_OrphanReviewSubjectSwitchPreservesRowIdentity(t *testing.T) {
	js := `const body = makeMockBody();
const terminal = { key: 'PR1507', kind: 'completed', status: 'success', review: true, issueLabel: 'PR1507', runId: 'PR1507', issueNumber: 1472, prNumber: 1507, startedAt: '2026-06-29T10:00:00Z' };
const live = { key: 'PR1508', kind: 'active', status: 'reviewing', review: true, issueLabel: 'PR1508', runId: 'PR1508', issueNumber: 1472, prNumber: 1508, startedAt: '2026-06-30T10:00:00Z' };
const orphanRow = { key: 'PR1508', runId: 'PR1508', review: true, groupedReview: false, issueLabel: 'Review of PR 1508 (#1472)', issueNumber: 1472, prNumber: 1508, kind: 'active', status: 'reviewing', startedAt: '2026-06-30T12:00:00Z' };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'PR1508', runs: [terminal, live], visibleRuns: [orphanRow] };
SandmanPortalDiff.diffRuns(body, [orphanRow], opts);
const visibleRow = body.querySelector('tr[data-run-key="PR1508"]');
if (!visibleRow) throw new Error('expected visible orphan review row under its real RunID');
const detailRow = body.querySelector('tr.detail-row[data-detail-for="PR1508"]');
if (!detailRow) throw new Error('expected detail row keyed by the visible orphan review RunID');
const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
if (!subjectSelect) throw new Error('expected subject selector on review-only orphan detail row');
if (subjectSelect.children.length !== 2) throw new Error('expected one option per real review run, got ' + subjectSelect.children.length);
const values = Array.from(subjectSelect.children).map((opt) => opt.getAttribute('value'));
if (values.indexOf('PR1507') < 0 || values.indexOf('PR1508') < 0) throw new Error('expected both review RunIDs as picker options, got ' + JSON.stringify(values));
if (subjectSelect.value !== 'PR1508') throw new Error('expected picker to default to the visible review RunID, got ' + subjectSelect.value);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalRunsView_VisibleRunForIssueGroup_TerminalReviewWinsOverActiveKind(t *testing.T) {
	js := `const review = { key: '260622193226-a0c19-1227', kind: 'active', status: 'success', review: true, issueLabel: '#1223', runId: '260622193226-a0c19-1227', issueNumber: 1223, prNumber: 5 };
const stub = visibleRunForIssueGroup(1223, [review]);
if (!stub) throw new Error('expected stub row for terminal review group');
if (stub.kind !== 'completed') throw new Error('expected completed kind once terminal status is present, got ' + JSON.stringify(stub.kind));
if (stub.status !== 'success') throw new Error('expected terminal status to win over live kind, got ' + JSON.stringify(stub.status));
// Issue #1729: orphan review-only groups can no longer project a verdict
// from run.status (the heuristic is wrong). The Go projection covers the
// canonical-parent path; the JS orphan path renders an empty verdict and
// the meta line guard in renderRunMeta hides the trailing dash.
if (stub.reviewVerdict !== '') throw new Error('expected empty reviewVerdict on orphan review-only group (issue #1729), got ' + JSON.stringify(stub.reviewVerdict));
if (stub.review !== true) throw new Error('expected review=true on visible row for terminal review-only group, got ' + JSON.stringify(stub.review));
if (stub.groupedReview !== false) throw new Error('expected groupedReview=false on visible row for review-only group, got ' + JSON.stringify(stub.groupedReview));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestPortalRunsView_VisibleRunForIssueGroup_PrefersLiveActiveOverCompletedParent(t *testing.T) {
	js := `const parent = { key: '260618113825-abcd-1-parent', kind: 'completed', status: 'success', review: false, issueLabel: '#1', runId: '260618113825-abcd-1-parent', issueNumber: 1, batchKey: 'batch-1' };
const active = { key: '260618113825-abcd-1-active', kind: 'active', status: 'running', review: false, issueLabel: '#1', runId: '260618113825-abcd-1-active', issueNumber: 1, batchKey: 'batch-1' };
const result = visibleRunForIssueGroup(1, [parent, active]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-1-active') throw new Error('expected live active child as visible row, got ' + JSON.stringify(result.key));
if (result.kind !== 'active') throw new Error('expected active kind for visible row, got ' + JSON.stringify(result.kind));
if (!helpers.isRunAbortable(result, new Set())) throw new Error('expected live active visible row to keep Abort control');
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestPortalRunsView_VisibleRunForIssueGroup_CompletedOnlyStillUsesCompletedRow(t *testing.T) {
	js := `const parent = { key: '260618113825-abcd-2-parent', kind: 'completed', status: 'success', review: false, issueLabel: '#2', runId: '260618113825-abcd-2-parent', issueNumber: 2, reviewCount: 3, reviewVerdict: 'Approved' };
const result = visibleRunForIssueGroup(2, [parent]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-2-parent') throw new Error('expected completed row to stay visible, got ' + JSON.stringify(result.key));
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
// parent run and a live review child, the visible row must be the terminal
// parent, not the live child. The review child must remain accessible in the
// expanded selector.
func TestPortalRunsView_VisibleRunForIssueGroup_TerminalParentWinsOverLiveChild(t *testing.T) {
	js := `const parent = { key: '260618113825-abcd-1', kind: 'completed', status: 'success', review: false, issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1 };
const liveChild = { key: 'PR42', kind: 'active', status: 'reviewing', review: true, issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42 };
const result = visibleRunForIssueGroup(1, [parent, liveChild]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-1') throw new Error('expected parent as visible row, got ' + JSON.stringify(result.key));
if (result.status !== 'success') throw new Error('expected visible badge to be parent status (no live review flip), got ' + JSON.stringify(result.status));
if (result.kind !== 'completed') throw new Error('expected completed kind, got ' + JSON.stringify(result.kind));
if (result.review) throw new Error('expected review flag false for parent row, got ' + JSON.stringify(result.review));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_NewerSuccessWinsOverOlderAbortedWithReviews
// is the regression test for issue #1825: when the same issue has two
// terminal impl rows from different batches (older aborted carrying
// review metadata, newer successful without), the visible row must be
// the newer successful run — never the older aborted run. The backend
// projection no longer stamps review metadata across batches, and the
// frontend's pickCanonicalParent must pick the first parent in the
// input order rather than preferring parents that happen to carry
// reviewCount/reviewVerdict from a previous batch. The caller
// (visibleRunsForTable) sorts runs by startedAt desc, so the first
// parent in this test fixture is the newer one by FinishedAt.
func TestPortalRunsView_VisibleRunForIssueGroup_NewerSuccessWinsOverOlderAbortedWithReviews(t *testing.T) {
	js := `// visibleRunsForTable sorts runs by startedAt desc, so the newest
// parent sits at index 0 when visibleRunForIssueGroup is invoked. The
// bug was that pickCanonicalParent reached past parents[0] to find
// the older impl row that carried reviewCount/reviewVerdict.
const newerSuccess = {
  key: 'impl-9744', kind: 'completed', status: 'success', review: false,
  issueLabel: '#1793', runId: 'impl-9744', issueNumber: 1793,
  startedAt: '2026-07-05T00:00:00Z', finishedAt: '2026-07-05T00:30:00Z',
};
const olderAborted = {
  key: 'impl-2bf9', kind: 'completed', status: 'aborted', review: false,
  issueLabel: '#1793', runId: 'impl-2bf9', issueNumber: 1793,
  startedAt: '2026-07-04T00:00:00Z', finishedAt: '2026-07-04T00:30:00Z',
  reviewCount: 1, reviewVerdict: 'Approved',
};
const result = visibleRunForIssueGroup(1793, [newerSuccess, olderAborted]);
if (!result) throw new Error('expected visible row');
if (result.runId !== 'impl-9744') throw new Error('expected newer successful parent as visible row, got ' + JSON.stringify(result.runId));
if (result.status !== 'success') throw new Error('expected visible status=success (newer run wins), got ' + JSON.stringify(result.status));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestPortalRunsView_VisibleRunsForTable_SortsByStartedDesc(t *testing.T) {
	js := `const runs = [
  { key: '260618113825-abcd-1014', kind: 'completed', status: 'failure', issueLabel: '#1014', runId: '260618113825-abcd-1014', issueNumber: 1014, startedAt: '2026-06-29T04:34:39Z' },
  { key: '260618113825-abcd-1401', kind: 'completed', status: 'failure', issueLabel: '#1401', runId: '260618113825-abcd-1401', issueNumber: 1401, startedAt: '2026-06-26T19:47:13Z' },
  { key: '260618113825-abcd-1402', kind: 'completed', status: 'failure', issueLabel: '#1402', runId: '260618113825-abcd-1402', issueNumber: 1402, startedAt: '2026-06-27T00:12:51Z' },
  { key: '260618113825-abcd-1467', kind: 'active', status: 'running', issueLabel: '#1467', runId: '260618113825-abcd-1467', issueNumber: 1467, startedAt: '2026-06-29T14:29:12Z' },
];
const visible = visibleRunsForTable(runs);
const order = visible.map((run) => run.key).join(',');
if (order !== '260618113825-abcd-1467,260618113825-abcd-1014,260618113825-abcd-1402,260618113825-abcd-1401') throw new Error('expected newest started rows first, got ' + JSON.stringify(order));
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
	js := `const review = { key: '260622193226-a0c19-1227', kind: 'active', status: 'reviewing', review: true, issueLabel: '#1223', runId: '260622193226-a0c19-1227', issueNumber: 1223, prNumber: 5 };
const visible = visibleRunsForTable([review]);
if (visible.length !== 1) throw new Error('expected 1 visible row, got ' + visible.length);
const rendered = visible[0];
if (rendered.runId !== '260622193226-a0c19-1227') throw new Error('expected visible[0].runId to preserve source RunID for meta-line rendering, got ' + JSON.stringify(rendered.runId));
const meta = helpers.renderRunMeta(rendered);
if (!meta.includes('260622193226-a0c19-1227')) throw new Error('expected renderRunMeta to surface real run identifier, got ' + JSON.stringify(meta));
if (meta.startsWith('issue-')) throw new Error('expected renderRunMeta not to start with synthetic "issue-" stub, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_CanonicalParentIdentityStaysPut
// is the regression test for issue #1525: when an issue group contains a
// canonical implementation row AND one or more review child rows, the
// visible row's batchKey/runId/issueTitle/startedAt must come from the
// canonical parent row, never from a review child. The visible row's key
// must equal the parent's key.
func TestPortalRunsView_VisibleRunForIssueGroup_CanonicalParentIdentityStaysPut(t *testing.T) {
	js := `const parent = {
  key: '260618113825-abcd-1', kind: 'completed', status: 'success', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-01T00:00:00Z',
};
const review = {
  key: 'PR42', kind: 'active', status: 'reviewing', review: true,
  issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42,
  batchKey: 'review-batch', issueTitle: 'Review PR42',
  startedAt: '2025-02-01T00:00:00Z',
};
const result = visibleRunForIssueGroup(1, [parent, review]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-1') throw new Error('expected canonical parent key issue-1, got ' + JSON.stringify(result.key));
if (result.runId !== '260618113825-abcd-1') throw new Error('expected canonical parent runId, got ' + JSON.stringify(result.runId));
if (result.batchKey !== 'parent-batch') throw new Error('expected canonical parent batchKey parent-batch, got ' + JSON.stringify(result.batchKey));
if (result.issueTitle !== 'Fix login bug') throw new Error('expected canonical parent issueTitle, got ' + JSON.stringify(result.issueTitle));
if (result.startedAt !== '2025-01-01T00:00:00Z') throw new Error('expected canonical parent startedAt, got ' + JSON.stringify(result.startedAt));
if (result.review) throw new Error('expected review=false on canonical parent row, got ' + JSON.stringify(result.review));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunsForTable_KeepsParentIssueVisible covers the
// end-to-end visibleRunsForTable path for issue #1525: even when the
// underlying group contains a review child with later startedAt than the
// canonical parent, the visible row for the group must remain the parent.
func TestPortalRunsView_VisibleRunsForTable_KeepsParentIssueVisible(t *testing.T) {
	js := `const parent = {
  key: '260618113825-abcd-1', kind: 'completed', status: 'success', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-01T00:00:00Z',
};
const review = {
  key: 'PR42', kind: 'active', status: 'reviewing', review: true,
  issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42,
  batchKey: 'review-batch', issueTitle: 'Review PR42',
  startedAt: '2026-06-30T10:00:00Z',
};
const visible = visibleRunsForTable([parent, review]);
if (visible.length !== 1) throw new Error('expected one visible row, got ' + visible.length);
if (visible[0].key !== '260618113825-abcd-1') throw new Error('expected visible row key issue-1, got ' + JSON.stringify(visible[0].key));
if (visible[0].batchKey !== 'parent-batch') throw new Error('expected visible row batchKey from parent, got ' + JSON.stringify(visible[0].batchKey));
if (visible[0].issueTitle !== 'Fix login bug') throw new Error('expected visible row issueTitle from parent, got ' + JSON.stringify(visible[0].issueTitle));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalDiff_SubjectRunsFor_KeepsCanonicalParentFirst pins the subject
// picker contract from issue #1525 AC3: when a row group has a canonical
// parent and review children, the related subject list must lead with the
// canonical parent so users can return to the implementation row by
// selecting the parent option, AND every review child must remain
// reachable as its own subject option.
func TestPortalDiff_SubjectRunsFor_KeepsCanonicalParentFirst(t *testing.T) {
	js := `const opts = { helpers, runs: [
  { key: '260618113825-abcd-1', runId: '260618113825-abcd-1', kind: 'completed', status: 'success', review: false, issueNumber: 1, issueLabel: '#1' },
  { key: 'PR42', runId: 'PR42', kind: 'active', status: 'reviewing', review: true, issueNumber: 1, issueLabel: 'PR42', prNumber: 42 },
  { key: 'PR43', runId: 'PR43', kind: 'completed', status: 'success', review: true, issueNumber: 1, issueLabel: 'PR43', prNumber: 43 },
] };
const rowRun = opts.runs[0];
const related = SandmanPortalDiff.subjectRunsFor(rowRun, opts);
if (!Array.isArray(related) || related.length !== 3) throw new Error('expected 3 related subjects (parent + 2 reviews), got ' + JSON.stringify(related.length));
if (related[0].review) throw new Error('expected first related subject to be the canonical parent, got review row first');
if (related[0].runId !== '260618113825-abcd-1') throw new Error('expected first related subject runId issue-1, got ' + JSON.stringify(related[0].runId));
const reviewIds = related.slice(1).map((r) => r.runId).sort();
if (reviewIds[0] !== 'PR42' || reviewIds[1] !== 'PR43') throw new Error('expected review children PR42 and PR43 in related subjects, got ' + JSON.stringify(reviewIds));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiff_SubjectRunsFor_IncludesPreviousContinuationRun(t *testing.T) {
	js := `const previous = {
  key: 'old-run', runId: 'old-run', kind: 'completed', status: 'success', review: false,
  issueNumber: 1, issueLabel: '#1', startedAt: '2026-07-06T10:00:00Z',
};
const continuation = {
  key: 'new-run', runId: 'new-run', kind: 'active', status: 'running', review: false,
  issueNumber: 1, issueLabel: '#1', startedAt: '2026-07-07T10:00:00Z',
  events: [{ type: 'run.continued', payload: { previous_run_id: 'old-run' } }],
};
const opts = { helpers, runs: [continuation, previous] };
const related = SandmanPortalDiff.subjectRunsFor(continuation, opts);
const ids = related.map((run) => run.runId);
if (ids.length !== 2) throw new Error('expected continuation plus previous run, got ' + JSON.stringify(ids));
if (ids[0] !== 'new-run') throw new Error('expected current continuation first, got ' + JSON.stringify(ids));
if (ids[1] !== 'old-run') throw new Error('expected previous run as sibling, got ' + JSON.stringify(ids));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_SubjectRunsFor_MultipleParentsPicksCanonical pins the
// regression observed on issue #1855: an issue group can carry two impl
// rows (one earlier abandoned, one later successful) plus sibling
// reviews. The earlier parent pick required `parents.length === 1` to
// fall back to the only parent; with two parents the dropdown lost
// the parent entirely and listed only the reviews, breaking the
// subject switcher. The canonical pick now mirrors the table-visibility
// helper pickCanonicalParent in portal.html (used by
// visibleRunForIssueGroup): prefer an active (currently-running)
// parent, otherwise trust the caller's order. visibleRunsForTable
// passes rows in startedAt desc, so parents[0] is the most-recent
// terminal impl — never the olderAborted-with-reviewMetadata row,
// which is exactly the #1825 cross-batch guard's contract.
func TestPortalDiff_SubjectRunsFor_MultipleParentsPicksCanonical(t *testing.T) {
	js := `const opts = { helpers, runs: [
  { key: '260706140827-1058-1855-PR1875', runId: '260706140827-1058-1855-PR1875', kind: 'completed', status: 'success', review: true, issueNumber: 1855, issueLabel: 'PR1875', prNumber: 1875, startedAt: '2026-07-06T14:08:32Z' },
  { key: '260706134357-e39d-1855-PR1875', runId: '260706134357-e39d-1855-PR1875', kind: 'completed', status: 'success', review: true, issueNumber: 1855, issueLabel: 'PR1875', prNumber: 1875, startedAt: '2026-07-06T13:44:02Z' },
  { key: '260706132041-fb4a-1855', runId: '260706132041-fb4a-1855', kind: 'completed', status: 'success', review: false, issueNumber: 1855, issueLabel: '#1855', startedAt: '2026-07-06T13:20:48Z' },
  { key: '260706132006-2569-1855', runId: '260706132006-2569-1855', kind: 'completed', status: 'aborted', review: false, issueNumber: 1855, issueLabel: '#1855', startedAt: '2026-07-06T12:00:00Z' },
] };
const rowRun = opts.runs[0];
const related = SandmanPortalDiff.subjectRunsFor(rowRun, opts);
if (!Array.isArray(related) || related.length !== 3) throw new Error('expected 3 related subjects (parent + 2 reviews), got ' + JSON.stringify(related.length));
if (related[0].review) throw new Error('expected first related subject to be the canonical parent, got review row first');
if (related[0].runId !== '260706132041-fb4a-1855') throw new Error('expected canonical parent 260706132041-fb4a-1855 (caller-order), got ' + JSON.stringify(related[0].runId));
const reviewIds = related.slice(1).map((r) => r.runId).sort();
if (reviewIds[0] !== '260706134357-e39d-1855-PR1875' || reviewIds[1] !== '260706140827-1058-1855-PR1875') throw new Error('expected both reviews in related subjects, got ' + JSON.stringify(reviewIds));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_SubjectRunsFor_ActiveParentWinsOverOlderTerminals pins
// the active-first branch of pickCanonicalParent: a currently-running
// parent (kind === 'active') owns the issue group even when an older
// successful impl exists in the caller's array. Within actives the
// latest by startedAt wins.
func TestPortalDiff_SubjectRunsFor_ActiveParentWinsOverOlderTerminals(t *testing.T) {
	js := `const opts = { helpers, runs: [
  { key: 'old-impl', runId: 'old-impl', kind: 'completed', status: 'success', review: false, issueNumber: 100, issueLabel: '#100', startedAt: '2026-07-06T10:00:00Z' },
  { key: 'new-impl-active', runId: 'new-impl-active', kind: 'active', status: 'running', review: false, issueNumber: 100, issueLabel: '#100', startedAt: '2026-07-06T15:00:00Z' },
  { key: 'newer-active', runId: 'newer-active', kind: 'active', status: 'running', review: false, issueNumber: 100, issueLabel: '#100', startedAt: '2026-07-06T15:30:00Z' },
] };
const rowRun = opts.runs[0];
const related = SandmanPortalDiff.subjectRunsFor(rowRun, opts);
if (related.length !== 1) throw new Error('expected 1 related subject (active parent only — no reviews), got ' + JSON.stringify(related.length));
if (related[0].runId !== 'newer-active') throw new Error('expected newest active parent newer-active, got ' + JSON.stringify(related[0].runId));
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_LiveActiveParentKeepsIdentity
// is slice 2 for issue #1525: a live, active canonical parent row must
// remain the visible row even when a later-started review child exists in
// the same group, and the parent's identity fields must not be blended
// with any review metadata. The badge status comes from the backend
// projection; the frontend passes it through without re-deriving.
func TestPortalRunsView_VisibleRunForIssueGroup_LiveActiveParentKeepsIdentity(t *testing.T) {
	js := `const liveParent = {
  key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-15T00:00:00Z',
};
const review = {
  key: 'PR42', kind: 'active', status: 'reviewing', review: true,
  issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42,
  batchKey: 'review-batch', issueTitle: 'Review PR42',
  startedAt: '2025-02-01T00:00:00Z',
};
const result = visibleRunForIssueGroup(1, [liveParent, review]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-1') throw new Error('expected live parent as visible row, got ' + JSON.stringify(result.key));
if (result.kind !== 'active') throw new Error('expected active kind, got ' + JSON.stringify(result.kind));
if (result.status !== 'reviewing') throw new Error('expected visible badge to be reviewing (backend-provided), got ' + JSON.stringify(result.status));
if (result.batchKey !== 'parent-batch') throw new Error('expected parent batchKey, got ' + JSON.stringify(result.batchKey));
if (result.issueTitle !== 'Fix login bug') throw new Error('expected parent issueTitle, got ' + JSON.stringify(result.issueTitle));
if (result.startedAt !== '2025-01-15T00:00:00Z') throw new Error('expected parent startedAt, got ' + JSON.stringify(result.startedAt));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_ArchivedParentKeepsIdentity
// is slice 3 for issue #1525: an archived canonical parent row must
// remain the visible row when an active review child exists, and the
// archived parent's identity fields must not be blended with the review
// child's identity.
func TestPortalRunsView_VisibleRunForIssueGroup_ArchivedParentKeepsIdentity(t *testing.T) {
	js := `const archivedParent = {
  key: '260618113825-abcd-1', kind: 'completed', status: 'success', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2024-12-01T00:00:00Z', archived: true,
};
const review = {
  key: 'PR42', kind: 'active', status: 'reviewing', review: true,
  issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42,
  batchKey: 'review-batch', issueTitle: 'Review PR42',
  startedAt: '2025-02-01T00:00:00Z',
};
const result = visibleRunForIssueGroup(1, [archivedParent, review]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-1') throw new Error('expected archived parent as visible row, got ' + JSON.stringify(result.key));
if (result.batchKey !== 'parent-batch') throw new Error('expected parent batchKey, got ' + JSON.stringify(result.batchKey));
if (result.issueTitle !== 'Fix login bug') throw new Error('expected parent issueTitle, got ' + JSON.stringify(result.issueTitle));
if (result.startedAt !== '2024-12-01T00:00:00Z') throw new Error('expected parent startedAt, got ' + JSON.stringify(result.startedAt));
if (!result.archived) throw new Error('expected archived flag preserved, got ' + JSON.stringify(result.archived));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_CompletedParentWithVerdict
// is slice 4 for issue #1525: a completed canonical parent row with a
// review verdict must remain the visible row when a review child exists,
// and the parent's identity fields must not be blended with the review
// child's identity. The parent's reviewVerdict is preserved as a
// summary, not as a substitution for the parent's own data.
func TestPortalRunsView_VisibleRunForIssueGroup_CompletedParentWithVerdict(t *testing.T) {
	js := `const parent = {
  key: '260618113825-abcd-1', kind: 'completed', status: 'success', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-01T00:00:00Z',
  reviewCount: 2, reviewVerdict: 'Approved',
};
const review = {
  key: 'PR42', kind: 'completed', status: 'success', review: true,
  issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42,
  batchKey: 'review-batch', issueTitle: 'Review PR42',
  startedAt: '2025-01-15T00:00:00Z',
};
const result = visibleRunForIssueGroup(1, [parent, review]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-1') throw new Error('expected completed parent as visible row, got ' + JSON.stringify(result.key));
if (result.batchKey !== 'parent-batch') throw new Error('expected parent batchKey, got ' + JSON.stringify(result.batchKey));
if (result.issueTitle !== 'Fix login bug') throw new Error('expected parent issueTitle, got ' + JSON.stringify(result.issueTitle));
if (result.runId !== '260618113825-abcd-1') throw new Error('expected parent runId, got ' + JSON.stringify(result.runId));
if (result.startedAt !== '2025-01-01T00:00:00Z') throw new Error('expected parent startedAt, got ' + JSON.stringify(result.startedAt));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_ReviewSubjectDoesNotChangeVisible
// is the AC4 regression test for issue #1525: switching the expanded
// subject selector to a review child must not change which row is
// visible. The visible row is decided by visibleRunForIssueGroup on the
// underlying group, not by the expanded subject; the two are
// independent selections.
func TestPortalRunsView_VisibleRunForIssueGroup_ReviewSubjectDoesNotChangeVisible(t *testing.T) {
	js := `const parent = {
  key: '260618113825-abcd-1', kind: 'completed', status: 'success', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-01T00:00:00Z',
};
const review1 = {
  key: 'PR42', kind: 'active', status: 'reviewing', review: true,
  issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42,
  batchKey: 'review-batch', issueTitle: 'Review PR42',
  startedAt: '2025-02-01T00:00:00Z',
};
const review2 = {
  key: 'PR43', kind: 'completed', status: 'success', review: true,
  issueLabel: 'PR43', runId: 'PR43', issueNumber: 1, prNumber: 43,
  batchKey: 'review-batch-2', issueTitle: 'Review PR43',
  startedAt: '2025-01-15T00:00:00Z',
};
// Visible row before any subject selection: parent.
const before = visibleRunForIssueGroup(1, [parent, review1, review2]);
if (!before || before.key !== '260618113825-abcd-1') throw new Error('expected visible row to be parent before subject switch, got ' + JSON.stringify(before && before.key));
// Switching the subject selector to a review child must not influence
// which row is visible — visibleRunForIssueGroup still picks the parent.
const after = visibleRunForIssueGroup(1, [parent, review1, review2]);
if (!after || after.key !== '260618113825-abcd-1') throw new Error('expected visible row to stay parent after subject switch, got ' + JSON.stringify(after && after.key));
if (after.runId !== '260618113825-abcd-1' || after.batchKey !== 'parent-batch') throw new Error('expected parent identity to stay intact, got ' + JSON.stringify({ runId: after.runId, batchKey: after.batchKey }));
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
	js := `const run = { key: '260618113825-abcd-42', runId: '260618113825-abcd-42', batchKey: '260618113825-abcd', kind: 'active', status: 'running' };
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('Batch:') < 0) throw new Error('expected Batch: in meta for active fresh batch row, got ' + JSON.stringify(meta));
if (meta.indexOf('Run:') < 0) throw new Error('expected Run: in meta for active fresh batch row, got ' + JSON.stringify(meta));
if (meta.indexOf('260618113825-abcd') < 0) throw new Error('expected batchKey value in Batch: label, got ' + JSON.stringify(meta));
if (meta.indexOf('260618113825-abcd-42') < 0) throw new Error('expected runId value in Run: label, got ' + JSON.stringify(meta));
const batchPos = meta.indexOf('Batch:');
const runPos = meta.indexOf('Run:');
if (batchPos > runPos) throw new Error('expected Batch: to appear before Run:, got Batch: at ' + batchPos + ', Run: at ' + runPos + ' in ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestRenderRunMeta_QueuedRow_SuppressesRunLabel(t *testing.T) {
	js := `const run = { key: '260618113825-abcd-42', runId: '', batchKey: '260618113825-abcd', kind: 'active', status: 'queued' };
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('Batch:') < 0) throw new Error('expected Batch: in meta for queued row, got ' + JSON.stringify(meta));
if (meta.indexOf('260618113825-abcd') < 0) throw new Error('expected batchKey value in Batch: label, got ' + JSON.stringify(meta));
if (meta.indexOf('Run:') >= 0) throw new Error('expected no Run: in meta for queued row, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestRenderRunMeta_QueuedAndBlockedRows_SuppressRunLabelForSyntheticRunID(t *testing.T) {
	js := `const cases = [
  { status: 'queued', runId: '260618113825-abcd-42' },
  { status: 'blocked', runId: '260618113825-abcd-43' },
];
for (const tc of cases) {
  const run = { key: tc.runId, runId: tc.runId, batchKey: '260618113825-abcd', kind: 'active', status: tc.status };
  const meta = helpers.renderRunMeta(run);
  if (meta.indexOf('Batch:') < 0) throw new Error('expected Batch: in meta for ' + tc.status + ' row, got ' + JSON.stringify(meta));
  if (meta.indexOf('260618113825-abcd') < 0) throw new Error('expected batchKey value in Batch: label for ' + tc.status + ' row, got ' + JSON.stringify(meta));
  if (meta.indexOf('Run:') >= 0) throw new Error('expected no Run: in meta for ' + tc.status + ' row with synthetic RunID, got ' + JSON.stringify(meta));
}
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestPortalDiffSubjectRunValue_QueuedAndBlockedRowsReturnEmptyForSyntheticRunID(t *testing.T) {
	js := `const cases = [
  { status: 'queued', runId: '260618113825-abcd-42' },
  { status: 'blocked', runId: '260618113825-abcd-43' },
];
for (const tc of cases) {
  const run = { key: tc.runId, runId: tc.runId, batchKey: '260618113825-abcd', kind: 'active', status: tc.status };
  const value = SandmanPortalDiff.subjectRunValue(run);
  if (value !== '') throw new Error('expected empty subjectRunValue for ' + tc.status + ' row with synthetic RunID, got ' + JSON.stringify(value));
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_LiveReviewFlipsParentStatusToReviewing
// verifies that the frontend passes through the parent row's status field
// without re-deriving it. The synthetic literal here explicitly sets
// status="reviewing" so the test pins the frontend's pass-through behavior
// rather than asserting on backend projection (the backend no longer stamps
// review metadata onto parent rows after the cross-batch aggregation
// removal in issue #1825).
func TestPortalRunsView_VisibleRunForIssueGroup_LiveReviewFlipsParentStatusToReviewing(t *testing.T) {
	js := `const parent = {
  key: '260618113825-abcd-1', kind: 'completed', status: 'reviewing', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-01T00:00:00Z',
};
const liveReview = {
  key: 'PR42', kind: 'active', status: 'reviewing', review: true,
  issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42,
  batchKey: 'review-batch', issueTitle: 'Review PR42',
  startedAt: '2025-02-01T00:00:00Z',
};
const result = visibleRunForIssueGroup(1, [parent, liveReview]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-1') throw new Error('expected canonical parent key issue-1, got ' + JSON.stringify(result.key));
if (result.runId !== '260618113825-abcd-1') throw new Error('expected canonical parent runId, got ' + JSON.stringify(result.runId));
if (result.batchKey !== 'parent-batch') throw new Error('expected canonical parent batchKey parent-batch, got ' + JSON.stringify(result.batchKey));
if (result.issueTitle !== 'Fix login bug') throw new Error('expected canonical parent issueTitle, got ' + JSON.stringify(result.issueTitle));
if (result.startedAt !== '2025-01-01T00:00:00Z') throw new Error('expected canonical parent startedAt, got ' + JSON.stringify(result.startedAt));
if (result.review) throw new Error('expected review=false on visible parent row, got ' + JSON.stringify(result.review));
if (result.status !== 'reviewing') throw new Error('expected visible status=reviewing (backend-provided), got ' + JSON.stringify(result.status));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_CompletedReviewKeepsParentBadge
// is slice #1527 acceptance criterion 3: when all review child rows have
// completed (no live/active review children), the visible run's status must
// revert to the canonical parent's own status — the badge does not flip.
func TestPortalRunsView_VisibleRunForIssueGroup_CompletedReviewKeepsParentBadge(t *testing.T) {
	js := `const parent = {
  key: '260618113825-abcd-1', kind: 'completed', status: 'success', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-01T00:00:00Z',
};
const completedReview = {
  key: 'PR42', kind: 'completed', status: 'success', review: true,
  issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42,
  batchKey: 'review-batch', issueTitle: 'Review PR42',
  startedAt: '2025-01-15T00:00:00Z',
};
const result = visibleRunForIssueGroup(1, [parent, completedReview]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-1') throw new Error('expected canonical parent key issue-1, got ' + JSON.stringify(result.key));
if (result.status !== 'success') throw new Error('expected visible status=success (no live review child → no flip), got ' + JSON.stringify(result.status));
if (result.liveReview) throw new Error('expected liveReview=false (no live review child), got ' + JSON.stringify(result.liveReview));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_NoReviewChildrenKeepsParentBadge
// is slice #1527 acceptance criterion 4: a canonical parent with no review
// children at all must produce a visible row whose status matches the
// parent's own status, no badge flip.
func TestPortalRunsView_VisibleRunForIssueGroup_NoReviewChildrenKeepsParentBadge(t *testing.T) {
	js := `const parent = {
  key: '260618113825-abcd-1', kind: 'completed', status: 'success', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-01T00:00:00Z',
};
const result = visibleRunForIssueGroup(1, [parent]);
if (!result) throw new Error('expected visible row');
if (result.status !== 'success') throw new Error('expected visible status=success (no review children → no flip), got ' + JSON.stringify(result.status));
if (result.liveReview) throw new Error('expected liveReview=false (no review children), got ' + JSON.stringify(result.liveReview));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_LiveActiveParentFlipsBadgeViaChild
// verifies that for a still-active parent with a live review child, the
// badge status from the backend projection passes through unchanged.
func TestPortalRunsView_VisibleRunForIssueGroup_LiveActiveParentFlipsBadgeViaChild(t *testing.T) {
	js := `const parent = {
  key: '260618113825-abcd-1', kind: 'active', status: 'reviewing', review: false,
  issueLabel: '#1', runId: '260618113825-abcd-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-15T00:00:00Z',
};
const liveReview = {
  key: 'PR42', kind: 'active', status: 'reviewing', review: true,
  issueLabel: 'PR42', runId: 'PR42', issueNumber: 1, prNumber: 42,
  batchKey: 'review-batch', issueTitle: 'Review PR42',
  startedAt: '2025-02-01T00:00:00Z',
};
const result = visibleRunForIssueGroup(1, [parent, liveReview]);
if (!result) throw new Error('expected visible row');
if (result.key !== '260618113825-abcd-1') throw new Error('expected canonical parent key, got ' + JSON.stringify(result.key));
if (result.status !== 'reviewing') throw new Error('expected visible status=reviewing (backend-provided), got ' + JSON.stringify(result.status));
if (result.batchKey !== 'parent-batch') throw new Error('expected parent batchKey, got ' + JSON.stringify(result.batchKey));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalRunsView_VisibleRunForIssueGroup_OrphanReviewOnlyHasItsOwnStatus
// is slice #1527 acceptance criterion 5: orphan review-only rows (no
// canonical parent) must NOT trigger any extra badge flip logic — the
// existing review-only stub synthesis presents its own live-reviewing or
// terminal verdict status honestly.
func TestPortalRunsView_VisibleRunForIssueGroup_OrphanReviewOnlyHasItsOwnStatus(t *testing.T) {
	js := `const liveReview = {
  key: '260622193226-a0c19-1227', kind: 'active', status: 'reviewing', review: true,
  issueLabel: '#1223', runId: '260622193226-a0c19-1227', issueNumber: 1223, prNumber: 5,
};
const result = visibleRunForIssueGroup(1223, [liveReview]);
if (!result) throw new Error('expected visible row');
if (result.status !== 'reviewing') throw new Error('expected orphan review-only visible status=reviewing (the review is live), got ' + JSON.stringify(result.status));
if (result.review !== true) throw new Error('expected orphan review-only stub review=true, got ' + JSON.stringify(result.review));
if (result.reviewLive !== true) throw new Error('expected orphan review-only stub reviewLive=true, got ' + JSON.stringify(result.reviewLive));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestPortalDiffUpdateDetailEvents_PollFingerprintUsesCheapTriplet (issue
// #1562) locks AC #1 for the events tab: after the first build-and-poll
// pair on a stable run, the `data-rendered-fingerprint` attribute on the
// `.detail-content` div is the cheap triplet
// `events|<len>:<ts0>:<tsN>|<subjects:...>`, not the full JSON.
//
// The expensive `eventsJSON(run)` string would include quoted JSON braces,
// escaped quotes, and per-event payload dumps; the cheap triplet is purely
// `events|3:1700000000000:1700000003000|subjects:...` for a 3-event run.
//
// We assert on the post-poll fingerprint AFTER two stable diffRuns (the
// first builds the panel, the second runs updateDetailContent which is
// the call site the issue targets).
func TestPortalDiffUpdateDetailEvents_PollFingerprintUsesCheapTriplet(t *testing.T) {
	js := `const body = makeMockBody();
const events = [
  { type: 'start', timestamp: 1700000000000, payload: { ok: true } },
  { type: 'progress', timestamp: 1700000001000, payload: { step: 1 } },
  { type: 'finish', timestamp: 1700000003000, payload: { ok: true } },
];
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: events };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const content = detailRow.querySelector('.detail-content');
if (!content) throw new Error('expected detail-content');
SandmanPortalDiff.diffRuns(body, [run], opts);
const fp = content.getAttribute('data-rendered-fingerprint') || '';
if (fp.indexOf('events|3:1700000000000:1700000003000') !== 0) {
  throw new Error('expected cheap triplet prefix events|3:1700000000000:1700000003000, got ' + fp);
}
if (fp.indexOf('"type"') !== -1 || fp.indexOf('{') !== -1) {
  throw new Error('fingerprint looks like JSON, expected cheap triplet, got ' + fp);
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateDetailEvents_PollFingerprintSingleEvent (issue
// #1562) locks the single-event branch of the cheap triplet: when
// `events.length === 1`, the triplet collapses to `<ts>:<ts>` so the
// length slot does not double-count the same event.
func TestPortalDiffUpdateDetailEvents_PollFingerprintSingleEvent(t *testing.T) {
	js := `const body = makeMockBody();
const events = [{ type: 'start', timestamp: 1700000000000, payload: { ok: true } }];
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: events };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const content = detailRow.querySelector('.detail-content');
SandmanPortalDiff.diffRuns(body, [run], opts);
const fp = content.getAttribute('data-rendered-fingerprint') || '';
if (fp.indexOf('events|1700000000000:1700000000000') !== 0) {
  throw new Error('expected cheap triplet prefix events|1700000000000:1700000000000, got ' + fp);
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateDetailEvents_PollFingerprintEmpty (issue #1562)
// locks the empty-events branch of the cheap triplet: when there are no
// events, the fingerprint collapses to `events|0|...` rather than
// `events|0::` (which would carry two trailing colons that look wrong
// under the issue's spec "If events is empty, use '0'").
func TestPortalDiffUpdateDetailEvents_PollFingerprintEmpty(t *testing.T) {
	js := `const body = makeMockBody();
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: [] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const content = detailRow.querySelector('.detail-content');
SandmanPortalDiff.diffRuns(body, [run], opts);
const fp = content.getAttribute('data-rendered-fingerprint') || '';
if (fp.indexOf('events|0|') !== 0) {
  throw new Error('expected empty-events fingerprint events|0|..., got ' + fp);
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiffUpdateDetailDetails_PollFingerprintUsesCheapTriplet
// (issue #1562) locks AC #1 for the details tab: after the first
// build-and-poll pair on a stable run, the `data-rendered-fingerprint`
// attribute is the cheap string
// `details|<issueNumber>:<issueTitle>:<branch>:<batchKey>:<logPath>:<peersLen>:<peersCsv>|...`,
// not the JSON.stringify'd details object.
func TestPortalDiffUpdateDetailDetails_PollFingerprintUsesCheapTriplet(t *testing.T) {
	js := `const body = makeMockBody();
const run = {
  key: 'a', kind: 'completed', status: 'success', issueLabel: 'A', runId: 'r1',
  startedAt: 1000, finishedAt: 2000, duration: 1,
  branch: 'main', batchKey: 'b1', batchIssues: [42, 99],
  issueNumber: 42, issueTitle: 'Fix the frobnicator',
  logPath: '/tmp/run.log', logUrl: '/log',
};
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'details' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const content = detailRow.querySelector('.detail-content');
SandmanPortalDiff.diffRuns(body, [run], opts);
const fp = content.getAttribute('data-rendered-fingerprint') || '';
if (fp.indexOf('details|42:Fix the frobnicator:main:b1:/tmp/run.log:1:99|') !== 0) {
  throw new Error('expected cheap prefix details|42:Fix the frobnicator:main:b1:/tmp/run.log:1:99|, got ' + fp);
}
if (fp.indexOf('"issueNumber"') !== -1 || fp.indexOf('{') !== -1) {
  throw new Error('fingerprint looks like JSON, expected cheap string, got ' + fp);
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalDiff_StablePollsDoNotInvokeJSONStringify (issue #1562) locks
// AC #3: across 100 stable polls on the Events tab, `JSON.stringify` is
// never called by the poll path. The poll path must use the cheap
// triplet (no payload serialization) so the per-poll cost stays O(1) in
// `events.length`.
//
// The sandbox monkey-patches `JSON.stringify` to count calls *only* in
// the poll path (the spy is installed after the first build so the
// build's own stringify invocations aren't counted). Before #1562 the
// poll path ran `eventsJSON(run)` per diffRuns, so 100 stable polls
// pushed the counter well past 100. After #1562 the poll path uses the
// cheap triplet, which never touches JSON.stringify — the counter
// stays at 0 (or 1, the single fingerprint-attr write on the first
// poll). We accept ≤ 1 as the structural proxy for AC #3.
//
// At the end of the test we mutate events so the rebuild branch still
// fires and the rendered <pre data-rendered-json> content includes the
// new event — this locks AC #2 (rebuild branch unchanged).
func TestPortalDiff_StablePollsDoNotInvokeJSONStringify(t *testing.T) {
	js := `const body = makeMockBody();
const events = [];
for (let i = 0; i < 200; i++) {
  events.push({ type: 'progress', timestamp: 1700000000000 + i * 1000, payload: { step: i, blob: 'x'.repeat(50) } });
}
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: events };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const content = detailRow.querySelector('.detail-content');
if (!content) throw new Error('expected detail-content');
const originalStringify = JSON.stringify;
let stringifyCalls = 0;
JSON.stringify = function() { stringifyCalls += 1; return originalStringify.apply(this, arguments); };
try {
  for (let i = 0; i < 100; i++) {
    SandmanPortalDiff.diffRuns(body, [run], opts);
  }
} finally {
  JSON.stringify = originalStringify;
}
	if (stringifyCalls !== 0) {
		throw new Error('expected 0 JSON.stringify calls across 100 stable polls, got ' + stringifyCalls);
	}
	// Rebuild branch must still call the real JSON.stringify via
	// buildEventsContent → eventsJSON, and the rebuilt <pre> content
	// must remain byte-identical to a direct JSON.stringify of the
	// event list. This locks AC #2 ("rebuilt content is byte-identical
	// to today") — the rebuilt JSON is the same JSON.stringify(run2)
	// the old code would have produced on a rebuild.
	const run2 = Object.assign({}, run, { events: events.concat([{ type: 'finish', timestamp: 1700000205000, payload: {} }]) });
	SandmanPortalDiff.diffRuns(body, [run2], opts);
	const pre = detailRow.querySelector('pre[data-rendered-json]');
	if (!pre) throw new Error('expected events pre after rebuild');
	const rendered = pre.getAttribute('data-rendered-json') || '';
	if (rendered.indexOf('finish') === -1) {
		throw new Error('rebuilt content should include the new finish event');
	}
	// Byte-equality against the canonical eventsJSON serialization
	// (defines "today's content" precisely, without coupling to
	// formatting choices elsewhere in the codebase).
	const expected = JSON.stringify(run2.events.map((event) => ({
		type: event && event.type ? event.type : 'event',
		timestamp: event && event.timestamp ? event.timestamp : null,
		payload: event && event.payload ? event.payload : {},
	})), null, 2);
 	if (rendered !== expected) {
 		throw new Error('rebuilt content must match JSON.stringify(events, null, 2) byte-for-byte (AC #2).\nGot: ' + rendered.slice(0, 200) + '\nExpected: ' + expected.slice(0, 200));
 	}
	console.log('PASS');
`
	runNodeScript(t, js)
}

// --- Issue #1856: slice 1 — summarizeReviewGroup verdict extraction ---

// Note: runInNewContext does not accept single-quoted strings that span
// multiple lines, so the fixture logs are built with String.fromCharCode(10)
// (= '\n') instead of literal newlines. This is a vm/parser constraint,
// not a portal.html contract.

// TestSummarizeReviewGroup_Verdict_EmptyLogReturnsEmptyVerdict pins the
// steady-state default for issue #1856: a review child whose `log` is
// missing or empty contributes no verdict, so the orphan-path stub and
// the parent-enrichment path both render the counter line as "N review"
// with no trailing dash. This is the realistic default in production
// because the summary endpoint strips logs from each row.
func TestSummarizeReviewGroup_Verdict_EmptyLogReturnsEmptyVerdict(t *testing.T) {
	js := `const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z' },
];
const summary = summarizeReviewGroup(reviews);
if (summary.verdict !== '') throw new Error('expected empty verdict when log is missing, got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_EmptyStringLogReturnsEmptyVerdict pins
// the same steady-state default with an explicit empty string log.
func TestSummarizeReviewGroup_Verdict_EmptyStringLogReturnsEmptyVerdict(t *testing.T) {
	js := `const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z', log: '' },
];
const summary = summarizeReviewGroup(reviews);
if (summary.verdict !== '') throw new Error('expected empty verdict when log is empty string, got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_NoDecisionSectionReturnsEmptyVerdict
// pins the case where the run.log has content but no `## Decision`
// section.
func TestSummarizeReviewGroup_Verdict_NoDecisionSectionReturnsEmptyVerdict(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z', log: 'some other log content' + NL + '**APPROVED**' + NL },
];
const summary = summarizeReviewGroup(reviews);
if (summary.verdict !== '') throw new Error('expected empty verdict when no ## Decision section, got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_ApprovedMarker pins the canonical
// "## Decision\n**APPROVED**" shape: verdict becomes "Approved".
func TestSummarizeReviewGroup_Verdict_ApprovedMarker(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z', log: '## Decision' + NL + '**APPROVED**' + NL },
];
const summary = summarizeReviewGroup(reviews);
if (summary.verdict !== 'Approved') throw new Error('expected verdict=Approved, got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_ChangesRequestedMarker pins the
// "**CHANGES_REQUESTED**" case.
func TestSummarizeReviewGroup_Verdict_ChangesRequestedMarker(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z', log: '## Decision' + NL + '**CHANGES_REQUESTED**' + NL },
];
const summary = summarizeReviewGroup(reviews);
if (summary.verdict !== 'Changes requested') throw new Error('expected verdict=Changes requested, got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_GoRegexParityBare pins Go-side parity
// for the bare marker line shape. Slice 1 of issue #1938 retargeted
// the server-side verdict reader from run.log to decision.md; the bare
// marker rule is the only rule the new Go helper accepts (decision.md
// is a controlled artefact with no shell prefix and no trailing
// debris).
func TestSummarizeReviewGroup_Verdict_GoRegexParityBare(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z', log: '## Decision' + NL + '**APPROVED**' + NL },
];
const summary = summarizeReviewGroup(reviews);
if (summary.verdict !== 'Approved') throw new Error('bare marker must match the Go-side bare marker rule, got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_JSOrphanDebrisStillTolerated pins the
// JS-side orphan helper's tolerance of shell-debris shapes
// (`**APPROVED**" 2>&1 | tail -5`). Issue #1938 slice 1 retargeted the
// Go server-side verdict reader to decision.md (no debris tolerated),
// but the JS orphan-fallback path still reads each review's saved
// run.log, where the same debris rule remains in place — the JS-side
// debris regex must continue to accept these lines so orphan rows in
// flight before slice 1 still surface a verdict. This test gates that
// behaviour so a future JS tidy-up does not silently drop orphan
// verdicts for legacy runs.
func TestSummarizeReviewGroup_Verdict_JSOrphanDebrisStillTolerated(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z', log: '## Decision' + NL + '**APPROVED**" 2>&1 | tail -5' + NL },
];
const summary = summarizeReviewGroup(reviews);
if (summary.verdict !== 'Approved') throw new Error('with-debris marker must still match (JS orphan helper retains debris tolerance for legacy run.log), got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_RejectsMidLineProse pins Go-side parity
// for the negative case: mid-line prose like **APPROVED** is unrelated
// prose is rejected (no false positive).
func TestSummarizeReviewGroup_Verdict_RejectsMidLineProse(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z', log: '## Decision' + NL + '**APPROVED** is unrelated prose' + NL },
];
const summary = summarizeReviewGroup(reviews);
if (summary.verdict !== '') throw new Error('mid-line prose must be rejected (Go regex parity), got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_LatestReviewWinsTieBreak pins the
// "latest review wins" contract: when two reviews have different
// startedAt and disagree on the marker, the verdict comes from the
// newer review. The existing summarizeReviewGroup sort puts the
// newest startedAt first; the verdict scan walks the ordered list
// in that order and returns the first recoverable marker.
func TestSummarizeReviewGroup_Verdict_LatestReviewWinsTieBreak(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
// r1 is older and has CHANGES_REQUESTED; r2 is newer and has
// APPROVED. The verdict scan must pick r2's marker (newer wins).
const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z', log: '## Decision' + NL + '**CHANGES_REQUESTED**' + NL },
  { key: 'r2', runId: 'r2', review: true, status: 'success', startedAt: '2026-07-02T00:00:00Z', log: '## Decision' + NL + '**APPROVED**' + NL },
];
const summary = summarizeReviewGroup(reviews);
// summarizeReviewGroup sorts by startedAt desc, so r2 (newer) is
// ordered first. The verdict scan walks reviews in that order and
// returns the verdict from the first review whose log has a marker.
if (summary.verdict !== 'Approved') throw new Error('expected verdict from newer review (r2, APPROVED), got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_LogLinePrefixStrips pins parity with
// the Go-side stripLogLabel: each line may be prefixed with
// "[<runID>] HH:MM:SS " by the agent output stream.
func TestSummarizeReviewGroup_Verdict_LogLinePrefixStrips(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const reviews = [
  { key: 'r1', runId: 'r1', review: true, status: 'success', startedAt: '2026-07-01T00:00:00Z', log: '[r1] 12:00:00 ## Decision' + NL + '[r1] 12:00:30 **APPROVED**' + NL },
];
const summary = summarizeReviewGroup(reviews);
if (summary.verdict !== 'Approved') throw new Error('expected verdict=Approved after stripping [runID] HH:MM:SS prefix, got ' + JSON.stringify(summary.verdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestSummarizeReviewGroup_Verdict_OrphanPathPicksUpNewExtraction pins
// the orphan path: with the new extraction, an orphan review-only group
// whose log has a Decision marker must also surface the verdict through
// the existing summary path (no separate code change in the orphan
// branch). Slice 1's effect propagates to the orphan stub automatically.
func TestSummarizeReviewGroup_Verdict_OrphanPathPicksUpNewExtraction(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const review = { key: 'r1', runId: 'r1', review: true, status: 'success', issueNumber: 1, prNumber: 5, startedAt: '2026-07-01T00:00:00Z', log: '## Decision' + NL + '**APPROVED**' + NL };
const stub = visibleRunForIssueGroup(1, [review]);
if (!stub) throw new Error('expected orphan stub row');
if (stub.reviewVerdict !== 'Approved') throw new Error('expected orphan stub to surface Approved via new verdict extraction, got ' + JSON.stringify(stub.reviewVerdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// --- Issue #1856: slice 2 — visibleRunForIssueGroup parent enrichment ---

// TestVisibleRunForIssueGroup_ParentStampsTwoReviews is AC 6 (a): a
// completed parent with two terminal review children that each carry
// an APPROVED marker in their run.log must surface reviewCount=2 and
// reviewVerdict="Approved" on the visible parent row, with the
// parent's identity fields preserved verbatim. After #1897 the server
// (aggregateReviewChildren) stamps these onto the parent during compute;
// the JS passes the server-stamped values through unchanged.
func TestVisibleRunForIssueGroup_ParentStampsTwoReviews(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const parent = {
  key: 'impl-1', kind: 'completed', status: 'success', review: false,
  issueLabel: '#1', runId: 'impl-1', issueNumber: 1,
  batchKey: 'parent-batch', issueTitle: 'Fix login bug',
  startedAt: '2025-01-01T00:00:00Z',
  // Server-stamped by aggregateReviewChildren (#1897).
  reviewCount: 2, reviewVerdict: 'Approved',
};
const review1 = {
  key: 'PR42', runId: 'PR42', kind: 'completed', status: 'success', review: true,
  issueNumber: 1, prNumber: 42, startedAt: '2025-01-15T00:00:00Z',
  log: '## Decision' + NL + '**APPROVED**' + NL,
};
const review2 = {
  key: 'PR43', runId: 'PR43', kind: 'completed', status: 'success', review: true,
  issueNumber: 1, prNumber: 43, startedAt: '2025-01-20T00:00:00Z',
  log: '## Decision' + NL + '**APPROVED**' + NL,
};
const visible = visibleRunForIssueGroup(1, [parent, review1, review2]);
if (!visible) throw new Error('expected visible row');
if (visible.reviewCount !== 2) throw new Error('expected reviewCount=2, got ' + JSON.stringify(visible.reviewCount));
if (visible.reviewVerdict !== 'Approved') throw new Error('expected reviewVerdict=Approved, got ' + JSON.stringify(visible.reviewVerdict));
if (visible.key !== 'impl-1') throw new Error('expected parent key preserved, got ' + JSON.stringify(visible.key));
if (visible.runId !== 'impl-1') throw new Error('expected parent runId preserved, got ' + JSON.stringify(visible.runId));
if (visible.batchKey !== 'parent-batch') throw new Error('expected parent batchKey preserved, got ' + JSON.stringify(visible.batchKey));
if (visible.issueTitle !== 'Fix login bug') throw new Error('expected parent issueTitle preserved, got ' + JSON.stringify(visible.issueTitle));
if (visible.startedAt !== '2025-01-01T00:00:00Z') throw new Error('expected parent startedAt preserved, got ' + JSON.stringify(visible.startedAt));
if (visible.status !== 'success') throw new Error('expected parent status preserved, got ' + JSON.stringify(visible.status));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestVisibleRunForIssueGroup_ParentStampsOneReview is AC 6 (b): a
// completed parent with one terminal review child must surface
// reviewCount=1 and reviewVerdict="Approved". After #1897 the server
// (aggregateReviewChildren) stamps these onto the parent during compute;
// the JS passes the server-stamped values through unchanged.
func TestVisibleRunForIssueGroup_ParentStampsOneReview(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const parent = {
  key: 'impl-2', kind: 'completed', status: 'success', review: false,
  issueLabel: '#2', runId: 'impl-2', issueNumber: 2,
  batchKey: 'parent-batch', issueTitle: 'Fix signup bug',
  startedAt: '2025-01-01T00:00:00Z',
  // Server-stamped by aggregateReviewChildren (#1897) from the sibling
  // review's saved run.log; the JS must pass these through unchanged.
  reviewCount: 1, reviewVerdict: 'Approved',
};
const review = {
  key: 'PR44', runId: 'PR44', kind: 'completed', status: 'success', review: true,
  issueNumber: 2, prNumber: 44, startedAt: '2025-01-15T00:00:00Z',
  log: '## Decision' + NL + '**APPROVED**' + NL,
};
const visible = visibleRunForIssueGroup(2, [parent, review]);
if (!visible) throw new Error('expected visible row');
if (visible.reviewCount !== 1) throw new Error('expected reviewCount=1, got ' + JSON.stringify(visible.reviewCount));
if (visible.reviewVerdict !== 'Approved') throw new Error('expected reviewVerdict=Approved, got ' + JSON.stringify(visible.reviewVerdict));
if (visible.key !== 'impl-2') throw new Error('expected parent key preserved, got ' + JSON.stringify(visible.key));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestVisibleRunForIssueGroup_ParentNoReviewsNoStamp is AC 6 (c): a
// completed parent with zero review children must not have
// reviewCount/reviewVerdict stamped onto the visible row. The visible
// row is the parent identity, unchanged.
func TestVisibleRunForIssueGroup_ParentNoReviewsNoStamp(t *testing.T) {
	js := `const parent = {
  key: 'impl-3', kind: 'completed', status: 'success', review: false,
  issueLabel: '#3', runId: 'impl-3', issueNumber: 3,
  batchKey: 'parent-batch', issueTitle: 'Fix reset bug',
  startedAt: '2025-01-01T00:00:00Z',
};
const visible = visibleRunForIssueGroup(3, [parent]);
if (!visible) throw new Error('expected visible row');
if (visible.reviewCount) throw new Error('expected no reviewCount when no review children, got ' + JSON.stringify(visible.reviewCount));
if (visible.reviewVerdict) throw new Error('expected no reviewVerdict when no review children, got ' + JSON.stringify(visible.reviewVerdict));
if (visible.key !== 'impl-3') throw new Error('expected parent key preserved, got ' + JSON.stringify(visible.key));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestVisibleRunForIssueGroup_ActiveParentLiveReviewShowsReviewingFlip is
// AC 6 (d) post-#1897: a terminal parent alongside a live (non-terminal)
// review child must show status="reviewing" — the server-side
// aggregateReviewChildren flips the parent's badge when a live sibling
// review exists (restored #1527/#1609 feature). reviewCount is stamped;
// reviewVerdict stays empty (no terminal review child with a marker).
func TestVisibleRunForIssueGroup_ActiveParentLiveReviewShowsReviewingFlip(t *testing.T) {
	js := `const parent = {
  key: 'impl-4', kind: 'completed', status: 'reviewing', review: false,
  issueLabel: '#4', runId: 'impl-4', issueNumber: 4,
  batchKey: 'parent-batch', issueTitle: 'Fix toggle bug',
  startedAt: '2025-01-01T00:00:00Z',
  // Server-stamped by aggregateReviewChildren (#1897): live sibling
  // review flips the parent badge to "reviewing" and stamps reviewCount.
  reviewCount: 1,
};
const liveReview = {
  key: 'PR50', runId: 'PR50', kind: 'active', status: 'reviewing', review: true,
  issueNumber: 4, prNumber: 50, startedAt: '2025-02-01T00:00:00Z',
};
const visible = visibleRunForIssueGroup(4, [parent, liveReview]);
if (!visible) throw new Error('expected visible row');
if (visible.status !== 'reviewing') throw new Error('expected visible status=reviewing (server flip for live review child), got ' + JSON.stringify(visible.status));
if (visible.reviewCount !== 1) throw new Error('expected reviewCount=1, got ' + JSON.stringify(visible.reviewCount));
// live review has no terminal marker, so verdict stays '' (omitted on
// the wire via json,omitempty; treat undefined and '' equivalently).
const verdict = visible.reviewVerdict || '';
if (verdict !== '') throw new Error('expected empty reviewVerdict when only live review, got ' + JSON.stringify(visible.reviewVerdict));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestVisibleRunForIssueGroup_ActiveKindParentAlongsideLiveReview_FlipsToReviewing
// pins AC 6 (d) for the active-parent path: when the parent row's own kind is
// 'active' (the implementation is still in flight) and a live review
// child is present, the server stamps status="reviewing" (badge-flip) and
// reviewCount onto the parent; the JS passes them through, preserving
// the parent's own identity fields (key, kind, runId).
func TestVisibleRunForIssueGroup_ActiveKindParentAlongsideLiveReview_FlipsToReviewing(t *testing.T) {
	js := `const parent = {
  key: 'impl-4b', kind: 'active', status: 'reviewing', review: false,
  issueLabel: '#4b', runId: 'impl-4b', issueNumber: 41,
  batchKey: 'parent-batch', issueTitle: 'Fix live bug',
  startedAt: '2025-01-01T00:00:00Z',
  // Server-stamped by aggregateReviewChildren (#1897).
  reviewCount: 1,
};
const liveReview = {
  key: 'PR51', runId: 'PR51', kind: 'active', status: 'reviewing', review: true,
  issueNumber: 41, prNumber: 51, startedAt: '2025-02-01T00:00:00Z',
};
const visible = visibleRunForIssueGroup(41, [parent, liveReview]);
if (!visible) throw new Error('expected visible row');
if (visible.kind !== 'active') throw new Error('expected active kind preserved, got ' + JSON.stringify(visible.kind));
if (visible.status !== 'reviewing') throw new Error('expected reviewing status (server flip for live review child), got ' + JSON.stringify(visible.status));
if (visible.reviewCount !== 1) throw new Error('expected reviewCount=1, got ' + JSON.stringify(visible.reviewCount));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestVisibleRunForIssueGroup_OrphanReviewOnlyPathUnchanged is AC 4:
// the orphan review-only group path (no canonical parent) must remain
// unchanged. The synthesized stub already gets reviewCount and
// reviewVerdict from summarizeReviewGroup (slice 1 propagation). The
// visible row is the orphan stub, not the canonical parent shape.
func TestVisibleRunForIssueGroup_OrphanReviewOnlyPathUnchanged(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const review1 = {
  key: 'r1', runId: 'r1', kind: 'completed', status: 'success', review: true,
  issueNumber: 5, prNumber: 60, startedAt: '2025-01-15T00:00:00Z',
  log: '## Decision' + NL + '**APPROVED**' + NL,
};
const review2 = {
  key: 'r2', runId: 'r2', kind: 'completed', status: 'success', review: true,
  issueNumber: 5, prNumber: 61, startedAt: '2025-01-20T00:00:00Z',
  log: '## Decision' + NL + '**APPROVED**' + NL,
};
const visible = visibleRunForIssueGroup(5, [review1, review2]);
if (!visible) throw new Error('expected visible orphan stub row');
if (visible.review !== true) throw new Error('expected review=true on orphan stub, got ' + JSON.stringify(visible.review));
if (visible.reviewCount !== 2) throw new Error('expected orphan stub reviewCount=2, got ' + JSON.stringify(visible.reviewCount));
if (visible.reviewVerdict !== 'Approved') throw new Error('expected orphan stub reviewVerdict=Approved, got ' + JSON.stringify(visible.reviewVerdict));
if (visible.kind !== 'completed') throw new Error('expected orphan stub kind=completed, got ' + JSON.stringify(visible.kind));
if (visible.status !== 'success') throw new Error('expected orphan stub status=success, got ' + JSON.stringify(visible.status));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestVisibleRunForIssueGroup_ParentEnrichmentNoFetch is AC 5: the
// parent row's reviewCount/reviewVerdict arrive server-stamped on the
// /api/runs?summary=1 payload (restored #1897); the JS must NOT fetch
// sibling review logs to recompute them. Zero fetch calls expected.
func TestVisibleRunForIssueGroup_ParentEnrichmentNoFetch(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
let fetchCount = 0;
const originalFetch = window.fetch;
window.fetch = function() { fetchCount++; return Promise.reject(new Error('unexpected fetch')); };
try {
  const parent = {
    key: 'impl-6', kind: 'completed', status: 'success', review: false,
    issueLabel: '#6', runId: 'impl-6', issueNumber: 6,
    batchKey: 'parent-batch', issueTitle: 'Fix six',
    startedAt: '2025-01-01T00:00:00Z',
    // Server-stamped by aggregateReviewChildren (#1897).
    reviewCount: 1, reviewVerdict: 'Approved',
  };
  const review = {
    key: 'PR66', runId: 'PR66', kind: 'completed', status: 'success', review: true,
    issueNumber: 6, prNumber: 66, startedAt: '2025-01-15T00:00:00Z',
    log: '## Decision' + NL + '**APPROVED**' + NL,
  };
  const visible = visibleRunForIssueGroup(6, [parent, review]);
  if (!visible) throw new Error('expected visible row');
  if (visible.reviewCount !== 1) throw new Error('expected reviewCount=1 (server-stamped, passed through), got ' + JSON.stringify(visible.reviewCount));
  if (visible.reviewVerdict !== 'Approved') throw new Error('expected reviewVerdict=Approved (server-stamped), got ' + JSON.stringify(visible.reviewVerdict));
  if (fetchCount !== 0) throw new Error('expected zero fetch calls (server stamps the verdict; JS must not refetch sibling logs), got ' + fetchCount);
} finally {
  window.fetch = originalFetch;
}
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestVisibleRunForIssueGroup_ParentServerStampAuthoritativeNotOverwritten
// pins the #1897 contract: the server-stamped reviewCount/reviewVerdict on
// the parent row are authoritative — the JS must NOT recompute or overwrite
// them from sibling reviews. (Pre-#1897 the #1856 JS enrichment overwrote
// stale Go projections; with server-side stamping restored, the parent's
// own stamped values are the canonical source and must pass through
// unchanged.) Identity fields are also preserved verbatim.
func TestVisibleRunForIssueGroup_ParentServerStampAuthoritativeNotOverwritten(t *testing.T) {
	js := `const NL = String.fromCharCode(10);
const parent = {
  key: 'impl-7', kind: 'completed', status: 'success', review: false,
  issueLabel: '#7', runId: 'impl-7', issueNumber: 7,
  batchKey: 'parent-batch', issueTitle: 'Fix seven',
  startedAt: '2025-01-01T00:00:00Z',
  // Server-stamped by aggregateReviewChildren (#1897). The JS must
  // pass these through unchanged — NOT recompute from the sibling
  // review's log (which the summary endpoint strips anyway).
  reviewCount: 5, reviewVerdict: 'Changes requested',
};
const review = {
  key: 'PR77', runId: 'PR77', kind: 'completed', status: 'success', review: true,
  issueNumber: 7, prNumber: 77, startedAt: '2025-01-15T00:00:00Z',
  log: '## Decision' + NL + '**APPROVED**' + NL,
};
const visible = visibleRunForIssueGroup(7, [parent, review]);
if (!visible) throw new Error('expected visible row');
// Server-stamped values are authoritative; JS must not overwrite with
// sibling-derived values (which would be 1 / Approved here).
if (visible.reviewCount !== 5) throw new Error('expected server-stamped reviewCount=5 preserved (JS must not overwrite), got ' + JSON.stringify(visible.reviewCount));
if (visible.reviewVerdict !== 'Changes requested') throw new Error('expected server-stamped reviewVerdict preserved (JS must not overwrite), got ' + JSON.stringify(visible.reviewVerdict));
// Identity fields preserved.
if (visible.key !== 'impl-7') throw new Error('expected parent key preserved, got ' + JSON.stringify(visible.key));
if (visible.batchKey !== 'parent-batch') throw new Error('expected parent batchKey preserved, got ' + JSON.stringify(visible.batchKey));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// --- Issue #1856: slice 3 — renderRunMeta line shape for parent with reviews ---

// TestRenderRunMeta_ParentWithTwoReviewsPluralWording is AC 1 (plural):
// the meta line for a visible parent row with reviewCount=2 and
// reviewVerdict="Approved" must include "2 reviews - Approved" on the
// counter line.
func TestRenderRunMeta_ParentWithTwoReviewsPluralWording(t *testing.T) {
	js := `const run = {
  key: 'impl-1', runId: 'impl-1', batchKey: 'parent-batch',
  kind: 'completed', status: 'success', reviewCount: 2, reviewVerdict: 'Approved',
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('2 reviews - Approved') < 0) throw new Error('expected "2 reviews - Approved" in meta, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_ParentWithOneReviewSingularWording is AC 1 (singular):
// the meta line for reviewCount=1 must use "1 review" (no plural 's').
func TestRenderRunMeta_ParentWithOneReviewSingularWording(t *testing.T) {
	js := `const run = {
  key: 'impl-2', runId: 'impl-2', batchKey: 'parent-batch',
  kind: 'completed', status: 'success', reviewCount: 1, reviewVerdict: 'Approved',
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('1 review - Approved') < 0) throw new Error('expected "1 review - Approved" in meta, got ' + JSON.stringify(meta));
if (meta.indexOf('1 reviews') >= 0) throw new Error('expected no plural "1 reviews" in meta, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_ParentWithTwoReviewsNoVerdictRendersUnclear pins AC 2
// (no marker recoverable) after issue #1939 slice 2 flipped the empty
// verdict contract: on a non-success parent, the meta line for
// reviewCount=2 with empty reviewVerdict must render "2 reviews -
// Unclear" — the server stamp for "decision.md was missing or
// unparseable" surfaces explicitly rather than dropping the suffix. The
// status="failure" (not "success") is deliberate: issue #2220 added a
// short-circuit that renders "Approved" instead when the impl run
// succeeded.
func TestRenderRunMeta_ParentWithTwoReviewsNoVerdictRendersUnclear(t *testing.T) {
	js := `const run = {
  key: 'impl-3', runId: 'impl-3', batchKey: 'parent-batch',
  kind: 'completed', status: 'failure', reviewCount: 2, reviewVerdict: '',
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('2 reviews - Unclear') < 0) throw new Error('expected "2 reviews - Unclear" in meta when reviewVerdict empty on a failed parent, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_ParentWithOneReviewEmptyVerdictRendersUnclear pins
// issue #1939 slice 2: when the Go server stamps reviewVerdict=” on a
// review row (because decision.md was missing or unparseable), the JS
// renderRunMeta projection must render "Unclear" on the counter line
// instead of dropping the verdict suffix. The orphan path can still
// surface ” to renderRunMeta today (when the review's saved run.log has
// no ## Decision marker), so this projection step is the front-end
// contract that closes the gap. The status="failure" (not "success") is
// deliberate: issue #2220 added a short-circuit that renders "Approved"
// instead when the impl run succeeded (test pinned separately in
// portal_review_success_test.go).
func TestRenderRunMeta_ParentWithOneReviewEmptyVerdictRendersUnclear(t *testing.T) {
	js := `const run = {
  key: 'impl-8', runId: 'impl-8', batchKey: 'parent-batch',
  kind: 'completed', status: 'failure', reviewCount: 1, reviewVerdict: '',
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('1 review - Unclear') < 0) throw new Error('expected "1 review - Unclear" in meta when reviewVerdict is empty on a failed parent, got ' + JSON.stringify(meta));
if (meta.indexOf('1 reviews') >= 0) throw new Error('expected no plural "1 reviews" in meta, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

// TestRenderRunMeta_ParentWithoutReviewCountUnchanged pins the no-reviews
// steady state: a parent row without reviewCount must render only
// "Batch: <key>" and "Run: <runId>" — no counter line.
func TestRenderRunMeta_ParentWithoutReviewCountUnchanged(t *testing.T) {
	js := `const run = {
  key: 'impl-4', runId: 'impl-4', batchKey: 'parent-batch',
  kind: 'completed', status: 'success',
};
const meta = helpers.renderRunMeta(run);
if (meta.indexOf('Batch: parent-batch') < 0) throw new Error('expected Batch: line in meta, got ' + JSON.stringify(meta));
if (meta.indexOf('Run: impl-4') < 0) throw new Error('expected Run: line in meta, got ' + JSON.stringify(meta));
if (meta.indexOf('review') >= 0) throw new Error('expected no "review" text in meta for no-review parent, got ' + JSON.stringify(meta));
console.log('PASS');
`
	runPortalHTMLScript(t, js)
}

func TestPortalDiffBuildEventsContent_RenderedEventCountAttribute(t *testing.T) {
	js := `const body = makeMockBody();
const events = [
  { type: 'start', timestamp: 1700000000000, payload: { ok: true } },
  { type: 'progress', timestamp: 1700000001000, payload: { step: 1 } },
  { type: 'finish', timestamp: 1700000003000, payload: { ok: true } },
];
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: events };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
if (!detailRow) throw new Error('expected detail row');
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected pre[data-rendered-json] in events tab');
const renderedCount = pre.getAttribute('data-rendered-event-count');
if (renderedCount !== '3') throw new Error('expected data-rendered-event-count="3", got ' + JSON.stringify(renderedCount));
const renderedJson = pre.getAttribute('data-rendered-json') || '';
const expected = JSON.stringify(events.map((event) => ({
  type: event && event.type ? event.type : 'event',
  timestamp: event && event.timestamp ? event.timestamp : null,
  payload: event && event.payload ? event.payload : {},
})), null, 2);
if (renderedJson !== expected) throw new Error('data-rendered-json must match JSON.stringify(mappedEvents, null, 2) byte-for-byte');
if (!pre.querySelector('.json-key')) throw new Error('expected json-key spans in highlighted HTML');
if (!pre.querySelector('.json-string')) throw new Error('expected json-string spans in highlighted HTML');
if (!pre.querySelector('.json-punctuation')) throw new Error('expected json-punctuation spans in highlighted HTML');
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.diffRuns(body, [run], opts);
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations > 1) throw new Error('stable poll must not re-render, mutations=' + counters.mutations);
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailPanelEvents_AppendPreservesChildren(t *testing.T) {
	js := `const body = makeMockBody();
const e1 = { type: 'start', timestamp: 1700000000000, payload: { ok: true } };
const e2 = { type: 'progress', timestamp: 1700000001000, payload: { step: 1 } };
const e3 = { type: 'progress', timestamp: 1700000002000, payload: { step: 2 } };
const e4 = { type: 'progress', timestamp: 1700000003000, payload: { step: 3 } };
const e5 = { type: 'finish', timestamp: 1700000004000, payload: { ok: true } };
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: [e1, e2, e3] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const preBefore = detailRow.querySelector('pre[data-rendered-json]');
if (!preBefore) throw new Error('expected events pre');
const snapshotChildren = preBefore.children.slice();
const snapshotPreRenderedJson = preBefore.getAttribute('data-rendered-json') || '';
SandmanPortalDiff.resetCounters();
SandmanPortalDiff.updateDetailPanelEvents(body, 'a', [e1, e2, e3, e4, e5], helpers);
if (preBefore.getAttribute('data-rendered-event-count') !== '5') throw new Error('expected data-rendered-event-count="5", got ' + preBefore.getAttribute('data-rendered-event-count'));
const expected = JSON.stringify([e1, e2, e3, e4, e5].map((event) => ({
  type: event && event.type ? event.type : 'event',
  timestamp: event && event.timestamp ? event.timestamp : null,
  payload: event && event.payload ? event.payload : {},
})), null, 2);
const actual = preBefore.getAttribute('data-rendered-json') || '';
if (actual !== expected) throw new Error('post-append data-rendered-json must match canonical, got ' + actual.slice(0, 200));
if (preBefore.children.length < snapshotChildren.length) throw new Error('pre should have at least as many children after append, got ' + preBefore.children.length);
for (let i = 0; i < snapshotChildren.length; i++) {
  if (preBefore.children[i] !== snapshotChildren[i]) throw new Error('child at index ' + i + ' lost identity across append (no-flash/no-rewrite violated)');
}
const counters = SandmanPortalDiff.getCounters();
if (counters.mutations === 0) throw new Error('expected mutations on append, got 0');
const preText = preBefore.textContent;
if (preText.indexOf('type') === -1) throw new Error('expected original event type field text in pre, got ' + JSON.stringify(preText.slice(0, 200)));
if (preText.indexOf('finish') === -1) throw new Error('expected appended finish event text in pre, got ' + JSON.stringify(preText.slice(-200)));
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailPanelEvents_RestructuredFallsBackToRebuild(t *testing.T) {
	js := `const body = makeMockBody();
const e1 = { type: 'start', timestamp: 1700000000000, payload: { ok: true } };
const e2 = { type: 'progress', timestamp: 1700000001000, payload: { step: 1 } };
const e3 = { type: 'finish', timestamp: 1700000002000, payload: { ok: true } };
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: [e1, e2, e3] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const preBefore = detailRow.querySelector('pre[data-rendered-json]');
if (!preBefore) throw new Error('expected events pre');
const snapshotChildren = preBefore.children.slice();
if (snapshotChildren.length === 0) throw new Error('expected non-empty pre children after initial build');
const e1Mutated = { type: 'restart', timestamp: 1700000000500, payload: { ok: true } };
SandmanPortalDiff.updateDetailPanelEvents(body, 'a', [e1Mutated, e2, e3], helpers);
if (preBefore.getAttribute('data-rendered-event-count') !== '3') throw new Error('expected data-rendered-event-count="3" after restructure, got ' + preBefore.getAttribute('data-rendered-event-count'));
const expectedMutated = JSON.stringify([e1Mutated, e2, e3].map((event) => ({
  type: event && event.type ? event.type : 'event',
  timestamp: event && event.timestamp ? event.timestamp : null,
  payload: event && event.payload ? event.payload : {},
})), null, 2);
if (preBefore.getAttribute('data-rendered-json') !== expectedMutated) throw new Error('post-rebuild data-rendered-json must match canonical after restructure');
let reused = 0;
for (const c of snapshotChildren) {
  if (preBefore.children.indexOf(c) !== -1) reused += 1;
}
if (reused !== 0) throw new Error('restructure must rebuild (all-new children), got ' + reused + ' reused children');
const snapshot2 = preBefore.children.slice();
SandmanPortalDiff.updateDetailPanelEvents(body, 'a', [e1, e2], helpers);
if (preBefore.getAttribute('data-rendered-event-count') !== '2') throw new Error('expected data-rendered-event-count="2" after shrink, got ' + preBefore.getAttribute('data-rendered-event-count'));
const expectedShrunk = JSON.stringify([e1, e2].map((event) => ({
  type: event && event.type ? event.type : 'event',
  timestamp: event && event.timestamp ? event.timestamp : null,
  payload: event && event.payload ? event.payload : {},
})), null, 2);
if (preBefore.getAttribute('data-rendered-json') !== expectedShrunk) throw new Error('post-rebuild data-rendered-json must match canonical after shrink');
let reused2 = 0;
for (const c of snapshot2) {
  if (preBefore.children.indexOf(c) !== -1) reused2 += 1;
}
if (reused2 !== 0) throw new Error('shrink must rebuild (all-new children), got ' + reused2 + ' reused children');
const snapshot3 = preBefore.children.slice();
SandmanPortalDiff.updateDetailPanelEvents(body, 'a', [e2, e1], helpers);
const expectedReorder = JSON.stringify([e2, e1].map((event) => ({
  type: event && event.type ? event.type : 'event',
  timestamp: event && event.timestamp ? event.timestamp : null,
  payload: event && event.payload ? event.payload : {},
})), null, 2);
if (preBefore.getAttribute('data-rendered-json') !== expectedReorder) throw new Error('post-rebuild data-rendered-json must match canonical after reorder');
let reused3 = 0;
for (const c of snapshot3) {
  if (preBefore.children.indexOf(c) !== -1) reused3 += 1;
}
if (reused3 !== 0) throw new Error('reorder must rebuild (all-new children), got ' + reused3 + ' reused children');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailPanelEvents_PrefixMismatchForcesRebuild(t *testing.T) {
	js := `const body = makeMockBody();
const e1 = { type: 'start', timestamp: 1700000000000, payload: { ok: true } };
const e2 = { type: 'progress', timestamp: 1700000001000, payload: { step: 1 } };
const e3 = { type: 'progress', timestamp: 1700000002000, payload: { step: 2 } };
const e4 = { type: 'finish', timestamp: 1700000003000, payload: { ok: true } };
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: [e1, e2, e3] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const preBefore = detailRow.querySelector('pre[data-rendered-json]');
if (!preBefore) throw new Error('expected events pre');
const snapshotChildren = preBefore.children.slice();
const e1Mutated = { type: 'restart', timestamp: 1700000000500, payload: { ok: true } };
SandmanPortalDiff.updateDetailPanelEvents(body, 'a', [e1Mutated, e2, e3, e4], helpers);
if (preBefore.getAttribute('data-rendered-event-count') !== '4') throw new Error('expected data-rendered-event-count="4", got ' + preBefore.getAttribute('data-rendered-event-count'));
const expected = JSON.stringify([e1Mutated, e2, e3, e4].map((event) => ({
  type: event && event.type ? event.type : 'event',
  timestamp: event && event.timestamp ? event.timestamp : null,
  payload: event && event.payload ? event.payload : {},
})), null, 2);
if (preBefore.getAttribute('data-rendered-json') !== expected) throw new Error('post-rebuild data-rendered-json must match canonical');
let reused = 0;
for (const c of snapshotChildren) {
  if (preBefore.children.indexOf(c) !== -1) reused += 1;
}
if (reused !== 0) throw new Error('prefix-mismatch must force full rebuild (no append), got ' + reused + ' reused children');
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailPanelEvents_AppendedSuffixHighlighted(t *testing.T) {
	js := `const body = makeMockBody();
const e1 = { type: 'start', timestamp: 1700000000000, payload: { ok: true } };
const e2 = {
  type: 'progress',
  timestamp: 1700000001000,
  payload: { ok: false, count: 42, ratio: 1.5, label: 'done', parent: null, child: { nested: 1 } },
};
const run = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: [e1] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const detailRow = body.children[1];
const pre = detailRow.querySelector('pre[data-rendered-json]');
if (!pre) throw new Error('expected events pre');
const beforeCount = pre.children.length;
SandmanPortalDiff.updateDetailPanelEvents(body, 'a', [e1, e2], helpers);
if (pre.children.length <= beforeCount) throw new Error('expected new child nodes after append, got ' + pre.children.length + ' (was ' + beforeCount + ')');
const classes = { 'json-key': 0, 'json-string': 0, 'json-boolean': 0, 'json-number': 0, 'json-null': 0, 'json-punctuation': 0 };
const tail = pre.children.slice(beforeCount);
for (const node of tail) {
  const visit = (n) => {
    if (!n) return;
    if (n.classList && n.classList._set) {
      for (const c of n.classList._set) {
        if (Object.prototype.hasOwnProperty.call(classes, c)) classes[c] += 1;
      }
    }
    if (n.children) for (const ch of n.children) visit(ch);
  };
  visit(node);
}
if (classes['json-key'] === 0) throw new Error('expected json-key spans on appended region, got ' + JSON.stringify(classes));
if (classes['json-string'] === 0) throw new Error('expected json-string spans on appended region, got ' + JSON.stringify(classes));
if (classes['json-boolean'] === 0) throw new Error('expected json-boolean spans on appended region, got ' + JSON.stringify(classes));
if (classes['json-number'] === 0) throw new Error('expected json-number spans on appended region, got ' + JSON.stringify(classes));
if (classes['json-null'] === 0) throw new Error('expected json-null spans on appended region, got ' + JSON.stringify(classes));
if (classes['json-punctuation'] === 0) throw new Error('expected json-punctuation spans on appended region, got ' + JSON.stringify(classes));
const expectedSuffix = [
  ',',
  '  {',
  '    "type": "progress",',
  '    "timestamp": 1700000001000,',
  '    "payload": {',
  '      "ok": false,',
  '      "count": 42,',
  '      "ratio": 1.5,',
  '      "label": "done",',
  '      "parent": null,',
  '      "child": {',
  '        "nested": 1',
  '      }',
  '    }',
  '  }',
  ']',
].join('\n');
const suffixText = tail.map((n) => n._textContent != null ? n._textContent : (n.textContent || '')).join('');
const decoded = suffixText.replace(/&quot;/g, '"').replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&#39;/g, "'");
if (decoded !== expectedSuffix) {
  for (let i = 0; i < Math.min(decoded.length, expectedSuffix.length); i++) {
    if (decoded[i] !== expectedSuffix[i]) {
      throw new Error('appended region text mismatch at ' + i + ': got=' + JSON.stringify(decoded.slice(Math.max(0, i-30), i+40)) + ' expected=' + JSON.stringify(expectedSuffix.slice(Math.max(0, i-30), i+40)));
    }
  }
  throw new Error('appended region text length mismatch: got ' + decoded.length + ' expected ' + expectedSuffix.length + ' got=' + JSON.stringify(decoded.slice(0, 200)));
}
console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalDiffUpdateDetailContent_EventsAppendPath(t *testing.T) {
	js := `const body = makeMockBody();
const e1 = { type: 'start', timestamp: 1700000000000, payload: { ok: true } };
const e2 = { type: 'progress', timestamp: 1700000001000, payload: { step: 1 } };
const e3 = { type: 'progress', timestamp: 1700000002000, payload: { step: 2 } };
const e4 = { type: 'progress', timestamp: 1700000003000, payload: { step: 3 } };
const e5 = { type: 'finish', timestamp: 1700000004000, payload: { ok: true } };
const run1 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: [e1, e2, e3] };
const stopGroups = new Set();
const opts = { helpers, stopGroups, expandedKey: 'a', tabs: { a: 'events' } };
SandmanPortalDiff.diffRuns(body, [run1], opts);
const detailRow = body.children[1];
const preBefore = detailRow.querySelector('pre[data-rendered-json]');
if (!preBefore) throw new Error('expected events pre');
const snapshotChildren = preBefore.children.slice();
const run2 = { key: 'a', kind: 'active', status: 'running', issueLabel: 'A', runId: 'r1', events: [e1, e2, e3, e4, e5] };
SandmanPortalDiff.diffRuns(body, [run2], opts);
if (preBefore.getAttribute('data-rendered-event-count') !== '5') throw new Error('expected data-rendered-event-count="5", got ' + preBefore.getAttribute('data-rendered-event-count'));
const expected = JSON.stringify([e1, e2, e3, e4, e5].map((event) => ({
  type: event && event.type ? event.type : 'event',
  timestamp: event && event.timestamp ? event.timestamp : null,
  payload: event && event.payload ? event.payload : {},
})), null, 2);
if (preBefore.getAttribute('data-rendered-json') !== expected) throw new Error('post-update data-rendered-json must match canonical, got ' + preBefore.getAttribute('data-rendered-json').slice(0, 200));
for (let i = 0; i < snapshotChildren.length; i++) {
  if (preBefore.children[i] !== snapshotChildren[i]) throw new Error('child at index ' + i + ' lost identity across updateDetailContent append (no-flash violated)');
}
console.log('PASS');
`
	runNodeScript(t, js)
}
