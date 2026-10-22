#!/usr/bin/env python3
"""Fail-closed release scenario accounting over Go JSON events."""
import argparse
import collections
import json
import os
import sys
from pathlib import Path


def evaluate(manifest, tier, events, returncode, platform=None, timed_out=False):
    required = {}
    errors = []
    for scenario in manifest['scenarios']:
        if scenario['tier'] != tier or (scenario.get('platforms') and platform not in scenario['platforms']):
            continue
        key = (scenario['package'], scenario['test'])
        if key in required:
            errors.append(f'duplicate manifest scenario: {key}')
        required[key] = scenario
    terminal, started, outputs = {}, set(), collections.defaultdict(str)
    for event in events:
        key = (event.get('Package', ''), event.get('Test', ''))
        action = event.get('Action')
        if action == 'output':
            outputs[key] += event.get('Output', '')
        if action == 'run':
            if key in started:
                errors.append(f'duplicate execution: {key}')
            started.add(key)
        if action in ('pass', 'skip', 'fail') and key[1]:
            if key in terminal:
                errors.append(f'duplicate terminal result: {key}')
            terminal[key] = action
    timed_out = bool(returncode) and (timed_out or returncode == 124 or any(
        'panic: test timed out' in value for value in outputs.values()))
    unfinished = started - terminal.keys()
    if unfinished:
        errors.append(f'{len(unfinished)} started tests have no terminal result')
    if any(action == 'fail' for action in terminal.values()):
        errors.append('observed failing tests')
    rows = []
    for key, scenario in required.items():
        action = terminal.get(key, ('timed_out' if timed_out else 'interrupted') if key in unfinished else 'missing')
        reason = outputs[key].strip()[-8000:] if action != 'pass' else ''
        if action == 'skip':
            reason = outputs[key].strip()
            permitted = scenario.get('permitted_skip')
            if not permitted or permitted not in reason:
                errors.append(f'required scenario skipped: {key}')
        elif action != 'pass':
            errors.append(f'required scenario {action}: {key}')
        rows.append(dict(package=key[0], test=key[1], status=action, reason=reason))
    # A passing parent must never hide skipped provider/scenario subtests.
    for key, action in terminal.items():
        root = (key[0], key[1].split('/')[0])
        if root in required and key != root and action == 'skip':
            errors.append(f'required subscenario skipped: {key}: {outputs[key].strip()}')
    if not required:
        errors.append('empty required scenario selection')
    if returncode:
        errors.append(f'test process exit {returncode}')
    counts = {action: sum(value == action for value in terminal.values()) for action in ('pass', 'skip', 'fail')}
    observations = [dict(package=key[0], test=key[1], status=action, reason=outputs[key].strip() if action != 'pass' else '') for key, action in terminal.items()]
    counts['executed'] = len(started)
    counts['expected_skips'] = sum(row['status'] == 'skip' and required[(row['package'], row['test'])].get('permitted_skip') in row['reason'] for row in rows if required[(row['package'], row['test'])].get('permitted_skip'))
    counts['missing'] = sum(row['status'] == 'missing' for row in rows)
    counts['timed_out'] = len(unfinished) if timed_out else 0
    counts['interrupted'] = len(unfinished) if not timed_out else 0
    observations.extend(dict(package=key[0], test=key[1], status='timed_out' if timed_out else 'interrupted', reason=outputs[key].strip()[-8000:]) for key in unfinished)
    return dict(success=not errors, counts=counts, errors=errors, scenarios=rows, observations=observations,
                classification='timeout' if timed_out else 'failure' if errors else 'pass')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('tier')
    parser.add_argument('log')
    parser.add_argument('returncode', type=int)
    parser.add_argument('output')
    args = parser.parse_args()
    manifest = json.loads(Path('scripts/release_scenarios.json').read_text())
    events = []
    raw = Path(args.log).read_text(errors='replace') if Path(args.log).exists() else ''
    for line in raw.splitlines():
        try:
            event = json.loads(line)
            if isinstance(event, dict):
                events.append(event)
        except ValueError:
            pass  # Go's timeout panic also emits non-JSON stderr.
    result = evaluate(manifest, args.tier, events, args.returncode, os.environ.get('RELEASE_PLATFORM', 'linux/amd64'), 'panic: test timed out' in raw or 'timeout: sending signal' in raw)
    result.update(sha=os.environ['GITHUB_SHA'], tier=args.tier,
                  providers={name: value for name, value in manifest['providers'].items()
                             if args.tier not in ('live', 'claude') or name == ('opencode' if args.tier == 'live' else 'claude')}, platform=os.environ.get('RELEASE_PLATFORM', 'linux/amd64'),
                  versions=Path('regression-artifacts/versions.txt').read_text() if Path('regression-artifacts/versions.txt').exists() else 'unavailable',
                  prerequisites=os.environ.get('RELEASE_PREREQUISITES', 'not recorded'))
    Path(args.output).parent.mkdir(parents=True, exist_ok=True)
    Path(args.output).write_text(json.dumps(result, indent=2) + '\n')
    summary = (
        f"\n## Release {args.tier}: {result['classification']}\n"
        f"SHA: `{result['sha']}` — {result['platform']}\n\n"
        f"Counts (tests and subtests): `{json.dumps(result['counts'])}`\n\n"
        f"Versions:\n```text\n{result['versions']}\n```\n"
        f"Models: `{json.dumps(result['providers'])}`\n\n"
        f"Prerequisites: `{result['prerequisites']}`\n\n"
        + '\n'.join('- ' + error[:500] for error in result['errors'][:50])
        + '\n\nFull scenario results and reasons: `report.json` in this job’s artifact.\n'
    )
    if os.environ.get('GITHUB_STEP_SUMMARY'):
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as stream:
            stream.write(summary)
    # Name each failure in the job log too; the step summary and artifact are easy to miss.
    for error in result['errors'][:50]:
        print(f'::error::{error[:500]}', file=sys.stderr)
    raise SystemExit(0 if result['success'] else 1)


if __name__ == '__main__':
    main()
