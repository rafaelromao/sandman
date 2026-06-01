package cmd

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPortalScrollCaptureAndRestore(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for portal scroll helper test")
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	helperPath := filepath.Join(filepath.Dir(currentFile), "portal_scroll.js")

	script := `const fs = require('fs');
const vm = require('vm');
const helperPath = process.argv[1];
const source = fs.readFileSync(helperPath, 'utf8');
const sandbox = { window: {}, globalThis: {}, Set, Map, JSON, console };
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: helperPath });
const api = sandbox.SandmanPortalScroll;
if (!api) throw new Error('missing SandmanPortalScroll');

const cases = [
  { name: 'at bottom exactly', el: { scrollHeight: 1000, clientHeight: 200, scrollTop: 800 }, want: true },
  { name: 'within threshold', el: { scrollHeight: 1000, clientHeight: 200, scrollTop: 797 }, want: true },
  { name: 'outside threshold', el: { scrollHeight: 1000, clientHeight: 200, scrollTop: 500 }, want: false },
  { name: 'at top', el: { scrollHeight: 1000, clientHeight: 200, scrollTop: 0 }, want: false },
  { name: 'no scrollable content', el: { scrollHeight: 200, clientHeight: 200, scrollTop: 0 }, want: true },
  { name: 'null element', el: null, want: false },
];
for (const c of cases) {
  const got = api.isAtBottom(c.el);
  if (got !== c.want) throw new Error(c.name + ': want ' + c.want + ' got ' + got);
}

if (typeof api.capture !== 'function') throw new Error('missing capture');
if (typeof api.restore !== 'function') throw new Error('missing restore');
if (typeof api.trackScroll !== 'function') throw new Error('missing trackScroll');
if (typeof api.isSticky !== 'function') throw new Error('missing isSticky');
if (typeof api.setSticky !== 'function') throw new Error('missing setSticky');

api.setSticky('clean-a', false);
if (api.isSticky('clean-a') !== false) throw new Error('explicit setSticky false should stick');
api.setSticky('clean-b', true);
if (api.isSticky('clean-b') !== true) throw new Error('explicit setSticky true should stick');
api.setSticky('clean-b', null);
if (api.isSticky('clean-b') !== false) throw new Error('setSticky with null should coerce to false');

{
  const key = 'fresh-' + Math.random();
  if (api.isSticky(key) !== true) throw new Error('unknown key should default to sticky=true');
}

{
  const a = { scrollHeight: 1000, clientHeight: 200, scrollTop: 800, getAttribute: (k) => k === 'data-scroll-key' ? 'default-a' : null };
  const states = api.capture([a]);
  if (!states['default-a'] || !states['default-a'].sticky) throw new Error('default sticky should be true on first capture');
}

{
  const key = 'restore-sticky-' + Math.random();
  api.setSticky(key, true);
  const a = { scrollHeight: 1000, clientHeight: 200, scrollTop: 0, getAttribute: (k) => k === 'data-scroll-key' ? key : null };
  const states = api.capture([a]);
  const fresh = { scrollHeight: 1500, clientHeight: 200, scrollTop: 0, getAttribute: (k) => k === 'data-scroll-key' ? key : null, addEventListener: () => {} };
  api.restore([fresh], states);
  if (fresh.scrollTop !== 1300) throw new Error('sticky restore should snap to new max scroll (1300), got ' + fresh.scrollTop);
}

{
  const key = 'restore-preserve-' + Math.random();
  api.setSticky(key, false);
  const a = { scrollHeight: 1000, clientHeight: 200, scrollTop: 250, getAttribute: (k) => k === 'data-scroll-key' ? key : null };
  const states = api.capture([a]);
  const fresh = { scrollHeight: 1500, clientHeight: 200, scrollTop: 0, getAttribute: (k) => k === 'data-scroll-key' ? key : null, addEventListener: () => {} };
  api.restore([fresh], states);
  if (fresh.scrollTop !== 250) throw new Error('non-sticky restore should preserve scrollTop 250, got ' + fresh.scrollTop);
}

{
  const key = 'restore-clamp-' + Math.random();
  api.setSticky(key, false);
  const a = { scrollHeight: 1000, clientHeight: 200, scrollTop: 800, getAttribute: (k) => k === 'data-scroll-key' ? key : null };
  const states = api.capture([a]);
  const fresh = { scrollHeight: 600, clientHeight: 200, scrollTop: 0, getAttribute: (k) => k === 'data-scroll-key' ? key : null, addEventListener: () => {} };
  api.restore([fresh], states);
  if (fresh.scrollTop !== 400) throw new Error('non-sticky restore should clamp to new max (400), got ' + fresh.scrollTop);
}

{
  const noKey = { scrollHeight: 1000, clientHeight: 200, scrollTop: 0, getAttribute: () => null };
  const states = api.capture([noKey]);
  if (Object.keys(states).length !== 0) throw new Error('elements without data-log-scroll should be ignored, got ' + JSON.stringify(states));
}

{
  const key = 'restore-fresh-' + Math.random();
  const el = { scrollHeight: 1200, clientHeight: 200, scrollTop: 0, getAttribute: (k) => k === 'data-scroll-key' ? key : null, addEventListener: () => {} };
  api.restore([el], {});
  if (el.scrollTop !== 1000) throw new Error('restore without state should default to sticky (snap to max scroll 1000), got ' + el.scrollTop);
}

{
  const key = 'restore-fresh-sticky-off-' + Math.random();
  api.setSticky(key, false);
  const el = { scrollHeight: 1200, clientHeight: 200, scrollTop: 0, getAttribute: (k) => k === 'data-scroll-key' ? key : null, addEventListener: () => {} };
  api.restore([el], {});
  if (el.scrollTop !== 0) throw new Error('restore with sticky=false and no state should keep scrollTop 0, got ' + el.scrollTop);
}

{
  const key = 'track-' + Math.random();
  let handler = null;
  const el = {
    scrollHeight: 1000, clientHeight: 200, scrollTop: 500,
    getAttribute: (k) => k === 'data-scroll-key' ? key : null,
    addEventListener: (name, fn) => { if (name === 'scroll') handler = fn; },
  };
  api.setSticky(key, true);
  api.trackScroll(el, key);
  if (typeof handler !== 'function') throw new Error('trackScroll should attach a scroll handler');
  el.scrollTop = 1000;
  handler();
  if (api.isSticky(key) !== true) throw new Error('scroll to bottom should set sticky=true');
  el.scrollTop = 400;
  handler();
  if (api.isSticky(key) !== false) throw new Error('scroll away from bottom should set sticky=false');
  api.trackScroll(el, key);
  el.scrollTop = 1000;
  handler();
  if (api.isSticky(key) !== true) throw new Error('subsequent scroll to bottom should set sticky=true again');
}
`
	cmd := exec.Command("node", "-e", script, helperPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portal scroll helper failed: %v\n%s", err, out)
	}
}
