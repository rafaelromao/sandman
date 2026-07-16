package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// Characterization net for the Orchestrator's RunBatch external seam.
//
// Load-bearing regression gate for the runSession -> RunExecutor elevation
// (wayfinder map #2226). It pins RunBatch's observable behaviour
// byte-for-byte — every event (type + full payload), every AgentRunResult
// field, the ErrAborted exit semantics, and any run.log the orchestrator
// writes — so the structural refactor (#2228 slice 1, #2234 slice 2) is
// provably behaviour-preserving.
//
// Determinism (per the #2232 spec):
//   - RunIDs / BatchIDs are fixed via Request.RunTS / RunShortID.
//   - Timestamps are normalized to <ts> (event timestamps and run.log HH:MM:SS).
//   - The temp working dir is normalized to <tmp> wherever it appears.
//   - Events are partitioned by RunID (each run's lifecycle is pinned in
//     emission order); cross-run interleaving in the shared events.jsonl is
//     allowed to vary, so partitions are sorted by RunID for a stable golden.
//
// Regenerate goldens:
//
//	SANDMAN_CHARNET_UPDATE=1 go test ./internal/batch/ -run TestCharacterizationNet
//
// run.log note: the runnable body is written to run.log by AgentRun.Execute
// (behind the Runnable seam). Fixtures here use fake runnables, so they capture
// only the run.log lines the orchestrator itself writes (retry markers,
// heartbeat context). A fixture using the real AgentRun would deepen the pin.
var charNetTimestampRe = regexp.MustCompile(`\d{2}:\d{2}:\d{2}`)

// charNetFixture is one scenario captured by the net. setup returns the ctx
// RunBatch runs under (so abort fixtures can pre-arm a cancellation).
type charNetFixture struct {
	name  string
	setup func(t *testing.T) (ctx context.Context, req Request, o *Orchestrator, el *events.JSONLLogger, tmpRoot string)
}

func TestCharacterizationNet(t *testing.T) {
	// Goldens anchor to the package source dir. Fixtures chdir into a temp
	// repo (t.Chdir), so capture the original CWD — Go runs package tests with
	// CWD = the package directory — before any subtest runs.
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	fixtures := []charNetFixture{
		{name: "issue-driven-success", setup: charNetIssueSuccess},
		{name: "issue-driven-abort-ErrAborted", setup: charNetIssueAbort},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			ctx, req, o, el, tmpRoot := f.setup(t)
			res, runErr := o.RunBatch(ctx, req)
			captured := charNetCapture(t, o, el, res, runErr, tmpRoot)
			charNetGolden(t, pkgDir, "charnet_"+f.name+".golden", captured)
		})
	}
}

// charNetBase builds the shared per-fixture scaffolding: an isolated git repo
// as CWD, a JSONL event log, an Orchestrator wired with fake sandbox/runnable
// factories, and the canonical issue-42 client/config.
func charNetBase(t *testing.T, opts ...OrchestratorOpt) (*Orchestrator, *events.JSONLLogger, string) {
	t.Helper()
	dir := testenv.MkdirShort(t, "charnet-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	el := &events.JSONLLogger{Path: filepath.Join(dir, "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
	}
	cfg := &config.Config{
		Agent:       "test-agent",
		Sandbox:     "worktree",
		WorktreeDir: ".sandman/worktrees",
		Git:         config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"test-agent": {Command: "true"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, el,
		append([]OrchestratorOpt{
			WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{workDir: filepath.Join(dir, "wt", "42")}}),
		}, opts...)...,
	)
	return o, el, dir
}

func charNetIssueSuccess(t *testing.T) (context.Context, Request, *Orchestrator, *events.JSONLLogger, string) {
	o, el, dir := charNetBase(t, WithRunnableFactory(&fakeRunnableFactory{results: []AgentRunResult{
		{IssueNumber: 42, Status: "success", Branch: "sandman/42-fix-bug"},
	}}))
	req := Request{Issues: []int{42}, RunTS: orchTestRunTS, RunShortID: orchTestRunShortID}
	return context.Background(), req, o, el, dir
}

func charNetIssueAbort(t *testing.T) (context.Context, Request, *Orchestrator, *events.JSONLLogger, string) {
	// charNetIssueAbort constructs its own NewOrchestrator (rather than
	// reusing charNetBase) because its sandboxFactory override depends on
	// the dir that charNetBase returns — and options are applied at
	// construction, before dir is known. Building the orchestrator here
	// keeps the override on the seam (no private-field poke) at the cost
	// of duplicating charNetBase's small setup.
	t.Helper()
	dir := testenv.MkdirShort(t, "charnet-")
	t.Chdir(dir)
	initGitRepo(t, dir)

	el := &events.JSONLLogger{Path: filepath.Join(dir, "events.jsonl")}
	client := &fakeGitHubClient{
		issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Fix bug", Body: "Users cannot log in."},
		},
		prs: map[string]*github.PR{"sandman/42-fix-bug": mergedPR("sandman/42-fix-bug", "")},
	}
	cfg := &config.Config{
		Agent:       "test-agent",
		Sandbox:     "worktree",
		WorktreeDir: ".sandman/worktrees",
		Git:         config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{
			"test-agent": {Command: "true"},
		},
	}
	o := NewOrchestrator(client, &noopRenderer{}, &fakeConfigStore{config: cfg}, el,
		WithSandboxFactory(&fakeSandboxFactory{sandbox: &fakeSandbox{process: makeFakeProcess(), workDir: filepath.Join(dir, "wt", "42")}}),
		WithRunnableFactory(&blockingRunnableFactory{runnable: &blockingRunnable{delayAfterCancel: 100 * time.Millisecond}}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	req := Request{Issues: []int{42}, RunTS: orchTestRunTS, RunShortID: orchTestRunShortID}
	return ctx, req, o, el, dir
}

// --- capture + serialize ---

type charNetCaptured struct {
	Error   string
	Results []AgentRunResult
	Events  []events.Event
	RunLogs map[string]string // runID -> normalized run.log content
}

func charNetCapture(t *testing.T, o *Orchestrator, el *events.JSONLLogger, res *Result, err error, tmpRoot string) string {
	t.Helper()
	evs, readErr := el.Read()
	if readErr != nil {
		t.Fatalf("read events: %v", readErr)
	}
	var c charNetCaptured
	if err != nil {
		c.Error = err.Error()
	} else {
		c.Error = "<nil>"
	}
	if res != nil {
		c.Results = res.Runs
	}
	c.Events = evs
	c.RunLogs = charNetReadRunLogs(o)
	return charNetNormalize(charNetSerialize(c), tmpRoot)
}

// charNetReadRunLogs walks the batch dir for any run.log files the orchestrator
// wrote (retry markers / heartbeat context). Fake runnables don't write the
// body, so this is often empty — captured as "<none>".
func charNetReadRunLogs(o *Orchestrator) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(o.layout.BatchesDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || filepath.Base(path) != "run.log" {
			return nil
		}
		runID := filepath.Base(filepath.Dir(path))
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			return nil
		}
		out[runID] = charNetTimestampRe.ReplaceAllString(string(data), "<ts>")
		return nil
	})
	return out
}

func charNetSerialize(c charNetCaptured) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== error ===\n%s\n", c.Error)

	fmt.Fprintf(&b, "\n=== results (%d) ===\n", len(c.Results))
	for i, r := range c.Results {
		fmt.Fprintf(&b, "--- result[%d] ---\n", i)
		fmt.Fprintf(&b, "IssueNumber: %d\n", r.IssueNumber)
		if r.Issue != nil {
			fmt.Fprintf(&b, "Issue: %d\n", *r.Issue)
		} else {
			fmt.Fprintf(&b, "Issue: <nil>\n")
		}
		fmt.Fprintf(&b, "Status: %s\n", r.Status)
		fmt.Fprintf(&b, "RetriesTotal: %d\n", r.RetriesTotal)
		fmt.Fprintf(&b, "Branch: %s\n", r.Branch)
		fmt.Fprintf(&b, "WorktreePath: %s\n", r.WorktreePath)
		fmt.Fprintf(&b, "Review: %v\n", r.Review)
		fmt.Fprintf(&b, "RunID: %s\n", r.RunID)
	}

	// Partition events by RunID; sort RunIDs for a stable golden; preserve
	// emission order within each RunID.
	byRun := map[string][]events.Event{}
	var runIDs []string
	for _, e := range c.Events {
		k := e.RunID
		if k == "" {
			k = "<no-run-id>"
		}
		if _, ok := byRun[k]; !ok {
			runIDs = append(runIDs, k)
		}
		byRun[k] = append(byRun[k], e)
	}
	sort.Strings(runIDs)
	fmt.Fprintf(&b, "\n=== events (partitioned by RunID, emission order) ===\n")
	for _, k := range runIDs {
		fmt.Fprintf(&b, "## RunID: %s\n", k)
		for _, e := range byRun[k] {
			payload := "<nil>"
			if e.Payload != nil {
				if js, jerr := json.Marshal(e.Payload); jerr == nil {
					payload = string(js)
				} else {
					payload = fmt.Sprintf("<marshal-error: %v>", jerr)
				}
			}
			fmt.Fprintf(&b, "<ts>\t%s\t%s\n", e.Type, payload)
		}
	}

	if len(c.RunLogs) > 0 {
		var logIDs []string
		for id := range c.RunLogs {
			logIDs = append(logIDs, id)
		}
		sort.Strings(logIDs)
		fmt.Fprintf(&b, "\n=== run.log ===\n")
		for _, id := range logIDs {
			fmt.Fprintf(&b, "## RunID: %s\n%s\n", id, c.RunLogs[id])
		}
	} else {
		fmt.Fprintf(&b, "\n=== run.log ===\n<none>\n")
	}

	return b.String()
}

// charNetNormalize replaces the temp working dir with <tmp> wherever it appears
// (event payload paths, result paths, logs).
func charNetNormalize(s, tmpRoot string) string {
	if tmpRoot == "" {
		return s
	}
	return strings.ReplaceAll(s, tmpRoot, "<tmp>")
}

// --- golden compare / update ---

func charNetGolden(t *testing.T, pkgDir, name, captured string) {
	t.Helper()
	// Goldens live in a committed `characterization/` dir (NOT testdata/, which
	// this repo gitignores — baselines there are run-generated, not committed).
	// .golden files aren't Go source, so the dir is excluded from the build.
	goldenPath := filepath.Join(pkgDir, "characterization", name)
	if os.Getenv("SANDMAN_CHARNET_UPDATE") == "1" {
		if mErr := os.MkdirAll(filepath.Dir(goldenPath), 0o755); mErr != nil {
			t.Fatalf("mkdir golden dir: %v", mErr)
		}
		if wErr := os.WriteFile(goldenPath, []byte(captured), 0o644); wErr != nil {
			t.Fatalf("write golden: %v", wErr)
		}
		t.Logf("updated golden %s", goldenPath)
		return
	}
	want, rErr := os.ReadFile(goldenPath)
	if rErr != nil {
		t.Fatalf("missing golden %s (run with SANDMAN_CHARNET_UPDATE=1 to create): %v", goldenPath, rErr)
	}
	if string(want) != captured {
		t.Fatalf("characterization drift for %s:\n--- want ---\n%s\n--- got ---\n%s", name, want, captured)
	}
}
