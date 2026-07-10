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

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
TESTDIR="${ROOT}/internal/cmd"

VALID_PRESETS="generic go node dotnet elixir rust java ruby python"

usage() {
    echo "Usage: $0 [preset...]

Valid presets: ${VALID_PRESETS}
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

exec go test -tags e2e "${TESTDIR}" -run "${filter}" -count=1
