(function (global) {
  const rowData = new WeakMap();
  const detailData = new WeakMap();
  let mutationCount = 0;
  let innerHTMLAssignmentCount = 0;

  // Per-subject cache of the rendered log pane (the <section>/<pre> built by
  // buildLogContent). Building a log pane tokenizes + parses the whole log
  // into thousands of nodes on the main thread; doing that on every tab
  // round-trip (Log -> Events -> Log) froze the UI. Instead of discarding
  // the pane when leaving the Log tab, updateDetailContent detaches it here
  // and re-attaches the same nodes on return, so the round-trip is O(1).
  // Keyed by subject run-id; bounded by clearing on overflow.
  const logPaneCache = new Map();
  const LOG_PANE_CACHE_LIMIT = 8;
  const shellCommands = 'gh|git|go|npm|yarn|node|npx|ls|echo|cat|make|mkdir|rm|cp|mv|find|grep|sed|awk|curl|wget|pwd|cd|printf|tar|unzip|jq|chmod|ln|whoami|sort|head|tail|less|more|touch|ssh|scp';
  const actionVerbs = 'Read|Edit|Glob|Skill|Bash|Write|Task|Grep|Search';
  function takeCachedLogPane(subjectValue) {
    const pane = logPaneCache.get(subjectValue);
    if (pane) logPaneCache.delete(subjectValue);
    return pane || null;
  }
  function storeCachedLogPane(subjectValue, pane) {
    if (!subjectValue || !pane) return;
    if (logPaneCache.size >= LOG_PANE_CACHE_LIMIT) logPaneCache.clear();
    logPaneCache.set(subjectValue, pane);
  }

  // tokenizeForCache builds the rendered log pane (section + pre) for a
  // single run and stashes it in the same logPaneCache Map the live pane-
  // swap path reads from. Issue #1564: pre-warm during idle periods so
  // the next click on this run hits the cache instead of the cold path.
  // No-op when the subject is already cached (idempotent across repeat
  // calls) and when the run's log is empty.
  function tokenizeForCache(run, helpers) {
    const subjectValue = subjectRunValue(run);
    if (!subjectValue) return null;
    if (logPaneCache.has(subjectValue)) return logPaneCache.get(subjectValue) || null;
    const log = run && run.log && String(run.log).trim() ? run.log : '';
    if (!log) return null;
    const content = global.document.createElement('div');
    buildLogContent(content, run, helpers);
    const pane = content.firstChild;
    if (!pane) return null;
    storeCachedLogPane(subjectValue, pane);
    return pane;
  }

  // prewarmLogPaneCache selects the top-N most-likely-to-be-clicked runs
  // and feeds each through tokenizeForCache. Selection order (issue
  // #1564 spec): active runs first, then completed; within each group,
  // most-recently-chatted first (lastOutputAt desc, falling back to
  // startedAt, then 0). Capped at opts.topN (default 3). Returns the
  // number of runs newly cached; idempotent across repeat calls because
  // tokenizeForCache is a no-op when the subject is already cached.
  // opts.disabled = true short-circuits the work entirely (issue #1564
  // open question: power-user opt-out for small screens).
  const PREWARM_DEFAULT_TOP_N = 3;
  function prewarmLogPaneCache(runs, helpers, opts) {
    const options = opts || {};
    if (options.disabled) return 0;
    const topN = Number.isFinite(options.topN) && options.topN >= 0 ? options.topN : PREWARM_DEFAULT_TOP_N;
    if (!Array.isArray(runs) || topN === 0) return 0;
    const candidates = [];
    for (const run of runs) {
      if (!run) continue;
      const subjectValue = subjectRunValue(run);
      if (!subjectValue) continue;
      const log = run.log && String(run.log).trim();
      if (!log) continue;
      const ts = Date.parse(run.lastOutputAt || run.startedAt || '') || 0;
      candidates.push({ run, subjectValue, kind: run.kind === 'active' ? 0 : 1, ts });
    }
    candidates.sort((a, b) => {
      if (a.kind !== b.kind) return a.kind - b.kind;
      return b.ts - a.ts;
    });
    let added = 0;
    for (let i = 0; i < candidates.length && added < topN; i += 1) {
      if (logPaneCache.has(candidates[i].subjectValue)) continue;
      const pane = tokenizeForCache(candidates[i].run, helpers);
      if (pane) added += 1;
    }
    return added;
  }

  // runPrewarmIfIdle is the integration point between
  // requestIdleCallback and prewarmLogPaneCache. Issue #1564 spec:
  // when the idle callback fires with didTimeout: true (or no
  // timeRemaining), the work is skipped — we never want prewarm to
  // block the main thread. The idle arg is the deadline object the
  // browser hands to the requestIdleCallback callback; tests can
  // construct one directly to drive the skip / proceed paths.
  function runPrewarmIfIdle(runs, helpers, opts, idle) {
    if (!idle || idle.didTimeout) return 0;
    if (typeof idle.timeRemaining === 'function' && idle.timeRemaining() <= 0) return 0;
    return prewarmLogPaneCache(runs, helpers, opts);
  }

  function markCachedLogPaneForBottom(pane) {
    if (!pane || !pane.querySelector) return;
    const pre = pane.querySelector('pre[data-scroll-key]');
    if (!pre) return;
    pre.setAttribute('data-scroll-force-bottom', '1');
  }

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

  // isRowExpandable gates the toggle-row affordance on the row's status.
  // Queued and blocked rows are wait-state placeholders: clicking them
  // would open a detail panel that has no real logs and, worse, has been
  // observed to surface sibling review runs' logs because the synthetic
  // detail-row id collides with whatever happens to be cached. Removing
  // the expand affordance here cuts off that path and lets the existing
  // portal.html click/keydown selectors (which key off
  // tr[data-action="toggle-run"]) skip the row entirely. All other
  // statuses, including terminal ones, remain expandable so the user can
  // still inspect finished runs.
  function isRowExpandable(run) {
    if (!run) return true;
    const s = String(run.status || '').toLowerCase();
    if (s === 'queued' || s === 'blocked') return false;
    return true;
  }

  function snapshotCellState(run, opts) {
    const h = opts.helpers;
    const snap = {
      kind: run.kind || '',
      archived: !!(run && run.archived),
      unavailable: !!(run && run.unavailable),
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
      ariaExpanded: String(matchesExpandedSubject(run, opts.expandedKey, opts) && isRowExpandable(run)),
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
    if (run.lastRetryReason) {
      meta.title = 'Last retry: ' + run.lastRetryReason;
    }
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
  // compute()). Returns null for non-active rows, terminal statuses, or rows
  // younger than the line threshold, so healthy quiet agents are not flagged.
  // The warn tier escalates the line to the --warning token past the second
  // threshold; no new color is introduced.
  function stalenessOf(run) {
    if (!run || run.kind !== 'active' || !run.lastOutputAt) return null;
    if (run.status !== 'running' && run.status !== 'reviewing' && run.status !== 'auto-selecting') return null;
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
    tr.classList.add('row-added');
    if (run.kind) tr.classList.add(run.kind);
    if (run.archived) tr.classList.add('row-archived');
    if (run.unavailable) tr.classList.add('row-unavailable');
    const expandable = isRowExpandable(run);
    if (!expandable) {
      tr.classList.add('row-non-expandable');
    }
    tr.setAttribute('data-run-key', run.key);
    tr.setAttribute('id', rowIDForKey(run.key));
    if (expandable) {
      tr.setAttribute('data-action', 'toggle-run');
      tr.setAttribute('role', 'button');
      tr.setAttribute('tabindex', '0');
      tr.setAttribute('aria-controls', detailIDForKey(run.key));
    }
    tr.setAttribute('aria-expanded', String(matchesExpandedSubject(run, opts.expandedKey, opts) && expandable));

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
    if (!isRowExpandable(run)) return '';
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
      if (!isRowExpandable(candidate)) continue;
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

  function createSubjectSelector(rowRun, subjectRun, opts) {
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
    return row;
  }

  function buildSubjectSelector(panel, rowRun, subjectRun, opts) {
    const row = createSubjectSelector(rowRun, subjectRun, opts);
    if (row) panel.appendChild(row);
    return row;
  }

  function buildLogPre(run, helpers) {
    const log = run.log && String(run.log).trim() ? run.log : '';
    const pre = global.document.createElement('pre');
    pre.classList.add('terminal-log');
    pre.setAttribute('data-scroll-key', run.key);
    fillTerminalPre(pre, log, helpers);
    return pre;
  }

  const ASYNC_CHUNK_THRESHOLD = 16 * 1024;
  const CHUNK_SIZE_LINES = 100;

  function fillTerminalPre(pre, text, helpers) {
    const value = String(text == null ? '' : text);
    while (pre.firstChild) pre.removeChild(pre.firstChild);
    pre.setAttribute('data-rendering-log', value);
    if (value.length < ASYNC_CHUNK_THRESHOLD) {
      const html = helpers.renderTerminalContent(value);
      const scratch = global.document.createElement('div');
      scratch.innerHTML = String(html || '');
      const frag = global.document.createDocumentFragment();
      let node = scratch.firstChild;
      while (node) {
        frag.appendChild(node);
        node = scratch.firstChild;
      }
      pre.appendChild(frag);
      pre.setAttribute('data-rendered-log', value);
      pre.removeAttribute('data-rendering-log');
      return;
    }
    const gen = (parseInt(pre.getAttribute('data-render-gen') || '0', 10) + 1) | 0;
    pre.setAttribute('data-render-gen', String(gen));
    const lines = value.split('\n');
    let lineIndex = 0;
    const htmlParts = [];
    function processChunk() {
      if (String(gen) !== pre.getAttribute('data-render-gen')) return;
      const end = Math.min(lineIndex + CHUNK_SIZE_LINES, lines.length);
      while (lineIndex < end) {
        htmlParts.push(helpers.renderTerminalContent(lines[lineIndex]));
        lineIndex++;
      }
      if (lineIndex < lines.length) {
        global.setTimeout(processChunk, 0);
      } else {
        const html = htmlParts.join('\n');
        const scratch = global.document.createElement('div');
        scratch.innerHTML = String(html || '');
        const frag = global.document.createDocumentFragment();
        let node = scratch.firstChild;
        while (node) {
          frag.appendChild(node);
          node = scratch.firstChild;
        }
        pre.appendChild(frag);
        if (String(gen) !== pre.getAttribute('data-render-gen')) return;
        pre.setAttribute('data-rendered-log', value);
        pre.removeAttribute('data-rendering-log');
      }
    }
    global.setTimeout(processChunk, 0);
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

  function escapeHTML(value) {
    return String(value == null ? '' : value)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function wrapToken(className, value) {
    return '<span class="' + className + '">' + escapeHTML(value) + '</span>';
  }

  function stripAnsi(value) {
    return String(value == null ? '' : value).replace(/\x1b\[[0-9;]*[A-Za-z]/g, '');
  }

  const Prism = global.Prism || (global.Prism = { languages: {}, util: {}, Token: { stringify: function(t) { var cls = ['token', t.type]; var s = '<span class="' + cls.join(' ') + '">' + t.content + '</span>'; return s; } } });

  if (Prism && Prism.languages && Prism.tokenize && Prism.util.encode) {
    const escapeHTMLPrism = function(value) {
      return String(value == null ? '' : value)
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    };

    const baseEncode = Prism.util.encode;
    Prism.util.encode = function(t) {
      if (typeof t === 'string') return escapeHTMLPrism(t);
      return baseEncode.call(Prism.util.encode, t);
    };

    const originalStringify = Prism.Token.stringify;
    Prism.Token.stringify = function(token) {
      const html = originalStringify.call(Prism.Token.stringify, token);
      return html.replace(/class="token /g, 'class="');
    };

    Prism.languages['sandman-log'] = {
      'term-time': { pattern: /\b\d{2}:\d{2}:\d{2}\b/, greedy: true },
      'term-command': {
        pattern: new RegExp('^\\$\\s+(?:' + shellCommands + ')\\b'),
        inside: {
          'term-prompt': /^\$\s+/,
          'term-command': new RegExp('\\b(?:' + shellCommands + ')\\b')
        },
        greedy: true
      },
      'term-prompt': { pattern: /^\$ /, greedy: true },
      'term-action': {
        pattern: new RegExp('^\\s*[→←✱]\\s+(?:' + actionVerbs + ')\\b'),
        inside: {
          'term-tool': /^\s*[→←✱]\s+/,
          'term-action': new RegExp('\\b(?:' + actionVerbs + ')\\b')
        },
        greedy: true
      },
      'term-tool': { pattern: /^(\s*)([→←✱])\s/, lookbehind: true, greedy: true },
      'term-mark': { pattern: /^--- (?:run|retry) \d+\/\d+ ---$/, greedy: true },
      'term-heading': { pattern: /^(?:```[A-Za-z0-9_+-]*|lang=[A-Za-z0-9_+-]+|> build.*|#{1,6} .*|@@.*@@)$/, greedy: true },
      'term-pass': { pattern: /^\*\*APPROVED(?:\s+with\s+comments)?\*\*|^--- PASS:|^--- FAIL:|^FAIL\s+\S|^ok\s+\S|^PASSED$|^FAILED$|^Passed!|^Failed!|^Tests run:.*Failures: 0|^\d+ tests?, 0 failures|^\d+ examples?, 0 failures|^test result: ok|^test result: FAILED|^✓|^✕/, greedy: true },
      'term-fail': { pattern: /^\*\*CHANGES_REQUESTED\*\*|^--- FAIL:|^FAIL\s+\S|^FAILED$|^Failed!|^Tests run:.*Failures: [1-9]|^\d+ tests?, \d+ failures|^\d+ examples?, [1-9]\d* failures?|^test result: FAILED/, greedy: true },
      'term-url': { pattern: /^https?:\/\/[^\s<&]+/, greedy: true },
      'term-path': { pattern: /^[\/\w.\-]+\.(?:go|js|ts|jsx|tsx|py|rs|rb|java|cs|ex|exs|c|cpp|h|hpp|zig|mod|sum):\d+|^\+\+\+ .*|^\-\-\- .*/, greedy: true },
      'term-issue': { pattern: /\B#\d+\b|PR#?\d+\b/, greedy: true },
      'term-hash': { pattern: /\b[0-9a-fA-F]{7,}\b/, greedy: true },
      'term-subagent': { pattern: /\b(?:sub-agent|subagent)\b/, greedy: true },
      'term-todo-done': { pattern: /^\[✓\]|^\[✔\]/, greedy: true },
      'term-todo-active': { pattern: /^\[•\]/, greedy: true },
      'term-todo-pending': { pattern: /^(\s*)\[ \]/, lookbehind: true, greedy: true },
    };
  } else {
    Prism.util.encode = escapeHTML;
    Prism.highlight = function(text, grammar) {
      return renderWithGrammar(text, grammar);
    };
    Prism.languages['sandman-log'] = terminalGrammar;
  }

  function terminalGrammar() {
    return [
      { regex: /^(?:\*\*CHANGES_REQUESTED\*\*)/, render: (m) => wrapToken('term-fail', m[0]) },
      { regex: /^(?:\*\*APPROVED with comments\*\*)/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^(?:\*\*APPROVED\*\*)/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^```[A-Za-z0-9_+-]*$/, render: (m) => wrapToken('term-heading', m[0]) },
      { regex: /^lang=[A-Za-z0-9_+-]+$/, render: (m) => wrapToken('term-heading', m[0]) },
      { regex: /^(?:--- PASS:.*)$/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^(?:--- FAIL:.*)$/, render: (m) => wrapToken('term-fail', m[0]) },
      { regex: /^(?:FAIL\s+\S+)/, render: (m) => wrapToken('term-fail', m[0]) },
      { regex: /^(?:ok\s+\S+)/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^(?:PASSED)\b/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^(?:FAILED)\b/, render: (m) => wrapToken('term-fail', m[0]) },
      { regex: /^(?:✓[^\n]*)/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^(?:✕[^\n]*)/, render: (m) => wrapToken('term-fail', m[0]) },
      { regex: /^(?:Passed!.*$)/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^(?:Failed!.*$)/, render: (m) => wrapToken('term-fail', m[0]) },
      { regex: /^(?:Tests run:.*Failures: [1-9])/, render: (m) => wrapToken('term-fail', m[0]) },
      { regex: /^(?:Tests run:.*Failures: 0)/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^(?:\d+ tests?, \d+ failures?)/, render: (m) => /0 failures/.test(m[0]) ? wrapToken('term-pass', m[0]) : wrapToken('term-fail', m[0]) },
      { regex: /^(?:\d+ examples?, 0 failures)/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^(?:\d+ examples?, [1-9]\d* failures?)/, render: (m) => wrapToken('term-fail', m[0]) },
      { regex: /^(?:test result: ok.*)/, render: (m) => wrapToken('term-pass', m[0]) },
      { regex: /^(?:test result: FAILED.*)/, render: (m) => wrapToken('term-fail', m[0]) },
      { regex: new RegExp('^(\\$\\s+)(' + shellCommands + ')\\b'), render: (m) => wrapToken('term-prompt', m[1]) + wrapToken('term-command', m[2]) },
      { regex: /^(?:\$ )/, render: (m) => wrapToken('term-prompt', m[0]) },
      { regex: /^(\s*)([→←✱])(\s+)(Read|Edit|Glob|Skill|Bash|Write|Task|Grep|Search)\b/, render: (m) => escapeHTML(m[1]) + wrapToken('term-tool', m[2] + m[3]) + wrapToken('term-action', m[4]) },
      { regex: /^--- (?:run|retry) \d+\/\d+ ---$/, render: (m) => wrapToken('term-mark', m[0]) },
      { regex: /^(> build.*)$/, render: (m) => wrapToken('term-heading', m[1]) },
      { regex: /^(#{1,6} .*?)$/, render: (m) => wrapToken('term-heading', m[1]) },
      { regex: /^(?:\[✓\]|\[✔\])/, render: (m) => wrapToken('term-todo-done', m[0]) },
      { regex: /^(?:\[•\])/, render: (m) => wrapToken('term-todo-active', m[0]) },
      { regex: /^(\s*)\[ \]/, render: (m) => escapeHTML(m[1]) + wrapToken('term-todo-pending', '[ ]') },
      { regex: /^(https?:\/\/[^\s<&]+)/, render: (m) => wrapToken('term-url', m[0]) },
      { regex: /^([\/\w.\-]+\.(?:go|js|ts|jsx|tsx|py|rs|rb|java|cs|ex|exs|c|cpp|h|hpp|zig|mod|sum):\d+)/, render: (m) => wrapToken('term-path', m[1]) },
      { regex: /^(?:\+\+\+ .*?)$/, render: (m) => wrapToken('term-path', m[0]) },
      { regex: /^(?:--- .*?)$/, render: (m) => wrapToken('term-path', m[0]) },
      { regex: /^(?:@@.*@@)$/, render: (m) => wrapToken('term-heading', m[0]) },
      { regex: /^(\d{2}:\d{2}:\d{2})\b/, render: (m) => wrapToken('term-time', m[1]) },
      { regex: /^(?:#\d+|PR#?\d+)\b/, render: (m) => wrapToken('term-issue', m[0]) },
      { regex: /^(?:[0-9a-fA-F]{7,})\b/, render: (m) => wrapToken('term-hash', m[0]) },
      { regex: /^\b(?:sub-agent|subagent)\b/, render: (m) => wrapToken('term-subagent', m[0]) },
      { regex: /^(?:gh|git|go|npm|yarn|node|npx|ls|echo|cat|make|mkdir|rm|cp|mv|find|grep|sed|awk|curl|wget|pwd|cd|printf|tar|unzip|jq|chmod|ln|whoami|sort|head|tail|less|more|touch|ssh|scp)\b/, render: (m) => wrapToken('term-command', m[0]) },
    ];
  }

  function renderWithGrammar(text, grammar) {
    const rules = (typeof grammar === 'function' ? grammar() : grammar);
    const value = String(text || '');
    if (!value) return '';
    const lines = value.split('\n');
    return lines.map((line) => {
      const raw = stripAnsi(line);
      return renderGrammarLine(raw, rules);
    }).join('\n');
  }

  function renderGrammarLine(line, grammar) {
    if (!line) return '';
    const rules = Array.isArray(grammar) && grammar.length ? grammar : terminalGrammar();
    let out = '';
    let index = 0;
    while (index < line.length) {
      let matched = null;
      for (const rule of rules) {
        const slice = line.slice(index);
        const match = rule.regex.exec(slice);
        if (match && match.index === 0) {
          matched = { rule, match };
          break;
        }
      }
      if (matched) {
        out += matched.rule.render(matched.match);
        index += matched.match[0].length;
        continue;
      }
      out += escapeHTML(line.charAt(index));
      index += 1;
    }
    return out;
  }

  // Memoize the expensive full-log tokenization. highlightTerminalLog is
  // called with the entire (up to 64KB) log every time a log pane is built
  // (row open / subject switch / tab return). The same log text yields the
  // same HTML, so cache the result keyed by length + djb2 hash with a full
  // value guard against collisions. Only large inputs are cached so the
  // per-line SSE path (small strings) cannot evict the costly entries.
  //
  // LRU eviction: Map preserves insertion order, so we delete+reinsert on
  // access to move the entry to the tail, and evict the head (oldest) when
  // the limit is reached.
  const highlightCache = new Map();
  const HIGHLIGHT_CACHE_LIMIT = 8;
  const HIGHLIGHT_CACHE_MIN_LEN = 4096;
  function djb2(s) {
    let h = 5381;
    for (let i = 0; i < s.length; i += 1) {
      h = ((h << 5) + h + s.charCodeAt(i)) | 0;
    }
    return (h >>> 0).toString(36);
  }
  function computeTerminalHighlight(value) {
    if (Prism && Prism.languages && Prism.languages['sandman-log'] && Prism.tokenize) {
      return renderWithGrammarPrism(value);
    }
    return Prism.highlight(value, terminalGrammar);
  }
  function highlightTerminalLog(text) {
    const value = String(text == null ? '' : text);
    if (!value) return '';
    if (value.length >= HIGHLIGHT_CACHE_MIN_LEN) {
      const key = value.length + ':' + djb2(value);
      const hit = highlightCache.get(key);
      if (hit !== undefined && hit.value === value) {
        // Move to tail (LRU touch) so the LRU evicts the truly oldest
        // entry instead of the most-recently-accessed one.
        highlightCache.delete(key);
        highlightCache.set(key, hit);
        return hit.html;
      }
      const html = computeTerminalHighlight(value);
      if (highlightCache.size >= HIGHLIGHT_CACHE_LIMIT) {
        const firstKey = highlightCache.keys().next().value;
        if (firstKey !== undefined) highlightCache.delete(firstKey);
      }
      highlightCache.set(key, { value: value, html: html });
      return html;
    }
    return computeTerminalHighlight(value);
  }

  function renderWithGrammarPrism(text) {
    const value = String(text || '');
    if (!value) return '';
    const lines = value.split('\n');
    return lines.map((line) => {
      const raw = stripAnsi(line);
      if (/^```[A-Za-z0-9_+-]*$/.test(raw) || /^lang=[A-Za-z0-9_+-]+$/.test(raw)) {
        return '<span class="term-heading">' + escapeHTML(raw) + '</span>';
      }
      const grammar = Prism.languages['sandman-log'];
      const tokens = Prism.tokenize(raw, grammar);
      return tokens.map(token => {
        if (typeof token === 'string') return escapeHTML(token);
        const type = token.type.replace(/^term-/, 'term-');
        return '<span class="' + type + '">' + escapeHTMLPrism(token.content) + '</span>';
      }).join('');
    }).join('\n');
  }

  function escapeHTMLPrism(value) {
    return String(value == null ? '' : value)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
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
      pre.setAttribute('data-rendered-log', oldLog + newSuffix);
      pre.removeAttribute('data-rendering-log');
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
    pre.setAttribute('data-rendered-log', oldLog + newSuffix);
    pre.removeAttribute('data-rendering-log');
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
    const issueNumber = Number.isInteger(run.issueNumber) && run.issueNumber > 0 ? run.issueNumber : null;
    return {
      runId: run.runId || '',
      issueNumber: issueNumber,
      issueTitle: run.issueTitle || '',
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

  // Cheap poll-time fingerprint for the Events tab. The expensive
  // `eventsFingerprint` re-stringifies every event payload; that costs
  // O(events) on every poll (every 2s) even when the events are stable.
  // The poll path only needs a hash to skip rebuilds, so we use a
  // constant-time triplet: length + first timestamp + last timestamp.
  // Trade-off: payload-only mutations inside the same length+timestamp
  // envelope rebuild more often than ideal, but the rebuilt content is
  // byte-identical (issue #1562, accepted out-of-scope).
  function cheapEventsFingerprint(run) {
    const events = Array.isArray(run && run.events) ? run.events : [];
    if (!events.length) return '0';
    const first = events[0] || {};
    const last = events[events.length - 1] || {};
    const firstTs = first.timestamp || '';
    const lastTs = last.timestamp || '';
    if (events.length === 1) return firstTs + ':' + firstTs;
    return events.length + ':' + firstTs + ':' + lastTs;
  }

  // Cheap poll-time fingerprint for the Details tab. Same trade-off as
  // cheapEventsFingerprint: ignore payload-only mutations of the
  // details object; rely on the scalar fingerprint changing to trigger
  // a rebuild (which is still byte-identical via buildDetailsContent).
  function cheapDetailsFingerprint(run, helpers) {
    const issueNumber = run && run.issueNumber != null ? run.issueNumber : '';
    const peers = batchPeers(run);
    return [
      issueNumber,
      run && run.issueTitle != null ? run.issueTitle : '',
      run && run.branch != null ? run.branch : '',
      run && run.batchKey != null ? run.batchKey : '',
      run && run.logPath != null ? run.logPath : '',
      peers.length,
      peers.join(','),
    ].join(':');
  }

  function buildLogContent(content, run, helpers) {
    const section = global.document.createElement('section');
    section.classList.add('detail-box', 'tab-pane', 'fill');
    const log = run.log && String(run.log).trim() ? run.log : '';
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
    const subjectValue = subjectRunValue(subjectRun);
    const subjectFp = subjectValue + '|' + subjectFingerprint(subjectRun, opts);
    const subjectPicker = buildSubjectSelector(panel, rowRun, subjectRun, opts);
    if (subjectPicker) panel.appendChild(subjectPicker);
    buildTabsRow(panel, rowRun, tabName);
    const content = global.document.createElement('div');
    content.classList.add('detail-content');
    content.setAttribute('data-rendered-subject-fingerprint', subjectFp);
    panel.appendChild(content);
    if (tabName === 'log') {
      const cached = takeCachedLogPane(subjectValue);
      if (cached) {
        content.appendChild(cached);
        markCachedLogPaneForBottom(cached);
      } else {
        buildLogContent(content, subjectRun, helpers);
      }
    } else if (tabName === 'events') {
      buildEventsContent(content, subjectRun, helpers);
      // Cheap triplet (issue #1562). Matches the poll-path write in
      // updateDetailContent so the first poll sees an unchanged attr
      // and skips the rebuild branch.
      content.setAttribute('data-rendered-fingerprint', 'events|' + cheapEventsFingerprint(subjectRun) + '|subjects:' + subjectFp);
    } else {
      buildDetailsContent(content, subjectRun, helpers);
      // Cheap scalar+peers string (issue #1562). Matches the poll-path
      // write in updateDetailContent so the first poll sees an
      // unchanged attr and skips the rebuild branch.
      content.setAttribute('data-rendered-fingerprint', 'details|' + cheapDetailsFingerprint(subjectRun, helpers) + '|subjects:' + subjectFp);
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
    const subjectValue = subjectRunValue(subjectRun);
    const subjectPicker = detailRow.querySelector('.detail-subject-picker');
    if (subjectPicker) {
      const panel = detailRow.querySelector('.detail-panel');
      const currentValues = Array.from(subjectPicker.querySelectorAll('option')).map((option) => String(option.getAttribute('value') || ''));
      const desiredValues = subjectRunsFor(run, opts).map((candidate) => subjectRunValue(candidate));
      const selectorChanged = currentValues.length !== desiredValues.length || currentValues.some((value, index) => value !== desiredValues[index]);
      if (selectorChanged && panel) {
        const nextPicker = createSubjectSelector(run, subjectRun, opts);
        if (nextPicker) {
          panel.insertBefore(nextPicker, subjectPicker);
          panel.removeChild(subjectPicker);
          mutationCount += 1;
        }
      }
    }
    const tabButtons = detailRow.querySelectorAll('button[data-tab]');
    for (const btn of tabButtons) {
      const want = String(btn.getAttribute('data-tab') === tabName);
      if (btn.getAttribute('aria-pressed') !== want) {
        setAttr(btn, 'aria-pressed', want);
      }
    }
    const subjectFp = subjectRunValue(subjectRun) + '|' + subjectFingerprint(subjectRun, opts);
    const subjectSelect = detailRow.querySelector('select[data-action="set-subject"]');
    if (subjectSelect && subjectSelect.value !== subjectValue) {
      subjectSelect.value = subjectValue;
    }
    const content = detailRow.querySelector('.detail-content');
    if (!content) return;
    if (tabName === 'log') {
      // While the Log tab is being fed by a live SSE stream
      // (opts.streamingKeys), the stream owns this <pre>; the poll path
      // must not overwrite it with its 64KB socket snapshot every 2s.
      if (opts.streamingKeys && opts.streamingKeys.has(run.key) && content.querySelector('pre[data-scroll-key]')) {
        return;
      }
      const newLog = subjectRun.log && String(subjectRun.log).trim() ? subjectRun.log : '';
      let pre = content.querySelector('pre[data-scroll-key]');
      const renderedSubjectFp = content.getAttribute('data-rendered-subject-fingerprint') || '';
      const renderedSubjectValue = renderedSubjectFp ? renderedSubjectFp.split('|')[0] : '';
      if (pre && renderedSubjectValue && renderedSubjectValue !== subjectValue) {
        // Subject switch while staying on the Log tab: preserve the outgoing
        // pane under the old subject, then mount the incoming subject's cached
        // pane (or build a fresh one). Updating the outgoing <pre> in place
        // would destroy the cached pane and keep subject switches on the full
        // rebuild path.
        const outgoingPane = (pre.parentNode && pre.parentNode !== content) ? pre.parentNode : pre;
        storeCachedLogPane(renderedSubjectValue, outgoingPane);
        while (content.firstChild) content.removeChild(content.firstChild);
        content.removeAttribute('data-rendered-fingerprint');
        const cached = takeCachedLogPane(subjectValue);
        if (cached) {
          content.appendChild(cached);
          pre = cached.querySelector('pre[data-scroll-key]');
          markCachedLogPaneForBottom(cached);
          mutationCount += 1;
        } else {
          buildLogContent(content, subjectRun, opts.helpers);
          content.setAttribute('data-rendered-subject-fingerprint', subjectFp);
          mutationCount += 1;
          return;
        }
      }
      if (!pre) {
        // No log pane is mounted (e.g. returning from Events/Details). Reuse
        // the per-subject cached pane instead of re-tokenizing + re-parsing
        // the whole log. Clear any stale pane first so content holds exactly
        // one pane.
        while (content.firstChild) content.removeChild(content.firstChild);
        content.removeAttribute('data-rendered-fingerprint');
        const cached = takeCachedLogPane(subjectValue);
        if (cached) {
          content.appendChild(cached);
          pre = cached.querySelector('pre[data-scroll-key]');
          markCachedLogPaneForBottom(cached);
          mutationCount += 1;
        }
      }
      if (pre) {
        const oldLog = pre.getAttribute('data-rendered-log') || '';
        const renderingLog = pre.getAttribute('data-rendering-log') || '';
        if (renderingLog === newLog) {
          content.setAttribute('data-rendered-subject-fingerprint', subjectFp);
          return;
        }
        if (oldLog === newLog) {
          content.setAttribute('data-rendered-subject-fingerprint', subjectFp);
          return;
        }
        if (renderedSubjectFp === subjectFp && oldLog && !newLog) {
          return;
        }
        if (oldRun && oldRun.kind === 'active' && run.kind !== 'active' && oldLog && newLog && newLog !== oldLog && !String(newLog).startsWith(oldLog)) {
          return;
        }
        if (oldLog && newLog.length >= oldLog.length && newLog.startsWith(oldLog)) {
          appendTerminalPre(pre, oldLog, newLog.slice(oldLog.length), opts.helpers);
        } else {
          fillTerminalPre(pre, newLog, opts.helpers);
        }
        content.setAttribute('data-rendered-subject-fingerprint', subjectFp);
        mutationCount += 1;
        return;
      }
      while (content.firstChild) content.removeChild(content.firstChild);
      content.removeAttribute('data-rendered-fingerprint');
      buildLogContent(content, subjectRun, opts.helpers);
      content.setAttribute('data-rendered-subject-fingerprint', subjectFp);
      mutationCount += 1;
      return;
    }
    let fingerprint = tabName + '|' + subjectFp;
    if (tabName === 'events') {
      // Cheap triplet (issue #1562). The poll no longer re-stringifies
      // event payloads. The rebuild branch below still calls
      // buildEventsContent, which re-runs eventsJSON so the rendered
      // <pre data-rendered-json> content stays byte-identical to today.
      fingerprint = 'events|' + cheapEventsFingerprint(subjectRun);
    } else {
      // Cheap scalar+peers string (issue #1562). The poll no longer
      // re-stringifies the details object. The rebuild branch below
      // still calls buildDetailsContent, which re-runs detailsJSON so
      // the rendered <pre data-rendered-json> content stays
      // byte-identical to today.
      fingerprint = 'details|' + cheapDetailsFingerprint(subjectRun, opts.helpers);
    }
    fingerprint += '|subjects:' + subjectFp;
    if (content.getAttribute('data-rendered-fingerprint') === fingerprint && content.firstChild) {
      return;
    }
    // Leaving the Log tab: preserve the rendered log pane in the per-subject
    // cache instead of discarding it, so returning to Log is O(1).
    const mountedLogPre = content.querySelector('pre[data-scroll-key]');
    if (mountedLogPre) {
      const pane = (mountedLogPre.parentNode && mountedLogPre.parentNode !== content) ? mountedLogPre.parentNode : mountedLogPre;
      storeCachedLogPane(subjectValue, pane);
      content.removeAttribute('data-rendered-subject-fingerprint');
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
    if (isRowExpandable(run) && matchesExpandedSubject(run, opts.expandedKey, opts)) {
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

  function removeAttr(node, name) {
    if (node.getAttribute(name) !== null) {
      node.removeAttribute(name);
      mutationCount += 1;
    }
  }

  // reconcileExpandabilityAttr adds or removes the row's toggle attrs so
  // they reflect whether the row is currently expandable. queued and
  // blocked rows lose data-action/role/tabindex/aria-controls and gain
  // the row-non-expandable class; all other statuses gain the attrs back
  // and lose the class. aria-expanded is reconciled separately by
  // snapshotCellState + the existing aria-expanded branch.
  function reconcileExpandabilityAttr(row, run) {
    const expandable = isRowExpandable(run);
    setClass(row, 'row-non-expandable', !expandable);
    if (expandable) {
      setAttr(row, 'data-action', 'toggle-run');
      setAttr(row, 'role', 'button');
      setAttr(row, 'tabindex', '0');
      setAttr(row, 'aria-controls', detailIDForKey(run.key));
    } else {
      removeAttr(row, 'data-action');
      removeAttr(row, 'role');
      removeAttr(row, 'tabindex');
      removeAttr(row, 'aria-controls');
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
    if (oldSnap.unavailable !== newSnap.unavailable) {
      setClass(row, 'row-unavailable', newSnap.unavailable);
    }
    const currentAria = row.getAttribute('aria-expanded');
    const desiredAria = newSnap.ariaExpanded;
    if (currentAria !== desiredAria) {
      setAttr(row, 'aria-expanded', desiredAria);
    }
    reconcileExpandabilityAttr(row, newRun);

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

  function cacheLogPaneBeforeRemove(detailRow) {
    const pre = detailRow.querySelector('pre[data-scroll-key]');
    if (!pre) return;
    // The fingerprint attr lives on the grandparent .detail-content (the
    // pre is wrapped in a .detail-box section). Walk up at most two levels
    // to find a node that carries the fingerprint, then use that node as
    // the cached pane so a re-expand reuses the whole log panel.
    let node = pre.parentNode;
    while (node && node !== detailRow) {
      const fp = node.getAttribute && node.getAttribute('data-rendered-subject-fingerprint');
      if (fp) break;
      node = node.parentNode;
    }
    if (!node || node === detailRow) return;
    const fp = node.getAttribute('data-rendered-subject-fingerprint') || '';
    const subjectValue = fp.split('|')[0];
    if (!subjectValue) return;
    storeCachedLogPane(subjectValue, node);
  }

  function removeRunRow(body, key) {
    const dataRow = dataRowOf(body, key);
    const detail = detailRowOf(body, key);
    const ctx = contextRowOf(body, key);
    const rows = [];
    if (dataRow) {
      dataRow.classList.add('row-removed');
      rows.push(dataRow);
      body.removeChild(dataRow);
      clearRowData(dataRow);
    }
    if (ctx) {
      ctx.classList.add('row-removed');
      rows.push(ctx);
      body.removeChild(ctx);
    }
    if (detail) {
      detail.classList.add('row-removed');
      rows.push(detail);
      cacheLogPaneBeforeRemove(detail);
      body.removeChild(detail);
      clearDetailData(detail);
    }
    return { count: rows.length, rows };
  }

  function setEmpty(body, html, colspan) {
    const placeholder = global.document.createElement('tr');
    placeholder.className = 'empty-row';
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
          detail.classList.add('row-removed');
          body.removeChild(detail);
          clearDetailData(detail);
          removed += 1;
        }
        const ctx = contextRowOf(body, key);
        if (ctx) {
          ctx.classList.add('row-removed');
          body.removeChild(ctx);
          removed += 1;
        }
        dataRow.classList.add('row-removed');
        body.removeChild(dataRow);
        clearRowData(dataRow);
        removed += 1;
        continue;
      }
      const oldRun = getRowData(dataRow) || newRun;
      const r = updateRunRowCells(dataRow, oldRun, newRun, opts);
      if (r.mutated) updated += 1;

      const wantDetail = isRowExpandable(newRun) && matchesExpandedSubject(newRun, opts.expandedKey, opts);
      const detail = detailRowOf(body, key);
      if (wantDetail && !detail) {
        const newDetail = buildDetailRow(body, newRun, opts);
        setDetailData(newDetail, newRun);
        inserted += 1;
      } else if (!wantDetail && detail) {
        // Capture the rendered log pane before removing the detail row so a
        // subsequent re-expand can reuse it. Caching on collapse is what
        // makes open → close → open ~O(1) instead of re-tokenizing the log.
        detail.classList.add('row-removed');
        cacheLogPaneBeforeRemove(detail);
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
    const renderedLog = newLog && String(newLog).trim() ? newLog : '';
    const oldLog = pre.getAttribute('data-rendered-log') || '';
    const renderingLog = pre.getAttribute('data-rendering-log') || '';
    if (renderingLog === renderedLog) return;
    if (oldLog === renderedLog) return;
    if (oldLog && renderedLog.length >= oldLog.length && renderedLog.startsWith(oldLog)) {
      appendTerminalPre(pre, oldLog, renderedLog.slice(oldLog.length), helpers);
    } else {
      fillTerminalPre(pre, renderedLog, helpers);
    }
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
    subjectRunValue,
    subjectRunsFor,
    highlightJSON,
    highlightTerminalLog,
    cheapEventsFingerprint,
    cheapDetailsFingerprint,
    tokenizeForCache,
    prewarmLogPaneCache,
    runPrewarmIfIdle,
    getLogPaneCacheSize: () => logPaneCache.size,
    hasLogPaneCached: (subjectValue) => logPaneCache.has(subjectValue),
  };
})(typeof window !== 'undefined' ? window : globalThis);
