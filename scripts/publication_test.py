import copy
import hashlib
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

from publication_candidate import candidate
from publication_finalize import verify
from publication_gate import validate


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.context = dict(GITHUB_SHA='a' * 40, GITHUB_WORKFLOW_SHA='a' * 40,
                            GITHUB_REF='refs/heads/main', GITHUB_EVENT_NAME='push',
                            GITHUB_REPOSITORY='rafaelromao/sandman',
                            GITHUB_WORKFLOW_REF='rafaelromao/sandman/.github/workflows/release.yml@refs/heads/main')
        self.needs = {name: dict(result='success') for name in
                      ('candidate', 'quality', 'regression', 'native')}
        self.needs['candidate']['outputs'] = dict(sha='a' * 40, tag='v1.0.0-rc.7')

    def test_validated_path(self):
        validate(self.needs, self.context)
        self.context['GITHUB_EVENT_NAME'] = 'workflow_dispatch'
        validate(self.needs, self.context)

    def test_incomplete_jobs(self):
        for name in self.needs:
            for state in ('failure', 'cancelled', 'skipped', '', 'in_progress'):
                with self.subTest(name=name, state=state):
                    needs = copy.deepcopy(self.needs)
                    needs[name]['result'] = state
                    with self.assertRaises(AssertionError):
                        validate(needs, self.context)
            needs = copy.deepcopy(self.needs)
            del needs[name]
            with self.assertRaises(AssertionError):
                validate(needs, self.context)

    def test_mismatched_source(self):
        self.needs['candidate']['outputs']['sha'] = 'b' * 40
        with self.assertRaises(AssertionError):
            validate(self.needs, self.context)

    def test_stale_and_invalid_provenance(self):
        for key, value in dict(GITHUB_WORKFLOW_SHA='b' * 40,
                               GITHUB_WORKFLOW_REF='spoofed-status',
                               GITHUB_EVENT_NAME='pull_request',
                               GITHUB_REF='refs/heads/release-please--branches--main',
                               GITHUB_REPOSITORY='attacker/sandman').items():
            context = dict(self.context, **{key: value})
            with self.subTest(key=key), self.assertRaises(AssertionError):
                validate(self.needs, context)

    def test_candidate(self):
        self.assertEqual(candidate({'.': '1.0.0-rc.7'}, {'.': '1.0.0-rc.6'}), 'v1.0.0-rc.7')
        self.assertEqual(candidate({'.': '1.0.0'}, {'.': '1.0.0'}), '')
        with self.assertRaises(ValueError):
            candidate({'.': '../wrong'}, {'.': '1.0.0'})

    def test_prepare_never_overwrites_or_moves_tags(self):
        for case, tag_sha, draft, assets, succeeds in (
            ('new', 'a' * 40, None, [], True),
            ('missing-tag', 'a' * 40, None, [], True),
            ('api-error', 'a' * 40, None, [], False),
            ('empty-draft', 'a' * 40, True, [], True),
            ('published', 'a' * 40, False, [{'name': 'archive'}], True),
            ('partial-draft', 'a' * 40, True, [{'name': 'archive'}], False),
            ('wrong-sha', 'b' * 40, True, [], False),
        ):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp:
                directory = Path(tmp)
                (directory / 'git').write_text('#!/bin/sh\ncase "$1" in\nshow-ref) if [ "$CASE" = missing-tag ]; then exit 1; fi;;\nrev-parse) if [ "$2" = HEAD ]; then echo "$RELEASE_SHA"; else echo "$TAG_SHA"; fi;;\nesac\n')
                (directory / 'gh').write_text('#!/bin/sh\necho "$*" >> "$CALLS"\nif [ "$CASE" = api-error ]; then exit 1; fi\ncase "$*" in\n*--paginate*) echo "$RELEASES";;\nesac\n')
                for name in ('git', 'gh'):
                    (directory / name).chmod(0o755)
                releases = [] if draft is None else [dict(tag_name='v1.0.0', draft=draft, assets=assets)]
                environment = dict(os.environ, PATH=tmp + ':' + os.environ['PATH'],
                                   CASE=case, RELEASE_SHA='a' * 40, TAG_SHA=tag_sha, RELEASE_TAG='v1.0.0',
                                   GITHUB_REPOSITORY='rafaelromao/sandman', GITHUB_OUTPUT=tmp + '/output',
                                   CALLS=tmp + '/calls', RELEASES=json.dumps([releases]))
                result = subprocess.run(['bash', str(Path(__file__).with_name('publication_prepare.sh'))],
                                        env=environment, capture_output=True, text=True)
                self.assertEqual(result.returncode == 0, succeeds, result.stderr)
                calls = (directory / 'calls').read_text() if (directory / 'calls').exists() else ''
                self.assertNotIn('--clobber', calls)
                self.assertNotIn('PATCH', calls)
                if case == 'missing-tag':
                    self.assertIn('POST', calls)
                if case == 'published':
                    self.assertEqual((directory / 'output').read_text(), 'complete=true\n')
                    self.assertNotIn('release create', calls)

    def test_finalization_checks_complete_assets(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            names = [f'sandman_1.0.0_{platform}.tar.gz' for platform in
                     ('linux_amd64', 'linux_arm64', 'darwin_amd64', 'darwin_arm64')]
            for name in names:
                (directory / name).write_bytes(b'archive')
            digest = hashlib.sha256(b'archive').hexdigest()
            (directory / 'checksums.txt').write_text(''.join(f'{digest}  {name}\n' for name in names))
            verify(directory, '1.0.0')
            (directory / names[0]).write_bytes(b'corrupt')
            with self.assertRaises(AssertionError):
                verify(directory, '1.0.0')
            (directory / names[0]).unlink()
            with self.assertRaises(AssertionError):
                verify(directory, '1.0.0')


if __name__ == '__main__':
    unittest.main()
