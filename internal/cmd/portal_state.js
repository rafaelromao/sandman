(function (global) {
  const DEFAULT_STORAGE_KEY = 'sandman.portal.view-state.v1';
  const DEFAULT_TAB = 'log';
  const VALID_TABS = new Set(['log', 'events', 'details']);
  let storageKey = DEFAULT_STORAGE_KEY;

  function defaultState() {
    return { expandedRunKey: null, tabs: {}, commandFormCollapsed: false, showArchived: false };
  }

  function readStorage() {
    try {
      return global.sessionStorage ? global.sessionStorage.getItem(storageKey) : null;
    } catch (err) {
      return null;
    }
  }

  function writeStorage(value) {
    try {
      if (global.sessionStorage) global.sessionStorage.setItem(storageKey, value);
    } catch (err) {
    }
  }

  function cleanKey(value) {
    const text = String(value || '').trim();
    return text || null;
  }

  function cleanTabs(value) {
    const tabs = {};
    if (!value || typeof value !== 'object') return tabs;
    for (const [key, tab] of Object.entries(value)) {
      const runKey = cleanKey(key);
      if (!runKey) continue;
      tabs[runKey] = String(tab || '').trim();
    }
    return tabs;
  }

  function sanitizeState(state) {
    const current = state && typeof state === 'object' ? state : defaultState();
    return {
      expandedRunKey: cleanKey(current.expandedRunKey),
      tabs: cleanTabs(current.tabs),
      commandFormCollapsed: typeof current.commandFormCollapsed === 'boolean' ? current.commandFormCollapsed : false,
      showArchived: typeof current.showArchived === 'boolean' ? current.showArchived : false,
    };
  }

  function load() {
    const raw = readStorage();
    if (!raw) return defaultState();
    try {
      return sanitizeState(JSON.parse(raw));
    } catch (err) {
      return defaultState();
    }
  }

  function save(state) {
    const value = state && state.state ? state.state : state;
    writeStorage(JSON.stringify(sanitizeState(value)));
  }

  function normalize(state, runs) {
    const current = sanitizeState(state);
    const runList = Array.isArray(runs) ? runs.map((run) => cleanKey(run && run.key)).filter(Boolean) : [];
    const runKeys = new Set(runList);
    let changed = false;

    if (current.expandedRunKey && !runKeys.has(current.expandedRunKey)) {
      current.expandedRunKey = runList[0] || null;
      changed = true;
    }

    for (const runKey of runKeys) {
      if (!VALID_TABS.has(current.tabs[runKey])) {
        current.tabs[runKey] = DEFAULT_TAB;
        changed = true;
      }
    }

    return { state: current, changed };
  }

  function getSelectedTab(state, runKey) {
    const tabs = state && state.tabs && typeof state.tabs === 'object' ? state.tabs : {};
    const tab = String(tabs[runKey] || '').trim();
    return VALID_TABS.has(tab) ? tab : DEFAULT_TAB;
  }

  global.SandmanPortalState = {
    storageKey: storageKey,
    defaultTab: DEFAULT_TAB,
    configure(options) {
      if (options && options.storageKey) {
        storageKey = String(options.storageKey);
        this.storageKey = storageKey;
      }
    },
    load,
    save,
    normalize,
    getSelectedTab,
  };
})(typeof window !== 'undefined' ? window : globalThis);
