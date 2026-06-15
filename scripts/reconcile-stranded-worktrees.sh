#!/usr/bin/env bash
set -euo pipefail

# Thin wrapper around `sandman stranded` — see internal/sandbox/stranded.go
# and internal/cmd/stranded.go for the detection logic. The Go subcommand
# is the single source of truth for what counts as a stranded worktree.

if ! command -v git >/dev/null 2>&1; then
    echo "Error: git is not installed or not on PATH" >&2
    exit 1
fi

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "Error: not inside a git repository" >&2
    exit 1
fi

if command -v sandman >/dev/null 2>&1; then
    exec sandman stranded "$@"
fi

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
if [[ -x "$repo_root/sandman" ]]; then
    exec "$repo_root/sandman" stranded "$@"
fi

echo "Error: sandman binary not found; install it or add it to PATH" >&2
exit 1
