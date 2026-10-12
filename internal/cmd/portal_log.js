(function (global) {
  // Transport acknowledgement belongs here, before any scheduled view work.
  // Offsets are raw source bytes; cleaned text is never an identity key.
  function createStore(options) {
    const opts = options || {};
    const maxRunBytes = opts.maxRunBytes || 256 * 1024;
    const maxTotalBytes = opts.maxTotalBytes || 8 * 1024 * 1024;
    const maxRuns = opts.maxRuns || 16;
    const runs = new Map();
    let nextEpoch = 0;
    function touch(runId, state) {
      runs.delete(runId);
      runs.set(runId, state);
    }
    function cost(state) {
      return 256 + 2 * (state.runId.length + state.generation.length + state.cursor.length
        + Array.from(state.retired).join('').length) + state.records.reduce((bytes, r) => bytes + 64 + Math.max(r.end - r.start, r.text.length * 2), 0);
    }
    function stats() {
      let cacheBytes = 0;
      for (const state of runs.values()) cacheBytes += cost(state);
      return { runs: runs.size, cacheBytes };
    }
    function bound(state) {
      let prefix = 0;
      while (state.records.length - prefix > 1 && state.end - state.records[prefix].start > maxRunBytes) prefix += 1;
      if (prefix) state.records = state.records.slice(prefix);
      state.start = state.records.length ? state.records[0].start : state.end;
      state.truncated = state.start > 0;
      touch(state.runId, state);
      while (runs.size > 1 && (runs.size > maxRuns || stats().cacheBytes > maxTotalBytes)) {
        runs.delete(runs.keys().next().value);
      }
      // The active range also fits the global budget at record boundaries.
      // A single oversized record is the only data exception.
      let bytes = cost(state);
      prefix = 0;
      while (state.records.length - prefix > 1 && bytes > maxTotalBytes) {
        const record = state.records[prefix++];
        bytes -= 64 + Math.max(record.end - record.start, record.text.length * 2);
      }
      if (prefix) {
        state.records = state.records.slice(prefix);
        state.start = state.records[0].start;
        state.truncated = true;
      }
      // Retired identities are also cache data. Evicting the whole model is
      // the explicit recovery boundary when metadata itself exhausts budget.
      if (state.retired.size > 128 || (bytes > maxTotalBytes && (!state.records.length
        || state.records[0].end - state.records[0].start <= maxRunBytes))) {
        runs.delete(state.runId);
        return false;
      }
      return true;
    }
    function begin(runId) {
      let state = runs.get(runId);
      if (!state) {
        state = { runId, generation: '', cursor: '', start: 0, end: 0, records: [], revision: 0, incarnation: 0, retired: new Set(), status: 'pending', reason: '', truncated: false };
        runs.set(runId, state);
      }
      state.epoch = ++nextEpoch;
      bound(state);
      return state.epoch;
    }
    function get(runId) {
      const state = runs.get(runId);
      if (!state) return null;
      touch(runId, state);
      const { retired, ...view } = state;
      return Object.freeze({ ...view, records: Object.freeze(state.records.slice()), text: state.records.map(r => r.text + '\n').join('') });
    }
    function disconnect(runId, epoch) {
      const state = runs.get(runId);
      if (state && state.epoch === epoch) state.epoch = 0;
    }
    function accept(runId, epoch, frame) {
      const state = runs.get(runId);
      if (!state || !epoch || state.epoch !== epoch || state.status === 'ended') return { accepted: false, changed: false };
      const reject = reason => ({ accepted: false, changed: false, reconnect: true, reason });
      const unchanged = () => {
        const changed = state.status !== 'available';
        if (changed) {
          state.status = 'available';
          state.unavailable = false;
          state.reason = '';
          state.revision += 1;
        }
        return { accepted: true, changed };
      };
      if (frame && frame.runId === runId && (frame.type === 'pending' || frame.type === 'unavailable' || frame.type === 'end')) {
        if (!Array.isArray(frame.records) || frame.records.length) return reject('invalid-control-frame');
        if (frame.type === 'end' && frame.generation && state.generation
          && (frame.generation !== state.generation || frame.end !== state.end || frame.cursor !== state.cursor)) return reject('end-position');
        if (frame.type !== 'end') state.unavailable = frame.type === 'unavailable';
        state.status = frame.type === 'end' ? 'ended' : frame.type;
        state.reason = frame.reason || (frame.type === 'end' ? state.reason : '');
        state.revision += 1;
        return { accepted: true, changed: true, end: frame.type === 'end' };
      }
      if (!frame || frame.runId !== runId || typeof frame.generation !== 'string' || !frame.generation
        || typeof frame.cursor !== 'string' || !frame.cursor
        || !Number.isSafeInteger(frame.start) || !Number.isSafeInteger(frame.end)
        || frame.start < 0 || frame.end < frame.start || !Array.isArray(frame.records)) return reject('invalid-frame');
      let end = frame.start;
      for (const record of frame.records) {
        if (!record || record.start !== end || !Number.isSafeInteger(record.end)
          || record.end <= record.start || typeof record.text !== 'string') return reject('invalid-record-range');
        end = record.end;
      }
      if (end !== frame.end) return reject('invalid-frame-range');
      if (state.retired.has(frame.generation)) return reject('retired-generation');
      if (frame.type === 'reset' && frame.generation === state.generation && frame.start === state.start && frame.end === state.end
        && frame.records.length === state.records.length && frame.records.every((r, i) => r.start === state.records[i].start
          && r.end === state.records[i].end && r.text === state.records[i].text)) return unchanged();
      if (frame.type === 'snapshot' && frame.generation === state.generation) {
        if (frame.end < state.end || frame.start < state.start) return reject('stale-snapshot');
        const retained = new Map(state.records.map(record => [record.start, record]));
        for (const record of frame.records) {
          if (record.start >= state.end) continue;
          const previous = retained.get(record.start);
          if (!previous || previous.end !== record.end || previous.text !== record.text) return reject('conflicting-snapshot');
        }
        if (frame.start === state.start && frame.end === state.end) return unchanged();
      }
      let records = frame.records;
      if (frame.type === 'append') {
        if (frame.generation === state.generation && frame.end <= state.start && frame.end < state.end) return { accepted: true, changed: false };
        if (frame.generation !== state.generation || frame.start > state.end || frame.start < state.start) return reject('gap');
        const retained = new Map(state.records.map(record => [record.start, record]));
        for (const record of records) {
          if (record.start >= state.end) continue;
          const previous = retained.get(record.start);
          if (!previous || previous.end !== record.end || previous.text !== record.text) return reject('conflicting-overlap');
        }
        records = records.filter(record => record.start >= state.end);
        if (!records.length) return unchanged();
        if (records[0].start !== state.end) return reject('gap');
      } else if (frame.type !== 'snapshot' && frame.type !== 'reset') {
        return reject('invalid-type');
      }
      if (frame.type === 'snapshot' || frame.type === 'reset') {
        if (state.generation && frame.generation !== state.generation) state.retired.add(state.generation);
        state.incarnation += 1;
        state.records = records.map(record => Object.freeze({ ...record }));
        state.start = frame.start;
      } else {
        state.records = state.records.concat(records.map(record => Object.freeze({ ...record })));
      }
      state.end = frame.end;
      state.generation = frame.generation;
      state.cursor = frame.cursor;
      state.status = 'available';
      state.unavailable = false;
      state.reason = frame.reason || '';
      state.revision += 1;
      if (!bound(state)) return { accepted: true, changed: true, reconnect: true, reason: 'model-cache-evicted' };
      return { accepted: true, changed: true };
    }
    return { begin, disconnect, get, accept, stats };
  }
  function createProjection(options) {
    const dirty = new Set();
    let scheduled = false;
    function flush() {
      scheduled = false;
      const runIds = Array.from(dirty);
      dirty.clear();
      for (const runId of runIds) options.project(runId, options.store.get(runId));
    }
    function schedule(runId) {
      dirty.add(runId);
      if (scheduled) return;
      scheduled = true;
      options.schedule(flush);
    }
    return { schedule, flush };
  }

  // One subscription lifecycle is shared by tabs, subjects, rows and recovery.
  // Native reconnect retains the epoch. Rejected frames force a fresh source
  // using the model cursor, never the browser's independently advanced SSE ID.
  function createSubscription(options) {
    const store = options.store;
    const setTimer = options.setTimer || global.setTimeout.bind(global);
    const clearTimer = options.clearTimer || global.clearTimeout.bind(global);
    let selected = null;
    let connection = null;
    let retryTimer = null;
    let attempts = 0;
    function closeConnection() {
      if (!connection) return;
      const old = connection;
      connection = null;
      store.disconnect(old.runId, old.epoch);
      old.source.close();
    }
    function stop() {
      selected = null;
      if (retryTimer) clearTimer(retryTimer);
      retryTimer = null;
      attempts = 0;
      closeConnection();
    }
    function recover(current) {
      if (connection !== current) return;
      closeConnection();
      const wanted = selected;
      const delay = Math.min(15000, 500 * Math.pow(2, attempts++));
      retryTimer = setTimer(() => {
        retryTimer = null;
        if (selected === wanted) select(wanted);
      }, delay);
    }
    function select(run) {
      const runId = run && run.runId;
      if (connection && connection.runId === runId) return;
      if (selected && selected.runId === runId && retryTimer) return;
      if (!selected || selected.runId !== runId) stop();
      selected = run || null;
      if (!runId) return;
      if (store.get(runId)?.status === 'ended') return;
      const epoch = store.begin(runId);
      const state = store.get(runId);
      const url = (options.path || '/api/runs/stream') + '?protocol=frames&runKey=' + encodeURIComponent(run.backendKey || run.key || runId)
        + (state.cursor ? '&cursor=' + encodeURIComponent(state.cursor) : '');
      const source = options.openSource(url);
      const current = { runId, epoch, source };
      connection = current;
      source.onmessage = function (event) {
        if (connection !== current) return;
        let result;
        try {
          result = store.accept(runId, epoch, JSON.parse(event.data));
        } catch (err) {
          result = { reconnect: true };
        }
        if (result.accepted) attempts = 0;
        if (result.changed && options.onChange) options.onChange(runId);
        if (result.end) closeConnection();
        else if (result.reconnect) recover(current);
      };
      source.onerror = function () {
        if (source.readyState === 2) recover(current);
      };
    }
    return { select, stop, inspect: () => connection && { runId: connection.runId, epoch: connection.epoch, url: connection.source.url } };
  }
  global.SandmanPortalLog = { createStore, createProjection, createSubscription };
})(typeof window !== 'undefined' ? window : globalThis);
