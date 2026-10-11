package cmd

import (
	"strconv"
	"testing"
)

func TestPortalPerf_HighlightCachePreservesCorrectnessAfterEviction(t *testing.T) {
	runNodeScript(t, `
const log = 'first source '.repeat(1000);
const expected = SandmanPortalDiff.highlightTerminalLog(log);
if (!expected || expected !== SandmanPortalDiff.highlightTerminalLog(log)) throw new Error('highlight cache mismatch');
for (let i = 0; i < 20; i++) SandmanPortalDiff.highlightTerminalLog(('source-' + i + ' ').repeat(1000));
if (SandmanPortalDiff.highlightTerminalLog(log) !== expected) throw new Error('eviction changed highlight output');
console.log('PASS');
`)
}

func TestPortalPerf_LogPaneLRUEvictionPreservesTouchedEntry(t *testing.T) {
	runNodeScript(t, `
const body = makeMockBody();
const runs = Array.from({ length: 9 }, (_, i) => ({ key: 'r' + (i + 1), runId: 'r' + (i + 1), kind: 'active', status: 'running', log: 'output-' + (i + 1) }));
for (const run of runs.slice(0, 8)) SandmanPortalDiff.tokenizeForCache(run, helpers);
const run = runs[1];
const opts = { helpers, expandedKey: run.key, tabs: { [run.key]: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const pre = body.children[1].querySelector('pre[data-scroll-key]');
if (pre.textContent !== run.log) throw new Error('cached pane content mismatch');
SandmanPortalDiff.diffRuns(body, [run], { ...opts, tabs: { [run.key]: 'details' } });
SandmanPortalDiff.tokenizeForCache(runs[8], helpers);
if (SandmanPortalDiff.getLogPaneCacheSize() !== 8) throw new Error('pane cache exceeded bound');
if (SandmanPortalDiff.hasLogPaneCached('r1')) throw new Error('LRU pane survived');
for (let i = 2; i <= 9; i++) if (!SandmanPortalDiff.hasLogPaneCached('r' + i)) throw new Error('recent pane evicted: r' + i);
SandmanPortalDiff.diffRuns(body, [run], opts);
if (body.children[1].querySelector('pre[data-scroll-key]') !== pre || pre.textContent !== run.log) throw new Error('warm navigation lost pane or records');
console.log('PASS');
`)
}

func TestPortalPerf_ChunkedSizeBoundariesAndWarmNavigationPreserveCompleteRange(t *testing.T) {
	for _, size := range []int{
		4 * 1024,
		15 * 1024,
		15*1024 + 1,
		16*1024 - 1,
		16 * 1024,
		16*1024 + 1,
		20 * 1024,
		63 * 1024,
		64*1024 - 1,
		64 * 1024,
		64*1024 + 1,
		255 * 1024,
		256*1024 - 1,
		256 * 1024,
		256*1024 + 1,
		320 * 1024,
	} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			runNodeScript(t, `
const store = sandbox.SandmanPortalLog.createStore();
const epoch = store.begin('review');
const size = `+strconv.Itoa(size)+`;
// The source range is exact; a single complete oversized record has the
// documented retention exception.
const text = 'x'.repeat(size - 1);
store.accept('review', epoch, { type: 'snapshot', runId: 'review', generation: 'g', start: 0, end: size, records: [{ start: 0, end: size, text }], cursor: 'end' });
helpers.logState = run => store.get(run.runId);
const run = { key: 'review', runId: 'review', kind: 'active', status: 'reviewing' };
const body = makeMockBody();
const opts = { helpers, expandedKey: run.key, tabs: { review: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const pre = body.children[1].querySelector('pre[data-scroll-key]');
if (size >= 16 * 1024 && !pre.getAttribute('data-rendering-log')) throw new Error('large log was not chunked');
if (size < 16 * 1024 && pre.getAttribute('data-rendering-log')) throw new Error('small log did not render synchronously');
function settle() {
  if (pre.getAttribute('data-rendering-log')) { setTimeout(settle, 5); return; }
  if (pre.textContent !== store.get('review').text) throw new Error('chunked render lost records');
  if (pre.getAttribute('data-log-end') !== String(store.get('review').end)) throw new Error('view range mismatch');
  const first = pre.firstChild;
  for (const tab of ['details', 'events']) {
    SandmanPortalDiff.diffRuns(body, [run], { ...opts, tabs: { review: tab } });
    SandmanPortalDiff.diffRuns(body, [run], opts);
    if (body.children[1].querySelector('pre[data-scroll-key]') !== pre || pre.firstChild !== first) throw new Error('warm navigation rebuilt log');
    if (pre.textContent !== store.get('review').text) throw new Error('warm navigation sequence mismatch');
  }
  console.log('PASS');
}
settle();
`)
		})
	}
}
