package cmd

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPortalStream_StripsQueuedMarkerFromPolledPaneOnStreamOpen pins the
// behaviour that closes the "Queued. Waiting to start." follow-up: when the
// polled snapshot has stamped the server-side fallback marker at the head
// of the run.log pane (portal_runs_view.go:1615 for state-less active
// batch issues), the live SSE stream that takes ownership of that pane
// must drop the marker from data-rendered-log and from the rendered DOM
// before any new tail is appended. Otherwise the marker sits at the head
// of the live log view until the next poll refresh catches up.
func TestPortalStream_StripsQueuedMarkerFromPolledPaneOnStreamOpen(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping portal stream lifecycle test")
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	htmlPath := filepath.Join(filepath.Dir(currentFile), "portal.html")
	diffPath := filepath.Join(filepath.Dir(currentFile), "portal_diff.js")
	prefix := `
const fs = require('fs');
const vm = require('vm');
const htmlSrc = fs.readFileSync(` + "`" + htmlPath + "`" + `, 'utf8');
const diffSrc = fs.readFileSync(` + "`" + diffPath + "`" + `, 'utf8');
const startMatch = htmlSrc.match(/function startRunStream\(run\) \{[\s\S]*?^\s{4}\}/m);
const fillMatch = diffSrc.match(/function fillTerminalPre\(pre, text, helpers\) \{[\s\S]*?^\s{2}\}/m);
if (!startMatch || !fillMatch) throw new Error('stream lifecycle functions not found: start=' + !!startMatch + ' fill=' + !!fillMatch);
const streamSources = {};
const streamingKeys = new Set();
const streamPath = '/api/runs/stream';
const streamCoalescer = {
  seedKnownLines() {},
  clearBuffer() {},
  scheduleLine() {},
  setBlocked() {},
  flushPending() {},
  pendingSize() { return 0; },
};
// Mirror the real highlight function used by the production
// startRunStream path so the rendered pane matches what the portal
// would otherwise have produced.
function highlightTerminalLog(text) {
  var value = String(text == null ? '' : text);
  if (!value) return '';
  var lines = value.split('\n');
  return lines.map(function (line) { return '<span>' + line + '</span>'; }).join('\n');
}
// Tiny HTML parser used by the pre's innerHTML setter below. Mirrors the
// structure produced by renderTerminalContent / highlightTerminalLog:
// <span>...</span>, "\n" text separators, and plain text.
function parseHtmlInto(parent, html) {
  var pos = 0;
  while (pos < html.length) {
    if (html.charCodeAt(pos) === 10) {
      parent.appendChild({ nodeType: 3, _textContent: '\n', textContent: '\n', parentNode: parent });
      pos += 1;
      continue;
    }
    if (html.substr(pos, 5) === '<span') {
      var openEnd = html.indexOf('>', pos);
      if (openEnd < 0) break;
      var closeStart = html.indexOf('</span>', openEnd);
      if (closeStart < 0) break;
      var text = html.slice(openEnd + 1, closeStart);
      parent.appendChild({ nodeType: 1, tagName: 'SPAN', children: [], childNodes: [], _textContent: text, textContent: text, parentNode: parent });
      pos = closeStart + 7;
      continue;
    }
    if (html.charCodeAt(pos) === 60) {
      var end = html.indexOf('>', pos);
      if (end < 0) break;
      pos = end + 1;
      continue;
    }
    var next = html.indexOf('<', pos + 1);
    var end2 = next < 0 ? html.length : next;
    var text = html.slice(pos, end2);
    if (text) parent.appendChild({ nodeType: 3, _textContent: text, textContent: text, parentNode: parent });
    pos = end2;
  }
}
function makeMockDocument() {
  return {
    createElement(tag) {
      var el = makePre();
      el.tagName = String(tag).toUpperCase();
      return el;
    },
    createDocumentFragment() {
      return {
        nodeType: 11, children: [], childNodes: [],
        appendChild(c) {
          var parent = c.parentNode;
          if (parent && parent !== this) {
            var i = parent.children.indexOf(c);
            if (i >= 0) parent.children.splice(i, 1);
            if (parent.childNodes) {
              var j = parent.childNodes.indexOf(c);
              if (j >= 0) parent.childNodes.splice(j, 1);
            }
          }
          c.parentNode = this;
          this.children.push(c);
          this.childNodes.push(c);
          return c;
        },
      };
    },
  };
}
function makePre() {
  var log = [];
  var pre = {
    tagName: 'PRE', nodeType: 1, children: [], childNodes: [], parentNode: null, _log: log,
    setAttribute(name, value) { this[name] = value; log.push(['setAttribute', name, value]); },
    getAttribute(name) { return this[name] != null ? String(this[name]) : null; },
    removeAttribute(name) { delete this[name]; log.push(['removeAttribute', name]); },
    appendChild(child) {
      if (child && child.nodeType === 11) {
        var fragChildren = child.children.slice();
        child.children.length = 0;
        child.childNodes.length = 0;
        for (var i = 0; i < fragChildren.length; i++) {
          var c = fragChildren[i];
          c.parentNode = null;
          this.appendChild(c);
        }
        return child;
      }
      var parent = child.parentNode;
      if (parent) { var i = parent.children.indexOf(child); if (i >= 0) parent.children.splice(i, 1); }
      child.parentNode = this;
      this.children.push(child);
      this.childNodes.push(child);
      log.push(['appendChild', child.__id || '?']);
      return child;
    },
    removeChild(child) {
      var i = this.children.indexOf(child);
      if (i >= 0) { this.children.splice(i, 1); this.childNodes.splice(i, 1); }
      child.parentNode = null;
      log.push(['removeChild', child.__id || '?']);
      return child;
    },
    get firstChild() { return this.children[0] || null; },
    get textContent() {
      var parts = [];
      function walk(n) {
        if (!n) return;
        if (n.nodeType === 3) { parts.push(n._textContent != null ? n._textContent : (n.textContent || '')); return; }
        if (n._textContent != null) { parts.push(n._textContent); return; }
        var list = n.childNodes || n.children || [];
        for (var i = 0; i < list.length; i++) walk(list[i]);
      }
      walk(this);
      return parts.join('');
    },
    set innerHTML(v) {
      this._innerHTML = v;
      while (this.children.length) { this.children[0].parentNode = null; this.children.shift(); }
      this.childNodes = this.children;
      parseHtmlInto(this, String(v || ''));
    },
    get innerHTML() { return this._innerHTML || ''; },
  };
  return pre;
}
// Stub the constants and globals fillTerminalPre depends on so it can
// run in the vm sandbox without the full portal.html IIFE.
const ASYNC_CHUNK_THRESHOLD = 16 * 1024;
const CHUNK_SIZE_LINES = 100;
global.document = makeMockDocument();
function EventSource(url) {
  this.url = url;
  this.readyState = 1;
  this.closed = false;
  this._onmessage = null;
  this._onerror = null;
  this.close = function () { this.closed = true; this.readyState = 2; };
}
global.EventSource = EventSource;
const pre = makePre();
function streamPreFor(runKey) { if (runKey === 'queued-run-1') return pre; return null; }
// Pre-fill the pane with the polled snapshot the user would otherwise see.
vm.runInThisContext(fillMatch[0] + '\nthis.fillTerminalPre = fillTerminalPre;', { filename: 'portal_diff.js' });
fillTerminalPre(pre, 'Queued. Waiting to start.\n10:59:11 actual line 1\n10:59:12 actual line 2', { renderTerminalContent: highlightTerminalLog });
const beforeAttr = pre.getAttribute('data-rendered-log') || '';
const beforeText = pre.textContent || '';
const beforeChildren = pre.children.length;
if (beforeAttr.indexOf('Queued. Waiting to start.') < 0) throw new Error('precondition: expected queued marker in polled snapshot, got attr=' + JSON.stringify(beforeAttr));
// Drive startRunStream with the polled pane mounted. The fix should
// strip the queued marker from both data-rendered-log and the DOM, then
// seed known lines from the cleaned log so the SSE replay (which never
// emits the placeholder) does not re-introduce it.
vm.runInThisContext(startMatch[0] + '\nthis.startRunStream = startRunStream;', { filename: 'portal.html' });
startRunStream({ key: 'queued-run-1', runId: 'queued-run-1', kind: 'active', socketPath: '/tmp/q.sock' });
const afterAttr = pre.getAttribute('data-rendered-log') || '';
const afterText = pre.textContent || '';
const afterChildren = pre.children.length;
if (afterAttr.indexOf('Queued. Waiting to start.') >= 0) {
  process.stdout.write(JSON.stringify({ phase: 'fail', where: 'attr', beforeAttr: beforeAttr, afterAttr: afterAttr, beforeChildren: beforeChildren, afterChildren: afterChildren }));
  throw new Error('expected queued marker stripped from data-rendered-log, got ' + JSON.stringify(afterAttr));
}
if (afterText.indexOf('Queued. Waiting to start.') >= 0) {
  process.stdout.write(JSON.stringify({ phase: 'fail', where: 'text', beforeText: beforeText, afterText: afterText }));
  throw new Error('expected queued marker stripped from rendered text, got ' + JSON.stringify(afterText));
}
if (afterAttr !== '10:59:11 actual line 1\n10:59:12 actual line 2') {
  process.stdout.write(JSON.stringify({ phase: 'fail', where: 'attrExact', afterAttr: afterAttr }));
  throw new Error('expected attr to drop only the queued line, got ' + JSON.stringify(afterAttr));
}
if (afterText !== '10:59:11 actual line 1\n10:59:12 actual line 2') {
  process.stdout.write(JSON.stringify({ phase: 'fail', where: 'textExact', afterText: afterText }));
  throw new Error('expected rendered text to drop only the queued line, got ' + JSON.stringify(afterText));
}
if (afterChildren >= beforeChildren) {
  process.stdout.write(JSON.stringify({ phase: 'fail', where: 'children', beforeChildren: beforeChildren, afterChildren: afterChildren }));
  throw new Error('expected fewer DOM children after stripping the queued marker, got ' + afterChildren + ' (was ' + beforeChildren + ')');
}
process.stdout.write(JSON.stringify({ phase: 'pass', beforeAttr: beforeAttr, afterAttr: afterAttr, beforeText: beforeText, afterText: afterText, beforeChildren: beforeChildren, afterChildren: afterChildren }));
console.log('PASS');
`
	out, err := exec.Command("node", "-e", prefix).CombinedOutput()
	if err != nil {
		t.Fatalf("queued-marker strip harness failed: %v\noutput=%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("expected PASS, got %s", out)
	}
}
