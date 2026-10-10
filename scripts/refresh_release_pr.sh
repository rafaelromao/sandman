#!/bin/sh
# Release Please can leave a pending branch unchanged when only test/docs/CI
# commits land. Merge current main so release checks include those repairs too.
set -eu

repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}
owner=${repository%%/*}
branch=release-please--branches--main

pages=$(gh api --paginate --slurp "repos/$repository/pulls?state=open&base=main&head=$owner:$branch")
pull_request=$(printf '%s\n' "$pages" | jq -c --arg repository "$repository" --arg branch "$branch" '
  if type != "array" or any(.[]; type != "array") then error("invalid pull-request pages") else . end |
  [.[][] | select(.base.ref == "main" and .head.ref == $branch and .head.repo.full_name == $repository)] |
  if length > 1 then error("multiple matching release pull requests") else .[0] end
')
if [ "$pull_request" = null ]; then
  printf '%s\n' 'No pending release branch to refresh.'
  exit 0
fi

number=$(printf '%s\n' "$pull_request" | jq -er '.number | select(type == "number" and floor == . and . > 0)')
head_sha=$(printf '%s\n' "$pull_request" | jq -er '.head.sha | select(type == "string" and test("^[0-9a-fA-F]{40}$"))')
comparison=$(gh api "repos/$repository/compare/main...$head_sha")
status=$(printf '%s\n' "$comparison" | jq -er '.status | strings')
case "$status" in
  ahead|identical)
    printf 'Release PR #%s already contains main.\n' "$number"
    ;;
  behind|diverged)
    # A concurrent head change or a merge conflict must fail visibly. Never
    # force-update a bot-owned branch or retry against an unobserved new head.
    gh api --method PUT "repos/$repository/pulls/$number/update-branch" -f "expected_head_sha=$head_sha"
    ;;
  *)
    printf 'Unrecognized comparison status: %s\n' "$status" >&2
    exit 1
    ;;
esac
