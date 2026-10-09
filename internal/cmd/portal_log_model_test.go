package cmd

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPortalLogModel_UsesRecordPositionsNotDisplayedText(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal log model test")
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	logPath := filepath.Join(filepath.Dir(currentFile), "portal_log.js")
	js := `
const logModuleSource = fs.readFileSync(` + "`" + logPath + "`" + `, 'utf8');
vm.runInThisContext(logModuleSource, { filename: 'portal_log.js' });
const model = SandmanPortalLog.create();
const generation = 'file-generation-1';
const snapshot = {
  runId: 'run-1', generation, start: 0, end: 4,
  cursor: { runId: 'run-1', generation, offset: 4 },
  records: [{ runId: 'run-1', generation, start: 0, end: 4, text: 'same' }]
};
if (!model.accept('run-1', snapshot, 'snapshot', 1)) throw new Error('snapshot was not accepted');
const duplicateText = {
  runId: 'run-1', generation, start: 4, end: 9,
  cursor: { runId: 'run-1', generation, offset: 9 },
  records: [{ runId: 'run-1', generation, start: 4, end: 9, text: 'same' }]
};
if (!model.accept('run-1', duplicateText, 'append', 1)) throw new Error('distinct repeated record was not accepted');
if (model.accept('run-1', duplicateText, 'append', 1)) throw new Error('duplicate identity changed the model');
const gap = {
  runId: 'run-1', generation, start: 12, end: 16,
  cursor: { runId: 'run-1', generation, offset: 16 },
  records: [{ runId: 'run-1', generation, start: 12, end: 16, text: 'gap' }]
};
if (model.accept('run-1', gap, 'append', 1)) throw new Error('gap was silently accepted');
const malformed = {
  runId: 'run-1', generation, start: 9, end: 12,
  cursor: { runId: 'run-1', generation, offset: 11 },
  records: [{ runId: 'run-1', generation, start: 9, end: 12, text: 'bad cursor' }]
};
if (model.accept('run-1', malformed, 'append', 1)) throw new Error('incoherent cursor was silently accepted');
const disconnectedSnapshot = {
  runId: 'run-1', generation, start: 10, end: 12,
  cursor: { runId: 'run-1', generation, offset: 12 },
  records: [{ runId: 'run-1', generation, start: 10, end: 12, text: 'disconnected' }]
};
if (model.accept('run-1', disconnectedSnapshot, 'snapshot', 1)) throw new Error('disconnected snapshot replaced accepted range');
const wrongRun = {
  runId: 'other-run', generation, start: 9, end: 12,
  cursor: { runId: 'other-run', generation, offset: 12 },
  records: [{ runId: 'other-run', generation, start: 9, end: 12, text: 'wrong run' }]
};
if (model.accept('run-1', wrongRun, 'append', 1)) throw new Error('wrong-run record was silently accepted');
if (model.text('run-1') !== 'same\nsame\n') throw new Error('record multiplicity/order was lost: ' + JSON.stringify(model.text('run-1')));
if (!model.accept('run-1', { runId: 'run-1' }, 'pending', 1)) throw new Error('pending source state was not accepted');
if (model.text('run-1') !== 'same\nsame\n') throw new Error('pending state discarded retained records');
if (!model.accept('run-1', { runId: 'run-1' }, 'unavailable', 1)) throw new Error('unavailable source state was not accepted');
if (model.text('run-1') !== 'same\nsame\n') throw new Error('unavailable observation discarded accepted records');

const largeRecords = [];
for (let i = 0; i < 4097; i++) {
  largeRecords.push({ runId: 'large-run', generation, start: i, end: i + 1, text: 'line-' + i });
}
const large = {
  runId: 'large-run', generation, start: 0, end: 4097,
  cursor: { runId: 'large-run', generation, offset: 4097 }, records: largeRecords,
};
if (!model.accept('large-run', large, 'snapshot', 1)) throw new Error('large snapshot was not accepted');
if (model.get('large-run').records.length !== 4096) throw new Error('retention bound was not enforced');
if (model.text('large-run').indexOf('line-0') !== -1) throw new Error('retention silently retained the oldest record');
if (model.text('large-run').indexOf('line-4096') === -1) throw new Error('retention dropped the newest record');
const range = model.range('large-run');
if (!range.truncated || range.start !== 1 || range.end !== 4097) throw new Error('retention boundary was not explicit: ' + JSON.stringify(range));
if (range.retainedBytes !== 4096) throw new Error('retained byte count was not tracked: ' + JSON.stringify(range));
for (let i = 0; i < 64; i++) model.get('evicted-' + i);
if (model.get('run-1').cursor) throw new Error('bounded model map retained an evicted run');
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortalLogModel_FixedSeedTransitionStress exercises the production model
// through the same source-oracle transitions used by tab, subject, and
// reconnect navigation. The oracle deliberately includes repeated text and
// blank records; only raw record positions establish identity.
func TestPortalLogModel_FixedSeedTransitionStress(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	logPath := filepath.Join(filepath.Dir(currentFile), "portal_log.js")
	js := `
const logModuleSource = fs.readFileSync(` + "`" + logPath + "`" + `, 'utf8');
vm.runInThisContext(logModuleSource, { filename: 'portal_log.js' });
const model = SandmanPortalLog.create();
const generation = 'stress-generation';
const runs = {
  'impl-run': { offset: 0, records: [], epoch: 0 },
  'review-run': { offset: 0, records: [], epoch: 0 },
};
function nextRandom() {
  nextRandom.seed = (nextRandom.seed * 1664525 + 1013904223) >>> 0;
  return nextRandom.seed;
}
nextRandom.seed = 2772;
function batch(runID, records, start, end) {
  return {
    runId: runID, generation, start, end,
    cursor: { runId: runID, generation, offset: end }, records,
  };
}
function sourceSnapshot(runID) {
  const source = runs[runID];
  return batch(runID, source.records, 0, source.offset);
}
function sourceAppend(runID, record) {
  const source = runs[runID];
  const start = source.offset;
  const end = start + record.text.length + 1;
  const value = { runId: runID, generation, start, end, text: record.text };
  source.records.push(value);
  source.offset = end;
  return batch(runID, [value], start, end);
}
for (const runID of Object.keys(runs)) {
  const source = runs[runID];
  for (const text of ['same', '', 'same']) sourceAppend(runID, { text });
  if (!model.accept(runID, sourceSnapshot(runID), 'snapshot', 1)) throw new Error('initial snapshot rejected for ' + runID);
  source.epoch = 1;
}
for (let transition = 0; transition < 50; transition++) {
  const runID = (nextRandom() & 1) === 0 ? 'impl-run' : 'review-run';
  const source = runs[runID];
  source.epoch++;
  model.setEpoch(runID, source.epoch);
  const action = nextRandom() % 4;
  if (action === 0) {
    const append = sourceAppend(runID, { text: (transition % 3 === 0) ? 'same' : 'live-' + transition });
    if (!model.accept(runID, append, 'append', source.epoch)) throw new Error('append rejected at transition ' + transition);
  } else if (action === 1) {
    if (!model.accept(runID, sourceSnapshot(runID), 'snapshot', source.epoch)) throw new Error('snapshot rejected at transition ' + transition);
  } else if (action === 2) {
    const last = source.records[source.records.length - 1];
    const duplicate = batch(runID, [last], last.start, last.end);
    if (model.accept(runID, duplicate, 'append', source.epoch)) throw new Error('duplicate identity accepted at transition ' + transition);
  } else {
    const stale = sourceAppend(runID, { text: 'stale-' + transition });
    if (model.accept(runID, stale, 'append', source.epoch - 1)) throw new Error('stale epoch accepted at transition ' + transition);
    source.records.pop();
    source.offset = stale.start;
  }
  for (const checkID of Object.keys(runs)) {
    const expected = runs[checkID].records.map(function (record) { return record.text; }).join('\n') + '\n';
    if (model.text(checkID) !== expected) throw new Error('oracle mismatch after transition ' + transition + ' for ' + checkID);
    const range = model.range(checkID);
    if (range.end !== runs[checkID].offset) throw new Error('cursor mismatch after transition ' + transition + ' for ' + checkID);
  }
}
console.log('PASS');
`
	runNodeScript(t, js)
}
