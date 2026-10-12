// Browser-only driver for the actual embedded Portal page. All interaction is
// through rendered controls; fetch, EventSource and animation frames are native.
(async function () {
  const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
  const frame = () => new Promise((resolve) => requestAnimationFrame(resolve));
  async function waitFor(description, predicate) {
    const deadline = performance.now() + 10000;
    while (performance.now() < deadline) {
      if (await predicate()) return;
      await pause(20);
    }
    throw new Error('timed out waiting for ' + description);
  }
  async function json(path) {
    const response = await fetch('/__test/' + path, { cache: 'no-store' });
    if (!response.ok) throw new Error(path + ': HTTP ' + response.status);
    return response.json();
  }
  async function control(path) {
    const response = await fetch('/__test/' + path, { method: 'POST' });
    if (!response.ok) throw new Error(path + ': HTTP ' + response.status);
  }
  async function postResult(result) {
    const response = await fetch('/__test/result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(result),
    });
    if (!response.ok) throw new Error('result: HTTP ' + response.status);
  }
  const reloadKey = '__sandman_portal_log_continuity_reloaded';
  const result = {};
  const pageErrors = [];
  window.addEventListener('error', (event) => pageErrors.push(event.message));
  window.addEventListener('unhandledrejection', (event) => pageErrors.push(String(event.reason)));
  try {
    const config = await json('config');
    const body = document.getElementById('runs-body');
    const pane = () => body.querySelector('pre.terminal-log[data-scroll-key]');
    const tabButton = (tab) => body.querySelector('button[data-action="set-tab"][data-tab="' + tab + '"]');
    const ready = () => {
      const pre = pane();
      const panel = body.querySelector('.detail-panel');
      return pre && !pre.hasAttribute('data-rendering-log') && panel && panel.getAttribute('aria-busy') !== 'true';
    };
    async function verifySourceRange(description, runId) {
      runId = runId || config.runKey;
      const state = window.sandmanPortalLogs.get(runId);
      const pre = body.querySelector('pre[data-scroll-key="' + runId + '"]');
      if (!pre || pre.textContent !== state.text
        || pre.getAttribute('data-log-start') !== String(state.start)
        || pre.getAttribute('data-log-end') !== String(state.end)
        || pre.getAttribute('data-log-generation') !== state.generation) {
        throw new Error(description + ': rendered range differs from accepted model');
      }
      if (pre.textContent.includes('{"type":"snapshot"') || pre.textContent.includes('{"type":"append"')) {
        throw new Error(description + ': rendered transport frame envelope');
      }
      const response = await fetch('/__test/source?runId=' + encodeURIComponent(runId) + '&start=' + state.start + '&end=' + state.end, { cache: 'no-store' });
      if (!response.ok) throw new Error(description + ': source oracle returned HTTP ' + response.status);
      if (await response.text() !== state.text) throw new Error(description + ': model range differs from authoritative Saved Run Log');
    }
    if (config.resumeOnly) {
      await waitFor('built Portal row after restart', () => body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]'));
      body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]').click();
      await waitFor('built Portal saved snapshot after restart', () => {
        const state = window.sandmanPortalLogs.get(config.runKey);
        return ready() && state && state.cursor && state.status === 'ended' && pane().textContent === state.text;
      });
      await verifySourceRange('built Portal restart snapshot');
      result.restartLoaded = true;
      result.visibleLog = pane().textContent;
      result.finalStart = Number(pane().getAttribute('data-log-start'));
      result.finalEnd = Number(pane().getAttribute('data-log-end'));
      result.finalGeneration = pane().getAttribute('data-log-generation');
      await postResult(result);
      return;
    }
    if (config.reloadOnce && sessionStorage.getItem(reloadKey)) {
      const previous = JSON.parse(sessionStorage.getItem(reloadKey));
      await waitFor('browser reload restored the Portal log subscription', () => {
        const state = window.sandmanPortalLogs.get(config.runKey);
        return ready() && state && state.cursor && state.status === 'ended' && pane().textContent === state.text;
      });
      await verifySourceRange('built Portal browser reload');
      if (window.sandmanPortalLogs.get(config.runKey).text !== previous.visibleLog) {
        throw new Error('browser reload changed the accepted Saved Run Log range');
      }
      Object.assign(result, previous, { reloadVerified: true });
      result.reloadGeneration = window.sandmanPortalLogs.get(config.runKey).generation;
      await postResult(result);
      return;
    }
    await waitFor('rendered active row', () => body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]'));
    body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]').click();
    await waitFor('accepted saved-log snapshot', () => {
      const state = window.sandmanPortalLogs && window.sandmanPortalLogs.get(config.runKey);
      return ready() && state && state.generation && state.cursor && state.end > state.start
        && pane().getAttribute('data-log-end') === String(state.end);
    });
    await verifySourceRange('initial snapshot');
    await waitFor('initial HTTP EventSource', async () => (await json('status')).connections >= 1);
    await frame();
    await frame();
    result.initialLog = pane().textContent;
    result.initialStart = Number(pane().getAttribute('data-log-start'));
    result.initialEnd = Number(pane().getAttribute('data-log-end'));
    result.initialGeneration = pane().getAttribute('data-log-generation');
    result.initialCursor = window.sandmanPortalLogs.get(config.runKey).cursor;
    let expectedConnections = 1;
    if (!config.skipNativeReconnect) {
      await waitFor('native EventSource reconnect with Last-Event-ID', async () => {
        const status = await json('status');
        return status.connections >= 2 && status.lastEventIds[1] === result.initialCursor;
      });
      expectedConnections = 2;
    }

    const pollInput = document.querySelector('#poll-interval');
    if (pollInput) {
      pollInput.value = '500';
      pollInput.dispatchEvent(new Event('change', { bubbles: true }));
    }
    const beforeBackoff304 = (await json('status')).summaryNotModified;
    await control('close-next?count=3');
    tabButton(config.tab).click();
    await waitFor('tab leaving before explicit replacement', () => !pane());
    tabButton('log').click();
    await waitFor('permanently closed EventSource response', async () => {
      const status = await json('status');
      return status.connections === expectedConnections + 1 && status.forcedCloses === 1;
    });
    await waitFor('bounded closed-source retry sequence', async () => {
      const status = await json('status');
      return status.connections === expectedConnections + 3 && status.forcedCloses === 3;
    });
    await waitFor('unchanged 304 poll while retry backoff is pending', async () => {
      const status = await json('status');
      return status.connections === expectedConnections + 3 && status.summaryNotModified > beforeBackoff304;
    });
    expectedConnections += 4;
    await waitFor('accepted-cursor retry after bounded backoff', async () => (await json('status')).connections === expectedConnections);
    if (!window.sandmanPortalLogs.get(config.runKey).cursor) throw new Error('closed-source recovery lost the accepted cursor');
    let appendedBlocks = 0;

    if (config.reviewKey) {
      const subject = body.querySelector('select[data-action="set-subject"]');
      if (!subject) throw new Error('built Portal did not render the implementation/review subject selector');
      subject.value = config.reviewKey;
      subject.dispatchEvent(new Event('change', { bubbles: true }));
      expectedConnections++;
      await waitFor('review RunID source snapshot', async () => ready()
        && (await json('status')).connections === expectedConnections
        && window.sandmanPortalLogs.get(config.reviewKey)?.cursor
        && pane().getAttribute('data-scroll-key') === config.reviewKey
        && pane().textContent === window.sandmanPortalLogs.get(config.reviewKey).text
        && pane().getAttribute('data-log-end') === String(window.sandmanPortalLogs.get(config.reviewKey).end));
      await verifySourceRange('review subject snapshot', config.reviewKey);
      const reviewText = window.sandmanPortalLogs.get(config.reviewKey).text;
      await control('append?runKey=' + encodeURIComponent(config.runKey));
      appendedBlocks++;
      subject.value = config.runKey;
      subject.dispatchEvent(new Event('change', { bubbles: true }));
      expectedConnections++;
      await waitFor('implementation RunID catches up from its own cursor', async () => ready()
        && (await json('status')).connections === expectedConnections
        && pane().getAttribute('data-scroll-key') === config.runKey
        && window.sandmanPortalLogs.get(config.runKey).text.split(config.lastLiveLine).length - 1 === appendedBlocks
        && pane().textContent === window.sandmanPortalLogs.get(config.runKey).text);
      if (window.sandmanPortalLogs.get(config.reviewKey).text !== reviewText) throw new Error('implementation output changed the review RunID model');
      await verifySourceRange('implementation return after review output', config.runKey);
    }

    // Hold the real page render frame across a row collapse/reopen pair. Output
    // arrives while detached; the final render must resubscribe and catch up.
    const nativeRaf = window.requestAnimationFrame.bind(window);
    const pendingRafs = [];
    window.requestAnimationFrame = callback => { pendingRafs.push(callback); return pendingRafs.length; };
    const pendingStart = window.sandmanPortalLogs.get(config.runKey).end;
    const row = body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]');
    row.click();
    if (!pendingRafs.length) throw new Error('row collapse did not queue a render frame');
    const viewStorageKey = Array.from({ length: sessionStorage.length }, (_, index) => sessionStorage.key(index))
      .find(key => String(key || '').startsWith('sandman.portal.view-state.v1:'));
    if (!viewStorageKey || JSON.parse(sessionStorage.getItem(viewStorageKey)).expandedRunKey !== null) {
      throw new Error('row collapse was not recorded while the render frame was pending');
    }
    await control('append');
    appendedBlocks++;
    row.click();
    if (!pendingRafs.length) throw new Error('row reopen did not queue a render frame');
    if (JSON.parse(sessionStorage.getItem(viewStorageKey)).expandedRunKey !== config.runKey) {
      throw new Error('row reopen was not recorded while the render frame was pending');
    }
    while (pendingRafs.length) pendingRafs.pop()(performance.now());
    expectedConnections++;
    await waitFor('row stream resumed during pending frame interleave', async () => (await json('status')).connections === expectedConnections);
    await waitFor('outgoing run caught up after row reopen', () => {
      const state = window.sandmanPortalLogs.get(config.runKey);
      return state.text.split(config.lastLiveLine).length - 1 === appendedBlocks && state.end > pendingStart;
    });
    while (pendingRafs.length) pendingRafs.pop()(performance.now());
    window.requestAnimationFrame = nativeRaf;
    try {
      await waitFor('reordered frames settle to the accepted run range', () => ready() && pane().textContent === window.sandmanPortalLogs.get(config.runKey).text);
    } catch (error) {
      const state = window.sandmanPortalLogs.get(config.runKey);
      const runRow = body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]');
      const panel = body.querySelector('.detail-panel');
      throw new Error(String(error) + '; state=' + JSON.stringify({
        status: state && state.status,
        end: state && state.end,
        textEnd: state && state.text.slice(-100),
        rowExpanded: runRow && runRow.getAttribute('aria-expanded'),
        pane: pane() && { textEnd: pane().textContent.slice(-100), rendering: pane().getAttribute('data-rendering-log') },
        panelBusy: panel && panel.getAttribute('aria-busy'),
      }));
    }
    await verifySourceRange('row collapse/reopen with pending output frames');

    const warmTransitions = [];
    for (let index = 0; index < 3; index++) {
      const tab = index % 2 === 0 ? config.tab : (config.tab === 'events' ? 'details' : 'events');
      const before = window.sandmanPortalLogs.get(config.runKey);
      const beforeEnd = before.end;
      tabButton(tab).click();
      await waitFor('rendered ' + tab + ' tab', () => !pane() && tabButton(tab).getAttribute('aria-pressed') === 'true');
      await control('append'); // Output arrives while no log pane is subscribed.
      appendedBlocks++;
      tabButton('log').click();
      expectedConnections++;
      await waitFor('returned Log pane and cursor-resumed HTTP stream', async () => ready()
        && tabButton('log').getAttribute('aria-pressed') === 'true'
        && (await json('status')).connections === expectedConnections);
      await waitFor('contiguous catch-up from accepted cursor', () => {
        const state = window.sandmanPortalLogs.get(config.runKey);
        return state.end > beforeEnd && state.text.split(config.lastLiveLine).length - 1 === appendedBlocks
          && pane().textContent === state.text;
      });
      const status = await json('status');
      if (!status.resumeCursors[expectedConnections - 1]) throw new Error('tab return omitted the accepted cursor');
      if (window.sandmanPortalLogs.get(config.runKey).generation !== result.initialGeneration) {
        throw new Error('same RunID append changed generation across tab round-trip');
      }
      await verifySourceRange('initial tab transition ' + index);
    }

    // Fixed-seed actual-page navigation stress. Every settled action compares
    // model, DOM range attributes, and independent Saved Run Log bytes.
    let seed = 2772;
    function random(limit) { seed = (1664525 * seed + 1013904223) >>> 0; return seed % limit; }
    for (let index = 0; index < 50; index++) {
      const transitionStarted = performance.now();
      const action = random(3);
      const before = window.sandmanPortalLogs.get(config.runKey);
      const beforeEnd = before.end;
      const appendWhileDetached = index % 5 === 4;
      if (action === 2) {
        const runRow = body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]');
        runRow.click();
        await waitFor('row collapse ' + index, () => !pane() && runRow.getAttribute('aria-expanded') === 'false');
      } else {
        const tab = action === 0 ? 'events' : 'details';
        tabButton(tab).click();
        await waitFor('tab transition ' + index, () => !pane() && tabButton(tab).getAttribute('aria-pressed') === 'true');
      }
      if (appendWhileDetached) {
        await control('append');
        appendedBlocks++;
      }
      if (action === 2) {
        body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]').click();
      } else {
        tabButton('log').click();
      }
      expectedConnections++;
      await waitFor('restored row/tab ' + index, async () => ready()
        && (await json('status')).connections === expectedConnections);
      await waitFor('settled model and pane ' + index, () => {
        const state = window.sandmanPortalLogs.get(config.runKey);
        return pane().textContent === state.text
          && (appendWhileDetached ? state.end > beforeEnd : state.end === beforeEnd)
          && state.text.split(config.lastLiveLine).length - 1 === appendedBlocks;
      });
      await verifySourceRange('fixed-seed transition ' + index);
      if (action !== 2 && !appendWhileDetached) warmTransitions.push(performance.now() - transitionStarted);
    }
    result.stressTransitions = 50;
    result.maxWarmTransitionMs = Math.max.apply(null, warmTransitions);
    result.warmTransitionMs = warmTransitions;

    const beforeLiveEnd = window.sandmanPortalLogs.get(config.runKey).end;
    const liveStarted = performance.now();
    await control('append');
    appendedBlocks++;
    await waitFor('sub-second live saved-log update', () => {
      const state = window.sandmanPortalLogs.get(config.runKey);
      return state.end > beforeLiveEnd && state.text.split(config.lastLiveLine).length - 1 === appendedBlocks
        && pane().textContent === state.text;
    });
    result.liveUpdateMs = performance.now() - liveStarted;
    const modelStats = window.sandmanPortalLogs.stats();
    result.modelCacheBytes = modelStats.cacheBytes;
    result.modelRuns = modelStats.runs;
    result.domLogNodes = pane().querySelectorAll('*').length;
    if (config.finishOnComplete) {
      await control('finish');
      await waitFor('event-derived terminal stream end', () => window.sandmanPortalLogs.get(config.runKey).status === 'ended');
      result.ended = true;
    }
    await frame();
    await frame();
    result.visibleLog = pane().textContent;
    result.finalStart = Number(pane().getAttribute('data-log-start'));
    result.finalEnd = Number(pane().getAttribute('data-log-end'));
    result.finalGeneration = pane().getAttribute('data-log-generation');
    result.finalCursor = window.sandmanPortalLogs.get(config.runKey).cursor;
    if (pageErrors.length) throw new Error('production page errors: ' + pageErrors.join('; '));
    if (config.reloadOnce) {
      sessionStorage.setItem(reloadKey, JSON.stringify(result));
      location.reload();
      return;
    }
  } catch (error) {
    result.error = String(error.stack || error);
  }
  await postResult(result);
})();
