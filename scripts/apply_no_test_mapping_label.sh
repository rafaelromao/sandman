#!/usr/bin/env bash
# apply_no_test_mapping_label.sh -- create (if missing) and apply the
# no-test-mapping label to every planning-only issue in the inventory.
#
# Idempotent: re-creating the label uses --force; gh issue edit --add-label
# is itself idempotent.
set -euo pipefail

TSV="${TSV:-scripts/cold_start_inventory.tsv}"
REPO_FLAG="${REPO_FLAG:--R rafaelromao/sandman}"
LABEL="${LABEL:-no-test-mapping}"
DESCRIPTION="${DESCRIPTION:-ACs are non-Go-testable (CI/ticket bookkeeping/markdown only). T1 cannot decide; backstop applies.}"
COLOR="${COLOR:-D93F0B}"

if [[ ! -f "$TSV" ]]; then
  echo "apply_no_test_mapping_label: $TSV not found" >&2
  exit 2
fi

require_bin() {
  for b in "$@"; do
    if ! command -v "$b" >/dev/null 2>&1; then
      echo "apply_no_test_mapping_label: missing required binary: $b" >&2
      exit 3
    fi
  done
}
require_bin gh

# Create the label if it does not already exist (idempotent via --force).
if ! gh label list $REPO_FLAG 2>/dev/null | awk '{ print $1 }' | grep -Fxq "$LABEL"; then
  gh label create "$LABEL" $REPO_FLAG --description "$DESCRIPTION" --color "$COLOR" --force >/dev/null
  echo "apply_no_test_mapping_label: created label $LABEL"
else
  echo "apply_no_test_mapping_label: label $LABEL already exists"
fi

applied=0
skipped=0
PLANNING=$(awk -F'\t' '$3 == "planning-only" { print $1 }' "$TSV")
for n in $PLANNING; do
  labels="$(gh issue view "$n" $REPO_FLAG --json labels --jq '.labels | map(.name) | join(",")' 2>/dev/null || true)"
  if printf '%s' "$labels" | grep -Fq "$LABEL"; then
    skipped=$((skipped+1))
    continue
  fi
  gh issue edit "$n" $REPO_FLAG --add-label "$LABEL" >/dev/null 2>&1 || {
    echo "apply_no_test_mapping_label: failed to label #$n" >&2
    continue
  }
  applied=$((applied+1))
done
echo "apply_no_test_mapping_label: applied=$applied skipped=$skipped"
