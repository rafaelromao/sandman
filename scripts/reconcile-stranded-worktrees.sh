#!/usr/bin/env bash
set -euo pipefail

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "Error: not inside a git repository" >&2
    exit 1
fi

repo_root=$(git rev-parse --show-toplevel)
worktree_dir_default=".sandman/worktrees"

# Read worktree_dir from .sandman/config.yaml, fall back to default
worktree_dir=$worktree_dir_default
if [[ -f "$repo_root/.sandman/config.yaml" ]]; then
    cfg_val=$(awk -F': ' '/^worktree_dir:/ {print $2; exit}' "$repo_root/.sandman/config.yaml")
    if [[ -n "$cfg_val" ]]; then
        worktree_dir=$cfg_val
    fi
fi

git worktree list --porcelain | awk -v prefix="$repo_root/$worktree_dir/" '
/^worktree / { path = substr($0, 10); next }
/^branch /  { branch = substr($0, 8); next }
/^detached$/ { detached = 1; next }
/^prunable/  { prunable = 1; next }
/^$/ {
    if (path != "" && !prunable && index(path, prefix) == 1) {
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
