"""Consume only same-invocation reusable workflow results, never named statuses."""
import json
import os
import re


def validate(needs, context):
    sha = context['GITHUB_SHA']
    assert re.fullmatch('[0-9a-f]{40}', sha), 'invalid SHA'
    assert context['GITHUB_REPOSITORY'] == 'rafaelromao/sandman', 'invalid repository'
    assert context['GITHUB_REF'] == 'refs/heads/main', 'invalid ref'
    assert context['GITHUB_EVENT_NAME'] in ('push', 'workflow_dispatch'), 'invalid event'
    assert context['GITHUB_WORKFLOW_REF'] == 'rafaelromao/sandman/.github/workflows/release.yml@refs/heads/main', 'invalid provenance'
    assert context['GITHUB_WORKFLOW_SHA'] == sha, 'stale workflow source'
    assert set(needs) == {'candidate', 'quality', 'regression', 'native'}, 'missing validation'
    assert all(job['result'] == 'success' for job in needs.values()), 'incomplete validation'
    assert needs['candidate']['outputs']['sha'] == sha, 'source mismatch'
    assert re.fullmatch(r'v\d+\.\d+\.\d+(?:-rc\.\d+)?', needs['candidate']['outputs']['tag']), 'invalid tag'


if __name__ == '__main__':
    validate(json.loads(os.environ['NEEDS_JSON']), os.environ)
