#!/usr/bin/env bash
set -euo pipefail

if ! command -v git >/dev/null 2>&1; then
    echo "Error: git is not installed or not on PATH" >&2
    exit 1
fi

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "Error: not inside a git repository" >&2
    exit 1
fi

# Resolve repo root from git-common-dir (works from linked worktrees too)
git_common_dir=$(git rev-parse --git-common-dir)
case "$git_common_dir" in
  /*) repo_root=$(dirname "$git_common_dir") ;;
  *)  repo_root=$(git rev-parse --show-toplevel) ;;
esac

worktree_dir_default=".sandman/worktrees"

# Read worktree_dir from .sandman/config.yaml, fall back to default
worktree_dir=$worktree_dir_default
if [[ -f "$repo_root/.sandman/config.yaml" ]]; then
    cfg_val=$(awk -F': ' '/^worktree_dir:/ {print $2; exit}' "$repo_root/.sandman/config.yaml")
    if [[ -n "$cfg_val" ]]; then
        # Strip surrounding quotes and trailing slashes
        cfg_val="${cfg_val%\"}"
        cfg_val="${cfg_val#\"}"
        cfg_val="${cfg_val%\'}"
        cfg_val="${cfg_val#\'}"
        worktree_dir="${cfg_val%%/}"
    fi
fi

# Resolve worktree_dir prefix
case "$worktree_dir" in
  /*) worktree_prefix="$worktree_dir" ;;
  ~/*) worktree_prefix="$HOME/${worktree_dir#~/}" ;;
  *)  worktree_prefix="$repo_root/$worktree_dir" ;;
esac
worktree_prefix="${worktree_prefix%%/}/"

git worktree list --porcelain | awk -v prefix="$worktree_prefix" '
/^worktree / { path = substr($0, 10); next }
/^branch /  { branch = substr($0, 8); next }
/^detached$/ { detached = 1; next }
/^prunable/  { prunable = 1; next }
/^$/ {
    if (path != "" && !prunable && index(path, prefix) == 1) {
        split(path, parts, "/")
        dirname = parts[length(parts)]
        # Dirname pattern matches Sandman issue-driven branches (CONTEXT.md Branch)
        if (dirname ~ /^[0-9]+-/) {
            expected = "refs/heads/sandman/" dirname
            if (detached || (branch != "" && branch != expected)) {
                actual = (detached ? "detached HEAD" : branch)
                check = "git rev-parse --verify --quiet " expected " >/dev/null 2>&1"
                missing = (system(check) != 0)
                if (missing) {
                    print "Worktree " path " is on " actual ", expected " expected " (branch does not exist locally)"
                } else {
                    print "Worktree " path " is on " actual ", expected " expected ". Run: git -C " path " checkout -f sandman/" dirname
                }
            }
        }
    }
    path = ""; branch = ""; detached = 0; prunable = 0; next
}
'
