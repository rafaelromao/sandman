#!/usr/bin/env bash
# Render coverage summaries and HTML artifacts for the full regression tiers.
#
# Usage:
#   render_coverage_report.sh <report_file> <artifact_dir>
#
# A tier is TRUSTED only when its command passed, produced a valid profile, and
# reported no skipped test events. Skips remain visible as PARTIAL coverage;
# they are never folded into a green coverage claim.

set -euo pipefail

report="${1:?report file required}"
artifact_dir="${2:?artifact directory required}"

if [ ! -f "$report" ]; then
  printf 'coverage report input does not exist: %s\n' "$report" >&2
  exit 1
fi

if [ ! -d "$artifact_dir" ]; then
  printf 'coverage artifact directory does not exist: %s\n' "$artifact_dir" >&2
  exit 1
fi

count_tests() {
  local jsonl="$1"
  local action="$2"

  if [ ! -s "$jsonl" ]; then
    printf '0\n'
    return 0
  fi

  if command -v jq >/dev/null 2>&1; then
    jq -sr --arg action "$action" \
      '[.[] | select(.Action == $action and (.Test // "") != "")] | length' \
      "$jsonl" 2>/dev/null || printf '0\n'
    return 0
  fi

  grep -E '"Action":"'$action'".*"Test"|"Test".*"Action":"'$action'"' "$jsonl" 2>/dev/null |
    wc -l |
    tr -d ' ' || true
}

status_for() {
  local status_file="$1"
  if [ -s "$status_file" ]; then
    tr -d '\r\n' < "$status_file"
  else
    printf 'UNKNOWN\n'
  fi
}

coverage_for() {
  local profile="$1"
  if [ ! -s "$profile" ]; then
    return 1
  fi

  awk '
    NR == 1 {
      if ($1 != "mode:") exit 1
      next
    }
    NF != 3 { exit 1 }
    {
      statements += $2
      if ($3 > 0) covered += $2
    }
    END {
      if (statements == 0) exit 1
      printf "%.1f%%\n", (covered * 100) / statements
    }
  ' "$profile"
}

slug_for() {
  printf '%s' "$1" | tr '[:upper:] ' '[:lower:]_' | tr -cd 'a-z0-9_-'
}

tiers=(
  "Unit + Race|Unit and race-enabled untagged suite|Unit_Race"
  "Integration (Smoke)|Provider and build-tools smoke scenarios|Smoke"
  "E2E|All tagged E2E gates and real-agent scenarios|E2E"
)

trusted=0
partial=0
unavailable=0
html_unavailable=0

{
  printf '\n## Coverage\n\n'
  printf 'Coverage is collected by the same commands that execute the full regression tiers. '
  printf 'Profiles instrument every Sandman package with `-coverpkg=./...` and use atomic counters.\n\n'
  printf '| Tier | Test events | Statement coverage | Validity |\n'
  printf '|------|-------------|-------------------|----------|\n'

  for tier in "${tiers[@]}"; do
    IFS='|' read -r name description base <<< "$tier"
    profile="$artifact_dir/$base.coverprofile"
    jsonl="$artifact_dir/$base.jsonl"
    status="$(status_for "$artifact_dir/$base.status")"
    passed="$(count_tests "$jsonl" pass)"
    failed="$(count_tests "$jsonl" fail)"
    skipped="$(count_tests "$jsonl" skip)"
    events="${passed} passed, ${failed} failed, ${skipped} skipped"
    coverage="not available"
    validity="UNAVAILABLE"

    if coverage_value="$(coverage_for "$profile")"; then
      coverage="$coverage_value"
      if [ "$status" = "PASS" ] && [ "$passed" != "0" ] && [ "$failed" = "0" ] && [ "$skipped" = "0" ]; then
        validity="TRUSTED"
        trusted=$((trusted + 1))
      elif [ "$status" = "PASS" ] && [ "$passed" != "0" ]; then
        validity="PARTIAL"
        partial=$((partial + 1))
      else
        unavailable=$((unavailable + 1))
      fi
    else
      validity="UNAVAILABLE"
      unavailable=$((unavailable + 1))
    fi

    printf '| %s | %s | %s | %s |\n' "$name" "$events" "$coverage" "$validity"

    if [ -s "$profile" ]; then
      html="$artifact_dir/$(slug_for "$base").html"
      if go tool cover -html="$profile" -o="$html"; then
        :
      else
        printf 'coverage HTML generation failed for %s\n' "$name" >&2
        html_unavailable=$((html_unavailable + 1))
      fi
    fi
  done

  printf '\n### Coverage validity\n\n'
  printf -- '- `TRUSTED`: the tier passed, ran at least one test, emitted a profile, and had no skipped test events.\n'
  printf -- '- `PARTIAL`: the tier passed, but one or more tests skipped; the percentage excludes those paths.\n'
  printf -- '- `UNAVAILABLE`: the tier failed, emitted no usable profile, or could not be evaluated.\n'
  printf '\nTrusted tiers: %d; partial tiers: %d; unavailable tiers: %d.\n' "$trusted" "$partial" "$unavailable"
  if [ "$html_unavailable" -eq 0 ]; then
    printf '\nHTML reports and raw profiles are stored beside the JSONL execution logs.\n'
  else
    printf '\nRaw profiles are stored beside the JSONL execution logs; HTML generation failed for %d tier(s).\n' "$html_unavailable"
  fi
  printf '\n### Scenario definitions\n\n'
  printf -- '- **Unit + Race**: untagged `./...` packages under the race detector.\n'
  printf -- '- **Integration (Smoke)**: all configured providers and build-tools presets with real sandbox resources.\n'
  printf -- '- **E2E**: `SANDMAN_E2E_GATES=all` plus the real-agent preset matrix.\n'
} >> "$report"

if [ "${SANDMAN_COVERAGE_STRICT:-0}" = "1" ] && { [ "$partial" -ne 0 ] || [ "$unavailable" -ne 0 ]; }; then
  printf 'coverage is incomplete: %d partial, %d unavailable tier(s)\n' "$partial" "$unavailable" >&2
  exit 1
fi
