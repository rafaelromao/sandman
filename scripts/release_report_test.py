import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from release_report import evaluate


class ReleaseReportTests(unittest.TestCase):
    def setUp(self):
        self.manifest = {'scenarios': [{'package': 'p', 'test': 'TestRequired', 'tier': 'live'}]}

    def event(self, action, test='TestRequired', **values):
        return dict(Package='p', Test=test, Action=action, **values)

    def test_pass_requires_observed_scenario(self):
        self.assertTrue(evaluate(self.manifest, 'live', [self.event('run'), self.event('pass')], 0)['success'])
        self.assertFalse(evaluate(self.manifest, 'live', [], 0)['success'])

    def test_expected_negative_control_skip_requires_matching_reason(self):
        self.manifest['scenarios'][0]['permitted_skip'] = 'runtime is available'
        events = [self.event('output', Output='runtime is available'), self.event('skip')]
        self.assertTrue(evaluate(self.manifest, 'live', events, 0)['success'])
        self.assertFalse(evaluate(self.manifest, 'live', [self.event('skip')], 0)['success'])

    def test_missing_credentials_and_nested_skips_fail(self):
        for events in [[self.event('output', Output='missing auth'), self.event('skip')],
                       [self.event('skip', test='TestRequired/claude'), self.event('pass')]]:
            self.assertFalse(evaluate(self.manifest, 'live', events, 0)['success'])

    def test_duplicates_fail_instead_of_inflating_coverage(self):
        for events in [[self.event('pass'), self.event('pass')],
                       [self.event('run'), self.event('run'), self.event('pass')]]:
            self.assertFalse(evaluate(self.manifest, 'live', events, 0)['success'])
        self.manifest['scenarios'] *= 2
        self.assertFalse(evaluate(self.manifest, 'live', [self.event('pass')], 0)['success'])

    def test_timeout_preserves_partial_evidence(self):
        result = evaluate(self.manifest, 'live', [self.event('run')], 124)
        self.assertEqual(result['classification'], 'timeout')
        self.assertEqual(result['counts']['timed_out'], 1)
        self.assertEqual(result['scenarios'][0]['status'], 'timed_out')

    def test_package_failure_and_go_timeout_fail(self):
        events = [self.event('pass'), dict(Action='output', Package='p', Output='panic: test timed out')]
        self.assertEqual(evaluate(self.manifest, 'live', events, 1)['classification'], 'timeout')
        self.assertFalse(evaluate(self.manifest, 'live', events, 1)['success'])

    def test_platform_boundaries(self):
        manifest = copy.deepcopy(self.manifest)
        manifest['scenarios'].append(dict(package='p', test='TestDarwin', tier='live', platforms=['darwin/arm64']))
        self.assertTrue(evaluate(manifest, 'live', [self.event('pass')], 0, 'linux/amd64')['success'])
        self.assertFalse(evaluate(manifest, 'live', [self.event('pass')], 0, 'darwin/arm64')['success'])

    def test_failure_or_unfinished_subtest_cannot_be_hidden_by_zero_exit(self):
        for event in [self.event('fail', test='TestOther'),
                      self.event('run', test='TestRequired/claude')]:
            self.assertFalse(evaluate(self.manifest, 'live', [self.event('pass'), event], 0)['success'])

    def test_empty_tier_fails(self):
        self.assertFalse(evaluate(self.manifest, 'integration', [], 0)['success'])


class ReleasePipelineTests(unittest.TestCase):
    def test_report_records_sha_versions_and_setup_failure(self):
        script = Path(__file__).with_name('release_report.py').resolve()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'scripts').mkdir()
            manifest = {'providers': {'claude': {'cli': 'pinned', 'model': 'model-id'}},
                        'scenarios': [dict(package='p', test='TestRequired', tier='live')]}
            (root / 'scripts/release_scenarios.json').write_text(json.dumps(manifest))
            artifacts = root / 'regression-artifacts'
            artifacts.mkdir()
            (artifacts / 'versions.txt').write_text('observed CLI version')
            env = dict(os.environ, GITHUB_SHA='exact-source-sha',
                       RELEASE_PREREQUISITES='auth failed', GITHUB_STEP_SUMMARY=str(root / 'summary'))
            result = subprocess.run(['python3', str(script), 'live', 'missing-log', '1',
                                     'regression-artifacts/report.json'], cwd=root, env=env, capture_output=True)
            self.assertEqual(result.returncode, 1, result.stderr)
            report = json.loads((artifacts / 'report.json').read_text())
            self.assertEqual(report['sha'], 'exact-source-sha')
            self.assertEqual(report['versions'], 'observed CLI version')
            self.assertEqual(report['prerequisites'], 'auth failed')
            self.assertEqual(report['counts']['missing'], 1)
            # The job log must name the failing scenario without opening the artifact.
            self.assertIn("::error::required scenario missing: ('p', 'TestRequired')", result.stderr.decode())

    def test_job_log_annotations_escape_test_output(self):
        script = Path(__file__).with_name('release_report.py').resolve()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'scripts').mkdir()
            manifest = {'providers': {}, 'scenarios': [dict(package='p', test='TestRequired', tier='live')]}
            (root / 'scripts/release_scenarios.json').write_text(json.dumps(manifest))
            events = [dict(Package='p', Test='TestRequired/sub', Action='output', Output='50% done\r\n::stop-commands::x\n'),
                      dict(Package='p', Test='TestRequired/sub', Action='skip'),
                      dict(Package='p', Test='TestRequired', Action='pass')]
            (root / 'events.jsonl').write_text('\n'.join(json.dumps(event) for event in events))
            env = dict(os.environ, GITHUB_SHA='sha')
            env.pop('GITHUB_STEP_SUMMARY', None)
            result = subprocess.run(['python3', str(script), 'live', 'events.jsonl', '0', 'report.json'],
                                    cwd=root, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 1, result.stderr)
            # Test output must not inject workflow commands into the job log.
            self.assertEqual([line for line in result.stderr.splitlines() if line.startswith('::')],
                             ["::error::required subscenario skipped: ('p', 'TestRequired/sub'): 50%25 done%0D%0A::stop-commands::x"])

    def test_tier_selectors_and_process_failures(self):
        scripts = Path(__file__).resolve().parent
        manifest = json.loads((scripts / 'release_scenarios.json').read_text())
        live = {row['test'] for row in manifest['scenarios'] if row['tier'] == 'live'}
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'scripts').symlink_to(scripts, target_is_directory=True)
            (root / 'regression-artifacts').mkdir()
            binary = root / 'go'
            binary.write_text('#!/usr/bin/env python3\nimport json,sys,os\nprint(json.dumps(sys.argv[1:]))\nsys.exit(int(os.environ["FAKE_RC"]))\n')
            binary.chmod(0o755)
            for tier in ['unit', 'smoke', 'integration', 'live', 'claude']:
                env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                           RELEASE_TIER=tier, FAKE_RC='7')
                result = subprocess.run(['bash', str(scripts / 'release_tier.sh')], cwd=root,
                                        env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 7, result.stderr)
                self.assertEqual((root / 'regression-artifacts/exit-code').read_text().strip(), '7')
                args = json.loads((root / 'regression-artifacts/events.jsonl').read_text())
                if tier in ('live', 'integration', 'claude'):
                    selector = args[args.index('-skip' if tier == 'integration' else '-run') + 1]
                    expected = {row['test'] for row in manifest['scenarios'] if row['tier'] in (('live', 'claude') if tier == 'integration' else (tier,))}
                    self.assertEqual(set(selector[2:-2].split('|')), expected)
                self.assertIn('-count=1', args)


if __name__ == '__main__':
    unittest.main()
