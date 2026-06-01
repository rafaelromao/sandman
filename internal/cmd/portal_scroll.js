(function (global) {
  const DEFAULT_THRESHOLD = 4;
  const stickyByKey = new Map();

  function distanceFromBottom(element) {
    if (!element) return 0;
    const max = Math.max(0, element.scrollHeight - element.clientHeight);
    return Math.max(0, max - element.scrollTop);
  }

  function isAtBottom(element) {
    if (!element) return false;
    return distanceFromBottom(element) <= DEFAULT_THRESHOLD;
  }

  function isSticky(key) {
    if (!key) return true;
    if (!stickyByKey.has(key)) return true;
    return stickyByKey.get(key) === true;
  }

  function setSticky(key, value) {
    if (!key) return;
    stickyByKey.set(key, value === true);
  }

  function trackScroll(element, key) {
    if (!element || !key || typeof element.addEventListener !== 'function') return;
    const flag = '__portalScrollTracked';
    if (element[flag] && element[flag][key]) return;
    if (!element[flag]) element[flag] = {};
    element[flag][key] = true;
    element.addEventListener('scroll', () => {
      setSticky(key, isAtBottom(element));
    });
  }

  function keyOf(element) {
    if (!element || typeof element.getAttribute !== 'function') return null;
    const value = element.getAttribute('data-scroll-key');
    return value && String(value).trim() ? String(value) : null;
  }

  function capture(elements) {
    const states = {};
    if (!elements) return states;
    for (const el of Array.from(elements)) {
      const key = keyOf(el);
      if (!key) continue;
      states[key] = { scrollTop: el.scrollTop, sticky: isSticky(key) };
    }
    return states;
  }

  function restore(elements, states) {
    if (!elements) return;
    for (const el of Array.from(elements)) {
      const key = keyOf(el);
      if (!key) continue;
      trackScroll(el, key);
      const state = states ? states[key] : null;
      const sticky = state ? state.sticky : isSticky(key);
      const maxScroll = Math.max(0, el.scrollHeight - el.clientHeight);
      if (sticky) {
        el.scrollTop = el.scrollHeight;
        continue;
      }
      const next = state ? (Number.isFinite(state.scrollTop) ? state.scrollTop : 0) : 0;
      el.scrollTop = Math.min(Math.max(0, next), maxScroll);
    }
  }

  global.SandmanPortalScroll = {
    threshold: DEFAULT_THRESHOLD,
    isAtBottom,
    isSticky,
    setSticky,
    trackScroll,
    capture,
    restore,
  };
})(typeof window !== 'undefined' ? window : globalThis);
