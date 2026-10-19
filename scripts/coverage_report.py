#!/usr/bin/env python3
"""Platform-local Go coverage reporting. Never sums profiles across platforms."""
import argparse
from datetime import date
import io
import json
import os
from pathlib import Path
import re
import subprocess
import urllib.request
import zipfile

MODULE = 'github.com/rafaelromao/sandman/'


def git(*args):
    return subprocess.check_output(['git', *args], text=True).strip()


def select_base(event, head):
    """Use the actual PR base (including stacks), not hard-coded main."""
    target = event.get('pull_request', {}).get('base', {}).get('sha')
    if target:
        return git('merge-base', target, head)
    before = event.get('before', '')
    if before and set(before) != {'0'}:
        return before
    return git('rev-parse', head + '^')


def profile(text):
    lines = text.splitlines()
    if not lines or lines[0] != 'mode: atomic':
        raise ValueError('expected atomic coverage profile')
    blocks = {}
    for line in lines[1:]:
        match = re.fullmatch(r'(.+):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)', line)
        if not match:
            raise ValueError('invalid coverage block: ' + line)
        file, start, col, end, endcol, statements, count = match.groups()
        file = file.removeprefix(MODULE)
        key = (file, int(start), int(col), int(end), int(endcol))
        value = (int(statements), int(count) > 0)
        if key in blocks and blocks[key][0] != value[0]:
            raise ValueError('conflicting statement counts')
        blocks[key] = (value[0], value[1] or blocks.get(key, (0, False))[1])
    return blocks


def packages(blocks):
    totals = {}
    for (file, *_), (n, covered) in blocks.items():
        pair = totals.setdefault(str(Path(file).parent), [0, 0])
        pair[0] += n * covered
        pair[1] += n
    return {p: {'covered': c, 'statements': n, 'percent': 100*c/n if n else None}
            for p, (c, n) in sorted(totals.items())}


def changed_lines(base, head):
    # --no-renames deliberately treats renames as delete/add: new executable
    # lines remain accountable, and deleted lines never enter the denominator.
    diff = git('diff', '--no-ext-diff', '--no-renames', '--unified=0', base, head, '--', '*.go')
    result, file = {}, None
    for line in diff.splitlines():
        if line.startswith('+++ b/'):
            file = line[6:]
        elif line.startswith('+++ /dev/null'):
            file = None
        elif file and line.startswith('@@'):
            m = re.search(r'\+(\d+)(?:,(\d+))? @@', line)
            start, size = int(m[1]), int(m[2] or 1)
            result.setdefault(file, set()).update(range(start, start + size))
    return result


def changed_coverage(blocks, changes):
    executable = {}
    for (file, start, _, end, endcol), (n, covered) in blocks.items():
        if not n:
            continue
        for line in changes.get(file, set()) & set(range(start, end + (endcol > 1))):
            # Overlapping blocks must all be covered; never hide an uncovered block.
            executable[(file, line)] = executable.get((file, line), True) and covered
    total = len(executable)
    covered = sum(executable.values())
    return {'covered': covered, 'lines': total, 'percent': 100*covered/total if total else None}


def evaluate(current, baseline, changed, policy, source_trees):
    if policy['mode'] not in ('report', 'enforce'):
        raise ValueError('policy mode must be report or enforce')
    if not 0 <= policy['changed_line_proposal'] <= 100 or not 0 <= policy['tolerance_percentage_points'] <= 100:
        raise ValueError('invalid coverage thresholds')
    findings = []
    if changed['percent'] is not None and changed['percent'] < policy['changed_line_proposal']:
        findings.append('changed executable-line coverage below proposal')
    if baseline is None:
        findings.append('baseline unavailable; regression comparison pending')
    else:
        for p in policy['protected_packages']:
            old, new = baseline.get(p, {}).get('percent'), current.get(p, {}).get('percent')
            if old is None:
                findings.append(p + ': baseline package unavailable')
                continue
            if new is None or old-new > policy['tolerance_percentage_points']:
                exempt = any(e['package'] == p and e.get('source_tree') and
                             e['source_tree'] == source_trees.get(p) and e.get('reason') and
                             e.get('review_url', '').startswith('https://') and
                             date.fromisoformat(e['expires_on']) >= date.today()
                             for e in policy['exemptions'])
                if not exempt:
                    findings.append(f'{p}: coverage regression ({old:.2f}% -> {new:.2f}%)'
                                    if new is not None else p + ': current package coverage unavailable')
    enforced = policy['mode'] == 'enforce'
    if enforced and not policy.get('approval_url'):
        raise ValueError('enforcement requires a reviewed policy approval_url')
    return {'mode': policy['mode'], 'result': 'fail' if enforced and findings else
            'advisory' if findings else 'pass', 'findings': findings}


def fetch_baseline(sha, platform):
    """Read data artifacts only, from successful same-repo main push runs."""
    repo, token = os.environ.get('GITHUB_REPOSITORY'), os.environ.get('GH_TOKEN')
    if not repo or not token:
        return None

    class SafeRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
            if redirected is not None:
                redirected.remove_header('Authorization')
            return redirected

    opener = urllib.request.build_opener(SafeRedirect)

    def request(path):
        req = urllib.request.Request('https://api.github.com/repos/' + repo + path,
                                     headers={'Authorization': 'Bearer ' + token,
                                              'Accept': 'application/vnd.github+json'})
        with opener.open(req, timeout=15) as response:
            return response.read()
    runs = json.loads(request('/actions/workflows/go.yml/runs?event=push&status=success&head_sha=' + sha))
    for run in runs['workflow_runs'][:3]:
        if run['head_sha'] != sha or run['head_branch'] != 'main':
            continue
        artifacts = json.loads(request(f"/actions/runs/{run['id']}/artifacts"))
        for artifact in artifacts['artifacts']:
            if artifact['name'] == 'coverage-' + platform and not artifact['expired']:
                data = request(f"/actions/artifacts/{artifact['id']}/zip")
                with zipfile.ZipFile(io.BytesIO(data)) as archive:
                    report = json.loads(archive.read('coverage.json'))
                if (report['head_sha'] == sha and report['platform'] == platform and
                        report.get('collection_status') == 'success' and
                        report.get('schema_version') == 1):
                    return report['packages']
    return None


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--profile', default='reports/coverage.out')
    parser.add_argument('--platform', required=True)
    args = parser.parse_args()
    head = git('rev-parse', 'HEAD')
    event_path = os.environ.get('GITHUB_EVENT_PATH')
    event = json.loads(Path(event_path).read_text()) if event_path else {}
    base = select_base(event, head)
    blocks = profile(Path(args.profile).read_text())
    current = packages(blocks)
    changed = changed_coverage(blocks, changed_lines(base, head))
    policy = json.loads(Path('.github/coverage-policy.json').read_text())
    baseline, warning = None, None
    try:
        baseline = fetch_baseline(base, args.platform)
    except Exception as exc:
        warning = 'Baseline retrieval unavailable: ' + str(exc)
    source_trees = {}
    for package in policy['protected_packages']:
        if Path(package).is_dir():
            source_trees[package] = git('rev-parse', head + ':' + package)
    result = evaluate(current, baseline, changed, policy, source_trees)
    collection_status = os.environ.get('COVERAGE_TEST_RESULT', 'unverified')
    if collection_status != 'success':
        result['findings'].append('test collection did not succeed; profile is diagnostic only')
        result['result'] = 'fail' if policy['mode'] == 'enforce' else 'advisory'
    total = sum(v['statements'] for v in current.values())
    covered = sum(v['covered'] for v in current.values())
    aggregate = dict(covered=covered, statements=total, percent=100*covered/total if total else None)
    report = dict(schema_version=1, collection_status=collection_status, source_trees=source_trees, aggregate=aggregate, head_sha=head, baseline_sha=base, platform=args.platform,
                  packages=current, baseline_packages=baseline, changed=changed, policy=result,
                  warning=warning)
    Path('reports').mkdir(exist_ok=True)
    Path('reports/coverage.json').write_text(json.dumps(report, indent=2) + '\n')
    rows = [f"## Go coverage ({args.platform})", f"Head: `{head}`; baseline: `{base}`",
            f"Policy: **{result['result']}** ({result['mode']})",
            f"Platform total: {covered}/{total} statements",
            f"Changed block-span lines: {changed['covered']}/{changed['lines']} " +
            (f"({changed['percent']:.2f}%)" if changed['lines'] else '(no executable changes)'),
            '', '| Package | Covered statements | Coverage | Baseline | Delta (pp) |',
            '|---|---:|---:|---:|---:|']
    for p, stats in current.items():
        percent = stats['percent']
        old = (baseline or {}).get(p, {}).get('percent')
        baseline_text = f'{old:.2f}%' if old is not None else 'unavailable'
        delta = f'{percent-old:+.2f}' if percent is not None and old is not None else 'n/a'
        rows.append(f"| {p} | {stats['covered']}/{stats['statements']} | " +
                    (f'{percent:.2f}%' if percent is not None else 'n/a') +
                    f' | {baseline_text} | {delta} |')
    rows += ['', *result['findings'], warning or '',
             'Go only; Portal JavaScript and browser evidence are separate.']
    summary = '\n'.join(rows) + '\n'
    Path('reports/coverage.md').write_text(summary)
    if os.environ.get('GITHUB_STEP_SUMMARY'):
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as out:
            out.write(summary)
    print(summary)
    return result['result'] == 'fail'


if __name__ == '__main__':
    raise SystemExit(main())
