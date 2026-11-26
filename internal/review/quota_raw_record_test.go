package review

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// A custom review command prints raw JSON, so the usage limit arrives as an
// unparsed record. The daemon must pause on it instead of relaunching on every
// poll, and keep pausing while its probes meet the same record.
func TestDaemon_CustomReviewCommandUsageLimitPausesReviews(t *testing.T) {
	// A wrapper that pipes the agent exits zero after the refusal.
	for _, code := range []int{1, 0} {
		t.Run(fmt.Sprintf("exit %d", code), func(t *testing.T) { testCustomReviewCommandUsageLimit(t, code) })
	}
}

func testCustomReviewCommandUsageLimit(t *testing.T, code int) {
	const prNumber = 3043
	dir := testenv.MkdirShort(t, "sm-quota-raw-")
	t.Chdir(dir)
	initReviewTestGitRepo(t, dir)
	remote := t.TempDir()
	runSnapshotGit(t, remote, "init", "--bare")
	runSnapshotGit(t, dir, "remote", "add", "origin", remote)
	runSnapshotGit(t, dir, "push", "origin", "main", "HEAD:refs/pull/3043/head")
	head := strings.TrimSpace(runSnapshotGit(t, dir, "rev-parse", "HEAD"))

	base := filepath.Join(dir, ".sandman")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	launches := filepath.Join(dir, "launches")
	// The command runs this script, so the provider can recover mid-test.
	agentScript := filepath.Join(dir, "agent.sh")
	record := `{"type":"error","sessionID":"ses_x","error":{"type":"provider.quota","message":"The usage limit has been reached"}}`
	if err := os.WriteFile(agentScript, []byte(fmt.Sprintf("printf '%%s\\n' '%s'\nexit %d\n", record, code)), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "config.yaml")
	content := "review_agent: opencode-v2\nreview_model: openai/test\nsandbox: worktree\nworktree_dir: " + filepath.Join(t.TempDir(), "worktrees") +
		"\nagents:\n  opencode-v2:\n    command: >-\n      printf 'x\\n' >> " + launches + "; sh " + agentScript + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	eventLog := &events.JSONLLogger{Path: filepath.Join(base, "events.jsonl")}
	orchestrator := batch.NewOrchestrator(nil, &prompt.Engine{}, reviewConfigStore{cfg: cfg}, eventLog,
		batch.WithErrorLog(io.Discard))
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	gh := &fakeGH{
		prs:      []github.PR{{Number: prNumber, State: "open"}},
		comments: map[int][]github.PRComment{prNumber: {{ID: "c1", Body: "/sandman review", CreatedAt: now, AuthorLogin: "sandman"}}},
		prFetch:  map[int]*github.PR{prNumber: {Number: prNumber, Title: "T", Body: "B", HeadRefOid: head}},
	}
	logs := &lockedBuffer{}
	d := New(base, gh, &prompt.Engine{}, orchestrator, cfg, logs, 0, false, nil)
	d.PollInterval = 0
	d.Clock = func() time.Time { return now }
	d.authenticatedLogin = "sandman"

	tickAndWait(t, d, context.Background())

	if !d.IsQuotaPaused() {
		t.Fatalf("review daemon did not pause: %s", logs.String())
	}
	launched := func() int {
		t.Helper()
		data, err := os.ReadFile(launches)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "x")
	}
	// Past the ordinary launch backoff but before the quota probe is due.
	now = now.Add(5 * time.Minute)
	tickAndWait(t, d, context.Background())
	if got := launched(); got != 1 {
		t.Fatalf("launches = %d, want one", got)
	}
	recorded, err := eventLog.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(recorded, func(event events.Event) bool {
		return event.Type == "run.finished" && event.Payload["reason"] == "AGENT_USAGE_LIMIT"
	}) {
		t.Fatalf("run.finished carries no AGENT_USAGE_LIMIT: %+v", recorded)
	}

	// The probe meets the same record and extends the pause.
	now = d.QuotaPausedUntil()
	tickAndWait(t, d, context.Background())
	if !d.IsQuotaPaused() || launched() != 2 {
		t.Fatalf("after a limited probe: paused = %t, launches = %d", d.IsQuotaPaused(), launched())
	}

	// A clean probe reopens reviews.
	if err := os.WriteFile(agentScript, []byte("exit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now = d.QuotaPausedUntil()
	tickAndWait(t, d, context.Background())
	if d.IsQuotaPaused() {
		t.Fatalf("daemon stayed paused after a clean probe: %s", logs.String())
	}
}
