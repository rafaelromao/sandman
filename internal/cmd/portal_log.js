(function (global) {
  // The rendered pane is disposable. This model is the continuity owner and
  // survives tab, subject, row, and connection changes.
  const MAX_RETAINED_RECORDS = 4096;
  const MAX_RETAINED_BYTES = 1024 * 1024;
  const MAX_RETAINED_RUNS = 64;

  function create() {
    const byRun = new Map();

    function state(runID) {
      const key = String(runID || '');
      if (!byRun.has(key)) {
        if (byRun.size >= MAX_RETAINED_RUNS) {
          const oldest = byRun.keys().next().value;
          if (oldest !== undefined) byRun.delete(oldest);
        }
        byRun.set(key, {
          runID: key,
          generation: '',
          cursor: null,
          rangeStart: 0,
          rangeEnd: 0,
          retainedBytes: 0,
          truncated: false,
          records: [],
          identities: new Set(),
          epoch: 0,
          seeded: false,
          sourceState: 'unknown',
        });
      }
      return byRun.get(key);
    }

    function seed(runID, text) {
      const model = state(runID);
      if (model.cursor || model.seeded) return model;
      model.seeded = true;
      model.legacyText = String(text == null ? '' : text);
      return model;
    }

    function accept(runID, payload, kind, epoch) {
      const model = state(runID);
      if (epoch != null && epoch < model.epoch) return false;
      if (epoch != null) model.epoch = epoch;
      if (!payload || String(payload.runId || runID) !== model.runID) return false;
      if (kind === 'pending') {
        model.sourceState = 'pending';
        return true;
      }
      if (kind === 'unavailable') {
        model.sourceState = 'unavailable';
        return true;
      }
      const generation = String(payload.generation || '');
      if (!generation) return false;
      if (kind === 'reset') {
        model.generation = generation;
        model.cursor = null;
        model.rangeStart = 0;
        model.rangeEnd = 0;
        model.retainedBytes = 0;
        model.truncated = true;
        model.records = [];
        model.identities.clear();
        model.legacyText = '';
        model.sourceState = 'reset';
        return true;
      }
      const incomingStart = Number(payload.start);
      const incomingEnd = Number(payload.end);
      const cursor = payload.cursor;
      if (!Number.isSafeInteger(incomingStart) || !Number.isSafeInteger(incomingEnd) || incomingStart < 0 || incomingEnd < incomingStart) return false;
      if (!cursor || String(cursor.runId || '') !== model.runID || String(cursor.generation || '') !== generation || Number(cursor.offset) !== incomingEnd) return false;
      let expected = incomingStart;
      const records = Array.isArray(payload.records) ? payload.records : [];
      const identities = [];
      for (const record of records) {
        if (!record || (record.runId != null && String(record.runId) !== model.runID)
          || (record.generation != null && String(record.generation) !== generation)) return false;
        const start = Number(record.start);
        const end = Number(record.end);
        if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start !== expected || end < start) return false;
        expected = end;
        identities.push(generation + ':' + start + ':' + end);
      }
      if (expected !== incomingEnd) return false;
      if (kind === 'snapshot') {
        if (model.cursor && model.generation === generation) {
          const currentOffset = Number(model.cursor.offset || 0);
          if (incomingEnd < currentOffset || incomingStart > currentOffset) return false;
        }
        model.generation = generation;
        model.records = [];
        model.identities.clear();
        let first = records.length;
        let retainedBytes = 0;
        while (first > 0 && first > records.length - MAX_RETAINED_RECORDS) {
          const candidate = records[first - 1];
          const candidateBytes = Math.max(0, Number(candidate.end) - Number(candidate.start));
          if (retainedBytes + candidateBytes > MAX_RETAINED_BYTES && first < records.length) break;
          retainedBytes += candidateBytes;
          first -= 1;
        }
        model.rangeStart = first < records.length ? Number(records[first].start) : incomingStart;
        model.rangeEnd = incomingEnd;
        model.retainedBytes = retainedBytes;
        model.truncated = !!payload.bounded || first > 0;
        for (let i = first; i < records.length; i++) {
          const record = records[i];
          const identity = identities[i];
          if (model.identities.has(identity)) continue;
          model.identities.add(identity);
          model.records.push(record);
        }
        model.cursor = cursor;
        model.legacyText = '';
        model.sourceState = 'available';
        return true;
      }
      if (kind !== 'append' || model.generation !== generation || !model.cursor) return false;
      const currentOffset = Number(model.cursor.offset || 0);
      if (incomingStart < currentOffset) {
        if (incomingEnd <= currentOffset && identities.every(function (identity) { return model.identities.has(identity); })) return false;
        return false;
      }
      if (incomingStart !== currentOffset) return false;
      let accepted = 0;
      for (let i = 0; i < records.length; i++) {
        const record = records[i];
        const identity = identities[i];
        if (model.identities.has(identity)) continue;
        model.identities.add(identity);
        model.records.push(record);
        model.retainedBytes += Math.max(0, Number(record.end) - Number(record.start));
        accepted += 1;
      }
      while (model.records.length > MAX_RETAINED_RECORDS) {
        const removed = model.records.shift();
        model.identities.delete(generation + ':' + removed.start + ':' + removed.end);
        model.retainedBytes -= Math.max(0, Number(removed.end) - Number(removed.start));
        model.truncated = true;
      }
      while (model.records.length > 0 && model.retainedBytes > MAX_RETAINED_BYTES) {
        const removed = model.records.shift();
        model.identities.delete(generation + ':' + removed.start + ':' + removed.end);
        model.retainedBytes -= Math.max(0, Number(removed.end) - Number(removed.start));
        model.truncated = true;
      }
      if (model.records.length) model.rangeStart = Number(model.records[0].start);
      else model.rangeStart = incomingEnd;
      model.rangeEnd = incomingEnd;
      model.cursor = cursor;
      model.sourceState = 'available';
      return accepted > 0;
    }

    function text(runID) {
      const model = state(runID);
      if (!model.cursor) return model.legacyText || '';
      return model.records.map(function (record) { return String(record.text == null ? '' : record.text); }).join('\n') + (model.records.length ? '\n' : '');
    }

    function cursor(runID) {
      const value = state(runID).cursor;
      if (!value) return '';
      const json = JSON.stringify(value);
      try {
        return btoa(unescape(encodeURIComponent(json))).replace(/=+$/, '').replace(/\+/g, '-').replace(/\//g, '_');
      } catch (_err) {
        return '';
      }
    }

    function setEpoch(runID, epoch) {
      state(runID).epoch = epoch;
    }

    function get(runID) {
      return state(runID);
    }

    function range(runID) {
      const model = state(runID);
      return {
        runID: model.runID,
        generation: model.generation,
        start: model.rangeStart,
        end: model.rangeEnd,
        retainedBytes: model.retainedBytes,
        truncated: model.truncated,
        sourceState: model.sourceState,
      };
    }

    return { seed, accept, text, cursor, setEpoch, get, range };
  }

  global.SandmanPortalLog = { create };
})(typeof window !== 'undefined' ? window : globalThis);
