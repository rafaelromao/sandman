(function (global) {
  const rowData = new WeakMap();
  const detailData = new WeakMap();
  let mutationCount = 0;
  let innerHTMLAssignmentCount = 0;

  function resetCounters() {
    mutationCount = 0;
    innerHTMLAssignmentCount = 0;
  }
  function getCounters() {
    return { mutations: mutationCount, innerHTMLAssignments: innerHTMLAssignmentCount };
  }

  function setRowData(row, run) { rowData.set(row, run); }
  function getRowData(row) { return rowData.get(row); }
  function clearRowData(row) { rowData.delete(row); }
  function setDetailData(detailRow, run) { detailData.set(detailRow, run); }
  function getDetailData(detailRow) { return detailData.get(detailRow); }
  function clearDetailData(detailRow) { detailData.delete(detailRow); }

  function snapshotCellState(run, opts) {
    const h = opts.helpers;
    const snap = {
      kind: run.kind || '',
      archived: !!(run && run.archived),
      nameText: run.issueLabel || run.key,
      metaText: h.renderRunMeta(run),
      badgeClass: h.statusClass(run),
      badgeLabel: run.status || (run.kind === 'active' ? 'running' : 'completed'),
      reason: (run.reason === 'auto-select' || run.reason === 'review') ? run.reason : '',
      contextText: contextText(run),
      startedText: h.formatTime(run.startedAt),
      durationText: h.formatDuration(run.duration),
      issueTitleText: h.formatIssueTitle(run),
      canAbort: opts.abortSupported !== false && h.isRunAbortable(run, opts.abortReservations),
      canArchive: opts.archiveSupported !== false && h.isRunArchivable(run),
      ariaExpanded: String(matchesExpandedSubject(run, opts.expandedKey, opts)),
    };
    const stale = stalenessOf(run);
    snap.staleText = stale ? stale.text : '';
    snap.staleWarn = stale ? stale.warn : false;
    return snap;
  }

  function cellOf(row, name) {
    return row.querySelector('[data-cell="' + name + '"]');
  }

  function detailRowOf(body, runKey) {
    return body.querySelector('tr.detail-row[data-detail-for="' + runKey + '"]');
  }

  function domSafeKey(value) {
    return String(value || '').trim().replace(/[^A-Za-z0-9_-]+/g, '-');
  }

  function rowIDForKey(runKey) {
    return 'run-row-' + domSafeKey(runKey);
  }

  function detailIDForKey(runKey) {
    return 'run-detail-' + domSafeKey(runKey);
  }

  function dataRowOf(body, runKey) {
    return body.querySelector('tr[data-run-key="' + runKey + '"]');
  }

  function makeRowCell(cellName, row) {
    const td = global.document.createElement('td');
    td.setAttribute('data-cell', cellName);
    row.appendChild(td);
    return td;
  }

  function rowColspan(_opts) {
    return '6';
  }

  function setFirstCellColspan(row, colspan) {
    if (!row || !row.children || !row.children.length) return;
    const cell = row.children[0];
    if (cell && cell.getAttribute && cell.getAttribute('colspan') !== colspan) {
      cell.setAttribute('colspan', colspan);
      mutationCount += 1;
    }
  }

  function buildTitleCell(td, run, helpers) {
    const wrap = global.document.createElement('div');
    wrap.classList.add('run-title');
    const name = global.document.createElement('span');
    name.classList.add('name');
    name.textContent = run.issueLabel || run.key;
    wrap.appendChild(name);
    const meta = global.document.createElement('span');
    meta.classList.add('meta-line', 'mono');
    meta.textContent = helpers.renderRunMeta(run);
    wrap.appendChild(meta);
    td.appendChild(wrap);
  }

  function contextText(run) {
    if (run.reason === 'auto-select' && Array.isArray(run.candidates) && run.candidates.length > 0) {
      return 'Auto-select candidates: ' + run.candidates.map((n) => '#' + n).join(', ');
    }
    return '';
  }

  function buildContextRow(run, opts) {
    const text = contextText(run);
    if (!text) return null;
    const tr = global.document.createElement('tr');
    tr.classList.add('context-row');
    tr.setAttribute('data-context-for', run.key);
    const td = global.document.createElement('td');
    td.setAttribute('colspan', rowColspan(opts));
    td.appendChild(buildContextChip(text));
    tr.appendChild(td);
    return tr;
  }

  function buildContextChip(text) {
    const chip = global.document.createElement('span');
    chip.classList.add('context-chip', 'mono');
    chip.textContent = text;
    return chip;
  }

  function contextRowOf(body, runKey) {
    return body.querySelector('tr.context-row[data-context-for="' + runKey + '"]');
  }

  function reconcileContextRow(body, dataRow, oldRun, newRun, opts) {
    const oldText = contextText(oldRun);
    const newText = contextText(newRun);
    if (oldText === newText) return;
    const existing = contextRowOf(body, newRun.key);
    if (!newText) {
      if (existing) {
        body.removeChild(existing);
        mutationCount += 1;
      }
      return;
    }
    if (existing) {
      const chip = existing.querySelector('.context-chip');
      if (chip) {
        setText(chip, newText);
      }
      setFirstCellColspan(existing, rowColspan(opts));
      return;
    }
    const fresh = buildContextRow(newRun, opts);
    if (!fresh) return;
    const nextRow = dataRow.nextElementSibling;
    if (nextRow && (nextRow.classList.contains('detail-row') || nextRow.classList.contains('context-row'))) {
      body.insertBefore(fresh, nextRow);
    } else {
      body.insertBefore(fresh, dataRow.nextSibling);
    }
    mutationCount += 1;
  }

  function buildBadgeCell(td, run, helpers) {
    const klass = helpers.statusClass(run);
    const label = run.status || (run.kind === 'active' ? 'running' : 'completed');
    const badge = global.document.createElement('span');
    badge.classList.add('badge', klass);
    const dot = global.document.createElement('span');
    dot.classList.add('dot');
    badge.appendChild(dot);
    const labelSpan = global.document.createElement('span');
    labelSpan.classList.add('badge-label');
    labelSpan.textContent = label;
    badge.appendChild(labelSpan);
    td.appendChild(badge);
  }

  const STALE_CHIP_SECONDS = 60;
  const STALE_WARN_SECONDS = 180;

  function formatStaleDuration(seconds) {
    if (seconds < 60) return seconds + 's';
    const m = Math.floor(seconds / 60);
    const s = seconds % 60;
    if (m < 60) return m + 'm' + (s ? ' ' + s + 's' : '');
    const h = Math.floor(m / 60);
    const mm = m % 60;
    return h + 'h' + (mm ? ' ' + mm + 'm' : '');
  }

  // stalenessOf derives the per-row staleness signal from the server-provided
  // lastOutputAt (saved-run-log mtime with a startedAt fallback, set by
  // compute()). Returns null for non-active rows or rows younger than the
  // line threshold, so healthy quiet agents are not flagged. The warn tier
  // escalates the line to the --warning token past the second threshold; no
  // new color is introduced.
  function stalenessOf(run) {
    if (!run || run.kind !== 'active' || !run.lastOutputAt) return null;
    if (run.status === 'queued' || run.status === 'blocked') return null;
    const ts = Date.parse(run.lastOutputAt);
    if (!Number.isFinite(ts)) return null;
    const seconds = Math.max(0, Math.floor((Date.now() - ts) / 1000));
    if (seconds < STALE_CHIP_SECONDS) return null;
    return { text: 'stale \u00b7 ' + formatStaleDuration(seconds), warn: seconds >= STALE_WARN_SECONDS };
  }

  function buildMonoCell(td, text, extraClass) {
    if (extraClass) td.classList.add(extraClass);
    td.classList.add('mono');
    td.textContent = text;
  }

  function buildDurationCell(td, run, helpers) {
    td.classList.add('mono', 'cell-duration');
    const stack = global.document.createElement('div');
    stack.classList.add('cell-duration-stack');
    const value = global.document.createElement('span');
    value.classList.add('duration-value');
    value.textContent = helpers.formatDuration(run.duration);
    stack.appendChild(value);
    const stale = stalenessOf(run);
    if (stale) stack.appendChild(buildStaleLine(stale.text, stale.warn));
    td.appendChild(stack);
  }

  function buildStaleLine(staleText, staleWarn) {
    const line = global.document.createElement('span');
    line.classList.add('stale-line');
    if (staleWarn) line.classList.add('warn');
    line.setAttribute('data-stale', '1');
    line.textContent = staleText;
    return line;
  }

  function reconcileStaleLine(cell, staleText, staleWarn) {
    const stack = cell.querySelector('.cell-duration-stack');
    if (!stack) return;
    let existing = stack.querySelector('.stale-line');
    if (!staleText) {
      if (existing) { stack.removeChild(existing); mutationCount += 1; }
      return;
    }
    if (existing && existing.textContent === staleText && existing.classList.contains('warn') === Boolean(staleWarn)) return;
    if (existing) stack.removeChild(existing);
    stack.appendChild(buildStaleLine(staleText, staleWarn));
    mutationCount += 1;
  }
  function buildActionsCell(td, run, opts) {
    td.classList.add('run-actions');
    if (reserveAbortButton(run, opts)) {
      const btn = global.document.createElement('button');
      btn.setAttribute('type', 'button');
      btn.classList.add('action-btn', 'danger');
      btn.setAttribute('data-action', 'abort-run');
      btn.setAttribute('data-run-key', run.key);
      if (run.issueNumber != null) btn.setAttribute('data-issue', String(run.issueNumber));
      btn.textContent = 'Abort';
      td.appendChild(btn);
    }
    if (reserveArchiveButton(run, opts)) {
      const btn = global.document.createElement('button');
      btn.setAttribute('type', 'button');
      btn.classList.add('action-btn');
      btn.setAttribute('data-action', 'archive-run');
      btn.setAttribute('data-run-key', run.key);
      if (run.runId) btn.setAttribute('data-run-id', run.runId);
      btn.textContent = 'Archive';
      td.appendChild(btn);
    }
  }

  function reserveAbortButton(run, opts) {
    const reservations = opts && opts.abortReservations;
    if (!run || !opts || !reservations) return false;
    if (opts.abortSupported === false) return false;
    if (!opts.helpers.isRunAbortable(run, reservations)) return false;
    const reservationKey = String(run.key || '') + ':' + String(run.issueNumber != null ? run.issueNumber : '');
    if (reservations.has(reservationKey)) return false;
    reservations.add(reservationKey);
    return true;
  }

  // reserveArchiveButton keeps the Archive button off rows that are
  // either still active or already archived. The button is hidden for
  // non-completed runs (no archive allowed) and for completed runs
  // whose Archived flag is true. archiveSupported defaults to true;
  // pass {archiveSupported: false} to disable the button cluster
  // entirely (e.g. tests or alternate portals).
  function reserveArchiveButton(run, opts) {
    if (!run) return false;
    if (!opts || opts.archiveSupported === false) return false;
    if (!opts.helpers || !opts.helpers.isRunArchivable(run)) return false;
    return true;
  }

  function buildDataRow(body, run, opts) {
    const tr = body.insertRow();
    tr.classList.add('run-row');
    if (run.kind) tr.classList.add(run.kind);
    if (run.archived) tr.classList.add('row-archived');
    tr.setAttribute('data-action', 'toggle-run');
    tr.setAttribute('data-run-key', run.key);
    tr.setAttribute('id', rowIDForKey(run.key));
    tr.setAttribute('role', 'button');
    tr.setAttribute('tabindex', '0');
    tr.setAttribute('aria-controls', detailIDForKey(run.key));
    tr.setAttribute('aria-expanded', String(matchesExpandedSubject(run, opts.expandedKey, opts)));

    const titleCell = makeRowCell('title', tr);
    buildTitleCell(titleCell, run, opts.helpers);

    const badgeCell = makeRowCell('badge', tr);
    buildBadgeCell(badgeCell, run, opts.helpers);

    const startedCell = makeRowCell('started', tr);
    buildMonoCell(startedCell, opts.helpers.formatTime(run.startedAt));

    const durationCell = makeRowCell('duration', tr);
    buildDurationCell(durationCell, run, opts.helpers);

    const issueTitleCell = makeRowCell('issue-title', tr);
    buildMonoCell(issueTitleCell, opts.helpers.formatIssueTitle(run));

    const actionsCell = makeRowCell('actions', tr);
    buildActionsCell(actionsCell, run, opts);

    const contextTr = buildContextRow(run, opts);
    if (contextTr) body.appendChild(contextTr);

    return { row: tr, contextRow: contextTr };
  }

  function buildTabsRow(panel, run, tabName) {
    const tabs = global.document.createElement('div');
    tabs.classList.add('tabs');
    tabs.setAttribute('role', 'tablist');
    tabs.setAttribute('aria-label', 'Run details tabs');
    for (const tab of ['log', 'events', 'details']) {
      const btn = global.document.createElement('button');
      btn.setAttribute('type', 'button');
      btn.classList.add('tab-btn');
      btn.setAttribute('data-action', 'set-tab');
      btn.setAttribute('data-run-key', run.key);
      btn.setAttribute('data-tab', tab);
      btn.setAttribute('role', 'tab');
      btn.setAttribute('aria-pressed', String(tab === tabName));
      btn.textContent = tab.charAt(0).toUpperCase() + tab.slice(1);
      tabs.appendChild(btn);
    }
    panel.appendChild(tabs);
  }

  function formatEventTime(timestamp) {
    if (!timestamp) return '—';
    const d = new Date(timestamp);
    if (Number.isNaN(d.getTime())) return '—';
    return new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(d);
  }

  function batchPeers(run) {
    const issues = Array.isArray(run && run.batchIssues) ? run.batchIssues : [];
    if (!issues.length) return [];
    return issues.filter((n) => n !== run.issueNumber);
  }

  function eventsJSON(run) {
    const events = Array.isArray(run && run.events) ? run.events : [];
    return JSON.stringify(events.map((event) => ({
      type: event && event.type ? event.type : 'event',
      timestamp: event && event.timestamp ? event.timestamp : null,
      payload: event && event.payload ? event.payload : {},
    })), null, 2);
  }

  function subjectRunValue(run) {
    if (!run) return '';
    return String(run.runId || run.key || '').trim();
  }

  function findRunByIdentity(runIdentity, opts) {
    if (!runIdentity || !Array.isArray(opts && opts.runs)) return null;
    return opts.runs.find((run) => run && (run.key === runIdentity || run.runId === runIdentity)) || null;
  }

  function matchesExpandedSubject(run, expandedKey, opts) {
    if (!run) return false;
    const subjectKey = subjectRunValue(run);
    if (expandedKey === subjectKey || expandedKey === run.key) return true;
    const expandedRun = expandedKey ? findRunByIdentity(expandedKey, opts) : null;
    if (!expandedRun) return false;
    if (!Number.isInteger(run.issueNumber) || run.issueNumber <= 0) return false;
    return run.issueNumber === expandedRun.issueNumber;
  }

  function subjectRunLabel(run) {
    if (!run) return 'Run';
    return subjectRunValue(run) || 'Run';
  }

  function subjectRunsFor(run, opts) {
    const visible = Array.isArray(opts && opts.runs) ? opts.runs : [];
    if (!run || run.issueNumber <= 0) return [];
    const seen = new Set();
    const parents = [];
    const reviews = [];
    for (const candidate of visible) {
      if (!candidate || candidate.issueNumber !== run.issueNumber) continue;
      const value = subjectRunValue(candidate);
      if (!value || seen.has(value)) continue;
      seen.add(value);
      if (candidate.review) reviews.push(candidate);
      else parents.push(candidate);
    }
    const canonicalParent = parents.find((candidate) => Number(candidate.reviewCount || 0) > 0 || candidate.reviewVerdict) || (parents.length === 1 ? parents[0] : null) || (!run.review && Number(run.reviewCount || 0) > 0 ? run : null);
    const related = [];
    if (canonicalParent) related.push(canonicalParent);
    reviews.sort((a, b) => {
      const aStarted = a.startedAt ? new Date(a.startedAt).getTime() : 0;
      const bStarted = b.startedAt ? new Date(b.startedAt).getTime() : 0;
      if (aStarted !== bStarted) return bStarted - aStarted;
      return subjectRunValue(a).localeCompare(subjectRunValue(b));
    });
    for (const candidate of reviews) {
      related.push(candidate);
    }
    return related;
  }

  function subjectFingerprint(run, opts) {
    const related = subjectRunsFor(run, opts);
    return related.map(subjectRunValue).join('|');
  }

  function buildSubjectSelector(panel, rowRun, subjectRun, opts) {
    const related = subjectRunsFor(rowRun, opts);
    if (!related.length) return;
    const row = global.document.createElement('div');
    row.classList.add('detail-subject-picker');
    const select = global.document.createElement('select');
    select.setAttribute('aria-label', 'Subject');
    select.setAttribute('data-action', 'set-subject');
    select.setAttribute('data-run-key', rowRun.key);
    const currentValue = subjectRunValue(subjectRun || rowRun);
    for (const subject of related) {
      const option = global.document.createElement('option');
      const value = subjectRunValue(subject);
      option.setAttribute('value', value);
      option.textContent = subjectRunLabel(subject);
      if (value === currentValue) option.setAttribute('selected', 'selected');
      select.appendChild(option);
    }
    select.value = currentValue;
    row.appendChild(select);
    panel.appendChild(row);
  }

  function buildLogPre(run, helpers) {
    const log = run.log && String(run.log).trim() ? run.log : 'No log file yet.';
    const pre = global.document.createElement('pre');
    pre.classList.add('terminal-text');
    pre.setAttribute('data-scroll-key', run.key);
    fillTerminalPre(pre, log, helpers);
    pre.setAttribute('data-rendered-log', log);
    return pre;
  }

  function fillTerminalPre(pre, text, helpers) {
    const html = helpers.renderTerminalContent(text);
    const scratch = global.document.createElement('div');
    scratch.innerHTML = html;
    const nodes = Array.from(scratch.childNodes);
    while (pre.firstChild) pre.removeChild(pre.firstChild);
    for (const node of nodes) pre.appendChild(node);
  }

  function highlightJSON(text) {
    if (!text) return '';
    // local escapeHTML — the helpers bag isn't always in scope
    const _e = function(v) {
      return String(v == null ? '' : v)
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    };
    return text.replace(
      /("(?:[^"\\]|\\.)*")(\s*):|("(?:[^"\\]|\\.)*")|(\b(?:true|false)\b)|(\bnull\b)|(-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b)|([{}[\]:,])/g,
      function(match, keyStr, ws, strVal, boolVal, nullVal, numVal, punct) {
        if (keyStr) return '<span class="json-key">' + _e(keyStr) + '</span>' + (ws || '') + '<span class="json-punctuation">:</span>';
        if (strVal) return '<span class="json-string">' + _e(strVal) + '</span>';
        if (boolVal) return '<span class="json-boolean">' + boolVal + '</span>';
        if (nullVal) return '<span class="json-null">' + nullVal + '</span>';
        if (numVal) return '<span class="json-number">' + numVal + '</span>';
        if (punct) return '<span class="json-punctuation">' + punct + '</span>';
        return match;
      }
    );
  }

  function highlightTerminalLog(text) {
    var value = String(text || '');
    if (!value) return '';
    var _e = function(v) {
      return String(v == null ? '' : v)
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    };
    var escaped = _e(value);
    return escaped.split('\n').map(function(line) {
      // Strip ANSI escape codes
      line = line.replace(/\x1b\[[0-9;]*[A-Za-z]/g, '');
      // Shell command prefix
      if (/^\$ /.test(line)) {
        return '<span class="term-prompt">$ </span>' + line.slice(2);
      }
      // Tool indicators
      line = line.replace(/^(\s*)([→←✱]) /, '$1<span class="term-tool">$2</span> ');
      // Markdown/terminal headers: ## Heading or > build · ...
      line = line.replace(/^(&gt; build.*)$/, '<span class="term-heading">$1</span>');
      line = line.replace(/^(#{1,6} .*)$/, '<span class="term-heading">$1</span>');
      // Todo checklists
      line = line.replace(/\[(✓|✔)\]/g, '<span class="term-todo-done">[✓]</span>');
      line = line.replace(/\[•\]/g, '<span class="term-todo-active">[•]</span>');
      line = line.replace(/^(\s*)\[ \]/g, '$1<span class="term-todo-pending">[ ]</span>');
      // Go test results
      line = line.replace(/(--- PASS:.*$)/gm, '<span class="term-pass">$1</span>');
      line = line.replace(/(--- FAIL:.*$)/gm, '<span class="term-fail">$1</span>');
      line = line.replace(/(FAIL\s+\S+)/g, '<span class="term-fail">$1</span>');
      line = line.replace(/(ok\s+\S+)/g, '<span class="term-pass">$1</span>');
      // Python test results
      line = line.replace(/\b(PASSED)\b/g, '<span class="term-pass">$1</span>');
      line = line.replace(/\b(FAILED)\b/g, '<span class="term-fail">$1</span>');
      // Node test results
      line = line.replace(/(✓[^\n]*)/g, '<span class="term-pass">$1</span>');
      line = line.replace(/(✕[^\n]*)/g, '<span class="term-fail">$1</span>');
      // .NET test results
      line = line.replace(/(Passed!.*$)/g, '<span class="term-pass">$1</span>');
      line = line.replace(/(Failed!.*$)/g, '<span class="term-fail">$1</span>');
      // Java test results
      line = line.replace(/(Tests run:.*Failures: [1-9])/g, '<span class="term-fail">$1</span>');
      line = line.replace(/(Tests run:.*Failures: 0)/g, '<span class="term-pass">$1</span>');
      // Elixir test results
      line = line.replace(/(\d+ tests?, \d+ failures?)/g, function(m) {
        return /0 failures/.test(m) ? '<span class="term-pass">' + m + '</span>' : '<span class="term-fail">' + m + '</span>';
      });
      // Rust test results
      line = line.replace(/(test result: ok.*)/g, '<span class="term-pass">$1</span>');
      line = line.replace(/(test result: FAILED.*)/g, '<span class="term-fail">$1</span>');
      // Ruby test results
      line = line.replace(/(\d+ examples?, 0 failures)/g, '<span class="term-pass">$1</span>');
      line = line.replace(/(\d+ examples?, [1-9]\d* failures?)/g, '<span class="term-fail">$1</span>');
      // Verdict keywords (bold markdown)
      line = line.replace(/(\*\*CHANGES_REQUESTED\*\*)/g, '<span class="term-fail">$1</span>');
      line = line.replace(/(\*\*APPROVED\*\*)/g, '<span class="term-pass">$1</span>');
      line = line.replace(/(\*\*APPROVED with comments\*\*)/g, '<span class="term-pass">$1</span>');
      // URLs
      line = line.replace(/(https?:\/\/[^\s<&]+)/g, '<span class="term-url">$1</span>');
      // File paths with line numbers
      line = line.replace(/([\/\w.\-]+\.(?:go|js|ts|jsx|tsx|py|rs|rb|java|cs|ex|exs|c|cpp|h|hpp|zig|mod|sum):\d+)/g, '<span class="term-path">$1</span>');
      // Diff markers
      line = line.replace(/^(\+\+\+ .*)$/gm, '<span class="term-path">$1</span>');
      line = line.replace(/^(\-\-\- .*)$/gm, '<span class="term-path">$1</span>');
      line = line.replace(/^(@@.*@@)/gm, '<span class="term-heading">$1</span>');
      return line;
    }).join('\n');
  }

  function appendTerminalPre(pre, oldLog, newSuffix, helpers) {
    if (!newSuffix) return;
    if (appendStartsAtBoundary(oldLog, newSuffix)) {
      const html = helpers.renderTerminalContent(newSuffix);
      if (!html) return;
      const scratch = global.document.createElement('div');
      scratch.innerHTML = html;
      const nodes = Array.from(scratch.childNodes);
      for (const node of nodes) pre.appendChild(node);
      return;
    }
    const lastNewline = oldLog.lastIndexOf('\n');
    const partialLastLine = lastNewline >= 0 ? oldLog.slice(lastNewline + 1) : oldLog;
    if (!partialLastLine) {
      fillTerminalPre(pre, oldLog + newSuffix, helpers);
      return;
    }
    const combined = partialLastLine + newSuffix;
    const html = helpers.renderTerminalContent(combined);
    if (!html) return;
    const scratch = global.document.createElement('div');
    scratch.innerHTML = html;
    const newNodes = Array.from(scratch.childNodes);
    const startIdx = findPartialLineStart(pre, partialLastLine);
    if (startIdx < 0) {
      fillTerminalPre(pre, oldLog + newSuffix, helpers);
      return;
    }
    while (pre.childNodes.length > startIdx) {
      pre.removeChild(pre.lastChild);
    }
    for (const node of newNodes) pre.appendChild(node);
  }

  function findPartialLineStart(pre, partialLastLine) {
    if (!partialLastLine) return -1;
    const nodes = Array.from(pre.childNodes);
    let acc = '';
    for (let i = nodes.length - 1; i >= 0; i -= 1) {
      acc = nodeText(nodes[i]) + acc;
      if (acc.length >= partialLastLine.length) {
        return i;
      }
    }
    return -1;
  }

  function nodeText(node) {
    if (!node) return '';
    if (node.nodeType === 3) {
      return node._textContent != null ? String(node._textContent) : String(node.textContent || '');
    }
    if (node._textContent != null) return String(node._textContent);
    let text = '';
    if (node.childNodes) {
      for (const c of node.childNodes) text += nodeText(c);
    } else if (node.children) {
      for (const c of node.children) text += nodeText(c);
    }
    return text;
  }

  function appendStartsAtBoundary(oldLog, suffix) {
    if (!suffix) return true;
    if (isBoundary(suffix.charAt(0))) return true;
    if (oldLog && isBoundary(oldLog.charAt(oldLog.length - 1))) return true;
    return false;
  }

  function isBoundary(ch) {
    if (ch === '\n' || ch === ' ' || ch === '\t') return true;
    return /[.,;:!?()[\]{}"'`/\\]/.test(ch);
  }

  function eventsFingerprint(run) {
    const events = Array.isArray(run.events) ? run.events : [];
    if (!events.length) return '';
    const parts = new Array(events.length);
    for (let i = 0; i < events.length; i += 1) {
      const e = events[i] || {};
      parts[i] = String(e.type || 'event') + '|' + String(e.timestamp || '') + '|' + JSON.stringify(e.payload || {});
    }
    return parts.join('\n');
  }

  function detailsData(run, helpers) {
    return {
      runId: run.runId || '',
      branch: run.branch || '',
      batch: run.batchKey || '',
      logPath: run.logPath || '',
      peers: batchPeers(run),
    };
  }

  function detailsJSON(run, helpers) {
    return JSON.stringify(detailsData(run, helpers), null, 2);
  }

  function detailsFingerprint(run, helpers) {
    const data = detailsData(run, helpers);
    if (run && run.kind === 'active') {
      // Active rows tick duration every poll. Keep the Details pane stable
      // while the run is live; the summary row already shows live duration.
      data.duration = '';
    }
    return JSON.stringify(data, null, 2);
  }

  function buildLogContent(content, run, helpers) {
    const section = global.document.createElement('section');
    section.classList.add('detail-box', 'tab-pane', 'fill');
    const log = run.log && String(run.log).trim() ? run.log : 'No log file yet.';
    const pre = buildLogPre(run, helpers);
    section.appendChild(pre);
    content.appendChild(section);
  }

  function buildEventsContent(content, run, helpers) {
    const section = global.document.createElement('section');
    section.classList.add('detail-box', 'tab-pane', 'fill');
    const pre = global.document.createElement('pre');
    pre.classList.add('terminal-text');
    const json = eventsJSON(run);
    fillTerminalPre(pre, json, {
      renderTerminalContent: highlightJSON,
    });
    pre.setAttribute('data-rendered-json', json);
    section.appendChild(pre);
    content.appendChild(section);
  }

  function buildDetailsContent(content, run, helpers) {
    const section = global.document.createElement('section');
    section.classList.add('detail-box', 'tab-pane', 'fill');
    const pre = global.document.createElement('pre');
    pre.classList.add('terminal-text');
    const json = detailsJSON(run, helpers);
    fillTerminalPre(pre, json, {
      renderTerminalContent: highlightJSON,
    });
    pre.setAttribute('data-rendered-json', json);
    section.appendChild(pre);
    content.appendChild(section);
  }

  function buildDetailContent(panel, rowRun, subjectRun, tabName, helpers, opts) {
    const subjectFp = subjectRunValue(subjectRun) + '|' + subjectFingerprint(subjectRun, opts);
    buildSubjectSelector(panel, rowRun, subjectRun, opts);
    buildTabsRow(panel, rowRun, tabName);
    const content = global.document.createElement('div');
    content.classList.add('detail-content');
    content.setAttribute('data-rendered-subject-fingerprint', subjectFp);
    panel.appendChild(content);
    if (tabName === 'log') buildLogContent(content, subjectRun, helpers);
    else if (tabName === 'events') {
      buildEventsContent(content, subjectRun, helpers);
      content.setAttribute('data-rendered-fingerprint', 'events|' + eventsFingerprint(subjectRun) + '|subjects:' + subjectFp);
    } else {
      buildDetailsContent(content, subjectRun, helpers);
      content.setAttribute('data-rendered-fingerprint', 'details|' + detailsFingerprint(subjectRun, helpers) + '|subjects:' + subjectFp);
    }
  }

  function tabNameFor(run, opts) {
    const subjectKey = subjectRunValue(run);
    if (opts.tabs && Object.prototype.hasOwnProperty.call(opts.tabs, subjectKey)) {
      const t = String(opts.tabs[subjectKey] || '').trim();
      if (t === 'log' || t === 'events' || t === 'details') return t;
    }
    if (opts.tabs && Object.prototype.hasOwnProperty.call(opts.tabs, run.key)) {
      const t = String(opts.tabs[run.key] || '').trim();
      if (t === 'log' || t === 'events' || t === 'details') return t;
    }
    return 'log';
  }

  function buildDetailRow(body, run, opts) {
    const subjectRun = findRunByIdentity(opts.expandedKey, opts) || run;
    const tr = global.document.createElement('tr');
    tr.classList.add('detail-row');
    tr.setAttribute('data-detail-for', run.key);
    tr.setAttribute('id', detailIDForKey(run.key));
    tr.setAttribute('role', 'region');
    tr.setAttribute('aria-labelledby', rowIDForKey(run.key));
    const td = global.document.createElement('td');
    td.setAttribute('colspan', rowColspan(opts));
    const panel = global.document.createElement('div');
    panel.classList.add('detail-panel');
    buildDetailContent(panel, run, subjectRun, tabNameFor(subjectRun, opts), opts.helpers, opts);
    td.appendChild(panel);
    tr.appendChild(td);
    body.appendChild(tr);
    return tr;
  }

  function updateDetailContent(detailRow, oldRun, run, opts) {
    const subjectRun = findRunByIdentity(opts.expandedKey, opts) || run;
    const tabName = tabNameFor(subjectRun, opts);
    const tabButtons = detailRow.querySelectorAll('button[data-tab]');
    for (const btn of tabButtons) {
      const want = String(btn.getAttribute('data-tab') === tabName);
      if (btn.getAttribute('aria-pressed') !== want) {
        setAttr(btn, 'aria-pressed', want);
      }
    }
    const subjectFp = subjectRunValue(subjectRun) + '|' + subjectFingerprint(subjectRun, opts);
    const content = detailRow.querySelector('.detail-content');
    if (!content) return;
    if (content.getAttribute('data-rendered-subject-fingerprint') !== subjectFp) {
      const panel = detailRow.querySelector('.detail-panel');
      if (!panel) return;
      while (panel.firstChild) panel.removeChild(panel.firstChild);
      buildDetailContent(panel, run, subjectRun, tabName, opts.helpers, opts);
      mutationCount += 1;
      return;
    }
    if (tabName === 'log') {
      // While the Log tab is being fed by a live SSE stream
      // (opts.streamingKeys), the stream owns this <pre>; the poll path
      // must not overwrite it with its 64KB socket snapshot every 2s.
      if (opts.streamingKeys && opts.streamingKeys.has(run.key) && content.querySelector('pre[data-scroll-key]')) {
        return;
      }
      const pre = content.querySelector('pre[data-scroll-key]');
      const newLog = subjectRun.log && String(subjectRun.log).trim() ? subjectRun.log : 'No log file yet.';
      if (pre) {
        const oldLog = pre.getAttribute('data-rendered-log') || '';
        if (oldLog === newLog) {
          return;
        }
        if (oldLog && oldLog !== 'No log file yet.' && newLog === 'No log file yet.') {
          return;
        }
        if (oldRun && oldRun.kind === 'active' && run.kind !== 'active' && oldLog && oldLog !== 'No log file yet.' && newLog && newLog !== oldLog && !String(newLog).startsWith(oldLog)) {
          return;
        }
        if (oldLog && newLog.length >= oldLog.length && newLog.startsWith(oldLog)) {
          appendTerminalPre(pre, oldLog, newLog.slice(oldLog.length), opts.helpers);
        } else {
          fillTerminalPre(pre, newLog, opts.helpers);
        }
        pre.setAttribute('data-rendered-log', newLog);
        mutationCount += 1;
        return;
      }
      while (content.firstChild) content.removeChild(content.firstChild);
      content.removeAttribute('data-rendered-fingerprint');
      buildLogContent(content, subjectRun, opts.helpers);
      mutationCount += 1;
      return;
    }
    let fingerprint = tabName + '|' + subjectFp;
    if (tabName === 'events') {
      fingerprint = 'events|' + eventsJSON(subjectRun);
    } else {
      fingerprint = 'details|' + detailsFingerprint(subjectRun, opts.helpers);
    }
    fingerprint += '|subjects:' + subjectFp;
    if (content.getAttribute('data-rendered-fingerprint') === fingerprint) {
      return;
    }
    while (content.firstChild) content.removeChild(content.firstChild);
    if (tabName === 'events') {
      buildEventsContent(content, subjectRun, opts.helpers);
    } else {
      buildDetailsContent(content, subjectRun, opts.helpers);
    }
    content.setAttribute('data-rendered-fingerprint', fingerprint);
    mutationCount += 1;
  }

  function insertRunRow(body, run, opts) {
    const built = buildDataRow(body, run, opts);
    setRowData(built.row, run);
    let detailTr = null;
    if (matchesExpandedSubject(run, opts.expandedKey, opts)) {
      detailTr = buildDetailRow(body, run, opts);
      setDetailData(detailTr, run);
    }
    return { row: built.row, contextRow: built.contextRow, batchRow: built.batchRow, detailRow: detailTr };
  }

  function setText(node, text) {
    if (node.textContent !== text) {
      node.textContent = text;
      mutationCount += 1;
    }
  }

  function setClass(node, name, present) {
    if (present && !node.classList.contains(name)) {
      node.classList.add(name);
      mutationCount += 1;
    } else if (!present && node.classList.contains(name)) {
      node.classList.remove(name);
      mutationCount += 1;
    }
  }

  function setAttr(node, name, value) {
    const current = node.getAttribute(name);
    if (current !== String(value)) {
      node.setAttribute(name, String(value));
      mutationCount += 1;
    }
  }

  function updateTitleCell(cell, oldSnap, newSnap, newRun) {
    const wrap = cell.children[0];
    if (!wrap) return;
    const name = wrap.children[0];
    const meta = wrap.children[1];
    if (oldSnap.nameText !== newSnap.nameText && name) {
      setText(name, newSnap.nameText);
    }
    if (oldSnap.metaText !== newSnap.metaText && meta) {
      setText(meta, newSnap.metaText);
    }
  }

  function updateBadgeCell(cell, oldSnap, newSnap) {
    const badge = cell.children[0];
    if (!badge) return;
    if (oldSnap.badgeClass !== newSnap.badgeClass) {
      setClass(badge, oldSnap.badgeClass, false);
      setClass(badge, newSnap.badgeClass, true);
    }
    if (oldSnap.badgeLabel !== newSnap.badgeLabel) {
      const labelSpan = badge.querySelector('.badge-label') || badge.children[1];
      if (labelSpan) {
        setText(labelSpan, newSnap.badgeLabel);
      }
    }
  }

  function updateDurationCell(cell, oldSnap, newSnap) {
    const value = cell.querySelector('.duration-value');
    if (value && oldSnap.durationText !== newSnap.durationText) {
      setText(value, newSnap.durationText);
    }
    reconcileStaleLine(cell, newSnap.staleText, newSnap.staleWarn);
  }

  function updateMonoCell(cell, newText) {
    if (cell.textContent !== newText) {
      setText(cell, newText);
    }
  }

  function updateActionsCell(cell, run, opts) {
    const wantAbort = reserveAbortButton(run, opts);
    const hasAbort = !!cell.querySelector('button[data-action="abort-run"]');
    const wantArchive = reserveArchiveButton(run, opts);
    const hasArchive = !!cell.querySelector('button[data-action="archive-run"]');
    if (wantAbort === hasAbort && wantArchive === hasArchive) {
      return;
    }
    while (cell.firstChild) cell.removeChild(cell.firstChild);
    if (wantAbort) {
      const btn = global.document.createElement('button');
      btn.setAttribute('type', 'button');
      btn.classList.add('action-btn', 'danger');
      btn.setAttribute('data-action', 'abort-run');
      btn.setAttribute('data-run-key', run.key);
      if (run.issueNumber != null) btn.setAttribute('data-issue', String(run.issueNumber));
      btn.textContent = 'Abort';
      cell.appendChild(btn);
    }
    if (wantArchive) {
      const btn = global.document.createElement('button');
      btn.setAttribute('type', 'button');
      btn.classList.add('action-btn');
      btn.setAttribute('data-action', 'archive-run');
      btn.setAttribute('data-run-key', run.key);
      if (run.runId) btn.setAttribute('data-run-id', run.runId);
      btn.textContent = 'Archive';
      cell.appendChild(btn);
    }
    mutationCount += 1;
  }

  function updateRunRowCells(row, oldRun, newRun, opts) {
    const before = mutationCount;
    const oldSnap = snapshotCellState(oldRun, opts);
    const newSnap = snapshotCellState(newRun, opts);

    if (oldSnap.kind !== newSnap.kind) {
      setClass(row, oldSnap.kind, false);
      setClass(row, newSnap.kind, true);
    }
    if (oldSnap.archived !== newSnap.archived) {
      setClass(row, 'row-archived', newSnap.archived);
    }
    const currentAria = row.getAttribute('aria-expanded');
    const desiredAria = newSnap.ariaExpanded;
    if (currentAria !== desiredAria) {
      setAttr(row, 'aria-expanded', desiredAria);
    }

    const titleCell = cellOf(row, 'title');
    if (titleCell) updateTitleCell(titleCell, oldSnap, newSnap, newRun);

    const badgeCell = cellOf(row, 'badge');
    if (badgeCell) updateBadgeCell(badgeCell, oldSnap, newSnap);

    const startedCell = cellOf(row, 'started');
    if (startedCell) updateMonoCell(startedCell, newSnap.startedText);

    const durationCell = cellOf(row, 'duration');
    if (durationCell) updateDurationCell(durationCell, oldSnap, newSnap);

    const issueTitleCell = cellOf(row, 'issue-title');
    if (issueTitleCell) updateMonoCell(issueTitleCell, newSnap.issueTitleText);

    const actionsCell = cellOf(row, 'actions');
    if (actionsCell) updateActionsCell(actionsCell, newRun, opts);

    const body = row.parentNode;
    if (body) {
      reconcileContextRow(body, row, oldRun, newRun, opts);
      const detail = detailRowOf(body, newRun.key);
      if (detail) {
        setFirstCellColspan(detail, rowColspan(opts));
      }
    }

    setRowData(row, newRun);
    return { mutated: mutationCount > before, cells: mutationCount - before };
  }

  function removeRunRow(body, key) {
    const dataRow = dataRowOf(body, key);
    const detail = detailRowOf(body, key);
    const ctx = contextRowOf(body, key);
    let removed = 0;
    if (dataRow) {
      body.removeChild(dataRow);
      clearRowData(dataRow);
      removed += 1;
    }
    if (ctx) {
      body.removeChild(ctx);
      removed += 1;
    }
    if (detail) {
      body.removeChild(detail);
      clearDetailData(detail);
      removed += 1;
    }
    return removed;
  }

  function setEmpty(body, html, colspan) {
    const placeholder = global.document.createElement('tr');
    const td = global.document.createElement('td');
    td.setAttribute('colspan', String(colspan || 6));
    td.innerHTML = html;
    placeholder.appendChild(td);
    body.replaceChildren(placeholder);
  }

  function diffRuns(body, runs, opts) {
    const before = mutationCount;
    const renderRuns = Array.isArray(opts && opts.visibleRuns) ? opts.visibleRuns : runs;
    const newKeys = new Set();
    const newRuns = new Map();
    for (const run of renderRuns) {
      if (!run || !run.key) continue;
      newKeys.add(run.key);
      newRuns.set(run.key, run);
    }

    const childList = Array.from(body.children);
    for (const child of childList) {
      if (!child.getAttribute) continue;
      if (child.getAttribute('data-run-key')) continue;
      if (child.getAttribute('data-detail-for')) continue;
      if (child.getAttribute('data-context-for')) continue;
      body.removeChild(child);
    }

    const existingDataRows = childList.filter((c) => c.parentNode === body && c.getAttribute && c.getAttribute('data-run-key'));
    const seen = new Set();
    let inserted = 0;
    let removed = 0;
    let updated = 0;

    for (const dataRow of existingDataRows) {
      const key = dataRow.getAttribute('data-run-key');
      seen.add(key);
      const newRun = newRuns.get(key);
      if (!newRun) {
        const detail = detailRowOf(body, key);
        if (detail) {
          body.removeChild(detail);
          clearDetailData(detail);
          removed += 1;
        }
        const ctx = contextRowOf(body, key);
        if (ctx) {
          body.removeChild(ctx);
          removed += 1;
        }
        body.removeChild(dataRow);
        clearRowData(dataRow);
        removed += 1;
        continue;
      }
      const oldRun = getRowData(dataRow) || newRun;
      const r = updateRunRowCells(dataRow, oldRun, newRun, opts);
      if (r.mutated) updated += 1;

      const wantDetail = matchesExpandedSubject(newRun, opts.expandedKey, opts);
      const detail = detailRowOf(body, key);
      if (wantDetail && !detail) {
        const newDetail = buildDetailRow(body, newRun, opts);
        setDetailData(newDetail, newRun);
        inserted += 1;
      } else if (!wantDetail && detail) {
        body.removeChild(detail);
        clearDetailData(detail);
        removed += 1;
      } else if (wantDetail && detail) {
        updateDetailContent(detail, oldRun, newRun, opts);
        setDetailData(detail, newRun);
      }
    }

    for (const [key, run] of newRuns) {
      if (seen.has(key)) continue;
      insertRunRow(body, run, opts);
      inserted += 1;
    }

    let pos = 0;
    for (let i = 0; i < renderRuns.length; i += 1) {
      const run = renderRuns[i];
      if (!run || !run.key) continue;
      const dataRow = dataRowOf(body, run.key);
      if (!dataRow) continue;
      if (body.children[pos] !== dataRow) {
        body.insertBefore(dataRow, body.children[pos] || null);
      }
      pos += 1;
      const ctx = contextRowOf(body, run.key);
      if (ctx) {
        if (body.children[pos] !== ctx) {
          body.insertBefore(ctx, body.children[pos] || null);
        }
        pos += 1;
      }
      const detail = detailRowOf(body, run.key);
      if (detail) {
        if (body.children[pos] !== detail) {
          body.insertBefore(detail, body.children[pos] || null);
        }
        pos += 1;
      }
    }

    return {
      inserted,
      removed,
      updated,
      mutations: mutationCount - before,
    };
  }

  function updateDetailPanelLog(body, runKey, newLog, helpers) {
    const detailRow = body.querySelector('tr.detail-row[data-detail-for="' + runKey + '"]');
    if (!detailRow) return;
    const pre = detailRow.querySelector('pre[data-scroll-key]');
    if (!pre) return;
    const renderedLog = newLog && String(newLog).trim() ? newLog : 'No log file yet.';
    const oldLog = pre.getAttribute('data-rendered-log') || '';
    if (oldLog === renderedLog) return;
    if (oldLog && renderedLog.length >= oldLog.length && renderedLog.startsWith(oldLog)) {
      appendTerminalPre(pre, oldLog, renderedLog.slice(oldLog.length), helpers);
    } else {
      fillTerminalPre(pre, renderedLog, helpers);
    }
    pre.setAttribute('data-rendered-log', renderedLog);
    mutationCount += 1;
  }

  global.SandmanPortalDiff = {
    diffRuns,
    insertRunRow,
    updateRunRowCells,
    removeRunRow,
    setEmpty,
    getRowData,
    getDetailData,
    resetCounters,
    getCounters,
    updateDetailPanelLog,
    highlightJSON,
    highlightTerminalLog,
  };
})(typeof window !== 'undefined' ? window : globalThis);
