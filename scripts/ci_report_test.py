"""Hermetic regressions for command exits and Go failure diagnostics."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class ReportTests(unittest.TestCase):
    def run_report(self, output, code):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / 'scripts').mkdir()
            for name in ('ci_report.sh', 'ci_report.py'):
                shutil.copy(Path(__file__).parent / name, root / 'scripts' / name)
            summary = root / 'summary.md'
            result = subprocess.run(
                ['bash', 'scripts/ci_report.sh', 'fixture', 'python3', '-c',
                 'import sys; print(sys.argv[1]); sys.exit(int(sys.argv[2]))', output, str(code)],
                cwd=root, env={**os.environ, 'GITHUB_STEP_SUMMARY': str(summary)},
                capture_output=True, text=True)
            self.assertEqual(result.returncode, code, result.stderr)
            report = json.loads((root / 'reports/fixture.json').read_text())
            self.assertEqual(report['exit_code'], code)
            self.assertTrue((root / 'reports/fixture.log').exists())
            self.assertIn('fixture.log', summary.read_text())
            return report, summary.read_text()

    def test_test_and_package_failures(self):
        events = '\n'.join(json.dumps(e) for e in [
            dict(Action='fail', Package='example/pkg', Test='TestBroken'),
            dict(Action='fail', Package='example/pkg'),
            dict(Action='skip', Package='example/pkg', Test='TestOptional')])
        report, summary = self.run_report(events, 1)
        self.assertEqual(len(report['failures']), 2)
        self.assertEqual(report['counts'], {'fail': 1, 'skip': 1})
        self.assertIn('TestBroken', summary)
        self.assertIn('example/pkg', summary)

    def test_compiler_failure_and_malformed_output(self):
        report, summary = self.run_report('compiler error\n{malformed\n[]', 2)
        self.assertEqual(report['failures'], [])
        self.assertIn('Command/setup failure', summary)

    def test_success(self):
        report, summary = self.run_report('{"Action":"pass","Package":"pkg","Test":"TestOK"}', 0)
        self.assertEqual(report['counts'], {'pass': 1})
        self.assertIn('PASS', summary)

    def test_format_check_is_non_mutating_and_fails_on_tool_errors(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            shutil.copy(Path(__file__).parent.parent / 'Makefile', root / 'Makefile')
            source = root / 'bad.go'
            source.write_text('package bad;func bad(){}')
            before = source.read_bytes()
            formatter = root / 'gofmt'
            formatter.write_text('#!/bin/sh\necho bad.go\n')
            formatter.chmod(0o755)
            env = {**os.environ, 'PATH': str(root) + os.pathsep + os.environ['PATH']}
            result = subprocess.run(['make', 'fmt-check'], cwd=root, env=env,
                                    capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('bad.go', result.stdout)
            self.assertEqual(source.read_bytes(), before)
            formatter.write_text('#!/bin/sh\nexit 7\n')
            result = subprocess.run(['make', 'fmt-check'], cwd=root, env=env,
                                    capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)

    def test_termination_exit(self):
        _, summary = self.run_report('terminated', 143)
        self.assertIn('FAIL', summary)


if __name__ == '__main__':
    unittest.main()
