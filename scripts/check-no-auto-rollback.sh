#!/usr/bin/env bash
# check-no-auto-rollback.sh
#
# Dev-side mirror of internal/devtools/release_pipeline_sentinels_test.go.
# Runs the same forbidden-wording scan in shell so editors and pre-commit
# hooks can flag reintroduction of the rolled-back `--auto` / `--count` /
# `auto_max_count` surface without spinning up a Go test binary.
#
# Exits 0 if no forbidden wording is found, 1 otherwise.
#
# Usage: scripts/check-no-auto-rollback.sh [<repo-root>]

set -u

ROOT="${1:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"

if [ ! -d "$ROOT" ]; then
  echo "FAIL: $ROOT is not a directory" >&2
  exit 1
fi

allowed=(
  ':!internal/devtools/release_pipeline_sentinels_test.go'
  ':!docs/adr/0022-rename-ralph-to-auto-mode.md'
  ':!docs/adr/0041-rollback-auto-mode.md'
  ':!docs/adr/0041-cancel-auto-mode.md'
  ':!docs/adr/README.md'
  ':!internal/cmd/run_test.go'
  ':!CHANGELOG.md'
  ':!docs/development/release-pipeline-review.md'
  ':!scripts/check-no-auto-rollback.sh'
  ':!.sandman/state/2378.head_sha'
)

skip=(
  ':!.git'
  ':!node_modules'
  ':!AGENTS.md'
  ':!.sandman'
)

fail=0

run_check() {
  local pattern="$1"
  if git -C "$ROOT" grep --fixed-strings --ignore-case --line-number \
      -- "$pattern" "${skip[@]}" "${allowed[@]}" > /dev/null 2>&1; then
    echo "FAIL: forbidden string \"$pattern\" leaked into the live surface." >&2
    git -C "$ROOT" grep --fixed-strings --ignore-case --line-number \
      -- "$pattern" "${skip[@]}" "${allowed[@]}" >&2 || true
    fail=1
  fi
}

run_check "--auto"
run_check "--count "
run_check "auto_max_count"

if [ "$fail" -ne 0 ]; then
  echo "FAIL: --auto rollback sentinel tripped. See matches above." >&2
  exit 1
fi

echo "PASS: --auto rollback sentinel clean."
exit 0
