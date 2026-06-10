#!/usr/bin/env bash
set -u
failures=0

script="$(cd "$(dirname "$0")/../../scripts" && pwd)/reconcile-stranded-worktrees.sh"

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

# 4-7. Integration tests using a temp git repo
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# Init a bare repo to serve as the main repo
bare="$tmpdir/bare.git"
git init --bare "$bare" >/dev/null 2>&1

# Clone a working copy
main="$tmpdir/main"
git clone "$bare" "$main" >/dev/null 2>&1
cd "$main"
git config user.email test@test
git config user.name test
echo "hello" > README
git add README
git commit -m "init" >/dev/null 2>&1
git push origin main >/dev/null 2>&1

# Create a sandman config with default worktree_dir
sandman_dir="$main/.sandman"
mkdir -p "$sandman_dir"
cat > "$sandman_dir/config.yaml" <<'YAML'
agent: opencode
worktree_dir: .sandman/worktrees
YAML

# Create branches for worktrees
git checkout -b sandman/42-test-issue >/dev/null 2>&1
echo "fix" > fix.txt
git add fix.txt
git commit -m "fix" >/dev/null 2>&1
git push origin sandman/42-test-issue >/dev/null 2>&1
git checkout main >/dev/null 2>&1

git branch sandman/99-wrong-branch >/dev/null 2>&1
git branch sandman/77-detached >/dev/null 2>&1

# Create worktree dir structure
wt_dir="$main/.sandman/worktrees/sandman"
mkdir -p "$wt_dir"

# 4. Healthy worktree (on correct branch)
wt_healthy="$wt_dir/42-test-issue"
git worktree add "$wt_healthy" sandman/42-test-issue >/dev/null 2>&1

# 5. Wrong-branch worktree (dirname 99-wrong-branch but on sandman/42-test-issue)
wt_wrong="$wt_dir/99-wrong-branch"
git worktree add --force "$wt_wrong" sandman/42-test-issue >/dev/null 2>&1

# 6. Detached HEAD worktree
wt_detached="$wt_dir/77-detached"
git worktree add "$wt_detached" sandman/77-detached >/dev/null 2>&1
git -C "$wt_detached" checkout --detach HEAD >/dev/null 2>&1

cd "$main"

out=$(bash "$script" 2>&1 || true)

# 4. Healthy worktree should NOT appear in output
# Use leading path to avoid matching branch names in other entries
if echo "$out" | grep -q "/42-test-issue "; then
    fail "healthy worktree flagged as stranded"
else
    pass "healthy worktree not flagged"
fi

# 5. Wrong-branch worktree should be flagged
if echo "$out" | grep -q "/99-wrong-branch "; then
    pass "wrong-branch worktree flagged"
else
    fail "wrong-branch worktree not flagged. Output: $out"
fi

# 6. Detached HEAD worktree should be flagged
if echo "$out" | grep -q "/77-detached "; then
    pass "detached worktree flagged"
else
    fail "detached worktree not flagged. Output: $out"
fi

# 7. No false-positive BranchExists warnings (both wrong-branch branches exist)
if echo "$out" | grep -q "does not exist locally"; then
    fail "false positive: expected branch exists for test worktrees: $out"
else
    pass "no false positive BranchExists warnings"
fi

# 8. No ERROR output
if echo "$out" | grep -qi "error"; then
    fail "unexpected error in output: $out"
else
    pass "no error output"
fi

exit $failures
