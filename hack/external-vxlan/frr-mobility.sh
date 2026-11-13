#!/usr/bin/env bash
set -euo pipefail
# VIF mobility, not a KubeVirt live-migration test. Guest IP/MAC/netns stay put;
# its veth attachment moves between the two disposable OVN chassis.
prefix=${EXTERNAL_VXLAN_LAB:-ko-frr}
fabric=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.191}
image=${EXTERNAL_VXLAN_IMAGE:?Set the pinned OVN lab image}
for name in hv1 hv2 peer; do
  [[ $(docker inspect --format '{{index .Config.Labels "kube-ovn.external-vxlan-lab"}}' "$prefix-$name") == "$prefix" ]] || exit 1
done
first=$(docker inspect --format '{{.State.Pid}}' "$prefix-hv1")
second=$(docker inspect --format '{{.State.Pid}}' "$prefix-hv2")
move_link() {
  # Host PID visibility is limited to this short-lived helper; its network
  # namespace is a lab chassis, never the host network namespace.
  docker run --rm --privileged --pid=host --network "container:$prefix-$1" --entrypoint ip "$image" link set migrate220 netns "$2"
}
restore() {
  docker exec "$prefix-hv2" ovs-vsctl --if-exists del-port br-int migrate220 || true
  docker exec "$prefix-hv2" ip link set migrate220 down || true
  move_link hv2 "$first" || true
  docker exec "$prefix-hv1" sh -c 'ip link set migrate220 name vm220; ip link set vm220 up; ovs-vsctl --may-exist add-port br-int vm220 -- set Interface vm220 external_ids:iface-id=vm220-hv1' || true
}
docker exec "$prefix-hv1" ovs-vsctl del-port br-int vm220
docker exec "$prefix-hv1" ip link set vm220 down
docker exec "$prefix-hv1" ip link set vm220 name migrate220
move_link hv1 "$second"
trap restore EXIT
docker exec "$prefix-hv2" sh -c 'ip link set migrate220 up; ovs-vsctl add-port br-int migrate220 -- set Interface migrate220 external_ids:iface-id=vm220-hv1'
for attempt in {1..45}; do
  if docker exec "$prefix-peer" bridge fdb show dev evx504221 | grep -i '02:00:dc:01:00:02' | grep -q "dst $fabric.12"; then break; fi
  [[ $attempt != 45 ]] || { echo 'MAC/IP did not move to the second VTEP' >&2; exit 1; }
  sleep 1
done
docker exec "$prefix-peer" bridge fdb show dev evx504221
docker exec "$prefix-frr-peer" vtysh -c 'show bgp l2vpn evpn route'
docker exec "$prefix-peer" ip netns exec v220 lab-ping -n -c 3 -W 2 -M do -s 8972 172.16.133.2
docker exec "$prefix-hv1" ip netns exec vm220 lab-ping -n -c 3 -W 2 -M do -s 8972 172.20.220.10
docker exec "$prefix-hv1" ip -n vm220 neigh show 172.16.133.1
restore
trap - EXIT
echo 'PASS: VIF MAC/IP moved through FRR EVPN to VTEP .12; guest IP/MAC and OVN gateway retained'
