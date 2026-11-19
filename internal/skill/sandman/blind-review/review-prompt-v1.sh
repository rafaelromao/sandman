#!/bin/sh
# GitHub calls: pr view --json number,title,body,state,headRefOid,baseRefName;
# api graphql --paginate for native closing references; issue view --json body;
# api --paginate for issue comments, pull reviews and pull inline comments.
# Every lookup fails closed.
set -eu
repository='' pull_request='' head='' run_dir=''
fail() { printf '%s\n' "$1" >&2; exit 1; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --repository) repository=$2; shift 2 ;;
    --pull-request) pull_request=$2; shift 2 ;;
    --head-sha) head=$2; shift 2 ;;
    --run-dir) run_dir=$2; shift 2 ;;
    *) fail REVIEW_PROMPT_INVALID_ARGUMENT ;;
  esac
done
[ -n "$repository" ] && [ -n "$pull_request" ] && [ -n "$head" ] &&
  [ -n "$run_dir" ] || fail REVIEW_PROMPT_INVALID_INPUT
owner=${repository%%/*}
repository_name=${repository#*/}
[ -n "$owner" ] && [ -n "$repository_name" ] &&
  [ "$repository_name" != "$repository" ] &&
  [ "${repository_name#*/}" = "$repository_name" ] ||
  fail REVIEW_PROMPT_INVALID_INPUT
helper_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
template=$helper_dir/review-prompt.md
root=$(git rev-parse --show-toplevel) || fail REVIEW_PROMPT_WORKTREE_MISSING
if [ -s "$root/.sandman/reviews/review-prompt.md" ] &&
  jq -Rse 'test("[^\\s]")' "$root/.sandman/reviews/review-prompt.md" >/dev/null; then
  template=$root/.sandman/reviews/review-prompt.md
fi
tmp=$(mktemp -d "${TMPDIR:-/tmp}/sandman-blind-prompt.XXXXXX")
trap 'rm -f "$tmp/pr" "$tmp/links" "$tmp/issue" "$tmp/comments" "$tmp/reviews" "$tmp/inline"; rmdir "$tmp"' EXIT HUP INT TERM
gh pr view "$pull_request" --repo "$repository" \
  --json number,title,body,state,headRefOid,baseRefName >"$tmp/pr" ||
  fail REVIEW_PROMPT_LOOKUP_FAILED
jq -e --arg head "$head" '
  .state == "OPEN" and .headRefOid == $head and (.baseRefName | type == "string")
' "$tmp/pr" >/dev/null || fail REVIEW_PROMPT_STALE_HEAD
gh api graphql --paginate \
  -f "owner=$owner" -f "repo=$repository_name" -F "number=$pull_request" \
  -f query='query($owner:String!,$repo:String!,$number:Int!,$endCursor:String){repository(owner:$owner,name:$repo){pullRequest(number:$number){closingIssuesReferences(first:100,after:$endCursor){nodes{number} pageInfo{hasNextPage endCursor}}}}}' \
  >"$tmp/links" || fail REVIEW_PROMPT_LINKAGE_LOOKUP_FAILED
linked=$(jq -ser '
  def refs: .data.repository.pullRequest.closingIssuesReferences;
  def valid_page:
    type == "object" and
    ((.errors // []) | type == "array" and length == 0) and
    (refs as $r |
      ($r | type == "object") and
      ($r.nodes | type == "array") and
      all($r.nodes[]; (.number | type == "number" and . > 0)) and
      ($r.pageInfo | type == "object") and
      ($r.pageInfo.hasNextPage | type == "boolean") and
      (if $r.pageInfo.hasNextPage then
         ($r.pageInfo.endCursor | type == "string" and length > 0)
       else
         ($r.pageInfo.endCursor == null or ($r.pageInfo.endCursor | type == "string"))
       end));
  if type != "array" or length == 0 then error("missing GraphQL pages")
  elif any(.[]; valid_page | not) then error("invalid GraphQL page")
  elif any(.[0:-1][]; .data.repository.pullRequest.closingIssuesReferences.pageInfo.hasNextPage != true)
    then error("incomplete GraphQL pagination")
  elif (last | .data.repository.pullRequest.closingIssuesReferences.pageInfo.hasNextPage) != false
    then error("incomplete GraphQL pagination")
  else [.[].data.repository.pullRequest.closingIssuesReferences.nodes[].number][0] // 0
  end
' "$tmp/links") || fail REVIEW_PROMPT_LINKAGE_INVALID
if [ "$linked" -eq 0 ]; then
  linked=$(jq -r '
    ((try (.body | capture("\\bimplements\\s+#(?<n>[0-9]+)"; "i").n | tonumber) catch null) //
     (try (.body | capture("\\b(?:close(?:s|d)?|fix(?:es|ed)?|resolve(?:s|d)?)\\s*:?\\s*#(?<n>[0-9]+)"; "i").n | tonumber) catch null) //
     0)
  ' "$tmp/pr") || fail REVIEW_PROMPT_INVALID_PR
fi
printf '{"body":""}\n' >"$tmp/issue"
if [ "$linked" -gt 0 ]; then
  gh issue view "$linked" --repo "$repository" --json body >"$tmp/issue" ||
    fail REVIEW_PROMPT_ISSUE_LOOKUP_FAILED
fi
gh api --paginate "repos/$repository/issues/$pull_request/comments" >"$tmp/comments" ||
  fail REVIEW_PROMPT_HISTORY_LOOKUP_FAILED
gh api --paginate "repos/$repository/pulls/$pull_request/reviews" >"$tmp/reviews" ||
  fail REVIEW_PROMPT_HISTORY_LOOKUP_FAILED
gh api --paginate "repos/$repository/pulls/$pull_request/comments" >"$tmp/inline" ||
  fail REVIEW_PROMPT_HISTORY_LOOKUP_FAILED
jq -jrns --rawfile template "$template" --arg run_dir "$run_dir" \
  --slurpfile pr "$tmp/pr" --slurpfile issue "$tmp/issue" \
  --slurpfile comments "$tmp/comments" --slurpfile reviews "$tmp/reviews" --slurpfile inline "$tmp/inline" '
  def inert: gsub("\\{\\{"; "&#123;&#123;") | gsub("\\}\\}"; "&#125;&#125;");
  def timestamp_key:
    capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") |
    [(.base + "Z" | fromdateiso8601), (((.fraction // "") + "000000000")[:9])];
  def request:
    sub("^[ \\t\\r\\n]+"; "") |
    test("^(?:@\\S+\\s+)?/sandman(?:\\s+|$)"; "i") or
    (test("^[/@]") and test("review"; "i"));
  (
    ($comments | add | sort_by(.created_at | timestamp_key) |
      map(select(.body | request | not) | .body)) +
    ($reviews | add | map("### Formal review (" + .state + ")\n\n" + .body)) +
    ($inline | add | map("### Inline review (" + .path +
      (if (.line // 0) > 0 then ":" + (.line | tostring) else "" end) + ")\n\n" + .body))
  ) as $entries |
  (if ($entries | length) > 0 then
    "## Authoritative prior review entries\n\n" + ($entries | join("\n\n---\n\n"))
   else "" end) as $history |
  {PR_NUMBER: ($pr[0].number | tostring), PR_TITLE: ($pr[0].title | inert),
   PR_BODY: ($pr[0].body | inert), ACCEPTANCE_CRITERIA: ($issue[0].body | inert),
   REVIEW_FOCUS: "", RUN_DIR: $run_dir,
   PRIOR_REVIEW_EXISTS: (if ($entries | length) > 0 then "YES" else "NO" end),
   PRIOR_REVIEW_CONTEXT: ($history | inert)} as $values |
  reduce ["PR_NUMBER","PR_TITLE","PR_BODY","ACCEPTANCE_CRITERIA","REVIEW_FOCUS","RUN_DIR","PRIOR_REVIEW_EXISTS","PRIOR_REVIEW_CONTEXT"][] as $key
    ($template; split("{" + "{" + $key + "}" + "}") | join($values[$key])) |
  if test("\\{\\{[^{}]+\\}\\}") then error("missing substitution keys") else
    "## Review snapshot\n\nCheckout commit: `" + $pr[0].headRefOid +
    "`\nComparison base: `origin/" + $pr[0].baseRefName + "`\n\n" + .
  end
' || fail REVIEW_PROMPT_RENDER_FAILED
