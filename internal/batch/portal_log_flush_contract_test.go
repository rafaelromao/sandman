package batch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/paths"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// AC-08: the Saved Run Log must already contain the final unterminated
// stdout/stderr when an event-derived terminal outcome becomes observable.
// Only sandbox execution is scripted: the default factory creates the real
// AgentRun, including its prefix writers, Flush calls and O_APPEND log file.
func TestPortalLogFlushBeforeTerminalEvent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		execErr   error
		abort     bool
		review    bool
		wantType  string
		wantState string
	}{
		{name: "normal_completion", wantType: "run.finished", wantState: "success"},
		{name: "execution_failure", execErr: errors.New("agent exited 1"), wantType: "run.finished", wantState: "failure"},
		{name: "operator_abort", abort: true, wantType: "run.aborted", wantState: "aborted"},
		{name: "review_completion", review: true, wantType: "run.finished", wantState: "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := newPortalFlushStep(1, tc.execErr)
			// Abort has an available retry budget: cancellation must still
			// publish exactly one terminal event without another execution.
			retries := 0
			if tc.abort {
				retries = 2
			}
			f := newPortalFlushFixture(t, tc.review, retries, step)
			done := f.start(t, f.row)
			portalFlushWait(t, step.written, "agent output")
			f.assertBuffered(t, step)
			if tc.abort {
				if err := f.coord.AbortIssue(f.row.IssueNumber); err != nil {
					t.Fatalf("operator abort: %v", err)
				}
				portalFlushWait(t, step.cancelled, "sandbox cancellation")
				// Simulate the sandbox still draining a cancelled process.
				// Cancellation itself must not publish terminality ahead of Exec.
				f.assertBuffered(t, step)
			}
			step.finish()
			portalFlushResult(t, done, tc.wantState)
			boundary := f.assertTerminal(t, tc.wantType, tc.wantState, step)
			if boundary.state.IsReview() != tc.review {
				t.Fatalf("terminal review projection = %v, want %v", boundary.state.IsReview(), tc.review)
			}
			if calls := f.sb.calls.Load(); calls != 1 {
				t.Fatalf("agent executions = %d, want 1", calls)
			}
			f.assertEventCount(t, "run.retry", 0)
		})
	}
}

func TestPortalLogFlushSameRunRetryAppendsBeforeTerminalEvent(t *testing.T) {
	first := newPortalFlushStep(1, errors.New("retryable execution failure"))
	second := newPortalFlushStep(2, nil)
	f := newPortalFlushFixture(t, false, 1, first, second)
	done := f.start(t, f.row)
	portalFlushWait(t, first.written, "first attempt")
	f.assertBuffered(t, first)
	initial := f.readLog(t)
	identity := portalFlushStat(t, f.log.logPath)
	first.finish()
	portalFlushWait(t, second.written, "ordinary retry")
	f.assertBuffered(t, second)
	retry := f.boundary(t, "run.retry")
	if !retry.state.IsActive() || retry.state.IsAwaiting() || retry.prior.IsTerminal() {
		t.Fatalf("retry prematurely ended the Run: %+v", retry)
	}
	portalFlushAssertFragments(t, retry.log, first)
	if !bytes.HasPrefix(retry.log, initial) || !bytes.Contains(retry.log, []byte("--- retry 1/1 ---")) {
		t.Fatalf("ordinary retry discarded old bytes or missed retry marker: %q", retry.log)
	}
	second.finish()
	result := portalFlushResult(t, done, "success")
	if result.RetriesTotal != 2 {
		t.Fatalf("attempts = %d, want 2", result.RetriesTotal)
	}
	terminal := f.assertTerminal(t, "run.finished", "success", first, second)
	if !bytes.HasPrefix(terminal.log, retry.log) || !os.SameFile(identity, portalFlushStat(t, f.log.logPath)) {
		t.Fatalf("same-RunID retry replaced/truncated its saved artifact: %q", terminal.log)
	}
	f.assertEventCount(t, "run.started", 1)
	f.assertEventCount(t, "run.retry", 1)
}

func TestPortalLogFlushSameRunAwaitContinuationAppendsBeforeTerminalEvent(t *testing.T) {
	first := newPortalFlushStep(1, nil)
	second := newPortalFlushStep(2, nil)
	f := newPortalFlushFixture(t, false, 0, first, second)
	pr := github.PR{Number: 17, State: "open", Body: "Closes #42", HeadRefName: f.row.Branches[42], HeadRefOid: "head", MergeStateStatus: "BLOCKED", StatusCheckRollup: "pending"}
	first.beforeReturn = func() { f.client.setPR(pr) }
	done := f.start(t, f.row)
	portalFlushWait(t, first.written, "pre-wait execution")
	f.assertBuffered(t, first)
	identity := portalFlushStat(t, f.log.logPath)
	first.finish()
	portalFlushResult(t, done, "await")
	await := f.boundary(t, "run.await")
	if await.state.IsTerminal() || !await.state.IsAwaiting() || await.state.Status() != "waiting" {
		t.Fatalf("await authorized premature end: %+v", await.state)
	}
	portalFlushAssertFragments(t, await.log, first)
	f.assertEventCount(t, "run.finished", 0)
	f.assertEventCount(t, "run.aborted", 0)
	if !bytes.Equal(await.log, f.readLog(t)) {
		t.Fatal("waiting changed the saved log after its flushed await boundary")
	}

	// Failed current-head checks select implementation-owned repair at entry;
	// the continuation actually executes and then observes a verified merge.
	pr.StatusCheckRollup = "failure"
	f.client.setPR(pr)
	second.beforeReturn = func() {
		pr.State, pr.Merged, pr.StatusCheckRollup = "merged", true, "success"
		f.client.setPR(pr)
	}
	row := f.row
	row.Mode = ModeContinue
	row.PreviousRunIDs = map[int]string{42: f.runID}
	row.PreviousRunBatchIDs = map[int]string{42: row.BatchID}
	done = f.start(t, row)
	portalFlushWait(t, second.written, "same-RunID continuation")
	f.assertBuffered(t, second)
	continued := f.boundary(t, "run.continued")
	if continued.prior.IsTerminal() || !continued.prior.IsAwaiting() || !continued.state.IsActive() || continued.state.IsAwaiting() {
		t.Fatalf("continuation lifecycle = %+v, prior = %+v", continued.state, continued.prior)
	}
	if !bytes.Equal(continued.log, await.log) || !bytes.HasPrefix(f.readLog(t), await.log) {
		t.Fatal("same-RunID continuation lost or replayed pre-wait saved bytes")
	}
	second.finish()
	portalFlushResult(t, done, "success")
	terminal := f.assertTerminal(t, "run.finished", "success", first, second)
	if !bytes.HasPrefix(terminal.log, await.log) || !os.SameFile(identity, portalFlushStat(t, f.log.logPath)) {
		t.Fatal("same-RunID continuation replaced/truncated the saved artifact")
	}
	if calls := f.sb.calls.Load(); calls != 2 {
		t.Fatalf("agent executions = %d, want 2", calls)
	}
	f.assertEventCount(t, "run.started", 1)
	f.assertEventCount(t, "run.await", 1)
	f.assertEventCount(t, "run.continued", 1)
	f.assertEventCount(t, "run.retry", 0)
}

type portalFlushBoundary struct {
	event events.Event
	log   []byte
	prior events.RunState
	state events.RunState
}

// Read the real artifact synchronously BEFORE publishing to the real JSONL
// logger. A post-execution-only assertion would miss terminal-before-flush.
type portalFlushEventLog struct {
	events.EventLog
	logPath    string
	mu         sync.Mutex
	boundaries []portalFlushBoundary
	err        error
}

func (l *portalFlushEventLog) Log(event events.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var boundary *portalFlushBoundary
	switch event.Type {
	case "run.finished", "run.aborted", "run.retry", "run.await", "run.continued":
		boundary = &portalFlushBoundary{event: event}
		var err error
		boundary.log, err = os.ReadFile(l.logPath)
		l.err = errors.Join(l.err, err)
		prior, err := l.EventLog.Read()
		l.err = errors.Join(l.err, err)
		boundary.prior = portalFlushState(prior, event.RunID)
	}
	if err := l.EventLog.Log(event); err != nil {
		l.err = errors.Join(l.err, err)
		return err
	}
	if boundary != nil {
		published, err := l.EventLog.Read()
		l.err = errors.Join(l.err, err)
		boundary.state = portalFlushState(published, event.RunID)
		l.boundaries = append(l.boundaries, *boundary)
	}
	return nil
}

func portalFlushState(log []events.Event, runID string) events.RunState {
	for _, state := range events.ProjectRunStates(log) {
		if state.RunID == runID {
			return state
		}
	}
	return events.RunState{}
}

type portalFlushStep struct {
	complete     string
	stdout       string
	stderr       string
	err          error
	written      chan struct{}
	cancelled    chan struct{}
	release      chan struct{}
	once         sync.Once
	beforeReturn func()
}

func newPortalFlushStep(attempt int, err error) *portalFlushStep {
	return &portalFlushStep{
		complete:  fmt.Sprintf("attempt-%d-complete", attempt),
		stdout:    fmt.Sprintf("attempt-%d-final-stdout", attempt),
		stderr:    fmt.Sprintf("attempt-%d-final-stderr", attempt),
		err:       err,
		written:   make(chan struct{}),
		cancelled: make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (s *portalFlushStep) finish() { s.once.Do(func() { close(s.release) }) }

type portalFlushSandbox struct {
	workDir string
	steps   []*portalFlushStep
	calls   atomic.Int32
}

func (s *portalFlushSandbox) Exec(ctx context.Context, _ string, stdout, stderr io.Writer) error {
	index := int(s.calls.Add(1)) - 1
	if index >= len(s.steps) {
		return errors.New("unexpected extra agent execution")
	}
	step := s.steps[index]
	for _, write := range []struct {
		writer io.Writer
		text   string
	}{{stdout, step.complete + "\n"}, {stdout, step.stdout}, {stderr, step.stderr}} {
		if _, err := io.WriteString(write.writer, write.text); err != nil {
			return err
		}
	}
	close(step.written)
	select {
	case <-ctx.Done():
		if _, err := io.WriteString(stderr, "-after-cancel"); err != nil {
			return err
		}
		close(step.cancelled)
		<-step.release // process-output drain is deliberately held after abort
		return ctx.Err()
	case <-step.release:
		if step.beforeReturn != nil {
			step.beforeReturn()
		}
		return step.err
	}
}

func (s *portalFlushSandbox) Start(sandbox.SandboxStart) error { return os.MkdirAll(s.workDir, 0o755) }
func (s *portalFlushSandbox) Stop() error                      { return nil }
func (s *portalFlushSandbox) WorkDir() string                  { return s.workDir }
func (s *portalFlushSandbox) RepoPath() string                 { return filepath.Dir(s.workDir) }
func (s *portalFlushSandbox) WritePrompt(string) error {
	return errors.New("fixture requires TaskPrompt")
}
func (s *portalFlushSandbox) Process() sandbox.Process { return nil }
func (s *portalFlushSandbox) RestoreHostPaths() error  { return nil }
func (s *portalFlushSandbox) ExecInteractive(context.Context, string) error {
	return errors.New("unexpected interactive execution")
}

type portalFlushSandboxFactory struct{ sb *portalFlushSandbox }

func (f portalFlushSandboxFactory) NewSandbox(string, string, string, string, sandbox.Container) sandbox.Sandbox {
	return f.sb
}

type portalFlushClient struct {
	fakeGitHubClient
	mu sync.Mutex
	pr *github.PR
}

func (c *portalFlushClient) FindPRByBranch(context.Context, string) (*github.PR, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pr == nil {
		return nil, nil
	}
	snapshot := *c.pr
	return &snapshot, nil
}

func (c *portalFlushClient) setPR(pr github.PR) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pr = &pr
}

type portalFlushFixture struct {
	executor RunExecutor
	coord    *batchCoordinator
	client   *portalFlushClient
	sb       *portalFlushSandbox
	log      *portalFlushEventLog
	row      RowSpec
	runID    string
}

func newPortalFlushFixture(t *testing.T, review bool, retries int, steps ...*portalFlushStep) *portalFlushFixture {
	t.Helper()
	// The legacy conflict post-check has its own documented subprocess seam;
	// it is unrelated to writer completion and must not call live GitHub.
	previousLookup := lookupOpenPRFn
	lookupOpenPRFn = func(string) (bool, int, string, error) { return false, 0, "", nil }
	t.Cleanup(func() { lookupOpenPRFn = previousLookup })
	root := testenv.MkdirShort(t, "pf-")
	cfg := &config.Config{WorktreeDir: filepath.Join(root, "wt")}
	layout := paths.NewLayout(cfg, root)
	row := RowSpec{
		IssueNumber: 42, Branches: map[int]string{42: "42-flush", 0: "review-flush"}, BaseBranch: "main",
		BatchID: "261010120000-abcd", RunTS: "261010120000", RunShortID: "abcd",
		RenderCfg: prompt.RenderConfig{TaskPrompt: "Complete the flush contract fixture.", ReviewCommandSet: true},
	}
	runID := buildRunID(42, row.RunTS, row.RunShortID)
	if review {
		row.Review, row.PRNumber, row.RunID = true, 17, row.BatchID+"-PR17"
		runID = row.RunID
	}
	client := &portalFlushClient{fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Flush", State: "closed"}}}}
	sb := &portalFlushSandbox{workDir: filepath.Join(root, "wt"), steps: steps}
	log := &portalFlushEventLog{EventLog: &events.JSONLLogger{Path: filepath.Join(root, "events.jsonl")}, logPath: layout.RunLogPath(row.BatchID, runID)}
	factory := portalFlushSandboxFactory{sb: sb}
	o := NewOrchestrator(client, &noopRenderer{}, nil, log, WithErrorLog(io.Discard), WithSandboxFactory(factory),
		WithRunSessionOpts(runSessionOptions{currentHead: func(string) (string, error) { return "head", nil }}))
	bc := BatchConfig{Cfg: cfg, IdentityResolver: noopIdentityResolver(), AgentName: "test-agent", AgentCfg: config.Agent{Command: "true"}, Retries: retries}
	coord := newBatchCoordinator(nil)
	executor := o.newRunExecutorWith(t.Context(), bc, factory, nil, coord, coord, layout)
	t.Cleanup(func() { _ = coord.stopCommandServer(row.IssueNumber) })
	return &portalFlushFixture{executor: executor, coord: coord, client: client, sb: sb, log: log, row: row, runID: runID}
}

type portalFlushExecution struct {
	result  AgentRunResult
	started bool
}

func (f *portalFlushFixture) start(t *testing.T, row RowSpec) <-chan portalFlushExecution {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	f.coord.registerIssueCancel(row.IssueNumber, cancel)
	done := make(chan portalFlushExecution, 1)
	settled := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		for _, step := range f.sb.steps {
			step.finish()
		}
		portalFlushWait(t, settled, "executor cleanup")
		f.coord.unregisterIssueCancel(row.IssueNumber)
	})
	go func() {
		result, started := f.executor.Execute(ctx, row)
		done <- portalFlushExecution{result, started}
		close(settled)
	}()
	return done
}

func portalFlushWait(t *testing.T, ready <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func portalFlushResult(t *testing.T, done <-chan portalFlushExecution, want string) AgentRunResult {
	t.Helper()
	select {
	case execution := <-done:
		if !execution.started || execution.result.Status != want {
			t.Fatalf("execution = %+v, want started %s", execution, want)
		}
		return execution.result
	case <-time.After(10 * time.Second):
		t.Fatal("executor did not settle")
		return AgentRunResult{}
	}
}

func (f *portalFlushFixture) readLog(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(f.log.logPath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func portalFlushStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func (f *portalFlushFixture) assertBuffered(t *testing.T, step *portalFlushStep) {
	t.Helper()
	data := f.readLog(t)
	if !bytes.Contains(data, []byte(step.complete+"\n")) || bytes.Contains(data, []byte(step.stdout)) || bytes.Contains(data, []byte(step.stderr)) {
		t.Fatalf("fixture must hold unterminated output in the real prefix writers: %q", data)
	}
	log, err := f.log.Read()
	if err != nil {
		t.Fatal(err)
	}
	state := portalFlushState(log, f.runID)
	if state.RunID != f.runID || !state.HasStarted() || !state.IsActive() || state.IsAwaiting() || state.IsCapacityQueued() {
		t.Fatalf("EOF/cancellation before writer flush authorized end: %+v", state)
	}
}

func (f *portalFlushFixture) boundary(t *testing.T, eventType string) portalFlushBoundary {
	t.Helper()
	f.log.mu.Lock()
	defer f.log.mu.Unlock()
	if f.log.err != nil {
		t.Fatalf("boundary observation failed: %v", f.log.err)
	}
	var found []portalFlushBoundary
	for _, boundary := range f.log.boundaries {
		if boundary.event.Type == eventType {
			found = append(found, boundary)
		}
	}
	if len(found) != 1 || found[0].event.RunID != f.runID {
		t.Fatalf("%s boundaries = %+v, want exactly one for %s", eventType, found, f.runID)
	}
	return found[0]
}

func portalFlushAssertFragments(t *testing.T, data []byte, steps ...*portalFlushStep) {
	t.Helper()
	last := -1
	for _, step := range steps {
		for _, fragment := range []string{step.complete, step.stdout, step.stderr} {
			position := bytes.Index(data, []byte(fragment))
			if bytes.Count(data, []byte(fragment)) != 1 || position <= last {
				t.Fatalf("saved bytes lost/repeated/reordered %q: %q", fragment, data)
			}
			last = position
		}
	}
}

func (f *portalFlushFixture) assertTerminal(t *testing.T, eventType, status string, steps ...*portalFlushStep) portalFlushBoundary {
	t.Helper()
	boundary := f.boundary(t, eventType)
	if boundary.prior.IsTerminal() || !boundary.state.IsTerminal() || boundary.state.Status() != status {
		t.Fatalf("terminal publication fold: prior=%+v published=%+v", boundary.prior, boundary.state)
	}
	portalFlushAssertFragments(t, boundary.log, steps...)
	finalFragment := steps[len(steps)-1].stderr
	if eventType == "run.aborted" {
		finalFragment += "-after-cancel"
	}
	if !bytes.HasSuffix(boundary.log, []byte(finalFragment)) {
		t.Fatalf("terminal boundary missed the unterminated final fragment %q: %q", finalFragment, boundary.log)
	}
	if !bytes.Equal(boundary.log, f.readLog(t)) {
		t.Fatal("final saved bytes appeared only after terminal event publication")
	}
	for _, terminalType := range []string{"run.finished", "run.aborted"} {
		want := 0
		if terminalType == eventType {
			want = 1
		}
		f.assertEventCount(t, terminalType, want)
	}
	return boundary
}

func (f *portalFlushFixture) assertEventCount(t *testing.T, eventType string, want int) {
	t.Helper()
	log, err := f.log.Read()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range log {
		if event.RunID != f.runID {
			t.Fatalf("unexpected RunID %q, want %q", event.RunID, f.runID)
		}
		if event.Type == eventType {
			count++
		}
	}
	if count != want {
		t.Fatalf("%s count = %d, want %d", eventType, count, want)
	}
}
