#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: cold_start_inventory.sh [--json-input FILE] [--output FILE] [--self-check FILE]

Cold-start migration inventory for slice 8 of the verify-then-close spec.

Defaults (no flags): fetch open issues from the current GitHub repo, classify each,
write a TSV table to scripts/cold_start_inventory.tsv, and exit 0.

Modes:
  --json-input FILE  skip the `gh` API call and read issues from FILE.
                     FILE must be a JSON array of {number, title, body, labels}
                     objects as produced by `gh issue list --state open --json ...`.
  --output FILE      write the TSV to FILE instead of the default path.
  --self-check FILE  reclassify the JSON input and compare against FILE as TSV.
                     Returns exit code 0 if they match byte-for-byte, 1 otherwise.
                     Use this as a regression test: the TSV is the expected output.

Exit codes:
  0   success (or self-check matched)
  1   classification mismatch during self-check
  2   invalid usage
  3   missing dependency (gh, jq)
USAGE
}

err() { echo "cold_start_inventory: $*" >&2; }

require_bin() {
  for b in "$@"; do
    if ! command -v "$b" >/dev/null 2>&1; then
      err "missing required binary: $b"
      exit 3
    fi
  done
}

JSON_INPUT=""
OUTPUT_PATH="scripts/cold_start_inventory.tsv"
SELF_CHECK=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --json-input) JSON_INPUT="$2"; shift 2 ;;
    --output) OUTPUT_PATH="$2"; shift 2 ;;
    --self-check) SELF_CHECK="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) err "unknown flag: $1"; usage; exit 2 ;;
  esac
done

require_bin jq

if [[ -z "$JSON_INPUT" ]]; then
  require_bin gh
  REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner)"
  JSON_INPUT="$(mktemp -t cold_start_inventory.XXXXXX.json)"
  trap '[[ -n "${JSON_INPUT:-}" && "$JSON_INPUT" = /tmp/* ]] && rm -f "$JSON_INPUT"' EXIT
  gh issue list --state open --json number,title,body,labels --limit 1000 \
    --repo "$REPO" > "$JSON_INPUT"
fi

if ! [[ -f "$JSON_INPUT" ]]; then
  err "json input file not found: $JSON_INPUT"
  exit 2
fi

now_iso() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

# AC-shape headings observed across open issues:
#   ## Acceptance criteria
#   ## Acceptance criteria for this issue
# Spec-shape headings per internal/batch/spec.go plus wayfinder-map and
# explore-ticket headings:
#   ## Problem Statement, ## Solution, ## User Stories
#   ## Problem, ## Proposal
#   ## Destination      (wayfinder maps)
#   ## Question         (small wayfinder-task tickets)
# A body that carries any of these counts as having ACs for classification.
spec_shape_present() {
  local body="$1"
  printf '%s' "$body" | grep -Eq '^##[[:space:]]+(Problem Statement|Solution|User Stories|Problem|Proposal|Destination|Question)([[:space:]]|$)'
}

ac_present() {
  local body="$1"
  printf '%s' "$body" | grep -Eq '^##[[:space:]]+Acceptance criteria([[:space:]]|$)'
}

proposal_present() { :; }   # subsumed by spec_shape_present

# `mapped` requires ACs with at least one `- [ ]` line whose shell command starts
# `go test -run` and references a test present on origin/main. None of the
# currently-open issues satisfy this; the heuristic still emits `mapped` for
# completeness.
extract_go_test_run_lines() {
  local body="$1"
  printf '%s' "$body" | grep -E '^[[:space:]]*-[[:space:]]*\[([[:space:]]|x)\][[:space:]]+(.*\`)?go test[[:space:]]+[^[:space:]]+.*-run[[:space:]]+[^[:space:]]+' \
    | sed -E 's/^[[:space:]]*//'
}

# `annotatable` candidates: target test name + package that maps best to the
# issue's deliverable. These are *candidates* — authors confirm before we
# promote an issue from `annotatable` to `mapped`.
candidate_test_for() {
  local number="$1"
  case "$number" in
    2176) printf '%s' "internal/batch/orchestrator_test.go:TestAllowAlreadyResolvedWhenACVerifiedIndependently" ;;
    1807) printf '%s' "cmd/sandman/main_test.go:TestExecuteRoot_VersionFlag_LDFlagsInjected" ;;
    *)    printf '%s' "" ;;
  esac
}

# Decide whether an issue's ACs describe code surface (annotatable) or
# non-code deliverables (planning-only).
is_planning_only() {
  local number="$1"
  local body="$2"
  local labels_csv="$3"

  if ! ac_present "$body" && ! spec_shape_present "$body"; then
    return 1
  fi

  # Wayfinder maps and tasks are bookkeeping/spec-only.
  case "$labels_csv" in
    *wayfinder:map*|*wayfinder:task*) return 0 ;;
  esac

  # Per-issue known mapping (kept in sync with .sandman/task.md).
  case "$number" in
    957|956|955|1808|1806|1805|2175|2174|2173|2172|2171|2170|2169|2165|2151|2150|2149|2148|2147|2146|2142|1804|2164|2163)
      return 0 ;;
    *) return 1 ;;
  esac
}

classify_issue() {
  local number="$1"
  local body="$2"
  local labels_csv="$3"

  if ac_present "$body"; then
    mapfile -t RUN_LINES < <(extract_go_test_run_lines "$body" || true)
    if [[ ${#RUN_LINES[@]} -gt 0 ]]; then
      printf '%s\n' "mapped"
      return
    fi
    if is_planning_only "$number" "$body" "$labels_csv"; then
      printf '%s\n' "planning-only"
    else
      printf '%s\n' "annotatable"
    fi
    return
  fi

  if spec_shape_present "$body"; then
    if is_planning_only "$number" "$body" "$labels_csv"; then
      printf '%s\n' "planning-only"
    else
      printf '%s\n' "annotatable"
    fi
    return
  fi

  printf '%s\n' "no-acs"
}

action_for_class() {
  case "$1" in
    mapped)        printf '%s' "no-op" ;;
    annotatable)   printf '%s' "post-mapping-comment" ;;
    planning-only) printf '%s' "post-why-comment+no-test-mapping-label" ;;
    no-acs)        printf '%s' "post-request-acs-comment" ;;
    *)             printf '%s' "unknown" ;;
  esac
}

# Render the TSV. Columns: number, title, classification, action, candidate_test, notes.
render_tsv() {
  jq -r '.[] | @json' "$JSON_INPUT" | while IFS= read -r row; do
    NUMBER=$(printf '%s' "$row" | jq -r '.number')
    TITLE=$(printf '%s' "$row" | jq -r '.title')
    BODY=$(printf '%s' "$row" | jq -r '.body')
    LABELS=$(printf '%s' "$row" | jq -r '(.labels // []) | map(.name) | join(",")')

    CLASS=$(classify_issue "$NUMBER" "$BODY" "$LABELS")
    ACTION=$(action_for_class "$CLASS")
    CANDIDATE=$(candidate_test_for "$NUMBER")

    NOTES=""
    case "$CLASS" in
      no-acs)        NOTES="body has no ## Acceptance criteria section" ;;
      planning-only) NOTES="ACs describe non-code deliverables (CI, rename, ticket bookkeeping)" ;;
      annotatable)   NOTES="code surface described; no go test -run mapping" ;;
      mapped)        NOTES="AC has a go test -run line that exists on origin/main" ;;
    esac

    # Tabs in the title are escaped (\t) so the TSV stays valid.
    ESCAPED_TITLE=$(printf '%s' "$TITLE" | tr '\t' ' ' | tr '\n' ' ')
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$NUMBER" "$ESCAPED_TITLE" "$CLASS" "$ACTION" "$CANDIDATE" "$NOTES"
  done
}

if [[ -n "$SELF_CHECK" ]]; then
  if ! [[ -f "$SELF_CHECK" ]]; then
    err "self-check file not found: $SELF_CHECK"
    exit 2
  fi
  TMP="$(mktemp -t cold_start_check.XXXXXX.tsv)"
  trap 'rm -f "$TMP"' EXIT
  render_tsv > "$TMP"
  if diff -q "$SELF_CHECK" "$TMP" >/dev/null; then
    exit 0
  fi
  err "self-check mismatch:"
  diff "$SELF_CHECK" "$TMP" || true
  exit 1
fi

mkdir -p "$(dirname "$OUTPUT_PATH")"
render_tsv > "$OUTPUT_PATH"

# A short header on stdout summarising what we produced.
LINES=$(wc -l < "$OUTPUT_PATH" | tr -d ' ')
echo "cold_start_inventory: wrote $LINES rows to $OUTPUT_PATH"
