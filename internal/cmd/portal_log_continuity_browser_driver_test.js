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
    const ready = () => {
      const pre = pane();
      const panel = body.querySelector('.detail-panel');
      return pre && !pre.hasAttribute('data-rendering-log') && panel && panel.getAttribute('aria-busy') !== 'true';
    };
    await waitFor('rendered active row', () => body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]'));
    body.querySelector('tr[data-action="toggle-run"][data-run-key="' + config.runKey + '"]').click();
    await waitFor('initial Log pane and HTTP EventSource', async () => ready() && (await json('status')).connections === 1);
    await frame();
    await frame();
    result.initialLog = pane().textContent;

    tabButton(config.tab).click();
    await waitFor('rendered ' + config.tab + ' tab', () => !pane() && tabButton(config.tab).getAttribute('aria-pressed') === 'true');
    await waitFor('initial HTTP stream closed by tab handler', async () => (await json('status')).closed >= 1);
    tabButton('log').click();
    await waitFor('returned Log pane and replacement HTTP stream', async () => ready() && tabButton('log').getAttribute('aria-pressed') === 'true' && (await json('status')).connections === 2);

    // Replay only once the real force-fetch/render/reattach path has settled.
    // The server reads the old 256 KiB range from the same saved run.log that
    // supplied metadata's newer 64 KiB suffix, then marks replay completion.
    await control('replay');
    await waitFor('replacement replay completion', async () => {
      const status = await json('status');
      if (status.error) throw new Error(status.error);
      return status.replayComplete;
    });
    await control('append');
    await waitFor('appended saved-log tail rendered', () => ready() && pane().textContent.includes(config.lastLiveLine));
    await frame();
    await frame();
    result.visibleLog = pane().textContent;
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
