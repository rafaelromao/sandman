#!/usr/bin/env bash
# Separate job results preserve each platform across partial reruns.
# Missing/unknown results fail closed, just like failures.
set -eu

require_success() {
  if [ "$2" != success ]; then
    echo "::error::$1 must succeed (result: ${2:-missing})" >&2
    exit 1
  fi
}

require_success static-analysis "${STATIC_RESULT:-}"
require_success contracts "${CONTRACT_RESULT:-}"
require_success build-linux "${LINUX_BUILD_RESULT:-}"
require_success build-macos "${MACOS_BUILD_RESULT:-}"
require_success portal-continuity-browser "${PORTAL_RESULT:-}"
require_success goreleaser "${PACKAGING_RESULT:-}"

# Only main pushes intentionally omit PR title validation.
if [ "${EVENT_NAME:-}" = push ] && [ "${TITLE_RESULT:-}" = skipped ]; then
  exit 0
fi
require_success semantic-pull-request "${TITLE_RESULT:-}"
