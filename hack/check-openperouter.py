#!/usr/bin/env python3
"""Reproduce a pinned source/API review; never change networking or use Kubernetes."""

import argparse
import hashlib
import json
from pathlib import Path
import sys


def audit(source, evidence):
    """Only inspect the allowlisted files whose exact bytes were reviewed."""
    for name, expected in evidence["sha256"].items():
        actual = hashlib.sha256((source / name).read_bytes()).hexdigest()
        if actual != expected:
            raise ValueError(f"unreviewed source: {name}: expected {expected}, got {actual}")

    # These are findings about the pinned source, not capability detection for
    # arbitrary later versions. Refuse different bytes rather than infer support.
    return {
        "commit": evidence["commit"],
        "status": "blocked" if evidence["blockers"] else "api-reviewed",
        "blockers": evidence["blockers"],
        "limitations": evidence.get("limitations", []),
        "capabilities": evidence.get("capabilities", []),
        "pendingValidation": evidence.get("pendingValidation", []),
        "integrationBlockers": evidence.get("integrationBlockers", []),
        "dataplaneTested": False,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path, help="unpacked OpenPERouter source directory")
    profiles = {
        "upstream": "upstream-evidence.json",
        "egor-fork": "fork-evidence.json",
        "mac-settings": "mac-settings-evidence.json",
    }
    parser.add_argument("--profile", choices=profiles, default="mac-settings")
    args = parser.parse_args()
    evidence_path = Path(__file__).resolve().parents[1] / "docs/openperouter" / profiles[args.profile]
    try:
        evidence = json.loads(evidence_path.read_text())
        result = audit(args.source, evidence)
    except (OSError, ValueError) as err:
        print(f"audit unavailable: {err}", file=sys.stderr)
        return 1
    print(json.dumps(result, indent=2))
    return 2 if result["blockers"] else 0


if __name__ == "__main__":
    sys.exit(main())
