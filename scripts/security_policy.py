#!/usr/bin/env python3
"""Validate reviewed, exact secret exceptions and render a temporary ignore file."""
import datetime
import json
import re
import sys
from pathlib import Path


def validate(entries, today):
    if not isinstance(entries, list):
        raise ValueError("exceptions must be a list")
    fingerprints = []
    for entry in entries:
        if set(entry) != {"fingerprint", "owner", "rationale", "expires"}:
            raise ValueError("exception requires fingerprint, owner, rationale, expires")
        if not all(isinstance(value, str) and value.strip() for value in entry.values()):
            raise ValueError("exception fields must be nonempty strings")
        if not re.fullmatch(r"[a-f0-9]{40}:[^\s:*?\[\]#]+:[\w-]+:[1-9][0-9]*", entry["fingerprint"]):
            raise ValueError("exception must match one exact Gitleaks fingerprint")
        if datetime.date.fromisoformat(entry["expires"]) <= today:
            raise ValueError("security exception expired")
        if entry["fingerprint"] in fingerprints:
            raise ValueError("duplicate exception")
        fingerprints.append(entry["fingerprint"])
    return "\n".join(fingerprints) + "\n"


if __name__ == "__main__":
    try:
        result = validate(json.loads(Path(".github/security-exceptions.json").read_text()), datetime.datetime.now(datetime.timezone.utc).date())
        Path(sys.argv[1]).write_text(result)
    except (ValueError, TypeError, KeyError, OSError, IndexError):
        raise SystemExit("Invalid or expired security policy; review exceptions locally") from None
