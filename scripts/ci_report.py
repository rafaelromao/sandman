"""Summarize Go JSON streams and non-test validation failures without hiding exits."""
import collections
import json
import os
from pathlib import Path
import sys

category, code, seconds = sys.argv[1:]
root = Path('reports')
events, failures = [], []
counts = collections.Counter()
for line in (root / f'{category}.log').read_text(errors='replace').splitlines():
    try:
        event = json.loads(line)
    except ValueError:
        continue
    if not isinstance(event, dict) or 'Action' not in event:
        continue
    events.append(event)
    if event['Action'] in ('pass', 'fail', 'skip') and event.get('Test'):
        counts[event['Action']] += 1
    if event['Action'] == 'fail':
        failures.append({'package': event.get('Package', ''),
                         'test': event.get('Test', 'package/build failure')})
(root / f'{category}.jsonl').write_text(''.join(json.dumps(e) + '\n' for e in events))
report = dict(category=category, exit_code=int(code), duration_seconds=int(seconds),
              counts=dict(counts), failures=failures, artifact=f'{category}.log')
(root / f'{category}.json').write_text(json.dumps(report, indent=2) + '\n')
summary = f'## {category}: {"PASS" if code == "0" else "FAIL"}\n\n'
summary += f'Duration: {seconds}s; exit: {code}; tests: {dict(counts)}.\n\n'
summary += f'Artifact: `{category}-{os.environ.get("RUNNER_OS", "local")}`; raw log: `reports/{category}.log`; structured report: `{category}.json`.\n\n'
for fail in failures:
    summary += f'- `{fail["package"]}` / `{fail["test"]}`\n'
if code != '0' and not failures:
    summary += '- Command/setup failure; see the raw log for compiler or validation diagnostics.\n'
(root / f'{category}.md').write_text(summary)
if os.environ.get('GITHUB_STEP_SUMMARY'):
    with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as stream:
        stream.write(summary)
print(summary)
