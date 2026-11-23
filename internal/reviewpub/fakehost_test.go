package reviewpub

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH models the GitHub CLI against a managed-sandbox host. It is installed
// as `gh` on PATH, so the shipped helpers run unmodified against it.
//
// Knobs (all files in FIXTURE_DIR):
//   - FIXTURE_STATE=open|closed|stale: the live pull request.
//   - FIXTURE_GRAPHQL=ok|blocked: blocked fails every GraphQL-backed call
//     (pr view/comment, issue view, api graphql) with the sandbox's 403.
//   - readback: when present, the exact body every read-back returns for the
//     posted comment (a host that rewrote it); otherwise the posted body.
//   - FIXTURE_PROBE_OK=1: with GraphQL blocked for pr view only, the minimal
//     probe query still succeeds.
//
// Every mutation is appended to FIXTURE_DIR/mutations.
const fakeGH = `#!/bin/sh
set -eu
log() { printf '%s\n' "$*" >>"$FIXTURE_DIR/calls"; }
blocked() { printf 'HTTP 403: GitHub GraphQL is not available (https://api.github.com/graphql)\n' >&2; exit 1; }
graphql_off() { [ "${FIXTURE_GRAPHQL:-ok}" = blocked ]; }
state=open; head=abc
[ "${FIXTURE_STATE:-open}" != closed ] || state=closed
[ "${FIXTURE_STATE:-open}" != stale ] || head=other
live_body() {
  if [ -f "$FIXTURE_DIR/readback" ]; then cat "$FIXTURE_DIR/readback"; elif [ -f "$FIXTURE_DIR/published" ]; then cat "$FIXTURE_DIR/published"; fi
}
comments_json() {
  [ -f "$FIXTURE_DIR/published" ] || { printf '[]\n'; return; }
  live_body >"$FIXTURE_DIR/live.tmp"
  jq -cn --rawfile body "$FIXTURE_DIR/live.tmp" '[{id:7099011765,html_url:"https://example.test/comment/1",created_at:"2026-11-15T00:00:00Z",body:$body}]'
}
log "$*"
case "$1 $2" in
  "pr view")
    graphql_off && blocked
    UP=$(printf '%s' "$state" | tr a-z A-Z)
    if [ -f "$FIXTURE_DIR/published" ]; then
      live_body >"$FIXTURE_DIR/live.tmp"
      jq -cn --arg state "$UP" --arg head "$head" --rawfile body "$FIXTURE_DIR/live.tmp" \
        '{state:$state,headRefOid:$head,comments:[{url:"https://example.test/comment/1",createdAt:"2026-11-15T00:00:00Z",body:$body}]}'
    else
      jq -cn --arg state "$UP" --arg head "$head" '{state:$state,headRefOid:$head}'
    fi ;;
  "pr comment")
    graphql_off && blocked
    shift 2
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --body-file ]; then cp "$2" "$FIXTURE_DIR/published"; break; fi
      shift
    done
    printf 'post\n' >>"$FIXTURE_DIR/mutations"
    printf 'https://example.test/comment/1\n' ;;
  "api graphql")
    graphql_off && { [ "${FIXTURE_PROBE_OK:-0}" = 1 ] || blocked; }
    printf '{"data":{"__typename":"Query"}}\n' ;;
  "api --paginate")
    case "$3" in
      repos/*/issues/*/comments) comments_json ;;
      repos/*/pulls/*/reviews|repos/*/pulls/*/comments) printf '[]\n' ;;
      *) exit 10 ;;
    esac ;;
  "api -X")
    # api -X POST repos/OWNER/REPO/issues/N/comments -F body=@FILE
    [ "$3" = POST ] || exit 10
    case "$4" in repos/*/issues/*/comments) ;; *) exit 10 ;; esac
    file=''
    for arg in "$@"; do case "$arg" in body=@*) file=${arg#body=@} ;; esac; done
    [ -n "$file" ] || exit 11
    [ "${FIXTURE_POST_FAIL:-0}" != 1 ] || { printf 'HTTP 502: bad gateway\n' >&2; exit 1; }
    cp "$file" "$FIXTURE_DIR/published"
    printf 'post\n' >>"$FIXTURE_DIR/mutations"
    printf '{"id":7099011765,"html_url":"https://example.test/comment/1"}\n' ;;
  "api repos/"*)
    case "$2" in
      repos/*/pulls/*) jq -cn --arg state "$state" --arg head "$head" \
        '{number:17,title:"Container compatibility",body:null,state:$state,head:{sha:$head},base:{ref:"main"}}' ;;
      repos/*/issues/*) jq -cn '{number:42,body:"REST acceptance criterion"}' ;;
      *) exit 10 ;;
    esac ;;
  "issue view")
    graphql_off && blocked
    printf '{"body":"GraphQL acceptance criterion"}\n' ;;
  *) exit 10 ;;
esac
`

type hostRun struct {
	stdout    string
	stderr    string
	err       error
	mutations int
	calls     []string
}

func (r hostRun) envelope(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.stdout)), &out); err != nil {
		t.Fatalf("stdout is not an envelope: %v\nstdout=%q\nstderr=%q", err, r.stdout, r.stderr)
	}
	return out
}

func (r hostRun) refusal(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.stderr)), &out); err != nil {
		t.Fatalf("stderr is not a refusal envelope: %v\nstderr=%q", err, r.stderr)
	}
	return out
}

// runPublishHelper runs publish-v1.sh against fakeGH. readback, when not nil,
// is the exact live body the host returns for the posted comment.
func runPublishHelper(t *testing.T, readback *string, env ...string) hostRun {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	helper, err := filepath.Abs("../skill/sandman/blind-review/publish-v1.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	decision := filepath.Join(dir, "decision.md")
	if err := os.WriteFile(decision, []byte("## Decision\nAPPROVED"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	if readback != nil {
		if err := os.WriteFile(filepath.Join(dir, "readback"), []byte(*readback), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", helper, "--repository", "owner/repo", "--pull-request", "17",
		"--head-sha", "abc", "--decision-file", decision)
	cmd.Env = append(append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+dir), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	mutations, _ := os.ReadFile(filepath.Join(dir, "mutations"))
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return hostRun{
		stdout: stdout.String(), stderr: stderr.String(), err: runErr,
		mutations: strings.Count(string(mutations), "post\n"),
		calls:     strings.Split(strings.TrimSpace(string(calls)), "\n"),
	}
}
