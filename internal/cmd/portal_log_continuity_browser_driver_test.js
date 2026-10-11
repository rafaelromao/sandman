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
  const result = {};
  const pageErrors = [];
  window.addEventListener('error', (event) => pageErrors.push(event.message));
  window.addEventListener('unhandledrejection', (event) => pageErrors.push(String(event.reason)));
  try {
    const config = await json('config');
    const body = document.getElementById('runs-body');
    const pane = () => body.querySelector('pre.terminal-log[data-scroll-key]');
    const tabButton = (tab) => body.querySelector('button[data-action="set-tab"][data-tab="' + tab + '"]');
    async function verifySourceRange(description) {
      const state = window.sandmanPortalLogs.get(config.runKey);
      const pre = pane();
      if (!pre || pre.textContent !== state.text
        || pre.getAttribute('data-log-start') !== String(state.start)
        || pre.getAttribute('data-log-end') !== String(state.end)
        || pre.getAttribute('data-log-generation') !== state.generation) {
        throw new Error(description + ': rendered range differs from accepted model');
      }
      const response = await fetch('/__test/source?start=' + state.start + '&end=' + state.end, { cache: 'no-store' });
      if (!response.ok) throw new Error(description + ': source oracle returned HTTP ' + response.status);
      if (await response.text() !== state.text) throw new Error(description + ': model range differs from authoritative Saved Run Log');
    }
    const ready = () => {
      const pre = pane();
      const panel = body.querySelector('.detail-panel');
      return pre && !pre.hasAttribute('data-rendering-log') && panel && panel.getAttribute('aria-busy') !== 'true';
    };
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
    await waitFor('native EventSource reconnect with Last-Event-ID', async () => {
      const status = await json('status');
      return status.connections >= 2 && status.lastEventIds[1] === result.initialCursor;
    });

    const warmTransitions = [];
    for (let index = 0; index < 3; index++) {
      const tab = index % 2 === 0 ? config.tab : (config.tab === 'events' ? 'details' : 'events');
      const before = window.sandmanPortalLogs.get(config.runKey);
      const beforeEnd = before.end;
      tabButton(tab).click();
      await waitFor('rendered ' + tab + ' tab', () => !pane() && tabButton(tab).getAttribute('aria-pressed') === 'true');
      await control('append'); // Output arrives while no log pane is subscribed.
      tabButton('log').click();
      const expectedConnections = 3 + index;
      await waitFor('returned Log pane and cursor-resumed HTTP stream', async () => ready()
        && tabButton('log').getAttribute('aria-pressed') === 'true'
        && (await json('status')).connections === expectedConnections);
      await waitFor('contiguous catch-up from accepted cursor', () => {
        const state = window.sandmanPortalLogs.get(config.runKey);
        return state.end > beforeEnd && state.text.split(config.lastLiveLine).length - 1 === index + 1
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
    let expectedConnections = 5;
    let appendedBlocks = 3;
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
    await frame();
    await frame();
    result.visibleLog = pane().textContent;
    result.finalStart = Number(pane().getAttribute('data-log-start'));
    result.finalEnd = Number(pane().getAttribute('data-log-end'));
    result.finalGeneration = pane().getAttribute('data-log-generation');
    if (pageErrors.length) throw new Error('production page errors: ' + pageErrors.join('; '));
  } catch (error) {
    result.error = String(error.stack || error);
  }
  await fetch('/__test/result', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(result),
  });
})();
