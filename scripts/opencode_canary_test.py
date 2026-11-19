import unittest

from opencode_canary import evaluate


class CanaryTest(unittest.TestCase):
    def test_requires_the_expected_cli_text_and_clean_exit(self):
        output = '{"type":"text","part":{"text":"SANDMAN_CANARY_OK"}}'
        self.assertTrue(evaluate(output, 0))
        self.assertFalse(evaluate(output, 1))
        self.assertFalse(evaluate(output.replace("CANARY_OK", "CANARY_WRONG"), 0))
        self.assertFalse(evaluate(output + '\n{"type":"error"}', 0))
        self.assertFalse(evaluate("", 0))


if __name__ == "__main__":
    unittest.main()
