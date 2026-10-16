#!/usr/bin/env bash
set -euo pipefail
# Never stream scanner output: even diagnostics can contain source fragments.
# Only status is public; temporary raw reports are removed on every exit.
scan_dir=$(mktemp -d)
trap 'rm -rf "$scan_dir"' EXIT
python3 scripts/security_policy.py "$scan_dir/ignore"
status=0
if ! govulncheck ./... >"$scan_dir/vulnerabilities" 2>&1; then
  echo '::error::Go vulnerability scan failed or found reachable vulnerabilities. Reproduce with make security-check.'
  status=1
fi
if ! gitleaks git --config .gitleaks.toml --gitleaks-ignore-path "$scan_dir/ignore" --redact=100 --no-banner --log-opts=--all >"$scan_dir/secrets" 2>&1; then
  echo '::error::Secret scan failed or found committed secrets. Reproduce locally with gitleaks git --redact=100.'
  status=1
fi
exit "$status"
