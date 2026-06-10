#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")/../../scripts" && pwd)/reconcile-stranded-worktrees.sh"
failures=0

pass()  { echo "PASS: $1"; }
fail()  { echo "FAIL: $1"; failures=$((failures + 1)); }

# 1. Non-git directory exits with error
out=$(cd /tmp && bash "$script" 2>&1 || true)
if echo "$out" | grep -q "not inside a git repository"; then
    pass "non-git cwd exits with error"
else
    fail "non-git cwd: expected error, got: $out"
fi

# 2. Script exists and is executable
if [[ -x "$script" ]]; then
    pass "script is executable"
else
    fail "script is not executable"
fi

# 3. Syntax check
if bash -n "$script"; then
    pass "bash syntax is valid"
else
    fail "bash syntax error"
fi

exit $failures
