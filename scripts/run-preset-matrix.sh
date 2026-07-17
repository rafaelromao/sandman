#!/usr/bin/env bash
# run-preset-matrix.sh
#
# Runs the preset-matrix e2e suite filtered to one or more presets.
# Each preset's tests (Scaffolds, RunExecutesRealTask, RunWithEditedDockerfile)
# are grouped under a single `-run` filter.
#
# Usage:
#   scripts/run-preset-matrix.sh go           # single preset
#   scripts/run-preset-matrix.sh go node     # multiple presets
#   scripts/run-preset-matrix.sh             # all presets
#
# The preset names are the same as those accepted by `sandman init --build-tools`.
#
# The real-agent sub-tests (*RunExecutesRealTask) need a live opencode
# install with auth; without SANDMAN_RUN_AGENT_E2E=1 they skip cleanly and
# only the build-only tests run.

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
TESTDIR="${ROOT}/internal/cmd"

VALID_PRESETS="generic go node dotnet elixir rust java ruby python"

# Per-preset podman builds dominate the wall time. 90m covers the full
# 9-preset matrix with comfortable headroom for slower hosts.
DEFAULT_TIMEOUT="90m"

usage() {
    echo "Usage: $0 [preset...]

Valid presets: ${VALID_PRESETS}

Env vars:
  SANDMAN_TEST_TIMEOUT   Per-test timeout passed to go test as -timeout.
                         Defaults to ${DEFAULT_TIMEOUT}.
  SANDMAN_RUN_AGENT_E2E  Set to 1 to opt the real-agent sub-tests in.
" >&2
    exit 1
}

validate_preset() {
    local preset="$1"
    if [[ ! " ${VALID_PRESETS} " == *" ${preset} "* ]]; then
        echo "Error: unknown preset '${preset}'" >&2
        usage
    fi
}

build_filter() {
    local presets=("$@")
    local filters=()
    for preset in "${presets[@]}"; do
        filters+=("TestPresetMatrixHarness_$(tr '[:lower:]' '[:upper:]' <<< "${preset:0:1}")${preset:1}")
    done
    local joined
    joined=$(IFS='|'; echo "${filters[*]}")
    echo "^(${joined})"
}

if [[ $# -eq 0 ]]; then
    presets=($VALID_PRESETS)
else
    for preset in "$@"; do
        validate_preset "$preset"
    done
    presets=("$@")
fi

filter=$(build_filter "${presets[@]}")
timeout="${SANDMAN_TEST_TIMEOUT:-${DEFAULT_TIMEOUT}}"

exec go test -tags e2e -timeout "${timeout}" "${TESTDIR}" -run "${filter}" -count=1
