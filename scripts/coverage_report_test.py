import io
import json
import zipfile
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import coverage_report as c


class CoverageTests(unittest.TestCase):
    def setUp(self):
        self.policy = dict(mode='report', changed_line_proposal=80,
                           tolerance_percentage_points=1,
                           protected_packages=['internal/events'], exemptions=[])

    def test_uncovered_changes_and_no_executable_changes(self):
        blocks = c.profile('mode: atomic\nx.go:2.1,4.2 2 0\nx.go:2.1,4.2 2 0\n')
        self.assertEqual(c.packages(blocks)['.']['statements'], 2)
        changed = c.changed_coverage(blocks, {'x.go': {2, 3}})
        self.assertEqual(changed['percent'], 0)
        result = c.evaluate({}, {}, changed, self.policy, {'internal/events': 'tree'})
        self.assertEqual(result['result'], 'advisory')
        self.assertIsNone(c.changed_coverage(blocks, {'x.go': {10}})['percent'])
        self.policy.update(mode='enforce', approval_url='https://review')
        self.assertEqual(c.evaluate({}, {}, changed, self.policy, {'internal/events': 'tree'})['result'], 'fail')

    def test_actual_stacked_base_and_push(self):
        with patch.object(c, 'git', return_value='ancestor') as git:
            self.assertEqual(c.select_base({'pull_request': {'base': {'sha': 'stack'}}}, 'head'), 'ancestor')
            git.assert_called_once_with('merge-base', 'stack', 'head')
        self.assertEqual(c.select_base({'before': 'abc'}, 'head'), 'abc')

    def test_renames_add_new_lines_deletions_excluded(self):
        diff = '+++ /dev/null\n@@ -1 +0,0 @@\n+++ b/new.go\n@@ -0,0 +2,2 @@\n'
        with patch.object(c, 'git', return_value=diff) as git:
            self.assertEqual(c.changed_lines('base', 'head'), {'new.go': {2, 3}})
            self.assertIn('--no-renames', git.call_args.args)

    def test_regression_tolerance_exemptions_bound_to_source_and_review(self):
        old = {'internal/events': {'percent': 90}}
        new = {'internal/events': {'percent': 88}}
        no_changes = {'percent': None}
        self.policy.update(mode='enforce', approval_url='https://review')
        self.assertEqual(c.evaluate(new, old, no_changes, self.policy, {'internal/events': 'tree'})['result'], 'fail')
        exemption = dict(package='internal/events', source_tree='tree', reason='platform branch', review_url='https://review', expires_on='2099-01-01')
        self.policy['exemptions'] = [exemption]
        self.assertEqual(c.evaluate(new, old, no_changes, self.policy, {'internal/events': 'tree'})['result'], 'pass')
        self.assertEqual(c.evaluate(new, old, no_changes, self.policy, {'internal/events': 'other-tree'})['result'], 'fail')
        self.assertEqual(c.evaluate({'internal/events': {'percent': 89}}, old, no_changes, self.policy, {'internal/events': 'other-tree'})['result'], 'pass')
        self.assertEqual(c.evaluate(new, None, no_changes, self.policy, {'internal/events': 'tree'})['result'], 'fail')

    def test_expired_exemptions_and_missing_package_baseline_fail_closed(self):
        self.policy.update(mode='enforce', approval_url='https://review')
        self.policy['exemptions'] = [dict(package='internal/events', source_tree='tree',
            reason='reviewed', review_url='https://review', expires_on='2000-01-01')]
        result = c.evaluate({'internal/events': {'percent': 80}},
                            {'internal/events': {'percent': 90}}, {'percent': None},
                            self.policy, {'internal/events': 'tree'})
        self.assertEqual(result['result'], 'fail')
        self.assertEqual(c.evaluate({}, {}, {'percent': None}, self.policy, {})['result'], 'fail')

    def test_baseline_requires_matching_platform_sha_and_successful_collection(self):
        for platform, sha, status, accepted in [('linux', 'base', 'success', True),
                ('macos', 'base', 'success', False), ('linux', 'other', 'success', False),
                ('linux', 'base', 'failure', False), ('linux', 'base', 'unverified', False)]:
            report = dict(schema_version=1, head_sha=sha, platform=platform,
                          collection_status=status, packages={'pkg': {'percent': 75}})
            archive = io.BytesIO()
            with zipfile.ZipFile(archive, 'w') as zipout:
                zipout.writestr('coverage.json', json.dumps(report))
            responses = [json.dumps({'workflow_runs': [dict(id=1, head_sha='base', head_branch='main')]}).encode(),
                         json.dumps({'artifacts': [dict(id=2, name='coverage-linux', expired=False)]}).encode(),
                         archive.getvalue()]
            with patch.dict(os.environ, {'GITHUB_REPOSITORY': 'owner/repo', 'GH_TOKEN': 'test'}), \
                 patch.object(c.urllib.request, 'build_opener') as build:
                build.return_value.open.side_effect = [io.BytesIO(response) for response in responses]
                self.assertEqual(c.fetch_baseline('base', 'linux'), report['packages'] if accepted else None)

    def test_cli_reports_and_fails_only_enforced_violations(self):
        script = str(Path(c.__file__).resolve())
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            def git(*args):
                subprocess.run(['git', '-C', tmp, *args], check=True,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            git('init')
            git('config', 'user.name', 'Test')
            git('config', 'user.email', 'test@example.com')
            (root / 'x.go').write_text('package x\nfunc X() {}\n')
            git('add', '.')
            git('commit', '-m', 'base')
            (root / 'x.go').write_text('package x\nfunc X() { println(1) }\n')
            git('add', '.')
            git('commit', '-m', 'head')
            (root / '.github').mkdir()
            (root / 'reports').mkdir()
            (root / 'reports/coverage.out').write_text('mode: atomic\nx.go:2.10,2.23 1 0\n')
            env = {k: v for k, v in os.environ.items() if not k.startswith(('GITHUB_', 'GH_'))}
            for mode, code in [('report', 0), ('enforce', 1)]:
                self.policy.update(mode=mode, approval_url='https://review')
                (root / '.github/coverage-policy.json').write_text(json.dumps(self.policy))
                result = subprocess.run(['python3', script, '--platform', 'linux'], cwd=tmp,
                                        env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, code, result.stderr)
                report = json.loads((root / 'reports/coverage.json').read_text())
                self.assertEqual(report['changed']['percent'], 0)
                self.assertEqual(report['platform'], 'linux')
                self.assertEqual(len(report['baseline_sha']), 40)

    def test_enforcement_requires_review(self):
        self.policy['mode'] = 'enforce'
        with self.assertRaises(ValueError):
            c.evaluate({}, {}, {'percent': None}, self.policy, {'internal/events': 'tree'})


if __name__ == '__main__':
    unittest.main()
