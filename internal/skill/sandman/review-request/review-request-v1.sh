#!/bin/sh

set -eu

repository=
pull_request=
head_sha=
trigger_prefix=
request_file=
context=

while [ "$#" -gt 0 ]; do
  case "$1" in
    --repository) repository=${2-}; shift 2 ;;
    --pull-request) pull_request=${2-}; shift 2 ;;
    --head-sha) head_sha=${2-}; shift 2 ;;
    --trigger-prefix) trigger_prefix=${2-}; shift 2 ;;
    --request-file) request_file=${2-}; shift 2 ;;
    --context) context=${2-}; shift 2 ;;
    *) printf '%s\n' "usage: review-request-v1.sh --repository <owner/repo> --pull-request <N> --head-sha <sha> --trigger-prefix <command> [--request-file <path>] [--context <text>]" >&2; exit 2 ;;
  esac
done

refuse() {
  jq -cn --arg reason "$1" \
    '{protocol:"review-request/v1",state:"refused",reason:$reason}'
  exit 1
}

[ -n "$repository" ] || refuse "repository-missing"
[ -n "$pull_request" ] || refuse "pull-request-missing"
[ -n "$head_sha" ] || refuse "head-sha-missing"
[ -n "$trigger_prefix" ] || refuse "trigger-prefix-missing"

pr_data=$(gh pr view "$pull_request" --repo "$repository" --json state,headRefOid) || refuse "pull-request-lookup-failed"
pr_state=$(printf '%s' "$pr_data" | jq -er '.state') || refuse "pull-request-state-invalid"
live_head=$(printf '%s' "$pr_data" | jq -er '.headRefOid') || refuse "pull-request-head-invalid"
[ "$pr_state" = "OPEN" ] || refuse "pull-request-not-open"
[ "$live_head" = "$head_sha" ] || refuse "head-changed"

skill_root=${SANDMAN_SKILL_ROOT:-${HOME}/.agents/skills/sandman}
if [ -n "$request_file" ] && [ -f "$request_file" ]; then
  guard_result=$(sh "$skill_root/pr-review/review-trigger-guard-v1.sh" \
    --repository "$repository" --pull-request "$pull_request" \
    --head-sha "$head_sha" --trigger-prefix "$trigger_prefix" \
    --request-file "$request_file") || refuse "trigger-guard-failed"
else
  guard_result=$(sh "$skill_root/pr-review/review-trigger-guard-v1.sh" \
    --repository "$repository" --pull-request "$pull_request" \
    --head-sha "$head_sha" --trigger-prefix "$trigger_prefix") ||
    refuse "trigger-guard-failed"
fi
guard_decision=$(printf '%s' "$guard_result" | jq -er '.decision') || refuse "trigger-guard-result-invalid"
[ "$guard_decision" = "allow" ] || refuse "trigger-guard-blocked"

body=$trigger_prefix
if [ -n "$context" ]; then
  body="$trigger_prefix $context"
fi
comment_url=$(gh pr comment "$pull_request" --repo "$repository" --body "$body") || refuse "trigger-post-failed"
[ -n "$comment_url" ] || refuse "trigger-url-missing"

comments=$(gh pr view "$pull_request" --repo "$repository" --json headRefOid,comments) || refuse "trigger-confirmation-lookup-failed"
trigger_created_at=$(printf '%s' "$comments" | jq -er \
  --arg head "$head_sha" --arg trigger_url "$comment_url" --arg prefix "$trigger_prefix" '
    def valid_timestamp:
      if type != "string" or (test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$") | not) then false
      else capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2})(?<time>T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $parts |
        (try (($parts.base + $parts.time + "Z") | fromdateiso8601) catch null) != null
      end;
    select(.headRefOid == $head) |
    first(.comments[] | select((.url // "") == $trigger_url) |
      select((.body // "") | startswith($prefix)) |
      select((.createdAt // "") | valid_timestamp) | .createdAt)
  ') || refuse "trigger-confirmation-failed"

confirmed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ) || refuse "confirmation-time-failed"
jq -cn \
  --arg repository "$repository" \
  --arg head_sha "$head_sha" \
  --arg trigger_id "$comment_url" \
  --arg trigger_prefix "$trigger_prefix" \
  --arg trigger_created_at "$trigger_created_at" \
  --arg confirmed_at "$confirmed_at" \
  --arg comment_url "$comment_url" \
  --arg created_at "$trigger_created_at" \
  --argjson pull_request "$pull_request" \
  '{protocol:"review-request/v1",state:"confirmed",repository:$repository,
    pull_request:$pull_request,head_sha:$head_sha,trigger_id:$trigger_id,
    trigger_prefix:$trigger_prefix,trigger_created_at:$trigger_created_at,
    confirmed_at:$confirmed_at,comment_url:$comment_url,created_at:$created_at}'
