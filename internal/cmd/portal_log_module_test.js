const assert = require('node:assert/strict');
const { test } = require('node:test');
require('./portal_log.js');
const { createStore, createSubscription, createProjection } = globalThis.SandmanPortalLog;

function frame(type, start, texts, options = {}) {
  let end = start;
  const records = texts.map(text => {
    const record = { start: end, end: end + Buffer.byteLength(text) + 1, text };
    end = record.end;
    return record;
  });
  return { type, runId: 'a', generation: 'g', start, end, records, cursor: 'opaque:' + end, ...options };
}

test('positions acknowledge before rendering and preserve repeated and blank records', () => {
  const store = createStore();
  const epoch = store.begin('a');
  const snapshot = frame('snapshot', 40, ['same', '', 'same']);
  assert.equal(store.accept('a', epoch, snapshot).accepted, true);
  const append = frame('append', snapshot.end, ['same', '00:00:00 midnight']);
  store.accept('a', epoch, append);
  assert.equal(store.get('a').cursor, append.cursor);
  assert.deepEqual(store.get('a').records.map(r => r.text), ['same', '', 'same', 'same', '00:00:00 midnight']);
  const before = store.get('a');
  assert.equal(store.accept('a', epoch, append).changed, false);
  assert.deepEqual(store.get('a'), before);
});

test('gaps and conflicting overlap reject the entire frame without advancing the cursor', () => {
  const store = createStore();
  const epoch = store.begin('a');
  const snapshot = frame('snapshot', 40, ['one', 'two']);
  store.accept('a', epoch, snapshot);
  const before = store.get('a');
  const badFrames = [
    frame('append', snapshot.end + 1, ['gap']),
    frame('append', 40, ['bad', 'two']),
    frame('append', 41, ['misaligned']),
    frame('append', snapshot.end, ['valid'], { runId: 'b' }),
    frame('append', snapshot.end, ['valid'], { end: snapshot.end + 500 }),
  ];
  for (const bad of badFrames) {
    assert.equal(store.accept('a', epoch, bad).reconnect, true);
    assert.deepEqual(store.get('a'), before);
  }
  // A retransmitted prefix may overlap retained records and extend the tail.
  const overlap = frame('append', 40, ['one', 'two', 'three']);
  assert.equal(store.accept('a', epoch, overlap).accepted, true);
  assert.deepEqual(store.get('a').records.map(r => r.text), ['one', 'two', 'three']);
});

test('old connections, stale snapshots and retired opaque generations cannot rewind a run', () => {
  const store = createStore();
  const oldEpoch = store.begin('a');
  const snapshot = frame('snapshot', 40, ['one'], { generation: 'z' });
  store.accept('a', oldEpoch, snapshot);
  const epoch = store.begin('a');
  const append = frame('append', snapshot.end, ['two'], { generation: 'z' });
  store.accept('a', epoch, append);
  const before = store.get('a');
  assert.equal(store.accept('a', oldEpoch, frame('reset', 0, ['old'])).accepted, false);
  assert.equal(store.accept('a', epoch, snapshot).reconnect, true);
  assert.deepEqual(store.get('a'), before);
  // Generation tokens have no lexical ordering; reset to "a" is valid.
  const reset = frame('reset', 0, ['new source'], { generation: 'a' });
  assert.equal(store.accept('a', epoch, reset).accepted, true);
  assert.deepEqual(store.get('a').records.map(r => r.text), ['new source']);
  assert.equal(store.accept('a', epoch, frame('reset', 0, ['old source'], { generation: 'z' })).reconnect, true);
  store.disconnect('a', epoch);
  assert.equal(store.accept('a', epoch, frame('append', reset.end, ['late'], { generation: 'a' })).accepted, false);
});

test('pending and unavailable are explicit and preserve the accepted range; end closes once', () => {
  const store = createStore();
  const epoch = store.begin('a');
  const pending = { type: 'pending', runId: 'a', generation: '', start: 0, end: 0, records: [], cursor: '', reason: 'not-created' };
  assert.equal(store.accept('a', epoch, pending).accepted, true);
  assert.equal(store.get('a').status, 'pending');
  const snapshot = frame('snapshot', 10, ['first']);
  store.accept('a', epoch, snapshot);
  store.accept('a', epoch, pending);
  assert.equal(store.get('a').cursor, snapshot.cursor);
  assert.equal(store.get('a').end, snapshot.end);
  store.accept('a', epoch, { ...pending, type: 'unavailable', reason: 'artifact-missing' });
  assert.equal(store.get('a').status, 'unavailable');
  assert.equal(store.get('a').reason, 'artifact-missing');
  const end = { ...snapshot, type: 'end', start: snapshot.end, records: [] };
  assert.equal(store.accept('a', epoch, end).end, true);
  assert.equal(store.get('a').status, 'ended');
  assert.equal(store.accept('a', epoch, frame('append', snapshot.end, ['late'])).accepted, false);
});

test('an empty contiguous resume clears pending without changing accepted records or cursor', () => {
  for (const texts of [[], ['one']]) {
    const store = createStore();
    const epoch = store.begin('a');
    const snapshot = frame('snapshot', 0, texts);
    store.accept('a', epoch, snapshot);
    store.accept('a', epoch, { type: 'pending', runId: 'a', records: [], cursor: '', reason: 'missing' });
    store.accept('a', epoch, { ...snapshot, type: 'append', start: snapshot.end, records: [] });
    assert.equal(store.get('a').status, 'available');
    assert.equal(store.get('a').cursor, snapshot.cursor);
    assert.deepEqual(store.get('a').records, snapshot.records);
  }
});

test('retention removes only a complete prefix and permits one oversized record; total cache is LRU bounded', () => {
  const store = createStore({ maxRunBytes: 12, maxRuns: 2, maxTotalBytes: 4096 });
  const epoch = store.begin('a');
  const snapshot = frame('snapshot', 0, ['aaaa', 'bbbb', 'cccc']);
  store.accept('a', epoch, snapshot);
  assert.equal(store.get('a').start, 5);
  assert.equal(store.get('a').end, 15);
  assert.equal(store.get('a').truncated, true);
  assert.deepEqual(store.get('a').records.map(r => r.text), ['bbbb', 'cccc']);
  const oversize = frame('append', snapshot.end, ['x'.repeat(100)]);
  store.accept('a', epoch, oversize);
  assert.equal(store.get('a').records.length, 1);
  assert.equal(store.get('a').start, oversize.start);
  assert.equal(store.get('a').cursor, oversize.cursor);
  store.accept('a', epoch, frame('append', oversize.end, ['small']));
  assert.deepEqual(store.get('a').records.map(r => r.text), ['small']);
  store.begin('b');
  store.get('a'); // Touch the authoritative state, independently of pane reuse.
  store.begin('c');
  assert.equal(store.get('b'), null);
  assert.notEqual(store.get('a'), null);
  assert.equal(store.accept('b', epoch, snapshot).accepted, false);
  const bounded = createStore({ maxTotalBytes: 900 });
  for (let i = 0; i < 10; i++) {
    const runId = 'run' + i;
    bounded.accept(runId, bounded.begin(runId), frame('snapshot', 0, ['output'], { runId }));
  }
  assert.ok(bounded.stats().cacheBytes <= 900);
  assert.equal(bounded.get('run0'), null);
});

test('100+ frames accepted with animation frames paused project the complete latest range once', () => {
  const store = createStore();
  const scheduled = [];
  const rendered = [];
  const projection = createProjection({ store, schedule: fn => scheduled.push(fn), project: (runId, state) => rendered.push(state) });
  const sources = [];
  const subscription = createSubscription({ store, openSource: url => {
    const source = { url, close() { this.closed = true; } };
    sources.push(source);
    return source;
  }, onChange: runId => projection.schedule(runId) });
  subscription.select({ runId: 'a', key: 'backend-a' });
  let position = 0;
  const expected = [];
  for (let i = 0; i < 125; i++) {
    const text = i % 10 === 0 ? '' : 'record-' + i;
    expected.push(text);
    const next = frame(i === 0 ? 'snapshot' : 'append', position, [text]);
    sources[0].onmessage({ data: JSON.stringify(next), lastEventId: 'browser-cursor' });
    position = next.end;
  }
  assert.equal(scheduled.length, 1);
  assert.equal(rendered.length, 0);
  assert.equal(store.get('a').cursor, 'opaque:' + position);
  assert.deepEqual(store.get('a').records.map(r => r.text), expected);
  scheduled.shift()();
  assert.deepEqual(rendered[0].records.map(r => r.text), expected);
  assert.equal(rendered[0].end, position);
});

test('rejection closes the browser subscription and reconnects from accepted opaque cursor; obsolete callbacks cannot revive it', () => {
  const store = createStore();
  const sources = [];
  const timers = [];
  const subscription = createSubscription({ store, openSource: url => {
    const source = { url, readyState: 1, close() { this.closed = true; this.readyState = 2; } };
    sources.push(source);
    return source;
  }, setTimer: (fn, delay) => { const timer = { fn, delay }; timers.push(timer); return timer; }, clearTimer: timer => { timer.cancelled = true; } });
  const send = (source, payload) => source.onmessage({ data: JSON.stringify(payload), lastEventId: 'unsafe browser ID' });
  subscription.select({ runId: 'a', key: 'a' });
  const snapshot = frame('snapshot', 0, ['one'], { cursor: 'opaque /+? token' });
  send(sources[0], snapshot);
  sources[0].readyState = 0;
  sources[0].onerror();
  assert.equal(timers.length, 0); // Browser handles native reconnect with Last-Event-ID.
  send(sources[0], frame('append', snapshot.end + 1, ['gap']));
  assert.equal(sources[0].closed, true);
  assert.equal(store.get('a').cursor, snapshot.cursor);
  subscription.select({ runId: 'a', key: 'a' });
  assert.equal(sources.length, 1); // Render/poll cannot bypass the retry timer.
  timers.shift().fn();
  assert.equal(new URL(sources[1].url, 'http://localhost').searchParams.get('cursor'), snapshot.cursor);
  send(sources[0], frame('reset', 0, ['obsolete']));
  assert.deepEqual(store.get('a').records.map(r => r.text), ['one']);
  sources[1].readyState = 2;
  sources[1].onerror();
  assert.equal(timers.length, 1);
  subscription.select(null);
  timers.shift().fn();
  assert.equal(sources.length, 2);
  subscription.select({ runId: 'a', key: 'a' });
  send(sources[2], { ...snapshot, type: 'end', start: snapshot.end, records: [] });
  assert.equal(sources[2].closed, true);
  subscription.select({ runId: 'a', key: 'a' });
  assert.equal(sources.length, 3); // End is a lifecycle boundary, not a retry.
});

test('a reset is one authoritative replacement and its identical retransmission is a no-op', () => {
  const store = createStore();
  const epoch = store.begin('a');
  store.accept('a', epoch, frame('snapshot', 0, ['old']));
  const reset = frame('reset', 100, ['current'], { generation: 'new' });
  store.accept('a', epoch, reset);
  const before = store.get('a');
  assert.equal(store.accept('a', epoch, reset).changed, false);
  assert.deepEqual(store.get('a'), before);
});

test('missing-generation unavailable and end retain the prior range and close without reconnect', () => {
  const store = createStore();
  const sources = [];
  const subscription = createSubscription({ store, openSource: url => {
    const source = { url, close() { this.closed = true; } };
    sources.push(source);
    return source;
  } });
  const run = { runId: 'a', key: 'a' };
  subscription.select(run);
  sources[0].onmessage({ data: JSON.stringify(frame('snapshot', 100, ['retained'])) });
  subscription.select(null);
  subscription.select(run);
  const missing = { runId: 'a', generation: '', start: 0, end: 0, records: [], cursor: '', reason: 'source-missing' };
  sources[1].onmessage({ data: JSON.stringify({ ...missing, type: 'unavailable' }) });
  const accepted = store.get('a');
  sources[1].onmessage({ data: JSON.stringify({ ...missing, type: 'end' }) });
  assert.equal(sources[1].closed, true);
  assert.equal(store.get('a').status, 'ended');
  assert.equal(store.get('a').cursor, accepted.cursor);
  assert.deepEqual(store.get('a').records, accepted.records);
  subscription.select(run);
  assert.equal(sources.length, 2);
});

test('fixed-seed mixed subscription transitions preserve independent RunID ranges after paused projections', () => {
  const store = createStore();
  const sources = [];
  const callbacks = [];
  let selected = 'a';
  const visible = new Map();
  const oracle = new Map([['a', []], ['b', []]]);
  const projection = createProjection({ store, schedule: fn => callbacks.push(fn), project: (runId, state) => {
    if (runId === selected) visible.set(runId, state.records.map(r => r.text));
  } });
  const subscription = createSubscription({ store, openSource: url => {
    const source = { url, readyState: 1, close() { this.closed = true; } };
    sources.push(source);
    return source;
  }, onChange: runId => projection.schedule(runId) });
  let seed = 2772;
  function random(n) { seed = (1664525 * seed + 1013904223) >>> 0; return seed % n; }
  function select(runId) {
    selected = runId;
    subscription.select(runId ? { runId, key: runId } : null);
    if (runId) {
      if (!store.get(runId).generation) {
        const source = sources.at(-1);
        source.onmessage({ data: JSON.stringify(frame('snapshot', 0, oracle.get(runId), { runId })) });
      }
      projection.schedule(runId);
    }
  }
  select('a');
  for (let i = 0; i < 75; i++) {
    const action = random(5);
    if (action === 0) { select(null); select(random(2) ? 'a' : 'b'); }
    else if (action === 1) select(selected === 'a' ? 'b' : 'a');
    else if (action === 2) { const current = selected; select(null); select(current); }
    else {
      const text = random(3) ? 'same-second repeated output' : '';
      const next = frame('append', store.get(selected).end, [text], { runId: selected });
      oracle.get(selected).push(text);
      const source = sources.at(-1);
      source.onmessage({ data: JSON.stringify(next) });
      source.onmessage({ data: JSON.stringify(next) });
    }
    // Stale connections must not mutate an old or newly selected run.
    if (sources.length > 1) sources[0].onmessage({ data: JSON.stringify(frame('reset', 0, ['obsolete'])) });
    while (callbacks.length) callbacks.shift()();
    assert.deepEqual(visible.get(selected), oracle.get(selected));
    for (const [runId, records] of oracle) {
      const state = store.get(runId);
      if (state) assert.deepEqual(state.records.map(r => r.text), records);
    }
  }
});

test('permanently closed streams use bounded backoff that polls cannot bypass', () => {
  const store = createStore();
  const sources = [];
  const timers = [];
  const subscription = createSubscription({ store, openSource: url => {
    const source = { url, readyState: 1, close() { this.closed = true; } };
    sources.push(source);
    return source;
  }, setTimer: (fn, delay) => { const timer = { fn, delay }; timers.push(timer); return timer; }, clearTimer: timer => { timer.cancelled = true; } });
  const run = { runId: 'a', key: 'a' };
  subscription.select(run);
  for (let i = 0; i < 8; i++) {
    sources.at(-1).readyState = 2;
    sources.at(-1).onerror();
    for (let poll = 0; poll < 100; poll++) subscription.select({ ...run });
    assert.equal(sources.length, i + 1);
    const timer = timers.shift();
    assert.equal(timer.delay, Math.min(15000, 500 * 2 ** i));
    timer.fn();
  }
  subscription.stop();
  assert.equal(subscription.inspect(), null);
});
