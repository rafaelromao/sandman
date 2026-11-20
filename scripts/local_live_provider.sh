#!/usr/bin/env bash
# Explicit operator-only validation. Never invoked by GitHub Actions.
set -euo pipefail

if [[ -n "${CI:-}" || -n "${GITHUB_ACTIONS:-}" ]]; then
  echo "Live provider validation is local-only." >&2
  exit 2
fi
if [[ $# != 1 ]]; then
  echo "Usage: bash scripts/local_live_provider.sh opencode|claude" >&2
  exit 2
fi

export SANDMAN_LOCAL_LIVE_PROVIDER=$1 SANDMAN_RUN_AGENT_E2E=1
case "$1" in
  opencode) test_name=TestPresetMatrixHarness_GenericRunExecutesRealTask ;;
  claude) test_name=TestPresetMatrixHarness_RealClaudeAgentReuseSession ;;
  *) echo "Unknown live provider: $1" >&2; exit 2 ;;
esac

command -v "$1" >/dev/null
command -v podman >/dev/null
pin=$(python3 -c 'import json,sys; print(json.load(open("scripts/release_scenarios.json"))["providers"][sys.argv[1]]["cli"])' "$1")
default_model=$(python3 -c 'import json,sys; print(json.load(open("scripts/release_scenarios.json"))["providers"][sys.argv[1]]["local_live_model"])' "$1")
version=$("$1" --version)
if [[ ${version%% *} != "$pin" ]]; then
  echo "Expected $1 $pin, got $version." >&2
  exit 2
fi
case "$1" in
  opencode) export SANDMAN_TEST_MODEL_OPENCODE=${SANDMAN_TEST_MODEL_OPENCODE:-$default_model} ;;
  claude) export SANDMAN_TEST_MODEL_CLAUDE=${SANDMAN_TEST_MODEL_CLAUDE:-$default_model} ;;
esac
echo "Local live-provider diagnostics: $1 (may use provider quota)"
go test -tags e2e -json -count=1 -timeout 90m ./internal/cmd -run "^$test_name$" |
  python3 -c '
import json, sys
name = sys.argv[1]
passed = False
for line in sys.stdin:
    try:
        event = json.loads(line)
    except ValueError:
        print(line, end="")
        continue
    if event.get("Action") == "output":
        print(event.get("Output", ""), end="", flush=True)
    if event.get("Action") == "pass" and event.get("Test") == name:
        passed = True
if not passed:
    sys.exit("The local provider scenario failed or did not execute.")
' "$test_name"
