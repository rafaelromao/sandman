#!/usr/bin/env bash
set -euo pipefail
# Actionlint also checks embedded shell via the pinned ShellCheck on PATH.
actionlint
# Include every tracked shell script, including distributed skill scripts.
git ls-files -z '*.sh' | xargs -0 shellcheck --severity=warning
