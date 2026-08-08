#!/usr/bin/env bash
# Run a single Full Regression tier with `go test -json` output capture and
# append its report section to the accumulated REPORT_FILE. When
# SANDMAN_COVERAGE=1 is set, the same command is instrumented with the
# repository-wide atomic coverage profile used by the coverage report.
#
# Usage:
#   full_regression_tier.sh <name> <cmd> [block_timeout] <report_file> <artifact_dir>
#
# <block_timeout> is an optional GNU `timeout` duration (e.g. "95m") applied
# as a hard backstop above the `go test -timeout` budget already baked into
# <cmd>; it never lowers that budget. The script always renders the tier's
# report section, including when the backstop fires, then exits non-zero when
# the tier failed so the calling workflow step fails.
set -uo pipefail

name="$1"
cmd="$2"
block_timeout="${3:-}"
report="${4:?REPORT_FILE path required}"
artifact_dir="${5:?artifact dir required}"

mkdir -p "$artifact_dir"
base="$(printf '%s' "$name" | sed -E 's/[^A-Za-z0-9]+/_/g; s/^_+//; s/_+$//')"
artifact_dir_abs="$(cd "$artifact_dir" && pwd)"
log="$artifact_dir/$base.log"
jsonl="$artifact_dir/$base.jsonl"
coverage_profile=""
effective_cmd="$cmd"

if [ "${SANDMAN_COVERAGE:-}" = "1" ]; then
  coverage_profile="$artifact_dir_abs/$base.coverprofile"
  coverage_flags="-covermode=atomic -coverpkg=./... -coverprofile=$coverage_profile"
  effective_cmd="${cmd/go test/go test $coverage_flags}"
fi

echo "▶ Running $name"
echo "::group::$name"

set +e
if [ -n "$block_timeout" ]; then
  timeout "$block_timeout" bash -c "$effective_cmd" 2>&1 | tee "$log"
  rc=${PIPESTATUS[0]}
else
  bash -c "$effective_cmd" 2>&1 | tee "$log"
  rc=${PIPESTATUS[0]}
fi
set -e

echo "::endgroup::"

if [ "$rc" -eq 124 ]; then
  status="TIMEOUT"
  reason="block hit ${block_timeout} backstop"
elif [ "$rc" -ne 0 ]; then
  status="FAIL"
  reason="exit code $rc"
else
  status="PASS"
  reason=""
fi

# The raw log already is the `go test -json` stream, but the testing
# framework's timeout panic interleaves raw stderr lines, so the .jsonl copy
# is filtered to lines that actually parse as JSON. jq is preinstalled on
# GitHub-hosted runners; fall back to grep when absent.
grep '^{' "$log" > "$jsonl" || true
if command -v jq >/dev/null 2>&1; then
  count_tests() {
    jq -sr --arg action "$1" \
      '[.[] | select(.Action == $action and (.Test // "") != "")] | length' \
      "$jsonl" 2>/dev/null || printf '0\n'
  }

  extract_fails() {
    jq -r 'select(.Action=="fail") |
      (if (.Test and .Test != "") then "\(.Package): \(.Test)" else "\(.Package): package-level failure" end)' "$jsonl" 2>/dev/null |
      sort -u
  }

  extract_skips() {
    jq -r 'select(.Action=="skip" and .Test and .Test != "") | "\(.Package): \(.Test)"' "$jsonl" 2>/dev/null |
      sort -u
  }
else
  count_tests() {
    grep -E '"Action":"'$1'".*"Test"|"Test".*"Action":"'$1'"' "$jsonl" 2>/dev/null |
      wc -l |
      tr -d ' ' || true
  }

  extract_fails() {
    grep -o '"Action":"fail"[^}]*"Test":"[^"]*"' "$jsonl" |
      sed -E 's/.*"Test":"([^"]*)".*/\1/' |
      sort -u
  }

  extract_skips() {
    grep -o '"Action":"skip"[^}]*"Test":"[^"]*"' "$jsonl" |
      sed -E 's/.*"Package":"([^"]*)".*"Test":"([^"]*)".*/\1: \2/' |
      sort -u
  }
fi

# "Tests running at timeout" lines are written by the testing framework's
# startAlarm panic straight to stderr, not through the -json encoder, so they
# are grepped from the raw combined log in both branches.
extract_running() {
  grep -E '^[[:space:]]+[^ ]+ \([0-9]+[hms]' "$log" |
    sed -E 's/^[[:space:]]+//' |
    sort -u
}

fails="$(extract_fails | sed '/^$/d' || true)"
skips_list="$(extract_skips | sed '/^$/d' || true)"
running="$(extract_running | sed '/^$/d' || true)"
passed="$(count_tests pass)"
failed="$(count_tests fail)"
skipped="$(count_tests skip)"

printf '%s\n' "$status" > "$artifact_dir_abs/$base.status"

render_section() {
  echo ""
  echo "## Tier: $name — $status"
  if [ -n "$reason" ]; then
    echo ""
    echo "Reason: $reason"
  fi
  echo ""
  echo "Command: \`$cmd\`"
  if [ -n "$coverage_profile" ]; then
    echo "Coverage profile: \`$base.coverprofile\`"
    echo "Coverage flags: \`$coverage_flags\`"
  fi
  echo ""
  echo "### Test events"
  echo "- passed: $passed"
  echo "- failed: $failed"
  echo "- skipped: $skipped"
  echo ""
  echo "### Failing tests"
  if [ -n "$fails" ]; then
    echo "$fails" | sed 's/^/- `/; s/$/`/'
  else
    echo "- none"
  fi
  echo ""
  echo "### Skipped tests"
  if [ -n "$skips_list" ]; then
    echo "$skips_list" | sed 's/^/- `/; s/$/`/'
  else
    echo "- none"
  fi
  echo ""
  echo "### Tests running at timeout"
  if [ -n "$running" ]; then
    echo "$running" | sed 's/^/- `/; s/$/`/'
  else
    echo "- none"
  fi
}

render_section >> "$report"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  render_section >> "$GITHUB_STEP_SUMMARY"
fi

echo "  $name: $status"

if [ "$rc" -eq 124 ]; then
  exit 1
fi
exit "$rc"
