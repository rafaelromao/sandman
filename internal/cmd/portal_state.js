(function (global) {
  const STORAGE_KEY = 'sandman.portal.view-state.v1';
  const DEFAULT_TAB = 'log';
  const VALID_TABS = new Set(['log', 'events', 'details']);

  function defaultState() {
    return { expandedRunKey: null, tabs: {} };
  }

  function readStorage() {
    try {
      return global.localStorage ? global.localStorage.getItem(STORAGE_KEY) : null;
    } catch (err) {
      return null;
    }
  }

  function writeStorage(value) {
    try {
      if (global.localStorage) global.localStorage.setItem(STORAGE_KEY, value);
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
    return { state: sanitizeState(state), changed: false };
  }

  function getSelectedTab(state, runKey) {
    const tabs = state && state.tabs && typeof state.tabs === 'object' ? state.tabs : {};
    const tab = String(tabs[runKey] || '').trim();
    return VALID_TABS.has(tab) ? tab : DEFAULT_TAB;
  }

  global.SandmanPortalState = {
    storageKey: STORAGE_KEY,
    defaultTab: DEFAULT_TAB,
    load,
    save,
    normalize,
    getSelectedTab,
  };
})(typeof window !== 'undefined' ? window : globalThis);
