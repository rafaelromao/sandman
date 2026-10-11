package cmd

import "testing"

// The transport owner and pane renderer are both production assets. Only the
// DOM and scheduling are faked; the oracle is the accepted positional range.
func TestPortalLogProjection_AppendAndNavigationPreservePaneAndCompleteSequence(t *testing.T) {
	runNodeScript(t, `
const store = sandbox.SandmanPortalLog.createStore();
const epoch = store.begin('a');
const initial = { type: 'snapshot', runId: 'a', generation: 'g', start: 20, end: 28, records: [{ start: 20, end: 24, text: 'one' }, { start: 24, end: 28, text: 'one' }], cursor: 'c28' };
store.accept('a', epoch, initial);
helpers.logState = run => store.get(run.runId);
const run = { key: 'a', runId: 'a', status: 'running', kind: 'active', log: 'metadata must never render' };
const body = makeMockBody();
const opts = { helpers, expandedKey: 'a', tabs: { a: 'log' }, runs: [run] };
SandmanPortalDiff.diffRuns(body, [run], opts);
const pre = body.children[1].querySelector('pre[data-scroll-key]');
const firstChild = pre.firstChild;
pre.scrollTop = 42;
store.accept('a', epoch, { type: 'append', runId: 'a', generation: 'g', start: 28, end: 33, records: [{ start: 28, end: 29, text: '' }, { start: 29, end: 33, text: 'one' }], cursor: 'c33' });
SandmanPortalDiff.projectLogState(pre, store.get('a'), helpers);
if (pre.firstChild !== firstChild) throw new Error('contiguous append replaced prefix nodes');
if (pre.scrollTop !== 42) throw new Error('append changed nonsticky scroll');
if (pre.textContent !== 'one\none\n\none\n') throw new Error('record multiplicity was lost: ' + JSON.stringify(pre.textContent));
if (pre.getAttribute('data-log-start') !== '20' || pre.getAttribute('data-log-end') !== '33') throw new Error('DOM range differs from model');
for (const tab of ['events', 'details']) {
  SandmanPortalDiff.diffRuns(body, [run], { ...opts, tabs: { a: tab } });
  SandmanPortalDiff.diffRuns(body, [run], opts);
  if (body.children[1].querySelector('pre[data-scroll-key]') !== pre || pre.firstChild !== firstChild) throw new Error('warm tab recreated pane');
  if (pre.textContent !== store.get('a').text) throw new Error('navigation did not project complete model');
}
SandmanPortalDiff.diffRuns(body, [run], { ...opts, expandedKey: null });
SandmanPortalDiff.diffRuns(body, [run], opts);
if (body.children[1].querySelector('pre[data-scroll-key]') !== pre) throw new Error('row reopen recreated pane');
if (pre.textContent !== store.get('a').text) throw new Error('row reopen sequence differs');
console.log('PASS');
`)
}

func TestPortalLogProjection_AsyncSnapshotCannotOverwriteNewReset(t *testing.T) {
	runNodeScript(t, `
const store = sandbox.SandmanPortalLog.createStore();
const epoch = store.begin('a');
helpers.logState = run => store.get(run.runId);
const records = [];
let end = 0;
for (let i = 0; i < 400; i++) {
  const text = 'old-' + i + ' ' + 'x'.repeat(100);
  records.push({ start: end, end: end + text.length + 1, text });
  end += text.length + 1;
}
store.accept('a', epoch, { type: 'snapshot', runId: 'a', generation: 'old', start: 0, end, records, cursor: 'old-end' });
const run = { key: 'a', runId: 'a', kind: 'active', status: 'running' };
const body = makeMockBody();
const opts = { helpers, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const pre = body.children[1].querySelector('pre[data-scroll-key]');
if (!pre.getAttribute('data-rendering-log')) throw new Error('large snapshot was not chunked');
store.accept('a', epoch, { type: 'reset', runId: 'a', generation: 'new', start: 0, end: 4, records: [{ start: 0, end: 4, text: 'new' }], cursor: 'new-end' });
SandmanPortalDiff.projectLogState(pre, store.get('a'), helpers);
setTimeout(() => {
  if (pre.textContent !== 'new\n') throw new Error('late old render corrupted new generation');
  if (pre.getAttribute('data-log-generation') !== 'new') throw new Error('wrong rendered generation');
  console.log('PASS');
}, 30);
`)
}

func TestPortalLogProjection_PausedChunkedRenderAndPrefixEvictionKeepLatestCompleteRange(t *testing.T) {
	runNodeScript(t, `
const chunks = [];
sandbox.setTimeout = fn => chunks.push(fn);
const store = sandbox.SandmanPortalLog.createStore();
const epoch = store.begin('a');
helpers.logState = run => store.get(run.runId);
let end = 0;
const oracleRecords = [];
function makeRecords(count) {
  const records = [];
  for (let i = 0; i < count; i++) {
    const text = 'record-' + oracleRecords.length + ' ' + 'x'.repeat(1024);
    const record = { start: end, end: end + text.length + 1, text };
    end = record.end;
    records.push(record);
    oracleRecords.push(record);
  }
  return records;
}
const first = makeRecords(200);
store.accept('a', epoch, { type: 'snapshot', runId: 'a', generation: 'g', start: 0, end, records: first, cursor: 'c' + end });
const body = makeMockBody();
const run = { key: 'a', runId: 'a', kind: 'active', status: 'running' };
const opts = { helpers, expandedKey: 'a', tabs: { a: 'log' } };
SandmanPortalDiff.diffRuns(body, [run], opts);
const pre = body.children[1].querySelector('pre[data-scroll-key]');
const start = end;
const burst = makeRecords(125);
store.accept('a', epoch, { type: 'append', runId: 'a', generation: 'g', start, end, records: burst, cursor: 'c' + end });
const accepted = store.get('a');
if (!accepted.truncated || accepted.start === 0) throw new Error('over-256KiB range was not explicitly bounded');
if (accepted.end - accepted.start > 256 * 1024) throw new Error('model retained too many raw bytes');
SandmanPortalDiff.projectLogState(pre, accepted, helpers);
while (chunks.length) chunks.shift()();
const expected = oracleRecords.filter(record => record.start >= accepted.start).map(record => record.text + '\n').join('');
if (pre.textContent !== expected || accepted.text !== expected) throw new Error('burst/chunk/retention sequence differs from source oracle');
if (pre.getAttribute('data-log-start') !== String(accepted.start) || pre.getAttribute('data-log-end') !== String(end)) throw new Error('retained DOM range differs from model');
SandmanPortalDiff.diffRuns(body, [run], { ...opts, tabs: { a: 'details' } });
SandmanPortalDiff.diffRuns(body, [run], opts);
if (body.children[1].querySelector('pre[data-scroll-key]') !== pre || pre.textContent !== expected) throw new Error('navigation after eviction lost retained records');
console.log('PASS');
`)
}

func TestPortalLogProjection_LargeContiguousAppendIsChunkedWithoutReplacingPrefix(t *testing.T) {
	runNodeScript(t, `
const chunks = [];
sandbox.setTimeout = fn => chunks.push(fn);
const store = sandbox.SandmanPortalLog.createStore();
const epoch = store.begin('a');
helpers.logState = run => store.get(run.runId);
store.accept('a', epoch, { type: 'snapshot', runId: 'a', generation: 'g', start: 0, end: 4, records: [{ start: 0, end: 4, text: 'one' }], cursor: 'c4' });
const body = makeMockBody();
const run = { key: 'a', runId: 'a', kind: 'active', status: 'running' };
SandmanPortalDiff.diffRuns(body, [run], { helpers, expandedKey: 'a', tabs: { a: 'log' } });
const pre = body.children[1].querySelector('pre[data-scroll-key]');
const firstChild = pre.firstChild;
const records = [];
let end = 4;
for (let i = 0; i < 125; i++) {
  const text = 'output-' + i + ' ' + 'x'.repeat(200);
  records.push({ start: end, end: end + text.length + 1, text });
  end += text.length + 1;
}

store.accept('a', epoch, { type: 'append', runId: 'a', generation: 'g', start: 4, end, records, cursor: 'c' + end });
SandmanPortalDiff.projectLogState(pre, store.get('a'), helpers);
if (!pre.getAttribute('data-rendering-log') || !chunks.length) throw new Error('large append did not yield chunked highlighting');
if (pre.firstChild !== firstChild) throw new Error('prefix disappeared while append chunking');
while (chunks.length) chunks.shift()();
if (pre.firstChild !== firstChild || pre.textContent !== store.get('a').text) throw new Error('chunked append changed prefix or lost records');
console.log('PASS');
`)
}

func TestPortalLogProjection_VeryLongSingleRecordYieldsByBytes(t *testing.T) {
	runNodeScript(t, `
const chunks = [];
sandbox.setTimeout = fn => { chunks.push(fn); return chunks.length; };
const store = sandbox.SandmanPortalLog.createStore();
const epoch = store.begin('long');
const text = 'x'.repeat(200 * 1024);
store.accept('long', epoch, { type: 'snapshot', runId: 'long', generation: 'g', start: 0, end: text.length + 1,
  records: [{ start: 0, end: text.length + 1, text }], cursor: 'long-end' });
helpers.logState = run => store.get(run.runId);
const run = { key: 'long', runId: 'long', kind: 'active', status: 'running' };
const body = makeMockBody();
SandmanPortalDiff.diffRuns(body, [run], { helpers, expandedKey: run.key, tabs: { long: 'log' } });
const pre = body.children[1].querySelector('pre[data-scroll-key]');
if (!pre.getAttribute('data-rendering-log')) throw new Error('large single record did not enter async rendering');
let chunksRun = 0;
while (chunks.length) {
  chunks.shift()();
  chunksRun++;
}
if (chunksRun < 10) throw new Error('large source record was rendered as one blocking task: ' + chunksRun);
if (pre.textContent !== store.get('long').text) throw new Error('byte-chunked record changed content');
if (pre.getAttribute('data-log-end') !== String(store.get('long').end)) throw new Error('byte-chunked DOM range was not committed');
console.log('PASS');
`)
}
