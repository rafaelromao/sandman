#!/bin/sh
# GitHub calls: pr view --json state,headRefOid; pr comment --body-file;
# pr view --json state,headRefOid,comments. A failed mutation is never retried.
set -eu
repository= pull_request= head= decision= print_body=false
refuse() {
  jq -cn --arg reason "$1" '{protocol:"blind-review/v1",state:"refused",reason:$reason}' >&2
  exit 1
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --repository) repository=$2; shift 2 ;;
    --pull-request) pull_request=$2; shift 2 ;;
    --head-sha) head=$2; shift 2 ;;
    --decision-file) decision=$2; shift 2 ;;
    --print-body) print_body=true; shift ;;
    *) refuse invalid-argument ;;
  esac
done
[ -n "$head" ] && [ -f "$decision" ] || refuse invalid-input
head=$(printf '%s' "$head" | jq -Rr 'gsub("^\\s+|\\s+$"; "") | ascii_downcase')
if command -v sha256sum >/dev/null 2>&1; then
  identity=$(printf 'blind-review\000%s' "$head" | sha256sum | awk '{print $1}')
else
  identity=$(printf 'blind-review\000%s' "$head" | shasum -a 256 | awk '{print $1}')
fi
helper_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ "$print_body" = true ]; then
  exec jq -Rrjs --arg identity "$identity" -f "$helper_dir/publication-v1.jq" "$decision"
fi
[ -n "$repository" ] && [ -n "$pull_request" ] || refuse invalid-input
tmp=$(mktemp -d "${TMPDIR:-/tmp}/sandman-blind-publication.XXXXXX")
trap 'rm -f "$tmp/body" "$tmp/pr"; rmdir "$tmp"' EXIT HUP INT TERM
gh pr view "$pull_request" --repo "$repository" --json state,headRefOid >"$tmp/pr" ||
  refuse lookup-failed
jq -e --arg head "$head" '
  (.state | ascii_downcase) == "open" and (.headRefOid | ascii_downcase) == $head
' "$tmp/pr" >/dev/null || refuse stale-or-closed
jq -Rrjs --arg identity "$identity" -f "$helper_dir/publication-v1.jq" "$decision" >"$tmp/body" ||
  refuse invalid-decision
posted_url=$(gh pr comment "$pull_request" --repo "$repository" --body-file "$tmp/body") ||
  refuse publication-uncertain
gh pr view "$pull_request" --repo "$repository" --json state,headRefOid,comments >"$tmp/pr" ||
  refuse confirmation-unavailable
jq -ce --arg repository "$repository" --argjson pull_request "$pull_request" \
  --arg head "$head" --arg url "$posted_url" --rawfile body "$tmp/body" '
  select((.state | ascii_downcase) == "open" and (.headRefOid | ascii_downcase) == $head) |
  .comments[] | select(.url == $url and .body == $body and (.createdAt | type == "string")) |
  {protocol:"blind-review/v1",state:"confirmed",repository:$repository,pull_request:$pull_request,
   head_sha:$head,comment_url:.url,created_at:.createdAt,
   verdict: (if ($body | split("\n") | map(select(test("\\S"))) | .[-2]) == "<!-- sandman-verdict:approved -->" then "approved"
     elif ($body | split("\n") | map(select(test("\\S"))) | .[-2]) == "<!-- sandman-verdict:changes_requested -->" then "changes_requested"
     else "unknown" end)}
' "$tmp/pr" || refuse confirmation-uncertain
