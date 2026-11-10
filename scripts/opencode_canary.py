#!/usr/bin/env python3
"""One bounded, tool-free request to detect provider rejection of our CLI pin."""
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile

TIMEOUT = 60
PIN_GUIDANCE = ('The provider rejected the OpenCode pin as obsolete. Update the OpenCode version '
                'in internal/scaffold/scaffolder.go, scripts/release_scenarios.json, and '
                '.github/workflows/release-regression-tier.yml (installation and version check), '
                'then rerun CI. Keep the pins consistent; do not disable this check.')


# OpenCode retries rate limits, overload and dropped connections itself and
# prints nothing on stdout while it does, so a stalled request looks the same
# whatever the cause. Its own log file still records each failed attempt.
STREAM_ERROR = re.compile(r'message="stream error"(?:.*?modelID=(\S+))?.*?error\.error="((?:[^"\\]|\\.)*)"')
LOG_TAIL_BYTES = 1 << 20


def read_cli_logs(data_home):
    text = []
    for path in sorted(Path(data_home, 'opencode', 'log').glob('*.log')):
        try:
            with open(path, 'rb') as handle:
                handle.seek(0, os.SEEK_END)
                handle.seek(max(0, handle.tell() - LOG_TAIL_BYTES))
                text.append(handle.read().decode('utf-8', 'replace'))
        except OSError:
            continue
    return '\n'.join(text)


def stall_detail(log_text, secret=''):
    """Say why a request that never completed kept failing.

    Only the provider's error text is reported, never raw log lines, and never
    the authentication key.
    """
    messages = []
    attempts = 0
    for line in log_text.splitlines():
        found = STREAM_ERROR.search(line)
        if not found:
            continue
        attempts += 1
        message = f'{found.group(1) or "model"}: {found.group(2)}'
        if secret:
            message = message.replace(secret, '[redacted]')
        message = message[:240]
        if message not in messages:
            messages.append(message)
    if not attempts:
        return (' OpenCode logged no provider error, so the request produced no response '
                '(connection or response stall).')
    return f' OpenCode logged {attempts} failed attempt(s) while retrying: ' + '; '.join(messages[:3])


def workflow_message(text):
    return text.replace('%', '%25').replace('\r', '%0D').replace('\n', '%0A')


def evaluate(output, returncode):
    lower = output.lower()
    if any(marker in lower for marker in ('required to use the free tier', 'deprecated',
                                          'no longer supported', 'upgrade opencode')):
        return PIN_GUIDANCE
    text = []
    errors = False
    provider_message = ""
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        errors |= event.get('type') == 'error'
        if event.get('type') == 'error':
            provider_message = str(event.get('error', {}).get('data', {}).get('message', ''))
        if event.get('type') == 'text':
            text.append(event.get('part', {}).get('text', ''))
    if returncode or errors or not ''.join(text).strip():
        secret = os.environ.get('OPENCODE_API_KEY', '')
        if secret:
            provider_message = provider_message.replace(secret, '[redacted]')
        detail = f' Exit code: {returncode}; error event: {errors}; response characters: {len("".join(text).strip())}.'
        if provider_message:
            detail += ' Provider: ' + provider_message[:400]
        return 'OpenCode provider canary failed. Check provider availability/authentication; if the CLI pin is deprecated, update the pins rather than bypassing this check.' + detail
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
                   XDG_DATA_HOME=directory + '/data', XDG_CACHE_HOME=directory + '/cache')
        # Keep OpenCode's normal tool schema: disabling tools can itself
        # trigger the provider's client-validation rejection. The prompt asks
        # for one literal response without calling tools.
        env.pop('OPENCODE_PERMISSION', None)
        # Request only the release model. Left alone, OpenCode asks its default
        # small model (a different, paid model) to title the session; --title
        # below spares that request, and pinning the small model to the release
        # model guarantees nothing else can be asked for.
        env['OPENCODE_CONFIG_CONTENT'] = json.dumps({'small_model': manifest['model']})
        token = env.get('OPENCODE_API_KEY', '')
        if token:
            auth = Path(env['XDG_DATA_HOME']) / 'opencode/auth.json'
            auth.parent.mkdir(parents=True)
            auth.write_text(json.dumps({'opencode': {'type': 'api', 'key': token}}))
            auth.chmod(0o600)
        command = ['opencode', 'run', '--format', 'json', '-m', manifest['model'],
                   '--title', 'sandman-canary', 'Reply with exactly OK. Do not use any tools.']
        process = subprocess.Popen(command, cwd=directory, env=env, stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT, text=True, start_new_session=True)
        try:
            output, _ = process.communicate(timeout=TIMEOUT)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate()
            detail = stall_detail(read_cli_logs(env['XDG_DATA_HOME']), token)
            print('::error::' + workflow_message(
                f'OpenCode provider canary exceeded its {TIMEOUT}-second deadline.' + detail))
            return 1
        error = evaluate(output, process.returncode)
        # Never print raw provider output, which could contain credential details.
        if error:
            print('::error::' + workflow_message(error))
            return 1
    print('OpenCode provider canary passed (one request).')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
