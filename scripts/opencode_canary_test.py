from contextlib import redirect_stdout
from io import StringIO
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from opencode_canary import check_exchange, evaluate, main
from opencode_fixture import Turn, serve


class CanaryTest(unittest.TestCase):
    def test_requires_the_expected_cli_text_and_clean_exit(self):
        output = '{"type":"text","part":{"text":"SANDMAN_CANARY_OK"}}'
        self.assertIsNone(evaluate(output, 0))
        self.assertIn("CLI exit 1", evaluate(output, 1))
        self.assertIn("expected text received: False", evaluate(output.replace("CANARY_OK", "CANARY_WRONG"), 0))
        error = '\n{"type":"error","error":{"name":"APIError","data":{"message":"unexpected request"}}}'
        self.assertIn("'APIError': 'unexpected request'", evaluate(output + error, 0))
        self.assertIn("expected text received: False", evaluate("", 0))

    def test_timeout_keeps_the_fixture_diagnostic(self):
        with serve([Turn("user", "expected", "OK")]) as fixture:
            with self.assertRaisesRegex(ValueError, r"60s deadline.*0 of 1 exchanges"):
                check_exchange(fixture, "", -9, timed_out=True)

    def test_stalled_cli_is_killed_and_canary_fails(self):
        pin = json.loads(Path(__file__).with_name("release_scenarios.json").read_text())["providers"]["opencode"]["cli"]
        with tempfile.TemporaryDirectory() as root:
            cli = Path(root, "opencode")
            cli.write_text(
                f'#!/bin/sh\nif [ "$1" = "--version" ]; then printf "{pin}\\n"; exit; fi\n'
                'sleep 10\n')
            cli.chmod(0o755)
            messages = StringIO()
            with patch.dict(os.environ, {"PATH": root + os.pathsep + os.environ["PATH"]}), \
                    patch("opencode_canary.TIMEOUT", 0.1), redirect_stdout(messages):
                self.assertEqual(main(), 1)
            self.assertIn("CLI exceeded its 0.1s deadline", messages.getvalue())
            self.assertIn("0 of 1 exchanges", messages.getvalue())


if __name__ == "__main__":
    unittest.main()
