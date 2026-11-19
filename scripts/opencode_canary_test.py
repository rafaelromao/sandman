import unittest

from opencode_canary import check_exchange, evaluate
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


if __name__ == "__main__":
    unittest.main()
