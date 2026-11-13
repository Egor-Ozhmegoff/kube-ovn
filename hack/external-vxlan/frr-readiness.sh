#!/usr/bin/env bash
set -euo pipefail
# Stop only the disposable kind node's kubelet, keeping its workloads/OVS alive.
: "${KUBECONFIG:?Set the dedicated test kubeconfig}"
: "${EXTERNAL_VXLAN_LAB:?Set the dedicated kind cluster}"
: "${EXTERNAL_VXLAN_GATEWAY_NODE:?Set the first kind node}"
: "${EXTERNAL_VXLAN_SECOND_NODE:?Set the second kind node}"
prefix=$EXTERNAL_VXLAN_LAB
first=$EXTERNAL_VXLAN_GATEWAY_NODE
second=$EXTERNAL_VXLAN_SECOND_NODE
kc() { kubectl --cache-dir "${EXTERNAL_VXLAN_KUBECTL_CACHE:-/tmp/ko-evx-kubectl}" "$@"; }
[[ $(docker inspect --format '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$first") == "$prefix" ]] || { echo 'Refusing non-test node' >&2; exit 1; }
[[ $(kc get vpc evx-tenant220 -o 'jsonpath={.spec.externalVXLAN.frr.bridge}') == evb220 ]] || exit 1
cid=$(kc -n evx-pods220 get pod secondary-1 -o 'jsonpath={.status.containerStatuses[0].containerID}')
cid=${cid#containerd://}
address=$(kc -n evx-pods220 get pod secondary-1 -o 'go-template={{index .metadata.annotations "evx-secondary220.kubevirt-networks.ovn.kubernetes.io/ip_address"}}')
restore() { docker exec "$first" systemctl start kubelet; }
trap restore EXIT
docker exec "$first" systemctl stop kubelet
withdrawn=false
for attempt in {1..120}; do
  if [[ $(kc get vpc evx-tenant220 -o 'jsonpath={.status.externalVXLANReadyNodes[*]}') == "$second" ]]; then withdrawn=true; break; fi
  sleep 2
done
[[ $withdrawn == true ]] || { echo 'Kubernetes readiness did not withdraw first egress' >&2; exit 1; }
kc get node "$first" -o 'jsonpath={.status.conditions}'
echo
# kubectl exec depends on the stopped kubelet, so use the kind CRI directly.
docker exec "$first" crictl exec "$cid" lab-ping -I net1 -n -c 3 -W 2 -M do -s 8972 172.20.220.10
docker exec "$prefix-peer" ip netns exec r220 lab-ping -n -c 3 -W 2 -M do -s 8972 "$address"
restore
kc wait "node/$first" --for=condition=Ready --timeout=120s
recovered=false
for attempt in {1..60}; do
  nodes=$(kc get vpc evx-tenant220 -o 'jsonpath={.status.externalVXLANReadyNodes[*]}')
  if [[ " $nodes " == *" $first "* && " $nodes " == *" $second "* ]]; then recovered=true; break; fi
  sleep 2
done
[[ $recovered == true ]] || { echo 'Recovered node did not regain egress' >&2; exit 1; }
echo "Kubernetes membership recovered at $(date -u +%FT%TZ); checking workload forwarding"
recovery_traffic() {
  local pod_address
  for kind in pods vm; do
    kc -n "evx-${kind}220" exec endpoint-1 -- lab-ping -n -c 1 -W 1 -M do -s 8972 172.20.220.10 || return
  done
  kc -n evx-pods220 exec secondary-1 -- lab-ping -I net1 -n -c 1 -W 1 -M do -s 8972 172.20.220.10 || return
  pod_address=$(kc -n evx-pods220 get pod endpoint-1 -o 'jsonpath={.status.podIP}')
  docker exec "$prefix-peer" ip netns exec r220 lab-ping -n -c 1 -W 1 -M do -s 8972 "$pod_address"
}
traffic_ready=false
for attempt in {1..90}; do
  if recovery_traffic; then traffic_ready=true; break; fi
  sleep 2
done
[[ $traffic_ready == true ]] || { echo 'Ready node did not recover Pod, VM and secondary forwarding' >&2; exit 1; }
trap - EXIT
echo "PASS: Kubernetes NotReady withdraws egress while retaining live VM ingress; membership and workload forwarding recovered at $(date -u +%FT%TZ)"
