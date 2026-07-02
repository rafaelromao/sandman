#!/usr/bin/env bash
# cleanup-orphaned-batches.sh
#
# Removes orphaned test batch directories under <repo>/.sandman/batches/.
# A batch directory is orphaned when it has a batch.json manifest, no
# run.started event in events.jsonl references it, and no live daemon
# socket is bound to it.
#
# Usage:
#   scripts/cleanup-orphaned-batches.sh [--apply] [--sandman-dir PATH]
#
# Default mode is --dry-run, which only prints what would be removed.
# Pass --apply to actually delete orphaned directories.
#
# Issue: #1632.

set -u

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
SANDMAN_DIR="${REPO_ROOT}/.sandman"
APPLY=0

while [ $# -gt 0 ]; do
  case "$1" in
    --apply)
      APPLY=1
      shift
      ;;
    --sandman-dir=*)
      SANDMAN_DIR="${1#--sandman-dir=}"
      shift
      ;;
    --sandman-dir)
      shift
      SANDMAN_DIR="${1:-}"
      shift
      ;;
    --dry-run)
      APPLY=0
      shift
      ;;
    -h|--help)
      sed -n '3,16p' "$0"
      exit 0
      ;;
    *)
      echo "cleanup-orphaned-batches.sh: unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

if [ "$APPLY" -eq 1 ]; then
  MODE_FLAG="--apply"
else
  MODE_FLAG="--dry-run"
fi

(cd "$REPO_ROOT" && go run ./cmd/cleanup-orphaned-batches "$MODE_FLAG" --sandman-dir "$SANDMAN_DIR")