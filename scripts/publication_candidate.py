"""Only a manifest-changing main commit is a publication candidate, including reruns."""
import json
import os
import re
import subprocess
from pathlib import Path


def candidate(current, previous):
    version = current['.']
    if version == previous['.']:
        return ''
    if not re.fullmatch(r'\d+\.\d+\.\d+(?:-rc\.\d+)?', version):
        raise ValueError('invalid release version')
    return 'v' + version


if __name__ == '__main__':
    sha = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    assert sha == os.environ['GITHUB_SHA']
    previous = json.loads(subprocess.check_output(
        ['git', 'show', sha + '^:.release-please-manifest.json'], text=True))
    tag = candidate(json.loads(Path('.release-please-manifest.json').read_text()), previous)
    with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
        output.write(f'tag={tag}\nsha={sha}\n')
