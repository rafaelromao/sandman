#!/usr/bin/env bash
# Each tier owns its process tree; GNU timeout sends TERM then KILL to the group.
set -euo pipefail
selector() {
  python3 - "$1" <<'PYSELECT'
import json, sys
names = sorted({scenario['test'].split('/')[0] for scenario in json.load(open('scripts/release_scenarios.json'))['scenarios'] if scenario['tier'] in sys.argv[1].split(',')})
print('^(' + '|'.join(names) + ')$')
PYSELECT
}
live_pattern=$(selector live)
provider_pattern=$(selector live,claude)
claude_pattern=$(selector claude)
smoke_pattern=$(selector smoke)
case "$RELEASE_TIER" in
  unit) command=(go test -race -tags visual -json -count=1 -timeout 25m ./...); budget=30m ;;
  smoke) export SANDMAN_RUN_SMOKE_E2E=1 SANDMAN_SMOKE_PREFETCH=1
         command=(go test -tags smoke -json -count=1 -timeout 60m ./internal/cmd -run "$smoke_pattern"); budget=65m ;;
  integration) export SANDMAN_RUN_AGENT_E2E=1
               command=(go test -tags e2e -json -count=1 -timeout 90m ./... -skip "$provider_pattern"); budget=95m ;;
  live) export SANDMAN_RUN_AGENT_E2E=1 SANDMAN_RUN_SMOKE_E2E=1
        command=(go test -tags e2e -json -count=1 -timeout 90m ./internal/cmd -run "$live_pattern"); budget=95m ;;
  claude) export SANDMAN_RUN_AGENT_E2E=1 SANDMAN_RUN_SMOKE_E2E=1
          command=(go test -tags e2e -json -count=1 -timeout 90m ./internal/cmd -run "$claude_pattern"); budget=95m ;;
  *) exit 2 ;;
esac
set +e
timeout --verbose --signal=TERM --kill-after=30s "$budget" "${command[@]}" 2>&1 | tee regression-artifacts/events.jsonl
rc=${PIPESTATUS[0]}
set -e
printf '%s\n' "$rc" > regression-artifacts/exit-code
exit "$rc"
