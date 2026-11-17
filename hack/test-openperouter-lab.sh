#!/usr/bin/env bash
# Read-only probes against an explicitly labelled disposable Kubernetes lab.
# Does not create resources, withdraw routes, migrate VMs or touch host links.
set -euo pipefail
if [[ $# != 4 ]]; then
  echo "Usage: $0 /tmp/lab-kubeconfig context base|withdrawn external-IP" >&2
  exit 2
fi
config=$1
context=$2
phase=$3
external=$4
case "$phase" in base|withdrawn) ;; *) echo 'Unknown phase' >&2; exit 2;; esac
k=(kubectl --kubeconfig "$config" --context "$context")
ns=tenant-a-pods
label=$("${k[@]}" get namespace "$ns" -o 'jsonpath={.metadata.labels.openperouter\.kubeovn\.io/disposable-lab}')
[[ "$label" == true ]] || { echo 'Disposable lab label is required' >&2; exit 2; }
for pair in probe-a:node-a probe-b:node-b; do
  pod=${pair%:*}
  node=${pair#*:}
  actual=$("${k[@]}" -n "$ns" get pod "$pod" -o 'jsonpath={.spec.nodeName}')
  [[ "$actual" == "$node" ]] || { echo "Unexpected placement: $pod on $actual" >&2; exit 1; }
  "${k[@]}" -n "$ns" wait --for=condition=Ready "pod/$pod" --timeout=30s
  target=probe-a
  [[ "$pod" == probe-a ]] && target=probe-b
  remote=$("${k[@]}" -n "$ns" get pod "$target" -o 'jsonpath={.status.podIP}')
  "${k[@]}" -n "$ns" exec "$pod" -- ping -c 3 -W 2 "$remote"
  if [[ "$phase" == base ]]; then
    "${k[@]}" -n "$ns" exec "$pod" -- ping -c 3 -W 2 "$external"
  elif "${k[@]}" -n "$ns" exec "$pod" -- ping -c 3 -W 2 "$external"; then
    echo "$pod still reaches withdrawn destination" >&2
    exit 1
  fi
  vip=$("${k[@]}" -n "$ns" get service probes -o 'jsonpath={.spec.clusterIP}')
  "${k[@]}" -n "$ns" exec "$pod" -- wget -q -T 5 -O - "http://$vip:8080/"
done
echo "Probe phase $phase passed; captures and the remaining acceptance matrix are still required."
