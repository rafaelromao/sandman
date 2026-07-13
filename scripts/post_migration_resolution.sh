#!/usr/bin/env bash
# post_migration_resolution.sh -- idempotently post the migration resolution
# comment on issue #2176 (the slice-8 ticket), naming the follow-on
# T3-retirement ticket.
#
# Usage: post_migration_resolution.sh <migration-issue> <retirement-issue>
# or:  MIGRATION_TICKET=... RETIREMENT_TICKET=... post_migration_resolution.sh
#
# Idempotent: if the cold-start-migration marker is already present in any
# of the issue's existing comments, the script exits 0 without re-posting.
set -euo pipefail

ISSUE="${1:-${MIGRATION_TICKET:-2176}}"
RETIREMENT_NUM="${2:-${RETIREMENT_TICKET:-}}"

if [[ -z "$RETIREMENT_NUM" ]]; then
  echo "post_migration_resolution: usage: $0 <migration-issue> <t3-retirement-issue>" >&2
  exit 2
fi

REPO_FLAG="${REPO_FLAG:--R rafaelromao/sandman}"
RUN_DATE="${RUN_DATE:-$(date -u +"%Y-%m-%d")}"
MARKER="<!-- cold-start-migration: ${RUN_DATE} -->"

require_bin() {
  for b in "$@"; do
    if ! command -v "$b" >/dev/null 2>&1; then
      echo "post_migration_resolution: missing required binary: $b" >&2
      exit 3
    fi
  done
}
require_bin gh

existing="$(gh issue view "$ISSUE" $REPO_FLAG --json comments --jq '.comments[] | .body' 2>/dev/null || true)"
if printf '%s' "$existing" | grep -Fq "$MARKER"; then
  echo "post_migration_resolution: #$ISSUE already carries the marker, skipping"
  exit 0
fi

planning_count="$(awk -F'\t' '$3 == "planning-only" { print $1 }' scripts/cold_start_inventory.tsv 2>/dev/null | wc -l | tr -d ' ')"

body="Cold-start migration sweep complete. The migration report at docs/agents/cold-start-migration-report.md records the per-issue classification:

- ${planning_count} issues classified planning-only and labelled no-test-mapping
- 0 issues classified annotatable
- 0 issues classified mapped (no go test -run line exists in any open issue's AC)
- 0 issues classified no-acs (every open issue carries some spec/AC-shaped content once the Specification shape and Wayfinder shape are recognised)

Follow-on T3-retirement ticket: #${RETIREMENT_NUM}. Once that ticket lands, T3's parser+verifier code and tests can be deleted cleanly. The conservative Layer-1 guard remains as the documented backstop.

The migration artefacts committed alongside this resolution are:

- scripts/cold_start_inventory.sh (classification driver; supports --json-input and --self-check so the script is testable offline without a gh round-trip)
- scripts/cold_start_inventory.tsv (the committed classification snapshot, regression fixture for --self-check)
- scripts/post_migration_comments.sh (idempotent comment-poster; uses an HTML idempotency marker per comment)
- scripts/apply_no_test_mapping_label.sh (creates the no-test-mapping label and applies it to every planning-only issue)
- scripts/post_migration_resolution.sh (idempotent resolution-comment poster)
- docs/agents/cold-start-migration-report.md (this report)

$MARKER"

gh issue comment "$ISSUE" $REPO_FLAG --body "$body" >/dev/null
echo "post_migration_resolution: commented on #$ISSUE naming #$RETIREMENT_NUM"
