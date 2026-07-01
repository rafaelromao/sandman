#!/usr/bin/env bash
# check-review-docs.sh
#
# Dev-side mirror of internal/review/docguard_test.go. Runs the same
# forbidden-wording scan in shell so editors / pre-commit hooks can
# flag stale review-run-identity wording without spinning up a Go
# test binary. The authoritative guard lives in
# internal/review/docguard_test.go and runs under `go test ./...`.
#
# Exits 0 if no forbidden canonical-style wording is found, 1
# otherwise.
#
# Issue: #1552.

set -u

ROOT="${1:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"

fail=0

scan_md() {
  local file="$1"
  while IFS= read -r line; do
    line_no=$((line_no + 1))
    case "$line" in
      *runs/review*|*RunID:\"review\"*)
        lower=$(printf '%s' "$line" | tr '[:upper:]' '[:lower:]')
        case "$lower" in
          *legacy*|*alias*|*replaced*|*replaces*|*rejected*|*no\ longer*|*intentionally\ not*|*not\ consulted*|*not\ used*|*explicitly\ not*|*must\ not*|*is\ not\ canonical*|*must\ never*|*never\ return*|*never\ writes*|*negative\ check*)
            ;;
          *canonical*|*as\ the\ run\ folder*|*as\ the\ run\ directory*|*is\ the\ run\ folder*|*use\ this*|*use\ it*|*writes\ this*|*written\ as*)
            printf '%s:%d: forbidden canonical-style wording: %s\n' "${file#$ROOT/}" "$line_no" "$line" >&2
            fail=1
            ;;
        esac
        ;;
    esac
  done < "$file"
}

line_no=0
while IFS= read -r -d '' md; do
  case "$md" in
    "$ROOT"/docs/adr/*|*"docs/adr/"*)
      ;;
    *)
      line_no=0
      scan_md "$md"
      ;;
  esac
done < <(find "$ROOT/docs" -type f -name '*.md' -print0 2>/dev/null)

line_no=0
while IFS= read -r -d '' go; do
  case "$go" in
    *internal/review/*_test.go)
      case "$go" in
        *runid_test.go) ;;
        *) continue ;;
      esac
      ;;
  esac
  line_no=0
  scan_md "$go"
done < <(find "$ROOT/internal/review" -type f -name '*.go' -print0 2>/dev/null)

if [ "$fail" -ne 0 ]; then
  echo "docguard: forbidden canonical-style wording found" >&2
  exit 1
fi

echo "docguard: ok"
exit 0
