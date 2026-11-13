#!/usr/bin/env bash
set -euo pipefail
export EXTERNAL_VXLAN_FABRIC_PREFIX=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.191}
export EXTERNAL_VXLAN_CLUSTER_PREFIX=${EXTERNAL_VXLAN_CLUSTER_PREFIX:-10.110.192}
dir=$(cd "$(dirname "$0")" && pwd)
export EXTERNAL_VXLAN_LAB=${EXTERNAL_VXLAN_LAB:-ko-frr}
export EXTERNAL_VXLAN_NODES=${EXTERNAL_VXLAN_NODES:-4}
prefix=$EXTERNAL_VXLAN_LAB
artifacts=${EXTERNAL_VXLAN_ARTIFACTS:-/tmp/$prefix-artifacts}
mkdir -p "$artifacts"
dc() { docker "$@"; }
nb() { dc exec "$prefix-central" ovn-nbctl --db=tcp:127.0.0.1:6641 "$@"; }
sb() { dc exec "$prefix-$1" ovn-sbctl --db=tcp:127.0.0.1:6642 --bare --no-heading --columns=ip_prefix find Learned_Route "${@:2}"; }
frr() { dc exec "$prefix-frr-peer" vtysh -c 'configure terminal' -c 'router bgp 65000 vrf vrf220' -c 'address-family l2vpn evpn' -c "$1"; }
retry() { for attempt in {1..45}; do if "$@"; then return; fi; sleep 1; done; return 1; }
no_forward_route() { [[ -z $(sb central ip_prefix=172.20.220.0/24) ]]; }
has_forward_route() { [[ -n $(sb central ip_prefix=172.20.220.0/24) ]]; }
no_reverse_first() { [[ -z $(sb peer-central ip_prefix=172.16.0.0/24 nexthop=${EXTERNAL_VXLAN_FABRIC_PREFIX}.11) ]]; }
has_reverse_first() { [[ -n $(sb peer-central ip_prefix=172.16.0.0/24 nexthop=${EXTERNAL_VXLAN_FABRIC_PREFIX}.11) ]]; }
external_ping() { dc exec "$prefix-hv1" ip netns exec pods220 lab-ping -n -c 2 -W 2 -M do -s 8972 172.20.220.10; }
restore_gateway() {
  nb --may-exist lrp-add tenant220 lr-transit220 02:ee:0b:00:00:00 169.254.220.11/24
  nb -- lrp-set-gateway-chassis lr-transit220 hv1 100 \
    -- set Logical_Router_Port lr-transit220 options:dynamic-routing-port-name=lr-transit220 options:dynamic-routing-maintain-vrf=false \
    -- --may-exist lsp-add transit220 transit220-lr \
    -- lsp-set-type transit220-lr router -- lsp-set-addresses transit220-lr router \
    -- lsp-set-options transit220-lr router-port=lr-transit220
  nb remove Logical_Router_Port lr-transit220 options dynamic-routing-no-learning
}
cleanup() {
  frr 'advertise ipv4 unicast' >/dev/null 2>&1 || true
  restore_gateway >/dev/null 2>&1 || true
  dc exec "$prefix-peer" ovs-vsctl set Interface v230 external_ids:iface-id=v230 >/dev/null 2>&1 || true
  for index in $(seq 1 "$EXTERNAL_VXLAN_NODES"); do
    dc exec "$prefix-hv$index" sh -c 'test ! -f /tmp/frr-capture.pid || kill -INT "$(cat /tmp/frr-capture.pid)"' || true
    dc cp "$prefix-hv$index:/tmp/frr-acceptance.pcap" "$artifacts/hv$index.pcap" || true
  done
}
trap cleanup EXIT
for index in $(seq 1 "$EXTERNAL_VXLAN_NODES"); do
  dc exec "$prefix-hv$index" sh -c 'tcpdump -Z root -i eth0 -U -s 0 -w /tmp/frr-acceptance.pcap "udp port 4789 or udp port 60000" >/tmp/frr-capture.log 2>&1 & echo $! >/tmp/frr-capture.pid'
done
echo 'Baseline: native FRR routes, both directions, tenant isolation and MTU9000'
retry has_forward_route
bash "$dir/lab.sh" check
echo 'Multiple flow hashes exercise every active egress gateway'
for tenant in 220 230; do
  addresses="02:ff:00:00:00:02 172.20.$tenant.10"
  for host in {11..18}; do
    dc exec "$prefix-peer" ip -n "r$tenant" address replace "172.20.$tenant.$host/32" dev eth0
    addresses="$addresses 172.20.$tenant.$host"
  done
  dc exec "$prefix-peer-central" ovn-nbctl --db=tcp:127.0.0.1:6641 lsp-set-addresses "r$tenant" "$addresses"
  for index in $(seq 1 "$EXTERNAL_VXLAN_NODES"); do
    for host in {11..18}; do
      dc exec "$prefix-hv$index" ip netns exec "pods$tenant" lab-ping -n -c 1 -W 2 -M do -s 8972 "172.20.$tenant.$host"
    done
  done
done
echo 'Type-5 withdrawal and restoration'
frr 'no advertise ipv4 unicast'
retry no_forward_route
if external_ping; then echo 'Traffic survived a withdrawn external prefix' >&2; exit 1; fi
frr 'advertise ipv4 unicast'
retry has_forward_route
retry external_ping
echo 'Type-2 withdrawal and restoration driven by local port binding'
dc exec "$prefix-peer" ovs-vsctl remove Interface v230 external_ids iface-id
remote_mac_absent() { ! dc exec "$prefix-hv1" ovn-appctl -t ovn-controller evpn/vtep-fdb-list | grep -qi '02:ff:00:e6:00:02'; }
remote_mac_present() { dc exec "$prefix-hv1" ovn-appctl -t ovn-controller evpn/vtep-fdb-list | grep -qi '02:ff:00:e6:00:02'; }
retry remote_mac_absent
dc exec "$prefix-peer" ovs-vsctl set Interface v230 external_ids:iface-id=v230
retry remote_mac_present
echo 'Egress withdrawal: retain an ingress RMAC for VM type-2 return routes'
nb clear Logical_Router_Port lr-transit220 gateway_chassis
nb set Logical_Router_Port lr-transit220 options:dynamic-routing-no-learning=true
retry no_reverse_first
dc exec "$prefix-peer" ip netns exec r220 lab-ping -n -c 3 -W 2 -M do -s 8972 172.16.0.11
retry external_ping
dc exec "$prefix-hv1" ip netns exec vm220 lab-ping -n -c 3 -W 2 -M do -s 8972 172.20.220.10
restore_gateway
retry has_reverse_first
echo 'ovn-controller restart preserves FRR remote entries and restores forwarding'
dc exec "$prefix-hv1" bridge fdb show dev evx220 | grep extern_learn | sort > "$artifacts/frr-fdb-before.txt"
dc exec "$prefix-hv1" ovn-appctl -t ovn-controller exit
dc exec "$prefix-hv1" ovn-controller --detach --pidfile --log-file
retry external_ping
dc exec "$prefix-hv1" bridge fdb show dev evx220 | grep extern_learn | sort > "$artifacts/frr-fdb-after.txt"
diff -u "$artifacts/frr-fdb-before.txt" "$artifacts/frr-fdb-after.txt"
echo 'Test FRR restart restores sessions, remote inputs and traffic'
dc restart "$prefix-frr-hv1"
retry external_ping
retry has_reverse_first
bash "$dir/lab.sh" check
cleanup
trap - EXIT
if [[ -n ${WIRECHECK:-} ]]; then
  captures=()
  gateways=()
  for index in $(seq 1 "$EXTERNAL_VXLAN_NODES"); do captures+=("$artifacts/hv$index.pcap"); gateways+=("${EXTERNAL_VXLAN_FABRIC_PREFIX}.1$index"); done
  required=$(IFS=,; echo "${gateways[*]}")
  EXTERNAL_VXLAN_REQUIRE_GATEWAYS=$required "$WIRECHECK" "${captures[@]}"
fi
echo "PASS: FRR/OVN acceptance; artifacts: $artifacts"
