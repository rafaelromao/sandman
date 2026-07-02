package cmd

import "testing"

// TestPortal_EventsSurvivePollCycle_MergePreservesActiveRunEvents is the
// behavior contract for issue #1626: the refresh() poll merge in
// portal.html must preserve the previous `events` array on every run
// whose incoming summary payload omits events, including active runs.
// Today the merge is skipped when existing.kind === 'active', so
// expanded events tabs rebuild from scratch every poll cycle.
//
// The production code is embedded JS with no browser-side test harness
// in this repo (per ADR-0031). The test extracts the merge loop
// directly from portal.html via a regex and drives it through Node's
// vm with a mock prevRuns/nextRuns pair, asserting that an active run
// keeps its events across one poll cycle.
func TestPortal_EventsSurvivePollCycle_MergePreservesActiveRunEvents(t *testing.T) {
	js := `const html = fs.readFileSync('portal.html', 'utf8');
// Extract the per-run merge loop from inside refresh(). The loop body
// is the only place in portal.html that overwrites next.events from
// existing.events, so matching it lets us drive the same code path the
// production poll cycle runs.
const mergeLoop = html.match(/for \(const next of nextRuns\) \{[\s\S]*?\n\s{8}\}/);
if (!mergeLoop) throw new Error('could not locate the refresh() merge loop in portal.html');
const loopSrc = mergeLoop[0];

// Fixture: an active run that has events on the previous snapshot and
// no events on the incoming summary payload. Today the active-run
// guard drops the events; after the fix the merge must keep them.
const activeRun = {
  key: 'run-42-1234567890',
  runId: 'run-42-1234567890',
  kind: 'active',
  status: 'running',
  issueLabel: '#42',
  events: [{ type: 'run.started', timestamp: '2026-01-01T12:00:00Z' }],
};
const completedRun = {
  key: 'run-99-1234567891',
  runId: 'run-99-1234567891',
  kind: 'completed',
  status: 'success',
  issueLabel: '#99',
  events: [{ type: 'run.started', timestamp: '2026-01-01T12:00:00Z' }],
};
const prevRuns = [activeRun, completedRun];
const nextRuns = [
  Object.assign({}, activeRun, { events: undefined }),
  Object.assign({}, completedRun, { events: undefined }),
];

// Drive the production loop body verbatim. The only variable names the
// body depends on are prevRuns and nextRuns; both are bound in the
// context before the loop runs.
const ctx = vm.createContext({
  prevRuns: prevRuns,
  nextRuns: nextRuns,
});
vm.runInContext(loopSrc, ctx, { filename: 'portal.html (merge loop)' });

if (!Array.isArray(nextRuns[0].events) || nextRuns[0].events.length !== 1) {
  throw new Error('expected active run to keep its events across poll, got ' + JSON.stringify(nextRuns[0].events));
}
if (!Array.isArray(nextRuns[1].events) || nextRuns[1].events.length !== 1) {
  throw new Error('expected completed run to keep its events across poll, got ' + JSON.stringify(nextRuns[1].events));
}
console.log('PASS');
`
	runNodeScript(t, js)
}

// TestPortal_EventsSurvivePollCycle_PortalHtmlWiringRemoved is a
// source-grep regression guard: the production refresh() in
// portal.html must no longer contain the active-run skip guard, and
// must still wire the events merge from existing into next.
func TestPortal_EventsSurvivePollCycle_PortalHtmlWiringRemoved(t *testing.T) {
	js := `const html = fs.readFileSync('portal.html', 'utf8');
// Locate the merge loop inside refresh(). The previous skip was
// written as: if (existing.kind !== 'active') { next.events = ... }.
const refreshBody = html.match(/async function refresh\(\)[\s\S]*?\n\s{4}\}/);
if (!refreshBody) throw new Error('could not locate refresh() in portal.html');
const body = refreshBody[0];
const mergeIdx = body.indexOf('for (const next of nextRuns)');
if (mergeIdx === -1) throw new Error('expected refresh() to contain the per-run merge loop');
// Find the merge loop body up to the next sibling for-loop or closing
// brace at the same indent.
const mergeSlice = body.slice(mergeIdx);
const mergeEnd = mergeSlice.search(/\n\s{8}\}/);
if (mergeEnd === -1) throw new Error('expected to find the merge loop closing brace');
const mergeBlock = mergeSlice.slice(0, mergeEnd);
if (/existing\.kind\s*!==?\s*'active'/.test(mergeBlock)) {
  throw new Error('expected refresh() merge to no longer skip active runs, found kind !== active guard in:\n' + mergeBlock);
}
if (!/next\.events\s*=\s*existing\.events/.test(mergeBlock)) {
  throw new Error('expected refresh() merge to copy next.events = existing.events when next has none');
}
if (!/existing\.events\s*&&\s*!next\.events/.test(mergeBlock)) {
  throw new Error('expected refresh() merge to gate on existing.events && !next.events');
}
console.log('PASS');
`
	runNodeScript(t, js)
}