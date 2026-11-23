#!/usr/bin/env python3
"""Read-only acceptance snapshot for the explicitly selected two-node test cluster."""
import collections
import json
import subprocess

KUBECTL = ["kubectl", "--kubeconfig=/etc/kubernetes/admin.conf"]
NODES = {"pve-test03-z04.bozon.test", "pve-test04-z04.bozon.test"}


def read(*args):
    return json.loads(subprocess.check_output(KUBECTL + list(args) + ["-o", "json"]))


def main():
    nodes = read("get", "nodes")["items"]
    assert {node["metadata"]["name"] for node in nodes} == NODES
    uids = {node["metadata"]["uid"] for node in nodes}
    subnets = [item for item in read("get", "subnets")["items"] if item["spec"].get("openPERouterL2")]
    not_ready = []
    for subnet in subnets:
        if not any(item["type"] == "Ready" and item["status"] == "True" for item in subnet.get("status", {}).get("conditions", [])):
            not_ready.append(subnet["metadata"]["name"])
    phases = collections.Counter()
    barriers = 0
    stale = []
    for item in read("-n", "kube-system", "get", "configmaps")["items"]:
        if not item["metadata"]["name"].startswith("kube-ovn-opr-guard-"):
            continue
        barriers += 1
        state = json.loads(item["data"]["state"])
        if set(state["members"]) != uids:
            stale.append(item["metadata"]["name"] + ":membership")
        for member in state["members"].values():
            phases[member["phase"]] += 1
            if not member["session"] or member["phase"] != "active" or member.get("revision") != state["revision"]:
                stale.append(item["metadata"]["name"])
    pods = read("-n", "kube-system", "get", "pods")["items"]
    unready = [pod["metadata"]["name"] for pod in pods if not any(condition["type"] == "Ready" and condition["status"] == "True" for condition in pod.get("status", {}).get("conditions", []))]
    result = {"l2_count": len(subnets), "l2_not_ready": not_ready, "barriers": barriers, "phases": dict(phases), "stale": stale, "unready_system_pods": unready}
    print(json.dumps(result, sort_keys=True))
    assert len(subnets) == 26 and barriers == 26 and phases == {"active": 52}
    assert not not_ready and not stale and not unready


if __name__ == "__main__":
    main()
