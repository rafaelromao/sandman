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
  --output FILE      write the TSV to FILE instead of the default path.
  --self-check FILE  reclassify the JSON input and compare against FILE as TSV.

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

# At slice-8 sweep time no open issue is annotatable; future annotatable
# issues would get a candidate-test entry here.
candidate_test_for() {
  local number="$1"
  case "$number" in
    *) printf '%s' "" ;;
  esac
}

# Decide whether an issue's ACs describe code surface (annotatable) or
# non-code deliverables (planning-only).
#
# Signals (in priority order, first match wins):
#   1. Wayfinder labels (wayfinder:map, wayfinder:task) - always planning-only.
#   2. CI / release / docs-only body regex - always planning-only.
#   3. Slice tickets whose title is "[slice N] ..." - always planning-only.
#   4. Spec / PRD / explore / wayfinder-map / T-retirement titles.
#
# When in doubt (no signal matches), default to annotatable.
PLANNING_RELEASE_CI_DOCS='(release-please|\.goreleaser\.yml|`goreleaser`|conventional commits|GitHub Releases|semantic versioning|`goreleaser-action`|workflow.* release|`pre-commit`|Ruleset on `main`|CHANGELOG\.md|DESIGN\.md|delete the four superseded ADR|renumber the surviving ADR|--version flag and Makefile|`go test -race -v ./internal/batch/`|`go test -race -v ./internal/sandbox/`|`gofmt -w\. \. && go vet`)'
SLICE_TITLE_RE='^\[slice [0-9]+\]'

is_planning_only() {
  local number="$1"
  local title="$2"
  local body="$3"
  local labels_csv="$4"

  if ! ac_present "$body" && ! spec_shape_present "$body"; then
    return 1
  fi

  case "$labels_csv" in
    *wayfinder:map*|*wayfinder:task*) return 0 ;;
  esac

  if printf '%s' "$body" | grep -Eq "$PLANNING_RELEASE_CI_DOCS"; then
    return 0
  fi

  if printf '%s' "$title" | grep -Eq "$SLICE_TITLE_RE"; then
    return 0
  fi

  if printf '%s' "$title" | grep -Eq 'spec: verify-then-close path|PRD: v1\.0 cleanup'; then
    return 0
  fi

  if printf '%s' "$title" | grep -Eq '^[Ee]xplore: '; then
    return 0
  fi

  if printf '%s' "$title" | grep -Eq '^\[wayfinder map\]'; then
    return 0
  fi

  if printf '%s' "$title" | grep -Eq '^\[T[1-9](_[a-zA-Z]+)? retirement\]'; then
    return 0
  fi

  return 1
}

classify_issue() {
  local number="$1"
  local title="$2"
  local body="$3"
  local labels_csv="$4"
  local run_lines

  if ac_present "$body"; then
    run_lines="$(extract_go_test_run_lines "$body" 2>/dev/null || true)"
    if [[ -n "$run_lines" ]]; then
      printf '%s\n' "mapped"
      return
    fi
    if is_planning_only "$number" "$title" "$body" "$labels_csv"; then
      printf '%s\n' "planning-only"
    else
      printf '%s\n' "annotatable"
    fi
    return
  fi

  if spec_shape_present "$body"; then
    if is_planning_only "$number" "$title" "$body" "$labels_csv"; then
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
    local NUMBER
    local TITLE
    local BODY
    local LABELS
    local CLASS
    local ACTION
    local CANDIDATE
    local NOTES
    local ESCAPED_TITLE
    NUMBER=$(printf '%s' "$row" | jq -r '.number')
    TITLE=$(printf '%s' "$row" | jq -r '.title')
    BODY=$(printf '%s' "$row" | jq -r '.body')
    LABELS=$(printf '%s' "$row" | jq -r '(.labels // []) | map(.name) | join(",")')

    CLASS=$(classify_issue "$NUMBER" "$TITLE" "$BODY" "$LABELS")
    ACTION=$(action_for_class "$CLASS")
    CANDIDATE=$(candidate_test_for "$NUMBER")

    NOTES=""
    case "$CLASS" in
      no-acs)        NOTES="body has no ## Acceptance criteria section" ;;
      planning-only) NOTES="ACs describe non-code deliverables (CI, rename, ticket bookkeeping)" ;;
      annotatable)   NOTES="code surface described; no go test -run mapping" ;;
      mapped)        NOTES="AC has a go test -run line that exists on origin/main" ;;
    esac

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

LINES=$(wc -l < "$OUTPUT_PATH" | tr -d ' ')
echo "cold_start_inventory: wrote $LINES rows to $OUTPUT_PATH"
