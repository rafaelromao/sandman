package skill

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	reviewRepository   = "owner/repo"
	reviewPullRequest  = "42"
	reviewHeadSHA      = "abc123"
	reviewPrefix       = "/sandman review"
	initialTriggerURL  = "https://github.com/owner/repo/pull/42#issuecomment-1001"
	followUpTriggerURL = "https://github.com/owner/repo/pull/42#issuecomment-1002"
)

func TestSyncReviewPost_PersistsConfirmedPrimaryTrigger(t *testing.T) {
	root, skillText := installRenderedPRReviewSkill(t)
	workDir := t.TempDir()
	stateDir := filepath.Join(workDir, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	requestFile := filepath.Join(stateDir, "42.review_request.json")
	headFile := filepath.Join(stateDir, "42.head_sha")
	bin := t.TempDir()
	postMarker := filepath.Join(t.TempDir(), "post-count")
	writePrimaryReviewGH(t, bin)

	script := reviewHarnessScript(
		root,
		reviewGuardBlock(t, skillText, requestFile, headFile),
		reviewPrimaryPostBlock(t, skillText, requestFile, headFile),
	)
	runReviewHarness(t, script, workDir, bin, root, postMarker)

	var request map[string]any
	readJSONFile(t, requestFile, &request)
	if request["protocol"] != "review-wait/v1" || request["repository"] != reviewRepository || request["pull_request"] != float64(42) {
		t.Fatalf("request identity = %#v", request)
	}
	if request["head_sha"] != reviewHeadSHA || request["trigger_id"] != initialTriggerURL || request["trigger_prefix"] != reviewPrefix {
		t.Fatalf("confirmed trigger identity = %#v", request)
	}
	if request["trigger_created_at"] != "2026-08-11T18:00:01Z" {
		t.Fatalf("trigger_created_at = %v, want server timestamp", request["trigger_created_at"])
	}

	head, err := os.ReadFile(headFile)
	if err != nil {
		t.Fatalf("read head sidecar: %v", err)
	}
	if string(head) != reviewHeadSHA+"\n" {
		t.Fatalf("head sidecar = %q, want %q", head, reviewHeadSHA+"\\n")
	}
	assertReviewStateFiles(t, stateDir, requestFile, headFile)
	assertPostCount(t, postMarker, 1)
}

func TestSyncReviewPost_BlocksUnansweredTriggerBeforePost(t *testing.T) {
	root, skillText := installRenderedPRReviewSkill(t)
	workDir := t.TempDir()
	stateDir := filepath.Join(workDir, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	requestFile := filepath.Join(stateDir, "42.review_request.json")
	headFile := filepath.Join(stateDir, "42.head_sha")
	bin := t.TempDir()
	postMarker := filepath.Join(t.TempDir(), "post-count")
	writeBlockedReviewGH(t, bin)

	script := reviewBlockedHarnessScript(
		root,
		reviewGuardBlock(t, skillText, requestFile, headFile),
	)
	runReviewHarness(t, script, workDir, bin, root, postMarker)

	if _, err := os.Stat(postMarker); !os.IsNotExist(err) {
		t.Fatalf("blocked rendered path posted a comment, stat error = %v", err)
	}
	record, err := os.ReadFile(postMarker + ".guard")
	if err != nil {
		t.Fatalf("blocked rendered path did not record the refusal: %v", err)
	}
	if !strings.Contains(string(record), "REVIEW_TRIGGER_GUARD_BLOCKED") {
		t.Fatalf("guard refusal record = %q", record)
	}
	if _, err := os.Stat(requestFile); !os.IsNotExist(err) {
		t.Fatalf("blocked rendered path created request envelope, stat error = %v", err)
	}
	if _, err := os.Stat(headFile); !os.IsNotExist(err) {
		t.Fatalf("blocked rendered path created head sidecar, stat error = %v", err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read blocked state directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("blocked rendered path created state files: %v", entryNames(entries))
	}
}

func TestSyncReviewPost_UpdatesExistingEnvelopeForFollowUp(t *testing.T) {
	root, skillText := installRenderedPRReviewSkill(t)
	workDir := t.TempDir()
	stateDir := filepath.Join(workDir, ".sandman", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	requestFile := filepath.Join(stateDir, "42.review_request.json")
	headFile := filepath.Join(stateDir, "42.head_sha")
	before := map[string]any{
		"protocol":                  "review-wait/v1",
		"repository":                reviewRepository,
		"pull_request":              42,
		"head_sha":                  reviewHeadSHA,
		"trigger_id":                initialTriggerURL,
		"trigger_prefix":            reviewPrefix,
		"trigger_created_at":        "2026-08-11T18:00:01Z",
		"confirmed_at":              "2026-08-11T18:00:02Z",
		"started_unix_seconds":      4102443000,
		"started_at":                "2100-01-01T00:10:00Z",
		"deadline_at":               "unix:4102444800",
		"deadline_unix_seconds":     4102444800,
		"effective_timeout_seconds": 1800,
		"poll_plan":                 []any{float64(120), float64(60), float64(60), float64(30)},
	}
	writeJSONFile(t, requestFile, before)
	var beforeOnDisk map[string]any
	readJSONFile(t, requestFile, &beforeOnDisk)
	if err := os.WriteFile(headFile, []byte(reviewHeadSHA+"\n"), 0o600); err != nil {
		t.Fatalf("write head sidecar: %v", err)
	}

	bin := t.TempDir()
	postMarker := filepath.Join(t.TempDir(), "post-count")
	writeFollowUpReviewGH(t, bin)

	script := reviewHarnessScript(
		root,
		reviewGuardBlock(t, skillText, requestFile, headFile),
		reviewFollowUpPostBlock(t, skillText, requestFile, headFile),
	)
	runReviewHarness(t, script, workDir, bin, root, postMarker)

	var after map[string]any
	readJSONFile(t, requestFile, &after)
	for key, value := range beforeOnDisk {
		if key == "trigger_id" || key == "trigger_created_at" {
			continue
		}
		if !reflect.DeepEqual(after[key], value) {
			t.Errorf("request field %q changed from %#v to %#v", key, value, after[key])
		}
	}
	if after["trigger_id"] != followUpTriggerURL || after["trigger_created_at"] != "2026-08-11T18:02:01Z" {
		t.Fatalf("follow-up trigger identity = %#v", after)
	}

	head, err := os.ReadFile(headFile)
	if err != nil {
		t.Fatalf("read head sidecar: %v", err)
	}
	if string(head) != reviewHeadSHA+"\n" {
		t.Fatalf("head sidecar = %q, want %q", head, reviewHeadSHA+"\\n")
	}
	assertReviewStateFiles(t, stateDir, requestFile, headFile)
	assertPostCount(t, postMarker, 1)
}

func installRenderedPRReviewSkill(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: reviewPrefix}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}
	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	data, err := os.ReadFile(filepath.Join(root, "pr-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read synced pr-review skill: %v", err)
	}
	return root, string(data)
}

func reviewGuardBlock(t *testing.T, skillText, requestFile, headFile string) string {
	t.Helper()
	return renderReviewBashBlock(t, skillText, "skill_root=\"${SANDMAN_SKILL_ROOT:-${HOME}/.agents/skills/sandman}\"", requestFile, headFile)
}

func reviewPrimaryPostBlock(t *testing.T, skillText, requestFile, headFile string) string {
	t.Helper()
	return strings.Join([]string{
		renderReviewBashBlock(t, skillText, "trigger_url=$(gh pr comment", requestFile, headFile),
		renderReviewBashBlock(t, skillText, "review_timeout=${REVIEW_TIMEOUT:-1800}", requestFile, headFile),
	}, "\n")
}

func reviewFollowUpPostBlock(t *testing.T, skillText, requestFile, headFile string) string {
	t.Helper()
	return renderReviewBashBlock(t, skillText, "[ -f \"$request_file\" ] || record", requestFile, headFile)
}

func renderReviewBashBlock(t *testing.T, skillText, start, requestFile, headFile string) string {
	t.Helper()
	needle := "```bash\n" + start
	startAt := strings.Index(skillText, needle)
	if startAt < 0 {
		t.Fatalf("skill bash block %q not found", start)
	}
	blockStart := startAt + len("```bash\n")
	endRel := strings.Index(skillText[blockStart:], "\n```")
	if endRel < 0 {
		t.Fatalf("skill bash block %q has no closing fence", start)
	}
	block := skillText[blockStart : blockStart+endRel]
	block = strings.ReplaceAll(block, ".sandman/state/<N>.review_request.json", requestFile)
	block = strings.ReplaceAll(block, ".sandman/state/<N>.head_sha", headFile)
	block = strings.ReplaceAll(block, "<owner/repo>", reviewRepository)
	block = strings.ReplaceAll(block, "<N>", reviewPullRequest)
	block = strings.ReplaceAll(block, "{{REVIEW_COMMAND}}", reviewPrefix)
	return block
}

func reviewHarnessScript(root, firstBlock, secondBlock string) string {
	return strings.Join([]string{
		"set -eu",
		"export SANDMAN_SKILL_ROOT=" + shellQuote(root),
		"head_sha=" + reviewHeadSHA,
		"record() { printf '%s\\n' \"$*\" >&2; return 1; }",
		firstBlock,
		secondBlock,
		"printf '%s\\n' success",
	}, "\n")
}

func reviewBlockedHarnessScript(root, guardBlock string) string {
	return strings.Join([]string{
		"set -eu",
		"export SANDMAN_SKILL_ROOT=" + shellQuote(root),
		"head_sha=" + reviewHeadSHA,
		"record() { printf '%s\\n' \"$*\" > \"$GUARD_RECORD_MARKER\"; exit 0; }",
		guardBlock,
		"printf '%s\\n' unexpected-post",
	}, "\n")
}

func runReviewHarness(t *testing.T, script, workDir, bin, root, postMarker string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"SANDMAN_SKILL_ROOT="+root,
		"POST_MARKER="+postMarker,
		"GUARD_RECORD_MARKER="+postMarker+".guard",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run rendered review path: %v\n%s\nscript:\n%s", err, output, script)
	}
}

func writePrimaryReviewGH(t *testing.T, bin string) {
	t.Helper()
	writeReviewGH(t, bin, `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  if [ -f "$POST_MARKER" ]; then
    printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"}]}'
  else
    printf '%s\n' '{"headRefOid":"abc123"}'
  fi
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */issues/*/comments|*/reviews|*/comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "comment" ]; then
  printf x >> "$POST_MARKER"
  printf '%s\n' 'https://github.com/owner/repo/pull/42#issuecomment-1001'
  exit 0
fi
exit 2
`)
}

func writeBlockedReviewGH(t *testing.T, bin string) {
	t.Helper()
	writeReviewGH(t, bin, `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */issues/*/comments) printf '%s\n' '[{"id":1001,"html_url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","created_at":"2026-08-11T18:00:01Z"}]' ;;
    */reviews|*/comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "comment" ]; then
  printf x >> "$POST_MARKER"
  printf '%s\n' 'https://github.com/owner/repo/pull/42#issuecomment-1002'
  exit 0
fi
exit 2
`)
}

func writeFollowUpReviewGH(t *testing.T, bin string) {
	t.Helper()
	writeReviewGH(t, bin, `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  if [ -f "$POST_MARKER" ]; then
    printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"},{"id":"1003","url":"https://github.com/owner/repo/pull/42#issuecomment-1003","body":"review response","createdAt":"2026-08-11T18:01:01Z"},{"id":"1002","url":"https://github.com/owner/repo/pull/42#issuecomment-1002","body":"/sandman review - please clarify which finding is actionable","createdAt":"2026-08-11T18:02:01Z"}]}'
  else
    printf '%s\n' '{"headRefOid":"abc123","comments":[{"id":"1001","url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","createdAt":"2026-08-11T18:00:01Z"},{"id":"1003","url":"https://github.com/owner/repo/pull/42#issuecomment-1003","body":"review response","createdAt":"2026-08-11T18:01:01Z"}]}'
  fi
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */issues/*/comments) printf '%s\n' '[{"id":1001,"html_url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","created_at":"2026-08-11T18:00:01Z"},{"id":1003,"html_url":"https://github.com/owner/repo/pull/42#issuecomment-1003","body":"review response","created_at":"2026-08-11T18:01:01Z"}]' ;;
    */reviews|*/comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "comment" ]; then
  printf x >> "$POST_MARKER"
  printf '%s\n' 'https://github.com/owner/repo/pull/42#issuecomment-1002'
  exit 0
fi
exit 2
`)
}

func writeReviewGH(t *testing.T, bin, script string) {
	t.Helper()
	path := filepath.Join(bin, "gh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read JSON file %s: %v", path, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode JSON file %s: %v", path, err)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode JSON file %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write JSON file %s: %v", path, err)
	}
}

func assertReviewStateFiles(t *testing.T, stateDir, requestFile, headFile string) {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read review state directory: %v", err)
	}
	want := map[string]bool{
		filepath.Base(requestFile): true,
		filepath.Base(headFile):    true,
	}
	if len(entries) != len(want) {
		t.Fatalf("review state files = %v, want only request envelope and head sidecar", entryNames(entries))
	}
	for _, entry := range entries {
		if !want[entry.Name()] {
			t.Fatalf("unexpected review state file %q", entry.Name())
		}
	}
	if _, err := os.Stat(requestFile + ".state"); !os.IsNotExist(err) {
		t.Fatalf("unexpected wait-state file, stat error = %v", err)
	}
}

func assertPostCount(t *testing.T, path string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read post marker: %v", err)
	}
	if len(data) != want {
		t.Fatalf("post count = %d, want %d", len(data), want)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
