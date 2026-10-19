"""Exercise scanner errors/findings without ever printing synthetic findings."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

class ScannerTransportTest(unittest.TestCase):
    def test_live_scanner_rejects_new_committed_secret(self):
        import shutil
        scanner = shutil.which("gitleaks")
        if scanner is None:
            self.skipTest("pinned gitleaks not on PATH")
        root = Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory() as directory:
            def git(*args):
                subprocess.run(["git", *args], cwd=directory, check=True, capture_output=True)
            git("init")
            git("config", "user.name", "Security test")
            git("config", "user.email", "test@example.invalid")
            # An intentionally synthetic AWS-shaped token exercises upstream rules.
            Path(directory, "credential.txt").write_text("aws_access_key_id = " + "AKIA" + "ABCDEFGHIJKLMNOP" + "\n")
            git("add", ".")
            git("commit", "-m", "synthetic detection fixture")
            result = subprocess.run([scanner, "git", "--config", str(root / ".gitleaks.toml"), "--redact=100", "--no-banner"], cwd=directory, capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertNotIn("AKIA" + "ABCDEFGHIJKLMNOP", result.stdout + result.stderr)

    def test_redaction_and_errors(self):
        root = Path(__file__).resolve().parent.parent
        for failing in ("none", "govulncheck", "gitleaks", "both"):
            with self.subTest(failing=failing), tempfile.TemporaryDirectory() as directory:
                for tool in ("govulncheck", "gitleaks"):
                    path = Path(directory, tool)
                    path.write_text("#!/bin/sh\necho SYNTHETIC_PRIVATE_DIAGNOSTIC >&2\nexit " + ("2" if failing in (tool, "both") else "0") + "\n")
                    path.chmod(0o755)
                result = subprocess.run(["bash", "scripts/security_check.sh"], cwd=root, env=os.environ | {"PATH": directory + ":" + os.environ["PATH"]}, capture_output=True, text=True)
                self.assertEqual(result.returncode == 0, failing == "none")
                self.assertNotIn("SYNTHETIC_PRIVATE_DIAGNOSTIC", result.stdout + result.stderr)

if __name__ == "__main__":
    unittest.main()
