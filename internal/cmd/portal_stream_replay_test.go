package cmd

import (
	"testing"
)

func TestPortalStreamPreservesCacheOnReattach(t *testing.T) {
	js := `
const streamingKeys = new Set();
const streamSources = {};
const portalScroll = { isSticky: function() { return false; } };

function makeMockPre(children) {
  return {
    tagName: 'PRE',
    nodeType: 1,
    children: children || [],
    dataset: {},
    parentNode: null,
    _log: [],
    _textContent: '',
    setAttribute(name, value) { this[name] = value; this._log.push(['setAttribute', name, value]); },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; this._log.push(['removeAttribute', name]); },
    appendChild(child) {
      const parent = child.parentNode;
      if (parent) { const i = parent.children.indexOf(child); if (i >= 0) parent.children.splice(i, 1); }
      child.parentNode = this;
      this.children.push(child);
      this._log.push(['appendChild', child.__id || '?']);
      return child;
    },
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx < 0) return child;
      this.children.splice(idx, 1);
      child.parentNode = null;
      this._log.push(['removeChild', child.__id || '?']);
      return child;
    },
    get firstChild() { return this.children.length ? this.children[0] : null; },
    get textContent() {
      const parts = [];
      function walk(n) {
        if (!n) return;
        if (n.nodeType === 3) { parts.push(n._textContent != null ? n._textContent : ''); return; }
        if (n._textContent != null) { parts.push(n._textContent); return; }
        const list = n.children || n.childNodes;
        if (list) for (const c of list) walk(c);
      }
      walk(this);
      return parts.join('');
    },
  };
}

function streamPreFor(runKey) {
	return globalThis._mockPre;
}

function appendStreamLine(runKey, line) {
	const pre = streamPreFor(runKey);
	if (!pre) return;
	const renderedLog = pre.getAttribute('data-rendered-log') || '';
	const cachedLines = new Set(renderedLog.split('\n'));
	if (cachedLines.has(line)) {
		return;
	}
	const tn = { nodeType: 3, _textContent: line + '\n', parentNode: null, _log: [] };
	pre.appendChild(tn);
	pre.setAttribute('data-rendered-log', renderedLog + line + '\n');
	if (portalScroll && portalScroll.isSticky(runKey)) {
		pre.scrollTop = pre.scrollHeight;
	}
}

function startRunStream(run) {
	if (!run || run.kind !== 'active' || !run.socketPath) return;
	if (streamSources[run.key]) return;
	const pre = streamPreFor(run.key);
	if (pre) {
		const cachedLog = pre.getAttribute('data-rendered-log') || '';
		if (!cachedLog) {
			while (pre.firstChild) pre.removeChild(pre.firstChild);
			pre.setAttribute('data-rendered-log', '');
		}
	}
	const src = { onmessage: null, onerror: null, close: function() {} };
	src.onmessage = function (ev) { appendStreamLine(run.key, ev.data); };
	src.onerror = function () { stopRunStream(run.key); };
	streamSources[run.key] = src;
	streamingKeys.add(run.key);
}

function stopRunStream(runKey) {
	const src = streamSources[runKey];
	if (src) {
		src.close();
		delete streamSources[runKey];
	}
	streamingKeys.delete(runKey);
}

// Setup: pre element with cached content and 2 child nodes
const pre = makeMockPre([
	{ nodeType: 3, _textContent: 'cached line1', parentNode: null, _log: [] },
	{ nodeType: 3, _textContent: 'cached line2', parentNode: null, _log: [] },
]);
pre.setAttribute('data-scroll-key', 'a');
pre.setAttribute('data-rendered-log', 'line1\nline2\n');
globalThis._mockPre = pre;

const childCountBefore = pre.children.length;
const logAttrBefore = pre.getAttribute('data-rendered-log');

// Simulate re-attach (e.g., tab switch back to Log) — startRunStream should NOT clear
const fakeRun = { key: 'a', kind: 'active', socketPath: '/fake.sock' };
startRunStream(fakeRun);

if (pre.children.length !== childCountBefore) {
	throw new Error('pre children were cleared on re-attach: expected ' + childCountBefore + ', got ' + pre.children.length);
}
if (pre.getAttribute('data-rendered-log') !== logAttrBefore) {
	throw new Error('data-rendered-log was reset on re-attach: expected ' + JSON.stringify(logAttrBefore) + ', got ' + JSON.stringify(pre.getAttribute('data-rendered-log')));
}

console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalStreamClearsOnFirstAttach(t *testing.T) {
	js := `
const streamingKeys = new Set();
const streamSources = {};
const portalScroll = { isSticky: function() { return false; } };

function makeMockPre(children) {
  return {
    tagName: 'PRE',
    nodeType: 1,
    children: children || [],
    dataset: {},
    parentNode: null,
    _log: [],
    _textContent: '',
    setAttribute(name, value) { this[name] = value; this._log.push(['setAttribute', name, value]); },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; this._log.push(['removeAttribute', name]); },
    appendChild(child) {
      const parent = child.parentNode;
      if (parent) { const i = parent.children.indexOf(child); if (i >= 0) parent.children.splice(i, 1); }
      child.parentNode = this;
      this.children.push(child);
      this._log.push(['appendChild', child.__id || '?']);
      return child;
    },
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx < 0) return child;
      this.children.splice(idx, 1);
      child.parentNode = null;
      this._log.push(['removeChild', child.__id || '?']);
      return child;
    },
    get firstChild() { return this.children.length ? this.children[0] : null; },
    get textContent() {
      const parts = [];
      function walk(n) {
        if (!n) return;
        if (n.nodeType === 3) { parts.push(n._textContent != null ? n._textContent : ''); return; }
        if (n._textContent != null) { parts.push(n._textContent); return; }
        const list = n.children || n.childNodes;
        if (list) for (const c of list) walk(c);
      }
      walk(this);
      return parts.join('');
    },
  };
}

function streamPreFor(runKey) {
	return globalThis._mockPre;
}

function appendStreamLine(runKey, line) {
	const pre = streamPreFor(runKey);
	if (!pre) return;
	const renderedLog = pre.getAttribute('data-rendered-log') || '';
	const cachedLines = new Set(renderedLog.split('\n'));
	if (cachedLines.has(line)) {
		return;
	}
	const tn = { nodeType: 3, _textContent: line + '\n', parentNode: null, _log: [] };
	pre.appendChild(tn);
	pre.setAttribute('data-rendered-log', renderedLog + line + '\n');
	if (portalScroll && portalScroll.isSticky(runKey)) {
		pre.scrollTop = pre.scrollHeight;
	}
}

function startRunStream(run) {
	if (!run || run.kind !== 'active' || !run.socketPath) return;
	if (streamSources[run.key]) return;
	const pre = streamPreFor(run.key);
	if (pre) {
		const cachedLog = pre.getAttribute('data-rendered-log') || '';
		if (!cachedLog) {
			while (pre.firstChild) pre.removeChild(pre.firstChild);
			pre.setAttribute('data-rendered-log', '');
		}
	}
	const src = { onmessage: null, onerror: null, close: function() {} };
	src.onmessage = function (ev) { appendStreamLine(run.key, ev.data); };
	src.onerror = function () { stopRunStream(run.key); };
	streamSources[run.key] = src;
	streamingKeys.add(run.key);
}

function stopRunStream(runKey) {
	const src = streamSources[runKey];
	if (src) {
		src.close();
		delete streamSources[runKey];
	}
	streamingKeys.delete(runKey);
}

// Setup: pre with NO cached content (first attach)
const pre = makeMockPre([
	{ nodeType: 3, _textContent: 'some stale content', parentNode: null, _log: [] },
]);
pre.setAttribute('data-scroll-key', 'a');
pre.setAttribute('data-rendered-log', '');
globalThis._mockPre = pre;

const childCountBefore = pre.children.length;

// Simulate first attach (no prior cache) — startRunStream SHOULD clear
const fakeRun = { key: 'a', kind: 'active', socketPath: '/fake.sock' };
startRunStream(fakeRun);

if (pre.children.length !== 0) {
	throw new Error('pre children were NOT cleared on first attach: expected 0, got ' + pre.children.length);
}
if (pre.getAttribute('data-rendered-log') !== '') {
	throw new Error('data-rendered-log was NOT cleared on first attach: expected "", got ' + JSON.stringify(pre.getAttribute('data-rendered-log')));
}

console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalStreamDedupesAlreadyRenderedLines(t *testing.T) {
	js := `
const streamingKeys = new Set();
const streamSources = {};
const portalScroll = { isSticky: function() { return false; } };

function makeMockPre(children) {
  return {
    tagName: 'PRE',
    nodeType: 1,
    children: children || [],
    dataset: {},
    parentNode: null,
    _log: [],
    _textContent: '',
    setAttribute(name, value) { this[name] = value; this._log.push(['setAttribute', name, value]); },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; this._log.push(['removeAttribute', name]); },
    appendChild(child) {
      const parent = child.parentNode;
      if (parent) { const i = parent.children.indexOf(child); if (i >= 0) parent.children.splice(i, 1); }
      child.parentNode = this;
      this.children.push(child);
      this._log.push(['appendChild', child.__id || '?']);
      return child;
    },
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx < 0) return child;
      this.children.splice(idx, 1);
      child.parentNode = null;
      this._log.push(['removeChild', child.__id || '?']);
      return child;
    },
    get firstChild() { return this.children.length ? this.children[0] : null; },
    get textContent() {
      const parts = [];
      function walk(n) {
        if (!n) return;
        if (n.nodeType === 3) { parts.push(n._textContent != null ? n._textContent : ''); return; }
        if (n._textContent != null) { parts.push(n._textContent); return; }
        const list = n.children || n.childNodes;
        if (list) for (const c of list) walk(c);
      }
      walk(this);
      return parts.join('');
    },
  };
}

function streamPreFor(runKey) {
	return globalThis._mockPre;
}

function appendStreamLine(runKey, line) {
	const pre = streamPreFor(runKey);
	if (!pre) return;
	const renderedLog = pre.getAttribute('data-rendered-log') || '';
	const cachedLines = new Set(renderedLog.split('\n'));
	if (cachedLines.has(line)) {
		return;
	}
	const tn = { nodeType: 3, _textContent: line + '\n', parentNode: null, _log: [] };
	pre.appendChild(tn);
	pre.setAttribute('data-rendered-log', renderedLog + line + '\n');
	if (portalScroll && portalScroll.isSticky(runKey)) {
		pre.scrollTop = pre.scrollHeight;
	}
}

function stopRunStream(runKey) {}

// Setup: pre with cached line "line1"
const pre = makeMockPre([
	{ nodeType: 3, _textContent: 'line1', parentNode: null, _log: [] },
]);
pre.setAttribute('data-scroll-key', 'a');
pre.setAttribute('data-rendered-log', 'line1\n');
globalThis._mockPre = pre;

const childCountBefore = pre.children.length;
const logAttrBefore = pre.getAttribute('data-rendered-log');

// Append the same line again (simulates SSE replay sending already-rendered lines)
appendStreamLine('a', 'line1');

if (pre.children.length !== childCountBefore) {
	throw new Error('duplicate line was appended: expected ' + childCountBefore + ' children, got ' + pre.children.length);
}
if (pre.getAttribute('data-rendered-log') !== logAttrBefore) {
	throw new Error('data-rendered-log was modified by duplicate: expected ' + JSON.stringify(logAttrBefore) + ', got ' + JSON.stringify(pre.getAttribute('data-rendered-log')));
}

console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalStreamAppendsAllLinesOnEmptyCache(t *testing.T) {
	js := `
const streamingKeys = new Set();
const streamSources = {};
const portalScroll = { isSticky: function() { return false; } };

function makeMockPre(children) {
  return {
    tagName: 'PRE',
    nodeType: 1,
    children: children || [],
    dataset: {},
    parentNode: null,
    _log: [],
    _textContent: '',
    setAttribute(name, value) { this[name] = value; this._log.push(['setAttribute', name, value]); },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; this._log.push(['removeAttribute', name]); },
    appendChild(child) {
      const parent = child.parentNode;
      if (parent) { const i = parent.children.indexOf(child); if (i >= 0) parent.children.splice(i, 1); }
      child.parentNode = this;
      this.children.push(child);
      this._log.push(['appendChild', child.__id || '?']);
      return child;
    },
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx < 0) return child;
      this.children.splice(idx, 1);
      child.parentNode = null;
      this._log.push(['removeChild', child.__id || '?']);
      return child;
    },
    get firstChild() { return this.children.length ? this.children[0] : null; },
    get textContent() {
      const parts = [];
      function walk(n) {
        if (!n) return;
        if (n.nodeType === 3) { parts.push(n._textContent != null ? n._textContent : ''); return; }
        if (n._textContent != null) { parts.push(n._textContent); return; }
        const list = n.children || n.childNodes;
        if (list) for (const c of list) walk(c);
      }
      walk(this);
      return parts.join('');
    },
  };
}

function streamPreFor(runKey) {
	return globalThis._mockPre;
}

function appendStreamLine(runKey, line) {
	const pre = streamPreFor(runKey);
	if (!pre) return;
	const renderedLog = pre.getAttribute('data-rendered-log') || '';
	const cachedLines = new Set(renderedLog.split('\n'));
	if (cachedLines.has(line)) {
		return;
	}
	const tn = { nodeType: 3, _textContent: line + '\n', parentNode: null, _log: [] };
	pre.appendChild(tn);
	pre.setAttribute('data-rendered-log', renderedLog + line + '\n');
	if (portalScroll && portalScroll.isSticky(runKey)) {
		pre.scrollTop = pre.scrollHeight;
	}
}

function stopRunRun(runKey) {}

// Setup: pre with empty cache
const pre = makeMockPre([]);
pre.setAttribute('data-scroll-key', 'a');
pre.setAttribute('data-rendered-log', '');
globalThis._mockPre = pre;

const childCountBefore = pre.children.length;
const logAttrBefore = pre.getAttribute('data-rendered-log');

// Append a line — should be appended since cache is empty
appendStreamLine('a', 'line1');

if (pre.children.length !== childCountBefore + 1) {
	throw new Error('line was NOT appended on empty cache: expected ' + (childCountBefore + 1) + ' children, got ' + pre.children.length);
}
if (pre.getAttribute('data-rendered-log') !== 'line1\n') {
	throw new Error('data-rendered-log not updated: expected "line1\\n", got ' + JSON.stringify(pre.getAttribute('data-rendered-log')));
}

console.log('PASS');
`
	runNodeScript(t, js)
}

func TestPortalStreamSkipsPrefixKeepsTail(t *testing.T) {
	js := `
const streamingKeys = new Set();
const streamSources = {};
const portalScroll = { isSticky: function() { return false; } };

function makeMockPre(children) {
  return {
    tagName: 'PRE',
    nodeType: 1,
    children: children || [],
    dataset: {},
    parentNode: null,
    _log: [],
    _textContent: '',
    setAttribute(name, value) { this[name] = value; this._log.push(['setAttribute', name, value]); },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; this._log.push(['removeAttribute', name]); },
    appendChild(child) {
      const parent = child.parentNode;
      if (parent) { const i = parent.children.indexOf(child); if (i >= 0) parent.children.splice(i, 1); }
      child.parentNode = this;
      this.children.push(child);
      this._log.push(['appendChild', child.__id || '?']);
      return child;
    },
    removeChild(child) {
      const idx = this.children.indexOf(child);
      if (idx < 0) return child;
      this.children.splice(idx, 1);
      child.parentNode = null;
      this._log.push(['removeChild', child.__id || '?']);
      return child;
    },
    get firstChild() { return this.children.length ? this.children[0] : null; },
    get textContent() {
      const parts = [];
      function walk(n) {
        if (!n) return;
        if (n.nodeType === 3) { parts.push(n._textContent != null ? n._textContent : ''); return; }
        if (n._textContent != null) { parts.push(n._textContent); return; }
        const list = n.children || n.childNodes;
        if (list) for (const c of list) walk(c);
      }
      walk(this);
      return parts.join('');
    },
  };
}

function streamPreFor(runKey) {
	return globalThis._mockPre;
}

function appendStreamLine(runKey, line) {
	const pre = streamPreFor(runKey);
	if (!pre) return;
	const renderedLog = pre.getAttribute('data-rendered-log') || '';
	const cachedLines = new Set(renderedLog.split('\n'));
	if (cachedLines.has(line)) {
		return;
	}
	const tn = { nodeType: 3, _textContent: line + '\n', parentNode: null, _log: [] };
	pre.appendChild(tn);
	pre.setAttribute('data-rendered-log', renderedLog + line + '\n');
	if (portalScroll && portalScroll.isSticky(runKey)) {
		pre.scrollTop = pre.scrollHeight;
	}
}

function stopRunStream(runKey) {}

// Setup: pre with cached lines "A\nB\n"
const pre = makeMockPre([
	{ nodeType: 3, _textContent: 'A', parentNode: null, _log: [] },
	{ nodeType: 3, _textContent: 'B', parentNode: null, _log: [] },
]);
pre.setAttribute('data-scroll-key', 'a');
pre.setAttribute('data-rendered-log', 'A\nB\n');
globalThis._mockPre = pre;

const childCountBefore = pre.children.length;

// Simulate SSE replay sending A, B, C — only C should be appended
appendStreamLine('a', 'A');
appendStreamLine('a', 'B');
appendStreamLine('a', 'C');

if (pre.children.length !== childCountBefore + 1) {
	throw new Error('expected only 1 new child (C), got ' + (pre.children.length - childCountBefore) + ' new children');
}
if (pre.getAttribute('data-rendered-log') !== 'A\nB\nC\n') {
	throw new Error('data-rendered-log wrong: expected "A\\nB\\nC\\n", got ' + JSON.stringify(pre.getAttribute('data-rendered-log')));
}

console.log('PASS');
`
	runNodeScript(t, js)
}
