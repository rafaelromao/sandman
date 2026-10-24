package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

// issueGroupJSON runs the Go issue-group projection and the summary transport
// strip, and returns the JSON the page receives from /api/runs?summary=1.
func issueGroupJSON(t *testing.T, runs []portalRun) string {
	t.Helper()
	layout := paths.NewLayout(nil, t.TempDir())
	projected := (&portalRunsView{}).projectIssueGroups(layout, runs)
	data, err := json.Marshal(portalSummaryRuns(projected))
	if err != nil {
		t.Fatalf("marshal runs: %v", err)
	}
	return string(data)
}

// subNanosecondParents returns two terminal parents for one issue whose
// finishedAt differ by 100ns (below JavaScript's millisecond precision), the
// later one carrying the lower runId, plus one linked terminal review.
func subNanosecondParents() []portalRun {
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	earlierFinished := time.Date(2026, 7, 4, 13, 0, 0, 0, time.UTC)
	laterFinished := earlierFinished.Add(100 * time.Nanosecond)
	reviewFinished := laterFinished.Add(time.Minute)
	return []portalRun{
		{IssueNumber: 2802, RunID: "impl-b", Key: "impl-b", IssueLabel: "#2802", Kind: "completed", Status: "aborted", StartedAt: started, FinishedAt: &earlierFinished},
		{IssueNumber: 2802, RunID: "impl-a", Key: "impl-a", IssueLabel: "#2802", Kind: "completed", Status: "success", StartedAt: started, FinishedAt: &laterFinished},
		{IssueNumber: 2802, Review: true, PRNumber: 42, RunID: "rev-1", Key: "rev-1", IssueLabel: "PR 42", Kind: "completed", Status: "success", StartedAt: laterFinished, FinishedAt: &reviewFinished},
	}
}

// TestPortalIssueGroup_TableShowsTheParentGoPicked is the tracer for one owner
// of the issue group: the table renders the parent the Go projection picked,
// even when the two candidate parents finish less than a millisecond apart.
func TestPortalIssueGroup_TableShowsTheParentGoPicked(t *testing.T) {
	runsJSON := issueGroupJSON(t, subNanosecondParents())
	runPortalHTMLScript(t, `
const runs = JSON.parse('`+runsJSON+`');
const visible = visibleRunsForTable(runs).filter((run) => run.issueNumber === 2802);
if (visible.length !== 1) throw new Error('want one visible row for the issue, got ' + JSON.stringify(visible.map((run) => run.runId)));
if (visible[0].runId !== 'impl-a') throw new Error('table shows ' + visible[0].runId + ', want the parent Go picked (impl-a)');
if (visible[0].reviewCount !== 1) throw new Error('visible row reviewCount=' + visible[0].reviewCount + ', want 1');
console.log('PASS');
`)
}

// TestPortalIssueGroup_SelectorAgreesWithTable asserts the subject selector's
// first subject is the parent the table shows, from the same Go projection.
func TestPortalIssueGroup_SelectorAgreesWithTable(t *testing.T) {
	runsJSON := issueGroupJSON(t, subNanosecondParents())
	runPortalHTMLScript(t, `
const runs = JSON.parse('`+runsJSON+`');
const row = visibleRunsForTable(runs).find((run) => run.issueNumber === 2802);
const subjects = portalDiff.subjectRunsFor(row, { runs: runs }).map((run) => run.runId);
if (subjects[0] !== row.runId) throw new Error('selector starts with ' + subjects[0] + ', table shows ' + row.runId);
if (subjects.join(',') !== 'impl-a,rev-1') throw new Error('subjects=' + subjects.join(','));
console.log('PASS');
`)
}

func findPortalRun(t *testing.T, runs []portalRun, runID string) portalRun {
	t.Helper()
	for _, run := range runs {
		if run.RunID == runID {
			return run
		}
	}
	t.Fatalf("run %q missing from projection", runID)
	return portalRun{}
}

// TestPortalIssueGroup_QueuedParentVisibleTerminalParentFirstSubject pins
// that a newer queued parent owns the table row while the selector opens the
// newest expandable parent, because a queued row has no run detail.
func TestPortalIssueGroup_QueuedParentVisibleTerminalParentFirstSubject(t *testing.T) {
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	finished := started.Add(time.Hour)
	reviewFinished := finished.Add(time.Minute)
	layout := paths.NewLayout(nil, t.TempDir())
	runs := (&portalRunsView{}).projectIssueGroups(layout, []portalRun{
		{IssueNumber: 7, RunID: "impl-done", Key: "impl-done", Kind: "completed", Status: "success", StartedAt: started, FinishedAt: &finished},
		{IssueNumber: 7, RunID: "impl-queued", Key: "impl-queued", Kind: "active", Status: "queued", StartedAt: reviewFinished.Add(time.Minute)},
		{IssueNumber: 7, Review: true, RunID: "rev-7", Key: "rev-7", Kind: "completed", Status: "success", StartedAt: finished, FinishedAt: &reviewFinished},
	})
	queued := findPortalRun(t, runs, "impl-queued")
	if !queued.GroupVisible {
		t.Fatalf("queued parent GroupVisible=false, want the newer queued row visible")
	}
	if got, want := queued.Subjects, []string{"impl-done", "rev-7"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Subjects=%v, want %v", got, want)
	}
	for _, id := range []string{"impl-done", "rev-7"} {
		if run := findPortalRun(t, runs, id); run.GroupVisible || len(run.Subjects) != 0 {
			t.Fatalf("%s GroupVisible=%v Subjects=%v, want only the visible row stamped", id, run.GroupVisible, run.Subjects)
		}
	}
}

// TestPortalIssueGroup_OrphanLinkedReviewGroup pins the review-only group:
// the newest live review owns the row, carries the count and the "PR n (#N)"
// label, and keeps the Kind and Status events.RunState projected so Abort
// stays offered.
func TestPortalIssueGroup_OrphanLinkedReviewGroup(t *testing.T) {
	started := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Hour)
	layout := paths.NewLayout(nil, t.TempDir())
	runs := (&portalRunsView{}).projectIssueGroups(layout, []portalRun{
		{IssueNumber: 9, Review: true, PRNumber: 90, RunID: "rev-done", Key: "rev-done", IssueLabel: "PR 90", Kind: "completed", Status: "success", StartedAt: started.Add(time.Hour), FinishedAt: &finished},
		{IssueNumber: 9, Review: true, PRNumber: 90, RunID: "rev-live", Key: "rev-live", IssueLabel: "PR 90", Kind: "active", Status: "reviewing", StartedAt: started},
	})
	live := findPortalRun(t, runs, "rev-live")
	if !live.GroupVisible {
		t.Fatalf("live review GroupVisible=false, want the newest live review visible")
	}
	if live.ReviewCount != 2 || !live.ReviewLive {
		t.Fatalf("ReviewCount=%d ReviewLive=%v, want 2 and true", live.ReviewCount, live.ReviewLive)
	}
	if live.IssueLabel != "PR 90 (#9)" {
		t.Fatalf("IssueLabel=%q, want PR 90 (#9)", live.IssueLabel)
	}
	if live.Kind != "active" || live.Status != "reviewing" {
		t.Fatalf("Kind=%q Status=%q, want the RunState projection active/reviewing", live.Kind, live.Status)
	}
	if got := live.Subjects; len(got) != 2 || got[0] != "rev-done" || got[1] != "rev-live" {
		t.Fatalf("Subjects=%v, want reviews newest first [rev-done rev-live]", got)
	}
	if done := findPortalRun(t, runs, "rev-done"); done.GroupVisible {
		t.Fatalf("terminal review GroupVisible=true, want only the live review visible")
	}
}

// TestPortalIssueGroup_RefreshDoesNotResurrectDroppedParent pins that a
// refresh with an expanded review keeps the expanded subject cached but never
// brings back a parent the server dropped as a second visible row.
func TestPortalIssueGroup_RefreshDoesNotResurrectDroppedParent(t *testing.T) {
	runPortalHTMLScript(t, `
const oldParent = { key: 'impl-old', runId: 'impl-old', kind: 'completed', status: 'success', issueNumber: 5, issueLabel: '#5', startedAt: '2026-07-04T10:00:00Z', finishedAt: '2026-07-04T11:00:00Z', groupVisible: true, subjects: ['impl-old', 'rev-5'] };
const review = { key: 'rev-5', runId: 'rev-5', kind: 'completed', status: 'success', issueNumber: 5, issueLabel: 'PR 50', review: true, prNumber: 50, startedAt: '2026-07-04T11:00:00Z', finishedAt: '2026-07-04T11:30:00Z' };
const newParent = { key: 'impl-new', runId: 'impl-new', kind: 'active', status: 'running', issueNumber: 5, issueLabel: '#5', startedAt: '2026-07-04T12:00:00Z', groupVisible: true, subjects: ['impl-new'] };
state.runs = [oldParent, review];
state.expandedRunKey = 'rev-5';
CSS = { escape: function (value) { return String(value); } };
fetch = function () {
  return Promise.resolve({ ok: true, status: 200, headers: { get() { return ''; } }, json: function () { return Promise.resolve({ runs: [newParent] }); } });
};
refresh().then(function () {
  const visible = visibleRunsForTable(state.runs).filter((run) => run.issueNumber === 5);
  if (visible.length !== 1 || visible[0].runId !== 'impl-new') throw new Error('visible rows after refresh: ' + JSON.stringify(visible.map((run) => run.runId)));
  const cached = state.runs.find((run) => run.runId === 'rev-5');
  if (!cached) throw new Error('expanded review subject was not kept across refresh');
  if (cached.groupVisible || cached.subjects) throw new Error('restored subject kept group stamps: ' + JSON.stringify(cached));
  console.log('PASS');
}).catch(function (err) { console.error(err); process.exit(1); });
`)
}

// stampIssueGroupsJSON stamps a hand-built /api/runs fixture (a JSON array
// of run objects) with the issue-group fields the server projects, so page
// fixtures receive what compute would send. It runs the production
// projectIssueGroups over a minimal portalRun view of each object and copies
// back only the group-owned fields of the visible rows.
func stampIssueGroupsJSON(t *testing.T, runsJSON []byte) []byte {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(runsJSON, &rows); err != nil {
		t.Fatalf("parse runs fixture: %v", err)
	}
	stampIssueGroups(rows)
	out, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal runs fixture: %v", err)
	}
	return out
}

func stampIssueGroups(rows []map[string]any) {
	str := func(row map[string]any, key string) string {
		value, _ := row[key].(string)
		return value
	}
	num := func(row map[string]any, key string) int {
		switch value := row[key].(type) {
		case float64:
			return int(value)
		case int:
			return value
		}
		return 0
	}
	ts := func(row map[string]any, key string) *time.Time {
		parsed, err := time.Parse(time.RFC3339Nano, str(row, key))
		if err != nil {
			return nil
		}
		return &parsed
	}
	runs := make([]portalRun, len(rows))
	for i, row := range rows {
		run := portalRun{
			Key: str(row, "key"), RunID: str(row, "runId"), Kind: str(row, "kind"), Status: str(row, "status"),
			IssueLabel: str(row, "issueLabel"), IssueNumber: num(row, "issueNumber"), PRNumber: num(row, "prNumber"),
			FinishedAt: ts(row, "finishedAt"),
		}
		run.Review, _ = row["review"].(bool)
		if started := ts(row, "startedAt"); started != nil {
			run.StartedAt = *started
		}
		if rawEvents, ok := row["events"].([]any); ok {
			for _, raw := range rawEvents {
				event, _ := raw.(map[string]any)
				payload, _ := event["payload"].(map[string]any)
				run.Events = append(run.Events, portalEvent{Type: str(event, "type"), Payload: payload})
			}
		}
		runs[i] = run
	}
	projected := (&portalRunsView{}).projectIssueGroups(paths.Layout{}, runs)
	for i, run := range projected {
		row := rows[i]
		delete(row, "groupVisible")
		delete(row, "subjects")
		if !run.GroupVisible {
			continue
		}
		row["groupVisible"] = true
		if len(run.Subjects) > 0 {
			row["subjects"] = run.Subjects
		}
		row["status"] = run.Status
		row["issueLabel"] = run.IssueLabel
		if run.ReviewCount > 0 {
			row["reviewCount"] = run.ReviewCount
		}
		if run.ReviewLive {
			row["reviewLive"] = true
		}
	}
}

// marshalPortalRunsFixture marshals a hand-built /api/runs fixture and stamps
// the issue-group fields the server projects (see stampIssueGroupsJSON).
func marshalPortalRunsFixture(rows any) ([]byte, error) {
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	var decoded []map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	stampIssueGroups(decoded)
	return json.Marshal(decoded)
}

// TestPortalIssueGroup_RefreshKeepsExpandedDroppedParentHidden pins that an
// expanded parent the server dropped stays cached for its detail but is not
// shown beside the group's new visible row.
func TestPortalIssueGroup_RefreshKeepsExpandedDroppedParentHidden(t *testing.T) {
	runPortalHTMLScript(t, `
const oldParent = { key: 'impl-old', runId: 'impl-old', kind: 'completed', status: 'success', issueNumber: 5, issueLabel: '#5', startedAt: '2026-07-04T10:00:00Z', finishedAt: '2026-07-04T11:00:00Z', groupVisible: true, subjects: ['impl-old'] };
const newParent = { key: 'impl-new', runId: 'impl-new', kind: 'active', status: 'running', issueNumber: 5, issueLabel: '#5', startedAt: '2026-07-04T12:00:00Z', groupVisible: true, subjects: ['impl-new'] };
state.runs = [oldParent];
state.expandedRunKey = 'impl-old';
CSS = { escape: function (value) { return String(value); } };
fetch = function () {
  return Promise.resolve({ ok: true, status: 200, headers: { get() { return ''; } }, json: function () { return Promise.resolve({ runs: [newParent] }); } });
};
refresh().then(function () {
  const visible = visibleRunsForTable(state.runs).filter((run) => run.issueNumber === 5);
  if (visible.length !== 1 || visible[0].runId !== 'impl-new') throw new Error('visible rows after refresh: ' + JSON.stringify(visible.map((run) => run.runId)));
  const cached = state.runs.find((run) => run.runId === 'impl-old');
  if (!cached || cached.groupVisible || cached.subjects) throw new Error('dropped parent not cached without stamps: ' + JSON.stringify(cached));
  console.log('PASS');
}).catch(function (err) { console.error(err); process.exit(1); });
`)
}

func subjectsOf(t *testing.T, runs []portalRun) []string {
	t.Helper()
	var visible []portalRun
	for _, run := range runs {
		if run.GroupVisible {
			visible = append(visible, run)
		}
	}
	if len(visible) != 1 {
		t.Fatalf("want exactly one GroupVisible row, got %d", len(visible))
	}
	return visible[0].Subjects
}

func assertSubjects(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Subjects=%v, want %v", got, want)
	}
}

func at(hour, minute int) time.Time {
	return time.Date(2026, 7, 6, hour, minute, 0, 0, time.UTC)
}

func atPtr(hour, minute int) *time.Time {
	value := at(hour, minute)
	return &value
}

// TestPortalIssueGroup_SubjectsLeadWithCanonicalParent pins the subject
// contract of issue #1525 AC3 (ported from the JS subjectRunsFor tests): the
// canonical parent comes first and every review stays reachable, newest first.
func TestPortalIssueGroup_SubjectsLeadWithCanonicalParent(t *testing.T) {
	layout := paths.Layout{}
	runs := (&portalRunsView{}).projectIssueGroups(layout, []portalRun{
		{IssueNumber: 1, RunID: "impl-1", Key: "impl-1", Kind: "completed", Status: "success", StartedAt: at(10, 0), FinishedAt: atPtr(11, 0)},
		{IssueNumber: 1, Review: true, RunID: "PR42", Key: "PR42", Kind: "active", Status: "reviewing", StartedAt: at(12, 0)},
		{IssueNumber: 1, Review: true, RunID: "PR43", Key: "PR43", Kind: "completed", Status: "success", StartedAt: at(11, 30), FinishedAt: atPtr(11, 45)},
	})
	assertSubjects(t, subjectsOf(t, runs), "impl-1", "PR42", "PR43")
}

// TestPortalIssueGroup_SubjectsIncludeContinuationPredecessor pins that a
// continuation row offers its predecessor run right after itself.
func TestPortalIssueGroup_SubjectsIncludeContinuationPredecessor(t *testing.T) {
	runs := (&portalRunsView{}).projectIssueGroups(paths.Layout{}, []portalRun{
		{IssueNumber: 1, RunID: "new-run", Key: "new-run", Kind: "active", Status: "running", StartedAt: at(10, 0),
			Events: []portalEvent{{Type: "run.continued", Payload: map[string]any{"previous_run_id": "old-run"}}}},
		{IssueNumber: 1, RunID: "old-run", Key: "old-run", Kind: "completed", Status: "success", StartedAt: at(9, 0), FinishedAt: atPtr(9, 30)},
	})
	assertSubjects(t, subjectsOf(t, runs), "new-run", "old-run")
}

// TestPortalIssueGroup_SubjectsPickCanonicalAmongParents pins issue #1855:
// with an older aborted parent and a newer successful one plus reviews, the
// newest terminal parent leads and the older aborted parent is not offered.
func TestPortalIssueGroup_SubjectsPickCanonicalAmongParents(t *testing.T) {
	runs := (&portalRunsView{}).projectIssueGroups(paths.Layout{}, []portalRun{
		{IssueNumber: 1855, Review: true, RunID: "rev-late", Key: "rev-late", Kind: "completed", Status: "success", StartedAt: at(14, 8), FinishedAt: atPtr(14, 20)},
		{IssueNumber: 1855, Review: true, RunID: "rev-early", Key: "rev-early", Kind: "completed", Status: "success", StartedAt: at(13, 44), FinishedAt: atPtr(13, 50)},
		{IssueNumber: 1855, RunID: "impl-success", Key: "impl-success", Kind: "completed", Status: "success", StartedAt: at(13, 20), FinishedAt: atPtr(13, 40)},
		{IssueNumber: 1855, RunID: "impl-aborted", Key: "impl-aborted", Kind: "completed", Status: "aborted", StartedAt: at(12, 0), FinishedAt: atPtr(12, 30)},
	})
	assertSubjects(t, subjectsOf(t, runs), "impl-success", "rev-late", "rev-early")
}

// TestPortalIssueGroup_ActiveParentWinsOverOlderTerminals pins that the
// newest active parent owns the group over older terminal parents.
func TestPortalIssueGroup_ActiveParentWinsOverOlderTerminals(t *testing.T) {
	runs := (&portalRunsView{}).projectIssueGroups(paths.Layout{}, []portalRun{
		{IssueNumber: 100, RunID: "old-impl", Key: "old-impl", Kind: "completed", Status: "success", StartedAt: at(10, 0), FinishedAt: atPtr(10, 30)},
		{IssueNumber: 100, RunID: "new-impl-active", Key: "new-impl-active", Kind: "active", Status: "running", StartedAt: at(15, 0)},
		{IssueNumber: 100, RunID: "newer-active", Key: "newer-active", Kind: "active", Status: "running", StartedAt: at(15, 30)},
	})
	if visible := findPortalRun(t, runs, "newer-active"); !visible.GroupVisible {
		t.Fatalf("newest active parent is not the visible row")
	}
	assertSubjects(t, subjectsOf(t, runs), "newer-active")
}

// TestPortalIssueGroup_ExpandedSubjectMatchesOnlyServerSubjects pins that the
// group row opens its detail for a subject only when the server listed it in
// the row's subjects; sharing the issue number is not membership.
func TestPortalIssueGroup_ExpandedSubjectMatchesOnlyServerSubjects(t *testing.T) {
	runNodeScript(t, `const body = makeMockBody();
const parent = { key: 'impl-new', runId: 'impl-new', kind: 'active', status: 'running', issueNumber: 1, issueLabel: '#1', groupVisible: true, subjects: ['impl-new', 'rev-1'] };
const review = { key: 'rev-1', runId: 'rev-1', kind: 'completed', status: 'success', review: true, issueNumber: 1, issueLabel: 'PR 9', prNumber: 9 };
const stray = { key: 'impl-old', runId: 'impl-old', kind: 'completed', status: 'aborted', issueNumber: 1, issueLabel: '#1' };
const runs = [parent, review, stray];
const strayOpts = { helpers, stopGroups: new Set(), expandedKey: 'impl-old', runs: runs, visibleRuns: [parent] };
const strayRow = SandmanPortalDiff.insertRunRow(makeMockBody(), parent, strayOpts);
if (strayRow.detailRow) throw new Error('group row opened for impl-old, which is not in the server subjects');
const reviewOpts = { helpers, stopGroups: new Set(), expandedKey: 'rev-1', runs: runs, visibleRuns: [parent] };
const reviewRow = SandmanPortalDiff.insertRunRow(body, parent, reviewOpts);
if (!reviewRow.detailRow) throw new Error('group row must open for a listed subject');
console.log('PASS');
`)
}

// TestPortalIssueGroup_StaleSubjectIDsAreSkipped pins that a subject id which
// no longer resolves to a run (for example on a restored row) is skipped.
func TestPortalIssueGroup_StaleSubjectIDsAreSkipped(t *testing.T) {
	runNodeScript(t, `const parent = { key: 'impl-1', runId: 'impl-1', kind: 'active', status: 'running', issueNumber: 1, groupVisible: true, subjects: ['impl-1', 'gone', 'rev-1'] };
const review = { key: 'rev-1', runId: 'rev-1', kind: 'completed', status: 'success', review: true, issueNumber: 1 };
const ids = SandmanPortalDiff.subjectRunsFor(parent, { runs: [parent, review] }).map((run) => run.runId);
if (ids.join(',') !== 'impl-1,rev-1') throw new Error('subjects=' + ids.join(','));
console.log('PASS');
`)
}

// TestPortalIssueGroup_SubjectSwitchAndHiddenSubjectChangesRerender pins that
// the detail panel, whose subject fingerprint is taken from the group row,
// still re-renders when the subject switches and when a non-visible subject's
// events change.
func TestPortalIssueGroup_SubjectSwitchAndHiddenSubjectChangesRerender(t *testing.T) {
	runNodeScript(t, `const body = makeMockBody();
const parent = { key: 'impl-1', runId: 'impl-1', kind: 'active', status: 'running', issueNumber: 1, issueLabel: '#1', groupVisible: true, subjects: ['impl-1', 'rev-1'], events: [{ type: 'run.started', timestamp: 1, payload: { who: 'parent' } }] };
const review = { key: 'rev-1', runId: 'rev-1', kind: 'active', status: 'reviewing', review: true, issueNumber: 1, issueLabel: 'PR 9', prNumber: 9, events: [{ type: 'run.started', timestamp: 2, payload: { who: 'review' } }] };
const stopGroups = new Set();
const tabs = { 'impl-1': 'events', 'rev-1': 'events' };
SandmanPortalDiff.diffRuns(body, [parent], { helpers, stopGroups, expandedKey: 'impl-1', tabs, runs: [parent, review], visibleRuns: [parent] });
const detail = () => body.querySelector('tr.detail-row[data-detail-for="impl-1"]');
const json = () => detail().querySelector('pre[data-rendered-json]').getAttribute('data-rendered-json');
if (json().indexOf('parent') < 0) throw new Error('expected parent events first, got ' + json());
SandmanPortalDiff.diffRuns(body, [parent], { helpers, stopGroups, expandedKey: 'rev-1', tabs, runs: [parent, review], visibleRuns: [parent] });
if (json().indexOf('review') < 0 || json().indexOf('parent') >= 0) throw new Error('switching subject did not re-render, got ' + json());
const changed = Object.assign({}, review, { events: review.events.concat([{ type: 'run.finished', timestamp: 3, payload: { who: 'review-done' } }]) });
SandmanPortalDiff.diffRuns(body, [parent], { helpers, stopGroups, expandedKey: 'rev-1', tabs, runs: [parent, changed], visibleRuns: [parent] });
if (json().indexOf('review-done') < 0) throw new Error('hidden subject event change did not re-render, got ' + json());
console.log('PASS');
`)
}

// TestPortalIssueGroup_ComputeFoldsQueuedAndBlockedEvents folds real events
// through the production compute path into the issue-group projection: an
// initial-admission queued parent owns its group while the selector opens the
// terminal parent, and a blocked parent keeps its outcome and is never a
// subject.
func TestPortalIssueGroup_ComputeFoldsQueuedAndBlockedEvents(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	eventsPath := filepath.Join(repoRoot, ".sandman", "events.jsonl")
	review := func(runID string, issue int, at time.Time) []events.Event {
		payload := map[string]any{"batch_id": "b-" + runID, "review": true, "pr_number": 900 + issue, "issue_number": issue}
		return []events.Event{
			{Type: "run.started", Timestamp: at, RunID: runID, Issue: issue, Payload: payload},
			{Type: "run.finished", Timestamp: at.Add(time.Minute), RunID: runID, Issue: issue, Payload: map[string]any{"batch_id": "b-" + runID, "review": true, "status": "success"}},
		}
	}
	log := []events.Event{
		{Type: "run.started", Timestamp: base, RunID: "impl-77-done", Issue: 77, Payload: map[string]any{"batch_id": "b-77-done"}},
		{Type: "run.finished", Timestamp: base.Add(10 * time.Minute), RunID: "impl-77-done", Issue: 77, Payload: map[string]any{"batch_id": "b-77-done", "status": "success"}},
	}
	log = append(log, review("rev-77", 77, base.Add(20*time.Minute))...)
	log = append(log,
		events.Event{Type: "run.queued", Timestamp: base.Add(30 * time.Minute), RunID: "impl-77-queued", Issue: 77, Payload: map[string]any{"batch_id": "b-77-queued", "initial_admission": true}},
		events.Event{Type: "run.started", Timestamp: base, RunID: "impl-78-blocked", Issue: 78, Payload: map[string]any{"batch_id": "b-78"}},
		events.Event{Type: "run.blocked", Timestamp: base.Add(5 * time.Minute), RunID: "impl-78-blocked", Issue: 78, Payload: map[string]any{"batch_id": "b-78"}},
	)
	log = append(log, review("rev-78", 78, base.Add(6*time.Minute))...)
	writePortalLog(t, eventsPath, log)

	runs, err := (&portalRunsView{}).compute(repoRoot, &events.JSONLLogger{Path: eventsPath})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	queued := findPortalRun(t, runs, "impl-77-queued")
	if !queued.GroupVisible || queued.Status != "queued" {
		t.Fatalf("queued parent GroupVisible=%v Status=%q, want the visible queued row", queued.GroupVisible, queued.Status)
	}
	assertSubjects(t, queued.Subjects, "impl-77-done", "rev-77")
	blocked := findPortalRun(t, runs, "impl-78-blocked")
	if !blocked.GroupVisible || blocked.Status != "blocked" || blocked.ReviewCount != 1 {
		t.Fatalf("blocked parent GroupVisible=%v Status=%q ReviewCount=%d, want visible blocked row with one review", blocked.GroupVisible, blocked.Status, blocked.ReviewCount)
	}
	assertSubjects(t, blocked.Subjects, "rev-78")
}
