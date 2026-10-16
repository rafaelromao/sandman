import datetime
import unittest
from security_policy import validate

class SecurityPolicyTest(unittest.TestCase):
    def test_empty_baseline(self):
        self.assertEqual(validate([], datetime.date(2026, 1, 1)), "\n")

    def test_exact_exception_and_expiry(self):
        entry = dict(fingerprint="a" * 40 + ":fixture.txt:generic-api-key:1", owner="rafaelromao", rationale="Reviewed synthetic fixture", expires="2026-02-01")
        self.assertIn(entry["fingerprint"], validate([entry], datetime.date(2026, 1, 1)))
        for change in [{"expires": "2026-01-01"}, {"owner": ""}, {"rationale": ""}, {"fingerprint": "*"}, {"expires": "invalid"}]:
            with self.assertRaises(ValueError):
                validate([entry | change], datetime.date(2026, 1, 1))
        with self.assertRaises(ValueError):
            validate([entry, entry], datetime.date(2026, 1, 1))

if __name__ == "__main__":
    unittest.main()
