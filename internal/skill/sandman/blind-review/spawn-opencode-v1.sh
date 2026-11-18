#!/bin/sh
# OpenCode v1 native-session transport. No prompt bytes are model-transcribed.
# Protocol sources: OpenCode v1.19.3 task.ts, subagent-permissions.ts and the
# generated session/agent/config HTTP SDK. V2 has a separate API contract.
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
server_pid='' request_pid='' child='' url=''
api() {
  method=$1 route=$2 destination=$3
  shift 3
  curl --config "$tmp/curl.conf" --silent --show-error --fail \
    --noproxy '*' --connect-timeout 5 --max-time 60 --request "$method" \
    --header "x-opencode-directory: $directory" \
    --output "$destination" "$url$route" "$@" 2>"$tmp/http-error"
}
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ -n "$request_pid" ]; then
    kill "$request_pid" 2>/dev/null || :
    wait "$request_pid" 2>/dev/null || :
  fi
  if [ -n "$child" ] && [ -n "$url" ]; then
    api POST "/session/$child/abort" "$tmp/abort" --max-time 5 || :
  fi
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || :
    i=0
    while kill -0 "$server_pid" 2>/dev/null && [ "$i" -lt 5 ]; do
      sleep 1
      i=$((i + 1))
    done
    kill -KILL "$server_pid" 2>/dev/null || :
    wait "$server_pid" 2>/dev/null || :
  fi
  # Keep diagnostics on failure; they are private and never printed as logs.
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
opencode session list --format json >"$tmp/sessions" 2>"$tmp/cli-error" ||
  fail session-list-failed
# Filter directory before any export; never choose a session by recency.
jq -er --arg directory "$directory" '
  if type != "array" then error("invalid session list") else
    .[] | select(.directory == $directory) | .id |
    select(test("^ses[A-Za-z0-9_-]+$"))
  end' "$tmp/sessions" >"$tmp/candidates" || fail no-candidate-roots
: >"$tmp/matches"
while IFS= read -r candidate; do
  opencode export "$candidate" >"$tmp/export" 2>"$tmp/cli-error" ||
    fail session-export-failed
  jq -e --arg candidate "$candidate" '.info.id == $candidate' "$tmp/export" >/dev/null ||
    fail exported-identity-mismatch
  jq -c --arg directory "$directory" --arg prompt_file "$prompt_file" \
    -f "$helper_dir/opencode-parent-v1.jq" "$tmp/export" >>"$tmp/matches" ||
    fail invalid-session-export
done <"$tmp/candidates"
jq -se 'length == 1' "$tmp/matches" >/dev/null || fail ambiguous-or-missing-live-owner
jq -s '.[0]' "$tmp/matches" >"$tmp/parent"
parent=$(jq -er '.id' "$tmp/parent")
# Only the explicitly selected V1 protocol is supported by this helper.
# Preserve existing server authentication without printing it or using argv.
jq -nr --arg user "${OPENCODE_SERVER_USERNAME:-opencode}" \
  --arg password "${OPENCODE_SERVER_PASSWORD:-}" '
  if $password == "" then "" else "user = " + (($user + ":" + $password) | tojson) end
' >"$tmp/curl.conf"
opencode serve --pure --port 0 --hostname 127.0.0.1 >"$tmp/server-log" 2>&1 &
server_pid=$!
i=0
while [ "$i" -lt 30 ]; do
  url=$(jq -Rrn '[inputs |
    select(test("^opencode server listening on http://127\\.0\\.0\\.1:[0-9]+$")) |
    sub("^opencode server listening on "; "")] | last // empty' "$tmp/server-log")
  [ -z "$url" ] || break
  kill -0 "$server_pid" 2>/dev/null || fail server-exited
  sleep 1
  i=$((i + 1))
done
[ -n "$url" ] || fail server-start-timeout
api GET "/session/$parent" "$tmp/parent-live" || fail parent-lookup-failed
jq -e --arg parent "$parent" --arg directory "$directory" '
  .id == $parent and .directory == $directory and (.parentID // "") == ""
' "$tmp/parent-live" >/dev/null || fail parent-identity-changed
api GET /agent "$tmp/agents" || fail agent-lookup-failed
api GET /config "$tmp/config" || fail config-lookup-failed
jq -e '(.policies // [] | length) == 0' "$tmp/config" >/dev/null ||
  fail unsupported-policy-configuration
jq -e '(.subagent_depth // 1) >= 1' "$tmp/config" >/dev/null ||
  fail subagent-depth-forbidden
jq -e '[.[] | select(.name == "general" and (.mode == "subagent" or .mode == "all"))] |
  length == 1' "$tmp/agents" >/dev/null || fail general-agent-unavailable
jq '[.[] | select(.name == "general")][0]' "$tmp/agents" >"$tmp/general"
jq -n -e -L "$helper_dir" --slurpfile parent "$tmp/parent" \
  --slurpfile agents "$tmp/agents" --slurpfile live "$tmp/parent-live" '
  include "permission-v1";
  ([$agents[0][] | select(.name == $parent[0].agent)] |
    if length == 1 then .[0].permission else error("unknown parent agent") end) as $rules |
  allowed(($rules + ($live[0].permission // [])); "task"; "general")
' >/dev/null || fail subagent-permission-not-allowed
# An explicit agent model changes native Task variant selection. Refuse it
# instead of silently reviewing on a different default.
jq -e '.model == null' "$tmp/general" >/dev/null || fail explicit-general-model
opencode export "$parent" >"$tmp/export" 2>"$tmp/cli-error" ||
  fail owner-recheck-failed
jq -c --arg directory "$directory" --arg prompt_file "$prompt_file" \
  -f "$helper_dir/opencode-parent-v1.jq" "$tmp/export" >"$tmp/rechecked" ||
  fail owner-recheck-failed
jq -se --slurpfile previous "$tmp/parent" 'length == 1 and .[0] == $previous[0]' \
  "$tmp/rechecked" >/dev/null || fail owner-no-longer-running
jq -n --slurpfile parent "$tmp/parent" --slurpfile live "$tmp/parent-live" \
  --slurpfile general "$tmp/general" --slurpfile config "$tmp/config" '
  ($general[0].permission) as $rules |
  (
    (($live[0].permission // []) |
      map(select(.permission == "external_directory" or .action == "deny"))) +
    (["todowrite","task"] | map(. as $name |
      select(any($rules[]; .permission == $name) | not) |
      {permission:$name,pattern:"*",action:"deny"}))
  ) as $permission |
  {parentID:$parent[0].id,title:"Blind pull-request review",agent:"general",
   permission:($permission +
    [($config[0].experimental.primary_tools // [])[] |
     {permission:.,pattern:"*",action:"deny"}])}
' >"$tmp/create"
api POST /session "$tmp/created" --header 'Content-Type: application/json' \
  --data-binary "@$tmp/create" || fail child-create-failed
child=$(jq -er --arg parent "$parent" --arg directory "$directory" '
  select(.parentID == $parent and .directory == $directory) | .id |
  select(test("^ses[A-Za-z0-9_-]+$"))
' "$tmp/created") || fail invalid-child-identity
api GET "/session/$child/message" "$tmp/empty" || fail child-history-failed
jq -e 'type == "array" and length == 0' "$tmp/empty" >/dev/null ||
  fail child-not-context-free
jq -n --rawfile prompt "$tmp/prompt" --slurpfile parent "$tmp/parent" '
  {agent:"general",model:$parent[0].model,parts:[{type:"text",text:$prompt}]} +
  (if $parent[0].variant == null then {} else {variant:$parent[0].variant} end)
' >"$tmp/input"
# A background curl lets signal traps immediately cancel both request and child.
curl --config "$tmp/curl.conf" --silent --show-error --fail --noproxy '*' \
  --connect-timeout 5 --max-time 3600 --request POST \
  --header "x-opencode-directory: $directory" --header 'Content-Type: application/json' \
  --data-binary "@$tmp/input" --output "$tmp/result" \
  "$url/session/$child/message" 2>"$tmp/request-error" &
request_pid=$!
while kill -0 "$request_pid" 2>/dev/null; do
  api GET /permission "$tmp/permissions" || fail permission-observation-failed
  jq -e --arg child "$child" 'type == "array" and
    all(.[]; .sessionID != $child)' "$tmp/permissions" >/dev/null ||
    fail child-permission-required
  sleep 1
done
wait "$request_pid" || fail child-execution-failed
request_pid=''
jq -e '.info.role == "assistant" and .info.error == null' "$tmp/result" >/dev/null ||
  fail child-execution-error
api GET "/session/$child/message" "$tmp/messages" || fail child-readback-failed
jq -e --rawfile prompt "$tmp/prompt" '
  [.[] | select(.info.role == "user")] |
  length == 1 and (.[0].parts | length == 1) and
  .[0].parts[0].type == "text" and .[0].parts[0].text == $prompt
' "$tmp/messages" >/dev/null || fail prompt-byte-mismatch
cmp -s "$prompt_file" "$tmp/prompt" || fail prompt-file-changed
[ -s "$run_dir/decision.md" ] || fail missing-decision
jq -cn --arg parent "$parent" --arg child "$child" \
  '{protocol:"blind-review-transport/v1",parent_session_id:$parent,child_session_id:$child}'
