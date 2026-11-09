import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import opencode_canary
from opencode_canary import evaluate, stall_detail, PIN_GUIDANCE


class CanaryTests(unittest.TestCase):
    def test_real_response_required(self):
        self.assertIsNone(evaluate('{"type":"text","part":{"text":"OK"}}', 0))
        self.assertIsNone(evaluate(json.dumps({'type': 'text', 'part': {'text': 'OK.'}}), 0))
        self.assertIsNotNone(evaluate(json.dumps({'type': 'text', 'part': {'text': '  '}}), 0))
        for output, code in [('', 0), ('{"type":"step_finish"}', 0),
                             ('{"type":"text","part":{"text":"OK"}}', 1),
                             ('{"type":"error","error":{"message":"offline"}}', 0)]:
            self.assertIsNotNone(evaluate(output, code))

    def test_deprecation_is_failure_even_when_cli_exits_zero(self):
        for message in ['OpenCode 1.18.0 or newer is required to use the free tier',
                        'This CLI version is deprecated', 'CLI no longer supported']:
            self.assertEqual(evaluate(message, 0), PIN_GUIDANCE)

    def test_provider_diagnostics_redact_the_authentication_key(self):
        with patch.dict(os.environ, {'OPENCODE_API_KEY': 'private-test-key'}):
            result = evaluate('{"type":"error","error":{"data":{"message":"Invalid key private-test-key"}}}', 0)
        self.assertIn('Invalid key [redacted]', result)
        self.assertNotIn('private-test-key', result)

    def test_client_configuration_error_is_not_assumed_to_be_deprecation(self):
        self.assertNotEqual(evaluate("OpenCode's free tier can only be used from within OpenCode", 1), PIN_GUIDANCE)


def stream_error(model, message):
    return ('timestamp=2026-11-09T22:14:07.512Z level=ERROR run=a26ac091 message="stream error" '
            f'providerID=opencode modelID={model} session.id=ses_x small=false agent=build '
            f'mode=primary error.error="{message}"')


class StallDiagnosticTests(unittest.TestCase):
    def test_reports_the_providers_error_for_a_stalled_request(self):
        log = '\n'.join(['INFO service=default starting',
                         stream_error('big-pickle', 'AI_APICallError: Rate limit exceeded'),
                         stream_error('big-pickle', 'AI_APICallError: Rate limit exceeded'),
                         stream_error('gpt-5.4-nano', 'AI_RetryError: Failed after 3 attempts')])
        detail = stall_detail(log)
        self.assertIn('3 failed attempt(s)', detail)
        self.assertIn('big-pickle: AI_APICallError: Rate limit exceeded', detail)
        self.assertEqual(detail.count('Rate limit exceeded'), 1, 'repeated messages are listed once')
        self.assertIn('gpt-5.4-nano: AI_RetryError', detail)

    def test_reports_a_stall_when_the_cli_logged_no_provider_error(self):
        for log in ['', 'INFO service=default starting\nINFO service=session created']:
            self.assertIn('no response', stall_detail(log))

    def test_never_echoes_raw_log_lines_or_the_authentication_key(self):
        secret = 'private-test-key'
        log = '\n'.join([stream_error('big-pickle', f'AI_APICallError: bad {secret} ' + 'x' * 500),
                         'level=INFO message="headers" authorization="Bearer ' + secret + '"'])
        detail = stall_detail(log, secret)
        self.assertNotIn(secret, detail)
        self.assertNotIn('authorization', detail)
        self.assertIn('[redacted]', detail)
        self.assertLess(len(detail), 400)

    def test_lists_at_most_three_distinct_messages(self):
        log = '\n'.join(stream_error('m', f'AI_APICallError: failure {n}') for n in range(6))
        detail = stall_detail(log)
        self.assertIn('6 failed attempt(s)', detail)
        self.assertEqual(detail.count('AI_APICallError'), 3)


class CanaryTimeoutTests(unittest.TestCase):
    def test_deadline_message_carries_the_providers_error(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'scripts').mkdir()
            (root / 'scripts/release_scenarios.json').write_text(json.dumps({
                'providers': {'opencode': {'cli': '1.18.0', 'model': 'opencode/big-pickle'}}}))
            cli = root / 'opencode'
            cli.write_text("#!/usr/bin/env python3\nimport os,sys,time\n"
                           "if sys.argv[1] == '--version': print('1.18.0'); sys.exit(0)\n"
                           "log = os.path.join(os.environ['XDG_DATA_HOME'], 'opencode', 'log')\n"
                           "os.makedirs(log)\n"
                           "open(os.path.join(log, 'opencode.log'), 'w').write(os.environ['STREAM_ERROR'] + '\\n')\n"
                           "time.sleep(60)\n")
            cli.chmod(0o755)
            env = {'PATH': str(root) + os.pathsep + os.environ['PATH'], 'OPENCODE_API_KEY': 'fixture-token',
                   'STREAM_ERROR': stream_error('big-pickle', 'AI_APICallError: Rate limit exceeded fixture-token')}
            output = io.StringIO()
            previous = Path.cwd()
            os.chdir(root)
            try:
                with patch.dict(os.environ, env), patch.object(opencode_canary, 'TIMEOUT', 1), \
                        contextlib.redirect_stdout(output):
                    result = opencode_canary.main()
            finally:
                os.chdir(previous)
        self.assertEqual(result, 1)
        message = output.getvalue()
        self.assertIn('exceeded its 1-second deadline', message)
        self.assertIn('big-pickle: AI_APICallError: Rate limit exceeded [redacted]', message)
        self.assertNotIn('fixture-token', message)


class CanaryProcessTests(unittest.TestCase):
    def test_single_request_in_isolated_home_with_current_pin(self):
        script = Path(__file__).with_name('opencode_canary.py').resolve()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'scripts').mkdir()
            (root / 'scripts/release_scenarios.json').write_text(json.dumps({
                'providers': {'opencode': {'cli': '1.18.0', 'model': 'opencode/big-pickle'}}}))
            cli = root / 'opencode'
            cli.write_text("#!/usr/bin/env python3\nimport json,os,sys\n"
                           "if sys.argv[1] == '--version': print('1.18.0'); sys.exit(0)\n"
                           "with open(os.environ['CALL_RECORD'], 'a') as f: f.write(json.dumps({'args':sys.argv[1:], 'home':os.environ['HOME'], 'cwd':os.getcwd(), 'permission':os.environ.get('OPENCODE_PERMISSION')})+'\\n')\n"
                           "print(os.environ['PROVIDER_OUTPUT'])\n")
            cli.chmod(0o755)
            record = root / 'calls'
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                       CALL_RECORD=str(record), OPENCODE_API_KEY='fixture-token', OPENCODE_PERMISSION='{"*":"deny"}',
                       PROVIDER_OUTPUT='{"type":"text","part":{"text":"OK"}}')
            result = subprocess.run(['python3', str(script)], cwd=root, env=env,
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            calls = [json.loads(line) for line in record.read_text().splitlines()]
            self.assertEqual(len(calls), 1)
            self.assertIsNone(calls[0]['permission'], 'canary must keep the official CLI tool schema')
            self.assertEqual(calls[0]['args'][:5], ['run', '--format', 'json', '-m', 'opencode/big-pickle'])
            self.assertEqual(calls[0]['home'], calls[0]['cwd'])
            self.assertNotEqual(calls[0]['cwd'], str(root))
            self.assertFalse(Path(calls[0]['home']).exists(), 'temporary provider state must be removed')


if __name__ == '__main__':
    unittest.main()
