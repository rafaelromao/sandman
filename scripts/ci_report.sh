#!/usr/bin/env bash
# Preserve the validation command's exit status while retaining raw and structured output.
set -uo pipefail
category="${1:?category required}"
shift
mkdir -p reports
start=$SECONDS
"$@" 2>&1 | tee "reports/$category.log"
rc=${PIPESTATUS[0]}
python3 scripts/ci_report.py "$category" "$rc" "$((SECONDS-start))"
report_rc=$?
if [ "$rc" -ne 0 ]; then exit "$rc"; fi
exit "$report_rc"
