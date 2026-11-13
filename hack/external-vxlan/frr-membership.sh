#!/usr/bin/env bash
set -euo pipefail
# Destructive API exercise ONLY for the disposable two-node FRR kind fixture.
: "${KUBECONFIG:?Set the dedicated test kubeconfig}"
: "${EXTERNAL_VXLAN_LAB:?Set the dedicated kind cluster name}"
: "${EXTERNAL_VXLAN_GATEWAY_NODE:?Set the first kind node}"
: "${EXTERNAL_VXLAN_SECOND_NODE:?Set the second kind node}"
prefix=$EXTERNAL_VXLAN_LAB
first=$EXTERNAL_VXLAN_GATEWAY_NODE
second=$EXTERNAL_VXLAN_SECOND_NODE
kc() { kubectl --cache-dir "${EXTERNAL_VXLAN_KUBECTL_CACHE:-/tmp/ko-evx-kubectl}" "$@"; }
for node in "$first" "$second"; do
  [[ $(docker inspect --format '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$node") == "$prefix" ]] || { echo "Refusing non-test kind node $node" >&2; exit 1; }
done
[[ $(kc get vpc evx-tenant220 -o 'jsonpath={.spec.externalVXLAN.frr.bridge}') == evb220 ]] || { echo 'Expected the FRR test fixture' >&2; exit 1; }
original=$(kc get vpc evx-tenant220 -o 'jsonpath={.spec.externalVXLAN.gatewayNodes}')
restore() { kc patch vpc evx-tenant220 --type=merge -p "{\"spec\":{\"externalVXLAN\":{\"gatewayNodes\":$original}}}"; }
trap restore EXIT
retry() { for attempt in {1..90}; do if "$@"; then return; fi; sleep 1; done; return 1; }
only_second() { [[ $(kc get vpc evx-tenant220 -o 'jsonpath={.status.externalVXLANReadyNodes[*]}') == "$second" ]]; }
both_ready() {
  local nodes
  nodes=$(kc get vpc evx-tenant220 -o 'jsonpath={.status.externalVXLANReadyNodes[*]}')
  [[ " $nodes " == *" $first "* && " $nodes " == *" $second "* ]]
}
probe() {
  local address
  for kind in pods vm; do
    kc -n "evx-${kind}220" exec endpoint-1 -- lab-ping -n -c 2 -W 2 -M do -s 8972 172.20.220.10 || return
  done
  kc -n evx-pods220 exec secondary-1 -- lab-ping -I net1 -n -c 2 -W 2 -M do -s 8972 172.20.220.10 || return
  address=$(kc -n evx-pods220 get pod secondary-1 -o 'go-template={{index .metadata.annotations "evx-secondary220.kubevirt-networks.ovn.kubernetes.io/ip_address"}}')
  docker exec "$prefix-peer" ip netns exec r220 lab-ping -n -c 2 -W 2 -M do -s 8972 "$address"
}
retry both_ready
retry probe
echo 'Disable first egress while keeping its VM ingress identity'
kc patch vpc evx-tenant220 --type=merge -p "{\"spec\":{\"externalVXLAN\":{\"gatewayNodes\":[{\"node\":\"$first\",\"transitCIDR\":\"169.254.220.11/24\",\"routingPort\":\"lr-transit220\",\"egress\":false},{\"node\":\"$second\",\"transitCIDR\":\"169.254.220.12/24\",\"routingPort\":\"lr-transit220-b\"}]}}}"
retry only_second
retry probe
echo 'Remove first member while its VPC IP allocations still exist'
kc patch vpc evx-tenant220 --type=merge -p "{\"spec\":{\"externalVXLAN\":{\"gatewayNodes\":[{\"node\":\"$second\",\"transitCIDR\":\"169.254.220.12/24\",\"routingPort\":\"lr-transit220-b\"}]}}}"
retry only_second
# Allow the periodic reconciliation to run as well as the immediate update.
sleep 35
retry probe
restore
retry both_ready
retry probe
trap - EXIT
echo 'PASS: API egress disable, membership removal with retained ingress, and recovery'
