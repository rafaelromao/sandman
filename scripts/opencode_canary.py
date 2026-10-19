#!/usr/bin/env python3
"""One bounded, tool-free request to detect provider rejection of our CLI pin."""
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile

TIMEOUT = 60
PIN_GUIDANCE = ('The provider rejected the OpenCode pin as obsolete. Update the OpenCode version '
                'in internal/scaffold/scaffolder.go, scripts/release_scenarios.json, and '
                '.github/workflows/release-regression-tier.yml (installation and version check), '
                'then rerun CI. Keep the pins consistent; do not disable this check.')


def evaluate(output, returncode):
    lower = output.lower()
    if any(marker in lower for marker in ('required to use the free tier', 'deprecated',
                                          'no longer supported', 'upgrade opencode',
                                          'free tier can only be used from within opencode')):
        return PIN_GUIDANCE
    text = []
    errors = False
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        errors |= event.get('type') == 'error'
        if event.get('type') == 'text':
            text.append(event.get('part', {}).get('text', ''))
    if returncode or errors or ''.join(text).strip() != 'OK':
        return 'OpenCode provider canary failed. Check provider availability/authentication; if the CLI pin is deprecated, update the pins rather than bypassing this check.'
    return None


def main():
    if not os.environ.get('OPENCODE_API_KEY'):
        print('::error::OpenCode provider canary requires the existing OPENCODE_API_KEY repository secret.')
        return 1
    manifest = json.loads(Path('scripts/release_scenarios.json').read_text())['providers']['opencode']
    version = subprocess.run(['opencode', '--version'], capture_output=True, text=True, timeout=10)
    if version.returncode or version.stdout.strip() != manifest['cli']:
        print('::error::Installed OpenCode must match the release manifest CLI pin.')
        return 1
    with tempfile.TemporaryDirectory(prefix='sandman-opencode-canary-') as directory:
        env = dict(os.environ, HOME=directory, XDG_CONFIG_HOME=directory + '/config',
                   XDG_DATA_HOME=directory + '/data', XDG_CACHE_HOME=directory + '/cache',
                   OPENCODE_PERMISSION='{"*":"deny"}')
        token = env.get('OPENCODE_API_KEY', '')
        if token:
            auth = Path(env['XDG_DATA_HOME']) / 'opencode/auth.json'
            auth.parent.mkdir(parents=True)
            auth.write_text(json.dumps({'opencode': {'type': 'api', 'key': token}}))
            auth.chmod(0o600)
        command = ['opencode', 'run', '--pure', '--format', 'json', '-m', manifest['model'],
                   'Reply with exactly OK. Do not use any tools.']
        process = subprocess.Popen(command, cwd=directory, env=env, stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT, text=True, start_new_session=True)
        try:
            output, _ = process.communicate(timeout=TIMEOUT)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate()
            print('::error::OpenCode provider canary exceeded its 60-second deadline.')
            return 1
        error = evaluate(output, process.returncode)
        # Never print raw provider output, which could contain credential details.
        if error:
            print('::error::' + error)
            return 1
    print('OpenCode provider canary passed (one request).')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
