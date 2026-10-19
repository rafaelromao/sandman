import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from opencode_canary import evaluate, PIN_GUIDANCE


class CanaryTests(unittest.TestCase):
    def test_real_response_required(self):
        self.assertIsNone(evaluate('{"type":"text","part":{"text":"OK"}}', 0))
        for output, code in [('', 0), ('{"type":"step_finish"}', 0),
                             ('{"type":"text","part":{"text":"OK"}}', 1),
                             ('{"type":"error","error":{"message":"offline"}}', 0)]:
            self.assertIsNotNone(evaluate(output, code))

    def test_deprecation_is_failure_even_when_cli_exits_zero(self):
        for message in ['OpenCode 1.18.0 or newer is required to use the free tier',
                        'This CLI version is deprecated', 'CLI no longer supported']:
            self.assertEqual(evaluate(message, 0), PIN_GUIDANCE)

    def test_client_configuration_error_is_not_assumed_to_be_deprecation(self):
        self.assertNotEqual(evaluate("OpenCode's free tier can only be used from within OpenCode", 1), PIN_GUIDANCE)


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
            self.assertEqual(calls[0]['args'][:6], ['run', '--pure', '--format', 'json', '-m', 'opencode/big-pickle'])
            self.assertEqual(calls[0]['home'], calls[0]['cwd'])
            self.assertNotEqual(calls[0]['cwd'], str(root))
            self.assertFalse(Path(calls[0]['home']).exists(), 'temporary provider state must be removed')


if __name__ == '__main__':
    unittest.main()
