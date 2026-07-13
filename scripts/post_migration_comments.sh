#!/usr/bin/env bash
# post_migration_comments.sh -- idempotently post per-issue comments for slice 8.
#
# Reads scripts/cold_start_inventory.tsv and posts one comment per non-mapped
# issue. The comment body is selected by classification:
#   planning-only: explains why no test mapping is possible.
#   annotatable:   proposes the candidate test mapping.
#   no-acs:        requests ## Acceptance criteria.
# Re-runs are safe: each comment carries an idempotency marker
# (`<!-- cold-start-migration: <DATE> -->`) and the script skips an issue
# whose existing comments already include that marker.
#
# Exits 0 if every applicable comment was posted or was already present.
set -euo pipefail

TSV="${TSV:-scripts/cold_start_inventory.tsv}"
RUN_DATE="${RUN_DATE:-$(date -u +"%Y-%m-%d")}"
MARKER="<!-- cold-start-migration: ${RUN_DATE} -->"
LINK_REPORT="${LINK_REPORT:-docs/agents/cold-start-migration-report.md}"
REPO_FLAG="${REPO_FLAG:--R rafaelromao/sandman}"

if [[ ! -f "$TSV" ]]; then
  echo "post_migration_comments: $TSV not found" >&2
  exit 2
fi

require_bin() {
  for b in "$@"; do
    if ! command -v "$b" >/dev/null 2>&1; then
      echo "post_migration_comments: missing required binary: $b" >&2
      exit 3
    fi
  done
}

require_bin gh jq

post_comment() {
  local number="$1"
  local body="$2"

  local existing
  existing="$(gh issue view "$number" $REPO_FLAG --json comments --jq '.comments[] | .body' 2>/dev/null || true)"
  if printf '%s' "$existing" | grep -Fq "$MARKER"; then
    echo "post_migration_comments: #$number already has the marker, skipping"
    return 0
  fi

  if gh issue comment "$number" $REPO_FLAG --body "$body" >/dev/null 2>&1; then
    echo "post_migration_comments: commented on #$number"
    return 0
  fi
  echo "post_migration_comments: failed to comment on #$number" >&2
  return 1
}

posted=0
skipped=0
while IFS=$'\t' read -r NUMBER TITLE CLASS ACTION CANDIDATE NOTES; do
  [[ -z "$NUMBER" || "$NUMBER" == "number" ]] && continue  # header
  case "$CLASS" in
    mapped)
      echo "post_migration_comments: #$NUMBER is mapped, no comment needed"
      skipped=$((skipped+1)) ;;
    planning-only)
      body="Planning-only classification (slice 8 of the verify-then-close spec, see #2176).

The issue's ACs describe deliverables that are not Go-testable (CI tooling, ticket bookkeeping, design rationale, or markdown-only changes); there is no test mapping that T1's verifier can decide, so the conservative guard remains the backstop. The no-test-mapping label has been applied.

See the migration report at $LINK_REPORT for the full per-issue classification table.

$MARKER"
      post_comment "$NUMBER" "$body" && posted=$((posted+1)) || true ;;
    annotatable)
      candidate="$CANDIDATE"
      [[ -z "$candidate" ]] && candidate="(no candidate auto-identified; please propose a target test in your reply)"
      body="Annotatable classification (slice 8 of the verify-then-close spec, see #2176).

The issue's ACs describe code surface, but the existing ## Acceptance criteria does not carry a go test -run line that T1's verifier can map to a test on origin/main.

Candidate test mapping (please confirm or correct by editing the ACs):

- Candidate: $candidate

If you confirm, promote the AC to a Mapped AC by replacing the matching - [ ] line with a go test -run shell line that names the test (e.g. - [ ] go test ./internal/... -run TestFoo).

See the migration report at $LINK_REPORT for the full per-issue classification table.

$MARKER"
      post_comment "$NUMBER" "$body" && posted=$((posted+1)) || true ;;
    no-acs)
      body="No-ACs classification (slice 8 of the verify-then-close spec, see #2176).

The body has no ## Acceptance criteria section (or any of the Specification-shape equivalents). Please add one -- either a standard ## Acceptance criteria block with concrete - [ ] items, or the Specification shape (## Problem Statement, ## Solution, ## User Stories) -- so T1's verifier can decide the run on origin/main.

See the migration report at $LINK_REPORT for the full per-issue classification table.

$MARKER"
      post_comment "$NUMBER" "$body" && posted=$((posted+1)) || true ;;
  esac
done < "$TSV"

echo "post_migration_comments: posted=$posted skipped=$skipped"
