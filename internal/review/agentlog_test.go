package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractBodiesFromLog_DoubleQuotedBody is the RED tracer for
// issue #1759. The bot posts its review body via a `gh pr comment`
// invocation with `--body "..."` (double-quoted). The daemon discovers
// the body by greping the run log. This is the minimal observable
// behavior: given a log containing one such call, the helper returns
// the body verbatim.
func TestExtractBodiesFromLog_DoubleQuotedBody(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 42 --repo owner/repo --body \"Hello, this is a review\"\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	want := []string{"Hello, this is a review"}
	if len(got) != len(want) {
		t.Fatalf("bodies: got %v, want %v", got, want)
	}
	if got[0] != want[0] {
		t.Errorf("body[0]: got %q, want %q", got[0], want[0])
	}
}

// TestExtractBodiesFromLog_SingleQuotedBody pins the single-quote
// variant of the gh pr comment body. The bot may quote with `'` instead
// of `"`; the helper must return the body in either case.
func TestExtractBodiesFromLog_SingleQuotedBody(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 42 --body 'Single quoted body'\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	want := []string{"Single quoted body"}
	if len(got) != len(want) {
		t.Fatalf("bodies: got %v, want %v", got, want)
	}
	if got[0] != want[0] {
		t.Errorf("body[0]: got %q, want %q", got[0], want[0])
	}
}

// TestExtractBodiesFromLog_NoGhComment pins the empty-log case: a log
// with no `gh pr comment` invocation returns an empty list and no
// error.
func TestExtractBodiesFromLog_NoGhComment(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ ls -la\n[run-1] 12:00:01 $ pwd\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty bodies, got %v", got)
	}
}

// TestExtractBodiesFromLog_UnclosedQuoteReturnsError pins the error
// path: when an opening quote is never closed, the helper returns an
// explicit error mentioning the line number. Per the plan, partial
// bodies are NOT returned alongside the error — the caller treats
// this as "no bodies extracted for this cycle" and retries next tick.
func TestExtractBodiesFromLog_UnclosedQuoteReturnsError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 42 --body \"This body never closes\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err == nil {
		t.Fatalf("expected error on unclosed quote, got bodies %v", got)
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("expected error to mention line number, got: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil bodies on error, got %v", got)
	}
}

// TestExtractBodiesFromLog_MultiplePosts pins the chronological
// ordering: two `gh pr comment` invocations on the same PR produce
// two separate body entries, in the order they appear in the log.
func TestExtractBodiesFromLog_MultiplePosts(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 42 --body \"first\"\n" +
		"[run-1] 12:00:30 $ gh pr comment 42 --body \"second\"\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	want := []string{"first", "second"}
	if len(got) != len(want) {
		t.Fatalf("bodies: got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("body[%d]: got %q, want %q", i, got[i], w)
		}
	}
}

// TestExtractBodiesFromLog_MultilineBody pins the multiline-body
// contract: the log writer drops the `$` shell prompt for lines after
// the first, and the helper joins continuation lines into one body.
func TestExtractBodiesFromLog_MultilineBody(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 42 --body \"line1\n" +
		"12:00:01 line2\n" +
		"12:00:02 line3\"\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	want := "line1\nline2\nline3"
	if len(got) != 1 {
		t.Fatalf("expected 1 body, got %d: %v", len(got), got)
	}
	if got[0] != want {
		t.Errorf("body: got %q, want %q", got[0], want)
	}
}

// TestExtractBodiesFromLog_BodyFileAbsolute pins the --body-file
// variant: the body is read from the file at the given absolute path.
// This is the most common shape for long review bodies (the shell
// wrapper writes the body to a file to avoid quoting trouble).
func TestExtractBodiesFromLog_BodyFileAbsolute(t *testing.T) {
	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "body.md")
	if err := os.WriteFile(bodyPath, []byte("Body from file"), 0644); err != nil {
		t.Fatalf("write body: %v", err)
	}
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 42 --body-file " + bodyPath + "\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 body, got %d: %v", len(got), got)
	}
	if got[0] != "Body from file" {
		t.Errorf("body: got %q, want %q", got[0], "Body from file")
	}
}

// TestExtractBodiesFromLog_CDAndRepoWrapped pins the wrapper shape:
// the agent shells out from its worktree, so the log line is
//
//	cd <worktree> && gh pr comment <N> --repo <owner/name> --body "..."
//
// The helper must strip the `cd` prefix and the `--repo` flag and
// extract the body.
func TestExtractBodiesFromLog_CDAndRepoWrapped(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ cd /tmp/worktree && gh pr comment 42 --repo owner/repo --body \"wrapped body\"\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 body, got %d: %v", len(got), got)
	}
	if got[0] != "wrapped body" {
		t.Errorf("body: got %q, want %q", got[0], "wrapped body")
	}
}

// TestExtractBodiesFromLog_IgnoresBodiesInImplementationRunLog pins
// the log-type discriminator (issue #1759 B9): the helper reads the
// sibling run.json and returns an empty list when its Kind is "issue"
// (an implementation run). A "kind: review" run.json with the same
// log content returns the bodies — that is the positive twin pin.
func TestExtractBodiesFromLog_IgnoresBodiesInImplementationRunLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 42 --body \"should not be picked up\"\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	runJSON := `{"Kind": "issue", "RunID": "x"}`
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(runJSON), 0644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty bodies for kind:issue log, got %v", got)
	}
}

// TestExtractBodiesFromLog_ReviewKind_ReturnsBodies is the positive
// twin of IgnoresBodiesInImplementationRunLog: a log whose sibling
// run.json declares kind "review" returns the bodies it contains.
// This pins the discriminator's positive direction.
func TestExtractBodiesFromLog_ReviewKind_ReturnsBodies(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	logContent := "[run-1] 12:00:00 $ gh pr comment 42 --body \"review body\"\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	runJSON := `{"Kind": "review", "RunID": "x"}`
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(runJSON), 0644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}

	got, err := extractBodiesFromLog(logPath)
	if err != nil {
		t.Fatalf("extractBodiesFromLog: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 body for kind:review log, got %d: %v", len(got), got)
	}
	if got[0] != "review body" {
		t.Errorf("body: got %q, want %q", got[0], "review body")
	}
}
