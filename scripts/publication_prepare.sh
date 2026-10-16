#!/usr/bin/env bash
set -euo pipefail
# Never move tags or overwrite assets. API errors other than absence fail closed.
test "$(git rev-parse HEAD)" = "$RELEASE_SHA"
if git show-ref --verify --quiet "refs/tags/$RELEASE_TAG"; then
  test "$(git rev-parse "$RELEASE_TAG^{commit}")" = "$RELEASE_SHA"
else
  gh api --method POST "repos/$GITHUB_REPOSITORY/git/refs" -f "ref=refs/tags/$RELEASE_TAG" -f "sha=$RELEASE_SHA"
fi
# List rather than treating arbitrary release-view failures as 'not found'.
releases=$(gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/releases?per_page=100")
release=$(jq -c --arg tag "$RELEASE_TAG" '[.[][] | select(.tag_name == $tag)] | if length > 1 then error("duplicate release") else .[0] end' <<< "$releases")
if [ "$release" = null ]; then
  gh release create "$RELEASE_TAG" --verify-tag --target "$RELEASE_SHA" --draft --generate-notes
elif [ "$(jq -r '.draft' <<< "$release")" = false ]; then
  # Finalized releases are immutable; reruns do no writes.
  echo 'complete=true' >> "$GITHUB_OUTPUT"
else
  test "$(jq '.assets | length' <<< "$release")" = 0
fi
# The action fetches the tag again before packaging.
git fetch origin "refs/tags/$RELEASE_TAG:refs/tags/$RELEASE_TAG"
test "$(git rev-parse "$RELEASE_TAG^{commit}")" = "$RELEASE_SHA"
