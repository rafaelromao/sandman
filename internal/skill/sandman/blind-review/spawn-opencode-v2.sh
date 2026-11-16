#!/bin/sh
# V2 uses the CLI's authenticated service context and the public /api contract.
# Sources: https://opencode.ai/docs/api and /docs/openapi.json.
set -eu
umask 077
fail() { printf '%s\n' "REVIEW_SUBAGENT_UNAVAILABLE: $1" >&2; exit 1; }
prompt_file='' run_dir=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --prompt-file) [ "$#" -ge 2 ] || fail invalid-argument; prompt_file=$2; shift 2 ;;
    --run-dir) [ "$#" -ge 2 ] || fail invalid-argument; run_dir=$2; shift 2 ;;
    *) fail invalid-argument ;;
  esac
done
case "$prompt_file" in /*) ;; *) fail absolute-prompt-path-required ;; esac
if [ ! -f "$prompt_file" ] || [ -L "$prompt_file" ]; then
  fail invalid-prompt-file
fi
directory=$(pwd -P)
case "$run_dir" in /*) ;; *) fail absolute-report-directory-required ;; esac
run_dir=$(CDPATH='' cd -- "$run_dir" && pwd -P) || fail invalid-report-directory
case "$run_dir" in "$directory"|"$directory"/*) fail external-report-directory-required ;; esac
case "$prompt_file" in "$directory"/*) fail external-prompt-required ;; esac
helper_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
temp_root=$(CDPATH='' cd -- "${TMPDIR:-/tmp}" && pwd -P)
case "$temp_root" in "$directory"|"$directory"/*) fail external-temp-directory-required ;; esac
tmp=$(mktemp -d "$temp_root/sandman-native-review.XXXXXXXXXXXX")
child='' request_pid=''
api() {
  case "$1" in GET) method='get' ;; POST) method='post' ;; *) fail invalid-http-method ;; esac
  route=$2 destination=$3
  shift 3
  opencode api "$method" "$route" "$@" >"$destination" 2>"$tmp/api-error"
}
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ -n "$request_pid" ]; then
    kill "$request_pid" 2>/dev/null || :
    wait "$request_pid" 2>/dev/null || :
  fi
  if [ -n "$child" ]; then
    api POST "/api/session/$child/interrupt" "$tmp/interrupt" || :
  fi
  if [ "$status" -eq 0 ]; then
    rm -f "$tmp"/*
    rmdir "$tmp"
  else
    printf '%s\n' "Native review diagnostics: $tmp" >&2
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
cp "$prompt_file" "$tmp/prompt"
encoded_directory=$(printf '%s' "$directory" | jq -sRr @uri)
route="/api/session?parentID=null&directory=$encoded_directory&limit=100"
: >"$tmp/matches"
while :; do
  api GET "$route" "$tmp/sessions" || fail session-list-failed
  jq -e '(.data | type == "array") and (.cursor | type == "object") and
    (.cursor | has("next")) and
    (.cursor.next == null or (.cursor.next | type == "string"))' \
    "$tmp/sessions" >/dev/null || fail invalid-session-list
  jq -r --arg directory "$directory" '
    .data | if type != "array" then error("invalid sessions") else
      .[] | select(.location.directory == $directory and (.parentID // "") == "") |
      .id | select(test("^ses[A-Za-z0-9_-]+$"))
    end' "$tmp/sessions" >"$tmp/candidates" || fail invalid-session-list
  while IFS= read -r candidate; do
    api GET "/api/experimental/session/$candidate/export" "$tmp/export" ||
      fail session-export-failed
    jq -c --arg directory "$directory" --arg candidate "$candidate" \
      --arg prompt_file "$prompt_file" -f "$helper_dir/opencode-parent-v2.jq" \
      "$tmp/export" >>"$tmp/matches" || fail invalid-session-export
  done <"$tmp/candidates"
  cursor=$(jq -er '.cursor.next // empty' "$tmp/sessions") || break
  encoded_cursor=$(printf '%s' "$cursor" | jq -sRr @uri)
  route="/api/session?parentID=null&directory=$encoded_directory&limit=100&cursor=$encoded_cursor"
done
jq -se 'length == 1' "$tmp/matches" >/dev/null || fail ambiguous-or-missing-live-owner
jq -s '.[0]' "$tmp/matches" >"$tmp/parent"
parent=$(jq -er '.id' "$tmp/parent")
api GET "/api/session/$parent" "$tmp/live" || fail parent-lookup-failed
jq -e --arg parent "$parent" --arg directory "$directory" '
  .data.id == $parent and .data.location.directory == $directory and
  (.data.parentID // "") == ""
' "$tmp/live" >/dev/null || fail parent-identity-changed
api GET "/api/agent/general?location%5Bdirectory%5D=$encoded_directory" "$tmp/general" ||
  fail agent-lookup-failed
jq -e '.data.id == "general" and .data.model == null and
  (.data.mode == "subagent" or .data.mode == "all")' "$tmp/general" >/dev/null ||
  fail unsupported-general-agent
# Evaluate through the service, including policies and saved permissions.
permission=$(jq -cn --slurpfile parent "$tmp/parent" '
  {action:"subagent",resources:["general"],agent:$parent[0].agent}')
api POST "/api/session/$parent/permission" "$tmp/permission" --data "$permission" ||
  fail permission-evaluation-failed
jq -e '.data.effect == "allow"' "$tmp/permission" >/dev/null ||
  fail subagent-permission-not-allowed
api GET "/api/experimental/session/$parent/export" "$tmp/export" ||
  fail owner-recheck-failed
jq -c --arg directory "$directory" --arg candidate "$parent" \
  --arg prompt_file "$prompt_file" -f "$helper_dir/opencode-parent-v2.jq" \
  "$tmp/export" >"$tmp/rechecked" || fail owner-recheck-failed
jq -se --slurpfile previous "$tmp/parent" 'length == 1 and .[0] == $previous[0]' \
  "$tmp/rechecked" >/dev/null || fail owner-no-longer-running
create=$(jq -cn --slurpfile parent "$tmp/parent" --slurpfile live "$tmp/live" '
  {parentID:$parent[0].id,agent:"general",title:"Blind pull-request review",
   model:$parent[0].model,
   permissions:(($live[0].data.permissions // []) |
     map(select(.effect == "deny" or .action == "external_directory")))}')
api POST /api/session "$tmp/created" --data "$create" || fail child-create-failed
child=$(jq -er --arg parent "$parent" --arg directory "$directory" \
  --slurpfile selected "$tmp/parent" '
  .data | select(.parentID == $parent and .location.directory == $directory and
    .agent == "general" and .model == $selected[0].model) |
  .id | select(test("^ses[A-Za-z0-9_-]+$"))
' "$tmp/created") || fail invalid-child-identity
api GET "/api/session/$child/message?order=asc&limit=1" "$tmp/empty" ||
  fail child-history-failed
jq -e '.data == [] and .cursor.next == null and .cursor.previous == null' \
  "$tmp/empty" >/dev/null || fail child-not-context-free
# Command substitution is safe here: jq encodes newlines inside a JSON string.
# It strips only jq's structural final newline, never a prompt byte.
input=$(jq -cn --rawfile prompt "$tmp/prompt" '{text:$prompt}')
api POST "/api/session/$child/prompt" "$tmp/admitted" --data "$input" ||
  fail child-prompt-failed
opencode api post "/api/experimental/session/$child/wait" \
  >"$tmp/wait" 2>"$tmp/wait-error" &
request_pid=$!
i=0
while kill -0 "$request_pid" 2>/dev/null; do
  api GET "/api/session/$child/permission" "$tmp/permissions" ||
    fail permission-observation-failed
  jq -e '.data == []' "$tmp/permissions" >/dev/null || fail child-permission-required
  [ "$i" -lt 3600 ] || fail child-execution-timeout
  sleep 1
  i=$((i + 1))
done
wait "$request_pid" || fail child-execution-failed
request_pid=''
api GET "/api/session/$child" "$tmp/finished" || fail child-status-failed
jq -e '.data.outcome == "succeeded"' "$tmp/finished" >/dev/null ||
  fail child-execution-error
api GET "/api/session/$child/message?type=user&order=asc&limit=2" "$tmp/messages" ||
  fail child-readback-failed
jq -e --rawfile prompt "$tmp/prompt" '
  (.data | length) == 1 and .cursor.next == null and
  .data[0].type == "user" and .data[0].text == $prompt and
  ((.data[0].files // []) | length) == 0 and
  ((.data[0].agents // []) | length) == 0 and
  ((.data[0].skills // []) | length) == 0
' "$tmp/messages" >/dev/null || fail prompt-byte-mismatch
cmp -s "$prompt_file" "$tmp/prompt" || fail prompt-file-changed
[ -s "$run_dir/decision.md" ] || fail missing-decision
jq -cn --arg parent "$parent" --arg child "$child" \
  '{protocol:"blind-review-transport/v1",parent_session_id:$parent,child_session_id:$child}'
