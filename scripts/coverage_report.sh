#!/usr/bin/env bash
# Run Sandman's full regression tiers with coverage instrumentation.
#
# Usage:
#   scripts/coverage_report.sh [report_file] [artifact_dir]
#
# The command deliberately runs unit, integration/smoke, and E2E tiers with
# the same environment gates and timeout budgets as full-regression-linux.yml.
# It returns non-zero when any tier fails. Use SANDMAN_COVERAGE_STRICT=1 to
# also fail when a tier skips tests, which is useful when credentials and all
# external runtime prerequisites are available.

set -euo pipefail

root="$(git rev-parse --show-toplevel)"
report="${1:-$root/TEST_REPORT.md}"
artifact_dir="${2:-$root/coverage-artifacts}"

mkdir -p "$(dirname "$report")" "$artifact_dir"

for base in Unit_Race Smoke E2E; do
  rm -f \
    "$artifact_dir/$base.log" \
    "$artifact_dir/$base.jsonl" \
    "$artifact_dir/$base.status" \
    "$artifact_dir/$base.coverprofile" \
    "$artifact_dir/$(printf '%s' "$base" | tr '[:upper:]' '[:lower:]').html"
done

{
  printf '# Sandman Test Coverage Report\n\n'
  printf 'Commit: `%s`\n' "$(git -C "$root" rev-parse HEAD)"
  printf 'Go: `%s`\n\n' "$(go version)"
  printf 'This report is generated from a full regression execution. A green test command is not treated as complete coverage when tests were skipped.\n'
} > "$report"

export CI="${CI:-true}"
export SANDMAN_FULL_REGRESSION=1
export SANDMAN_COVERAGE=1

run_tier() {
  local name="$1"
  local command="$2"
  local backstop="$3"

  set +e
  "$root/scripts/full_regression_tier.sh" "$name" "$command" "$backstop" "$report" "$artifact_dir"
  local rc=$?
  set -e
  return "$rc"
}

overall_rc=0

if run_tier "Unit + Race" \
  "go test -race -json ./..." \
  "30m"; then
  :
else
  overall_rc=1
fi

if run_tier "Smoke" \
  "SANDMAN_TEST_PROVIDERS=all SANDMAN_RUN_SMOKE_E2E=1 go test -tags smoke -json -timeout 60m ./internal/cmd -run Smoke" \
  "65m"; then
  :
else
  overall_rc=1
fi

if run_tier "E2E" \
  "SANDMAN_RUN_AGENT_E2E=1 SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all go test -tags e2e -json -timeout 90m ./..." \
  "95m"; then
  :
else
  overall_rc=1
fi

set +e
"$root/scripts/render_coverage_report.sh" "$report" "$artifact_dir"
render_rc=$?
set -e

if [ "$render_rc" -ne 0 ]; then
  overall_rc=1
fi

exit "$overall_rc"
