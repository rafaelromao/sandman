# shellcheck shell=sh
# shellcheck disable=SC2034
# Sourced by the blind-review helpers (publish-v1.sh, review-prompt-v1.sh); not
# executed on its own. POSIX sh; needs gh and jq. Nothing here exits the caller:
# every function returns a status and the caller maps it to its own refusal.
#
# Capability-based GitHub access. A helper tries its GraphQL-backed gh call
# first. Only when that read fails does it ask gh_select_rest, which probes a
# minimal GraphQL query: if GraphQL itself is unavailable (a managed sandbox
# that blocks it answers every GraphQL-backed call with HTTP 403), the rest of
# the run uses the REST equivalents below. A failure while GraphQL works is not
# a missing capability and stays the caller's refusal. The mode is chosen from
# the probe alone, never from the host or product name, is sticky for the run,
# and is fixed before any mutation: a failed post is never retried through the
# other route. Output on stdout is identical in both modes; the one notice goes
# to stderr.
#
# REST results are normalized to the JSON shape the GraphQL-backed calls print,
# so the callers' jq filters do not care which route produced them.
gh_mode=graphql

# gh_select_rest: succeeds, and switches to REST, only when GraphQL is
# unavailable. Call it after a GraphQL-backed read has failed.
gh_select_rest() {
  if gh api graphql -f 'query=query{__typename}' >/dev/null 2>&1; then
    return 1
  fi
  gh_mode=rest
  printf '%s\n' 'blind-review: GraphQL-backed gh calls are unavailable; using REST equivalents' >&2
}

# gh_rest_pr REPOSITORY PULL_REQUEST OUT TMPDIR: the pull request as
# {number,title,body,state,headRefOid,baseRefName} (state upper-cased, a null
# body empty).
gh_rest_pr() {
  gh api "repos/$1/pulls/$2" >"$4/rest-pr.raw" || return 1
  jq -c '{number:.number, title:(.title // ""), body:(.body // ""), state:(.state | ascii_upcase),
    headRefOid:.head.sha, baseRefName:.base.ref}' "$4/rest-pr.raw" >"$3"
}

# gh_rest_pr_with_comments REPOSITORY PULL_REQUEST OUT TMPDIR: the pull request
# as above plus comments:[{id,url,createdAt,body}] over every page.
gh_rest_pr_with_comments() {
  gh_rest_pr "$1" "$2" "$4/rest-pr.json" "$4" || return 1
  gh api --paginate "repos/$1/issues/$2/comments" >"$4/rest-comments.raw" || return 1
  jq -s 'add // []' "$4/rest-comments.raw" >"$4/rest-comments.json" || return 1
  jq -c --slurpfile comments "$4/rest-comments.json" '. + {comments: ($comments[0] | map(
    {id:.id, url:.html_url, createdAt:.created_at, body:(.body // "")}))}' "$4/rest-pr.json" >"$3"
}

# gh_rest_issue_body REPOSITORY ISSUE OUT TMPDIR: {body} of a work item.
gh_rest_issue_body() {
  gh api "repos/$1/issues/$2" >"$4/rest-issue.raw" || return 1
  jq -c '{body:(.body // "")}' "$4/rest-issue.raw" >"$3"
}

# gh_rest_post_comment REPOSITORY PULL_REQUEST BODY_FILE OUT: posts the file as
# a pull request comment (exactly one attempt) and writes the created comment.
gh_rest_post_comment() {
  gh api -X POST "repos/$1/issues/$2/comments" -F "body=@$3" >"$4"
}
