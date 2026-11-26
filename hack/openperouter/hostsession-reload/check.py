#!/usr/bin/env python3
"""Reject failed reloads even when the sampled runtime has the desired state."""

import json
from pathlib import Path
import re
import sys


def check_results(root):
    errors = []
    for mode in ("cold", "reload"):
        directory = root / mode
        rows = [line.split("\t") for line in (directory / "status.tsv").read_text().splitlines()]
        if len(rows) != 3 or [row[0] for row in rows] != ["1", "2", "3"]:
            errors.append(f"{mode}: expected exactly three reload attempts")
        for row in rows:
            if len(row) != 3 or row[1:] != ["0", "0"]:
                errors.append(f"{mode}: attempt/test/reload={row}")

        if mode == "reload":
            before = (directory / "start-running.conf").read_text()
            for command in ("neighbor 169.254.16.13 remote-as 64515",
                            "neighbor fd00:16::d remote-as 64515",
                            "network 169.254.16.13/32", "network fd00:16::d/128"):
                if command not in before:
                    errors.append(f"reload: positive control missing {command}")
            for family, prefix in ((4, "169.254.16.13/32"), (6, "fd00:16::d/128")):
                rib = json.loads((directory / f"start-rib{family}.json").read_text())
                if prefix not in rib.get("routes", {}):
                    errors.append(f"reload: positive BGP RIB control missing {prefix}")

        samples = [f"reload-{attempt}" for attempt in (1, 2, 3)] + ["restart"]
        if mode == "cold":
            samples.insert(0, "start")
        for sample in samples:
            running = (directory / f"{sample}-running.conf").read_text()
            if re.search(r"^\s*neighbor (169\.254\.16\.13|fd00:16::d)\s", running, re.M):
                errors.append(f"{mode}/{sample}: local neighbor remains")
            if re.search(r"^\s*network (169\.254\.16\.13/32|fd00:16::d/128)(?:\s|$)", running, re.M):
                errors.append(f"{mode}/{sample}: attachment network remains")
            for command in ("neighbor 192.0.2.2 remote-as 64512",
                            "redistribute kernel route-map OVN_SUBNET4",
                            "redistribute kernel route-map OVN_SUBNET6",
                            "match metric 8399656",
                            "ip prefix-list OVN_SUBNET4 seq 10 permit 10.101.3.0/24",
                            "ipv6 prefix-list OVN_SUBNET6 seq 10 permit fd00:3::/64"):
                if command not in running:
                    errors.append(f"{mode}/{sample}: missing {command}")
            for family, prefix in ((4, "169.254.16.13/32"), (6, "fd00:16::d/128")):
                rib = json.loads((directory / f"{sample}-rib{family}.json").read_text())
                if prefix in rib.get("routes", {}):
                    errors.append(f"{mode}/{sample}: attachment {prefix} remains in BGP RIB")
    return errors


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: python3 check.py RESULTS_DIR")
    try:
        failures = check_results(Path(sys.argv[1]))
    except (OSError, ValueError, IndexError) as error:
        sys.exit(f"INCOMPLETE results: {error}")
    if failures:
        print("FAIL: configuration contract has not passed normal reload", file=sys.stderr)
        sys.exit("\n".join(failures))
    print("PASS: reload and sampled configuration checks (external peering/dataplane not tested)")
