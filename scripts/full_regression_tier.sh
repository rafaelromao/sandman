#!/usr/bin/env bash
# Run a single Full Regression tier with `go test -json` output capture and
# append its report section to the accumulated REPORT_FILE.
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
base="$(printf '%s' "$name" | tr ' ' '_' | tr -cd 'A-Za-z0-9_-')"
log="$artifact_dir/$base.log"
jsonl="$artifact_dir/$base.jsonl"

echo "▶ Running $name"
echo "::group::$name"

set +e
if [ -n "$block_timeout" ]; then
  timeout "$block_timeout" bash -c "$cmd" 2>&1 | tee "$log"
  rc=${PIPESTATUS[0]}
else
  bash -c "$cmd" 2>&1 | tee "$log"
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
  extract_fails() {
    jq -r 'select(.Action=="fail") |
      (if (.Test and .Test != "") then "\(.Package): \(.Test)" else "\(.Package): package-level failure" end)' "$jsonl" 2>/dev/null |
      sort -u
  }
else
  extract_fails() {
    grep -o '"Action":"fail"[^}]*"Test":"[^"]*"' "$jsonl" |
      sed -E 's/.*"Test":"([^"]*)".*/\1/' |
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
running="$(extract_running | sed '/^$/d' || true)"

render_section() {
  echo ""
  echo "## Tier: $name — $status"
  if [ -n "$reason" ]; then
    echo ""
    echo "Reason: $reason"
  fi
  echo ""
  echo "### Failing tests"
  if [ -n "$fails" ]; then
    echo "$fails" | sed 's/^/- `/; s/$/`/'
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
