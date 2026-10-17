"""Verify all four archives and their checksums before exposing the draft."""
import hashlib
import os
import subprocess
import tempfile
from pathlib import Path


def verify(directory, version):
    expected = {f'sandman_{version}_{platform}.tar.gz' for platform in
                ('linux_amd64', 'linux_arm64', 'darwin_amd64', 'darwin_arm64')}
    entries = {}
    for line in (directory / 'checksums.txt').read_text().splitlines():
        digest, filename = line.split()
        assert filename not in entries, 'duplicate checksum'
        entries[filename] = digest
    assert set(entries) == expected, 'incomplete artifacts'
    assert {p.name for p in directory.iterdir()} == expected | {'checksums.txt'}, 'unexpected assets'
    for name, digest in entries.items():
        assert hashlib.sha256((directory / name).read_bytes()).hexdigest() == digest, 'invalid checksum'


if __name__ == '__main__':
    tag = os.environ['RELEASE_TAG']
    with tempfile.TemporaryDirectory() as temporary:
        subprocess.run(['gh', 'release', 'download', tag, '--dir', temporary], check=True)
        verify(Path(temporary), tag.removeprefix('v'))
    subprocess.run(['gh', 'release', 'edit', tag, '--draft=false', '--prerelease=' + str('-rc.' in tag).lower()], check=True)
