#!/usr/bin/env bash
set -euo pipefail
fabric_prefix=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.191}

endpoint() {
  local name=$1 ip=$2 gateway=$3 mac=$4
  ip netns add "$name"
  ip link add "$name" type veth peer name eth0 netns "$name"
  ip link set "$name" mtu 9000 up
  ip -n "$name" link set lo up
  ip -n "$name" link set eth0 address "$mac" mtu 9000 up
  ip -n "$name" addr add "$ip" dev eth0
  ip -n "$name" route add default via "$gateway"
  ovs-vsctl add-port br-int "$name"
}

for tenant in 220 230; do
  vm_vni=504221
  [[ $tenant == 230 ]] && vm_vni=504231
  if [[ $1 == worker ]]; then
    index=${2:?worker index}
    second=0
    [[ $tenant == 230 ]] && second=1
    for kind in pods vm; do
      ip="172.16.$second.$((10 + index))/24"
      gateway="172.16.$second.1"
      if [[ $kind == vm ]]; then
        if [[ $tenant == 220 ]]; then ip="172.16.133.$((index + 1))/28"; gateway=172.16.133.1;
        else ip="172.16.136.$((index + 1))/24"; gateway=172.16.136.1; fi
      fi
      mac=$(printf '02:00:%02x:%02x:00:%02x' "$tenant" "$index" "$([[ $kind == vm ]] && echo 2 || echo 1)")
      endpoint "$kind$tenant" "$ip" "$gateway" "$mac"
      ovs-vsctl set Interface "$kind$tenant" "external_ids:iface-id=$kind$tenant-hv$index"
    done
    for vni in "$tenant" "$vm_vni"; do
      ip link add "evb$vni" type bridge
      ip link add "evx$vni" type vxlan id "$vni" local "$fabric_prefix.1$index" dstport 60000 nolearning
      ip link set "evx$vni" master "evb$vni"
      ip link set "evx$vni" addrgenmode none multicast off arp off
      ip link set "evx$vni" up
      ip link add "eva$vni" type dummy
      ip link set "eva$vni" master "evb$vni"
    done
  else
    # Independent OVN reference fabric endpoints; no manual tunnel/OpenFlow setup.
    endpoint "r$tenant" "172.20.$tenant.10/24" "172.20.$tenant.1" "02:ff:00:00:00:02"
    peer_mac=$(printf '02:ff:00:%02x:00:02' "$tenant")
    ip=172.16.133.10/28
    gateway=172.16.133.1
    [[ $tenant == 230 ]] && ip=172.16.136.200/24 && gateway=172.16.136.1
    endpoint "v$tenant" "$ip" "$gateway" "$peer_mac"

  fi
done
