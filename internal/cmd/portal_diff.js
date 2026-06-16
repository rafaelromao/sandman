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
    return {
      kind: run.kind || '',
      nameText: run.issueLabel || run.key,
      metaText: h.renderRunMeta(run),
      badgeClass: h.statusClass(run),
      badgeLabel: run.status || (run.kind === 'active' ? 'running' : 'completed'),
      reason: (run.reason === 'auto-select' || run.reason === 'review') ? run.reason : '',
      reasonText: (run.reason === 'auto-select' || run.reason === 'review') ? run.reason : '',
      startedText: h.formatTime(run.startedAt),
      durationText: h.formatDuration(run.duration),
      branchText: h.formatBranch(run),
      issueTitleText: h.formatIssueTitle(run),
      canAbort: opts.abortSupported !== false && h.isRunAbortable(run, opts.abortReservations),
      ariaExpanded: String(opts.expandedKey === run.key),
    };
  }

  function cellOf(row, name) {
    return row.querySelector('[data-cell="' + name + '"]');
  }

  function detailRowOf(body, runKey) {
    return body.querySelector('tr.detail-row[data-detail-for="' + runKey + '"]');
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
    const reasonChip = renderReasonChip(run);
    if (reasonChip) wrap.appendChild(reasonChip);
    const batchMembership = renderBatchMembership(run);
    if (batchMembership) wrap.appendChild(batchMembership);
    td.appendChild(wrap);
  }

  function renderReasonChip(run) {
    const reason = run && run.reason;
    if (reason !== 'auto-select' && reason !== 'review') return null;
    const span = global.document.createElement('span');
    span.classList.add('badge', 'kind-chip', reason);
    span.setAttribute('data-reason', reason);
    const dot = global.document.createElement('span');
    dot.classList.add('dot');
    span.appendChild(dot);
    const label = global.document.createElement('span');
    label.classList.add('badge-label');
    label.textContent = reason;
    span.appendChild(label);
    return span;
  }

  function renderBatchMembership(run) {
    const issues = Array.isArray(run && run.batchIssues) ? run.batchIssues : [];
    if (issues.length <= 1) return null;
    const span = global.document.createElement('span');
    span.classList.add('batch-membership', 'mono');
    span.setAttribute('data-batch-membership', '1');
    const labels = issues.map((n) => '#' + n);
    span.textContent = 'Part of batch: ' + labels.join(', ');
    return span;
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

  function buildMonoCell(td, text, extraClass) {
    if (extraClass) td.classList.add(extraClass);
    td.classList.add('mono');
    td.textContent = text;
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

  function buildDataRow(body, run, opts) {
    const tr = body.insertRow();
    tr.classList.add('run-row');
    if (run.kind) tr.classList.add(run.kind);
    tr.setAttribute('data-action', 'toggle-run');
    tr.setAttribute('data-run-key', run.key);
    tr.setAttribute('role', 'button');
    tr.setAttribute('tabindex', '0');
    tr.setAttribute('aria-expanded', String(opts.expandedKey === run.key));

    const titleCell = makeRowCell('title', tr);
    buildTitleCell(titleCell, run, opts.helpers);

    const badgeCell = makeRowCell('badge', tr);
    buildBadgeCell(badgeCell, run, opts.helpers);

    const startedCell = makeRowCell('started', tr);
    buildMonoCell(startedCell, opts.helpers.formatTime(run.startedAt));

    const durationCell = makeRowCell('duration', tr);
    buildMonoCell(durationCell, opts.helpers.formatDuration(run.duration));

    const issueTitleCell = makeRowCell('issue-title', tr);
    buildMonoCell(issueTitleCell, opts.helpers.formatIssueTitle(run));

    const branchCell = makeRowCell('branch', tr);
    buildMonoCell(branchCell, opts.helpers.formatBranch(run));

    const actionsCell = makeRowCell('actions', tr);
    buildActionsCell(actionsCell, run, opts);

    return tr;
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
      key: run.key || '',
      issueLabel: run.issueLabel || '',
      issueNumber: run.issueNumber || null,
      kind: run.kind || '',
      runId: run.runId || '',
      status: run.status || '',
      startedAt: run.startedAt || null,
      finishedAt: run.finishedAt || null,
      duration: run.duration || '',
      branch: run.branch || '',
      source: helpers.formatSource(run),
      logUrl: run.logUrl || '',
    };
  }

  function detailsJSON(run, helpers) {
    return JSON.stringify(detailsData(run, helpers), null, 2);
  }

  function detailsFingerprint(run, helpers) {
    return detailsJSON(run, helpers);
  }

  function buildLogContent(content, run, helpers) {
    const section = global.document.createElement('section');
    section.classList.add('detail-box', 'tab-pane', 'fill');
    const h3 = global.document.createElement('h3');
    h3.textContent = 'Log';
    section.appendChild(h3);
    const pre = buildLogPre(run, helpers);
    section.appendChild(pre);
    content.appendChild(section);
  }

  function buildEventsContent(content, run, helpers) {
    const section = global.document.createElement('section');
    section.classList.add('detail-box', 'tab-pane', 'fill');
    const h3 = global.document.createElement('h3');
    h3.textContent = 'Events';
    section.appendChild(h3);
    const events = Array.isArray(run.events) ? run.events : [];
    if (!events.length) {
      const empty = global.document.createElement('div');
      empty.classList.add('empty-state');
      empty.textContent = 'No events captured for this run yet.';
      section.appendChild(empty);
    } else {
      const list = global.document.createElement('div');
      list.classList.add('tab-events');
      for (const event of events) {
        const row = global.document.createElement('div');
        row.classList.add('event-row');
        const head = global.document.createElement('div');
        head.classList.add('event-head');
        const type = global.document.createElement('span');
        type.classList.add('event-type');
        type.textContent = event.type || 'event';
        head.appendChild(type);
        const time = global.document.createElement('span');
        time.classList.add('event-time');
        time.textContent = formatEventTime(event.timestamp);
        head.appendChild(time);
        row.appendChild(head);
        if (event.payload && Object.keys(event.payload).length) {
          const pre = global.document.createElement('pre');
          pre.classList.add('event-payload');
          pre.innerHTML = highlightJSON(JSON.stringify(event.payload, null, 2));
          row.appendChild(pre);
        }
        list.appendChild(row);
      }
      section.appendChild(list);
    }
    content.appendChild(section);
  }

  function buildDetailsContent(content, run, helpers) {
    const section = global.document.createElement('section');
    section.classList.add('detail-box', 'tab-pane', 'fill');
    const h3 = global.document.createElement('h3');
    h3.textContent = 'Run details';
    section.appendChild(h3);
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

  function buildDetailContent(panel, run, tabName, helpers) {
    buildTabsRow(panel, run, tabName);
    const content = global.document.createElement('div');
    content.classList.add('detail-content');
    panel.appendChild(content);
    if (tabName === 'log') buildLogContent(content, run, helpers);
    else if (tabName === 'events') {
      buildEventsContent(content, run, helpers);
      content.setAttribute('data-rendered-fingerprint', 'events|' + eventsFingerprint(run));
    } else {
      buildDetailsContent(content, run, helpers);
      content.setAttribute('data-rendered-fingerprint', 'details|' + detailsFingerprint(run, helpers));
    }
  }

  function tabNameFor(run, opts) {
    if (opts.tabs && Object.prototype.hasOwnProperty.call(opts.tabs, run.key)) {
      const t = String(opts.tabs[run.key] || '').trim();
      if (t === 'log' || t === 'events' || t === 'details') return t;
    }
    return 'log';
  }

  function buildDetailRow(body, run, opts) {
    const tr = global.document.createElement('tr');
    tr.classList.add('detail-row');
    tr.setAttribute('data-detail-for', run.key);
    const td = global.document.createElement('td');
    td.setAttribute('colspan', '7');
    const panel = global.document.createElement('div');
    panel.classList.add('detail-panel');
    buildDetailContent(panel, run, tabNameFor(run, opts), opts.helpers);
    td.appendChild(panel);
    tr.appendChild(td);
    body.appendChild(tr);
    return tr;
  }

  function updateDetailContent(detailRow, run, opts) {
    const tabName = tabNameFor(run, opts);
    const tabButtons = detailRow.querySelectorAll('button[data-tab]');
    for (const btn of tabButtons) {
      const want = String(btn.getAttribute('data-tab') === tabName);
      if (btn.getAttribute('aria-pressed') !== want) {
        setAttr(btn, 'aria-pressed', want);
      }
    }
    const content = detailRow.querySelector('.detail-content');
    if (!content) return;
    if (tabName === 'log') {
      const pre = content.querySelector('pre[data-scroll-key]');
      const newLog = run.log && String(run.log).trim() ? run.log : 'No log file yet.';
      if (pre) {
        const oldLog = pre.getAttribute('data-rendered-log') || '';
        if (oldLog === newLog) {
          return;
        }
        if (oldLog && oldLog !== 'No log file yet.' && newLog === 'No log file yet.') {
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
      buildLogContent(content, run, opts.helpers);
      mutationCount += 1;
      return;
    }
    let fingerprint = tabName;
    if (tabName === 'events') {
      fingerprint = 'events|' + eventsFingerprint(run);
    } else {
      fingerprint = 'details|' + detailsFingerprint(run, opts.helpers);
    }
    if (content.getAttribute('data-rendered-fingerprint') === fingerprint) {
      return;
    }
    while (content.firstChild) content.removeChild(content.firstChild);
    if (tabName === 'events') {
      buildEventsContent(content, run, opts.helpers);
    } else {
      buildDetailsContent(content, run, opts.helpers);
    }
    content.setAttribute('data-rendered-fingerprint', fingerprint);
    mutationCount += 1;
  }

  function insertRunRow(body, run, opts) {
    const tr = buildDataRow(body, run, opts);
    setRowData(tr, run);
    let detailTr = null;
    if (opts.expandedKey === run.key) {
      detailTr = buildDetailRow(body, run, opts);
      setDetailData(detailTr, run);
    }
    return { row: tr, detailRow: detailTr };
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

  function updateTitleCell(cell, oldSnap, newSnap) {
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
    reconcileReasonChip(wrap, newSnap.reason, newSnap.reasonText);
  }

  function reconcileReasonChip(wrap, reason, reasonText) {
    const existing = wrap.querySelector('.kind-chip');
    const wantChip = reason === 'auto-select' || reason === 'review';
    if (!wantChip) {
      if (existing) {
        wrap.removeChild(existing);
        mutationCount += 1;
      }
      return;
    }
    if (!existing) {
      const chip = buildReasonChip(reason, reasonText);
      // Insert after name and meta (children[0] and [1]) so the
      // chip sits before any batch-membership child.
      const insertBefore = wrap.children[2] || null;
      if (insertBefore) {
        wrap.insertBefore(chip, insertBefore);
      } else {
        wrap.appendChild(chip);
      }
      mutationCount += 1;
      return;
    }
    if (existing.getAttribute('data-reason') !== reason) {
      setClass(existing, existing.getAttribute('data-reason'), false);
      setClass(existing, reason, true);
      existing.setAttribute('data-reason', reason);
      mutationCount += 1;
    }
    const label = existing.querySelector('.badge-label') || existing.children[1];
    if (label && label.textContent !== reasonText) {
      setText(label, reasonText);
      mutationCount += 1;
    }
  }

  function buildReasonChip(reason, reasonText) {
    const span = global.document.createElement('span');
    span.classList.add('badge', 'kind-chip', reason);
    span.setAttribute('data-reason', reason);
    const dot = global.document.createElement('span');
    dot.classList.add('dot');
    span.appendChild(dot);
    const label = global.document.createElement('span');
    label.classList.add('badge-label');
    label.textContent = reasonText;
    span.appendChild(label);
    return span;
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

  function updateMonoCell(cell, newText) {
    if (cell.textContent !== newText) {
      setText(cell, newText);
    }
  }

  function updateActionsCell(cell, run, opts) {
    const wantAbort = reserveAbortButton(run, opts);
    const hasAbort = !!cell.querySelector('button[data-action="abort-run"]');
    if (wantAbort === hasAbort) {
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
    const currentAria = row.getAttribute('aria-expanded');
    const desiredAria = newSnap.ariaExpanded;
    if (currentAria !== desiredAria) {
      setAttr(row, 'aria-expanded', desiredAria);
    }

    const titleCell = cellOf(row, 'title');
    if (titleCell) updateTitleCell(titleCell, oldSnap, newSnap);

    const badgeCell = cellOf(row, 'badge');
    if (badgeCell) updateBadgeCell(badgeCell, oldSnap, newSnap);

    const startedCell = cellOf(row, 'started');
    if (startedCell) updateMonoCell(startedCell, newSnap.startedText);

    const durationCell = cellOf(row, 'duration');
    if (durationCell) updateMonoCell(durationCell, newSnap.durationText);

    const branchCell = cellOf(row, 'branch');
    if (branchCell) updateMonoCell(branchCell, newSnap.branchText);

    const issueTitleCell = cellOf(row, 'issue-title');
    if (issueTitleCell) updateMonoCell(issueTitleCell, newSnap.issueTitleText);

    const actionsCell = cellOf(row, 'actions');
    if (actionsCell) updateActionsCell(actionsCell, newRun, opts);

    setRowData(row, newRun);
    return { mutated: mutationCount > before, cells: mutationCount - before };
  }

  function removeRunRow(body, key) {
    const dataRow = dataRowOf(body, key);
    const detail = detailRowOf(body, key);
    let removed = 0;
    if (dataRow) {
      body.removeChild(dataRow);
      clearRowData(dataRow);
      removed += 1;
    }
    if (detail) {
      body.removeChild(detail);
      clearDetailData(detail);
      removed += 1;
    }
    return removed;
  }

  function setEmpty(body, html) {
    const placeholder = global.document.createElement('tr');
    const td = global.document.createElement('td');
    td.setAttribute('colspan', '7');
    td.innerHTML = html;
    placeholder.appendChild(td);
    body.replaceChildren(placeholder);
  }

  function diffRuns(body, runs, opts) {
    const before = mutationCount;
    const newKeys = new Set();
    const newRuns = new Map();
    for (const run of runs) {
      if (!run || !run.key) continue;
      newKeys.add(run.key);
      newRuns.set(run.key, run);
    }

    const childList = Array.from(body.children);
    for (const child of childList) {
      if (!child.getAttribute) continue;
      if (child.getAttribute('data-run-key')) continue;
      if (child.getAttribute('data-detail-for')) continue;
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
        body.removeChild(dataRow);
        clearRowData(dataRow);
        removed += 1;
        continue;
      }
      const oldRun = getRowData(dataRow) || newRun;
      const r = updateRunRowCells(dataRow, oldRun, newRun, opts);
      if (r.mutated) updated += 1;

      const wantDetail = opts.expandedKey === key;
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
        updateDetailContent(detail, newRun, opts);
        setDetailData(detail, newRun);
      }
    }

    for (const [key, run] of newRuns) {
      if (seen.has(key)) continue;
      insertRunRow(body, run, opts);
      inserted += 1;
    }

    let pos = 0;
    for (let i = 0; i < runs.length; i += 1) {
      const run = runs[i];
      if (!run || !run.key) continue;
      const dataRow = dataRowOf(body, run.key);
      if (!dataRow) continue;
      if (body.children[pos] !== dataRow) {
        body.insertBefore(dataRow, body.children[pos] || null);
      }
      pos += 1;
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
  };
})(typeof window !== 'undefined' ? window : globalThis);
