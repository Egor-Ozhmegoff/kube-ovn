#!/usr/bin/env bash
set -euo pipefail
# Runs ONLY inside the disposable lab containers, never on a production host.
role=${1:?worker|peer}
index=${2:-1}
fabric_prefix=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.191}
local_ip=$fabric_prefix.100
[[ $role != worker ]] || local_ip=$fabric_prefix.1$index
for tenant in 220 230; do
  vm_vni=504221
  [[ $tenant != 230 ]] || vm_vni=504231
  ip link add "vrf$tenant" type vrf table "$tenant"
  ip link set "vrf$tenant" up
  for vni in "$tenant" "$vm_vni"; do
    if [[ $role == peer ]]; then
      ip link add "evb$vni" type bridge
      ip link add "evx$vni" type vxlan id "$vni" local "$local_ip" dstport 60000 nolearning
      ip link set "evx$vni" master "evb$vni" addrgenmode none multicast off arp off up
      ip link add "eva$vni" type dummy
      ip link set "eva$vni" master "evb$vni"
    fi
    # addrgenmode none does not remove link-local addresses created at enslave.
    # These test-owned control-plane interfaces are IPv4-only; never touch underlay.
    for iface in "evb$vni" "evx$vni" "eva$vni"; do
      sysctl -qw "net.ipv6.conf.$iface.disable_ipv6=1"
    done
    mac=$(printf '02:ee:%02x:00:00:00' "$((index+10))")
    [[ $role != peer ]] || mac=02:ff:00:00:00:01
    ip link set "evb$vni" master "vrf$tenant" address "$mac" addrgenmode none mtu 9000 up
    ip link set "eva$vni" addrgenmode none mtu 9000 up
  done
  if [[ $role == worker ]]; then
    case "$index" in 1) suffix="" ;; 2) suffix=-b ;; 3) suffix=-c ;; 4) suffix=-d ;; esac
    ovs-vsctl set Open_vSwitch . "external_ids:dynamic-routing-port-mapping=lr-transit220$suffix=evb220,lr-transit230$suffix=evb230"
  fi
done
