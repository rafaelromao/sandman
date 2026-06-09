#!/usr/bin/env bash
set -euo pipefail

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "Error: not inside a git repository" >&2
    exit 1
fi

git worktree list --porcelain | awk '
/^worktree / { path = substr($0, 10); next }
/^branch /  { branch = substr($0, 8); next }
/^detached$/ { detached = 1; next }
/^prunable/  { prunable = 1; next }
/^$/ {
    if (path != "" && !prunable && path ~ /\.sandman\/worktrees\/sandman\//) {
        split(path, parts, "/")
        dirname = parts[length(parts)]
        if (dirname ~ /^[0-9]+-/) {
            expected = "refs/heads/sandman/" dirname
            if (detached || (branch != "" && branch != expected)) {
                actual = (detached ? "detached HEAD" : branch)
                print "Worktree " path " is on " actual ", expected " expected ". Run: git -C " path " checkout -f sandman/" dirname
            }
        }
    }
    path = ""; branch = ""; detached = 0; prunable = 0; next
}
'
