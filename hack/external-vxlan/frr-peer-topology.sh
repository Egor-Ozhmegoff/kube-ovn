#!/usr/bin/env bash
set -euo pipefail
# A separate OVN reference fabric, so reverse routes and EVPN failover exercise
# real FRR -> Netlink -> OVN -> OVS forwarding in both directions.
export OVN_NB_DB=tcp:127.0.0.1:6641
for tenant in 220 230; do
  vni=504221
  vm_ip=172.16.133.10
  [[ $tenant != 230 ]] || { vni=504231; vm_ip=172.16.136.200; }
  vm_mac=$(printf '02:ff:00:%02x:00:02' "$tenant")
  ovn-nbctl -- lr-add "fabric$tenant" \
    -- set Logical_Router "fabric$tenant" options:dynamic-routing=true \
       "options:dynamic-routing-vrf-name=vrf$tenant" "options:dynamic-routing-vrf-id=$tenant" options:dynamic-routing-redistribute=connected \
    -- ls-add "external$tenant" \
    -- lrp-add "fabric$tenant" "fabric-external$tenant" 02:ff:00:00:00:01 "172.20.$tenant.1/24" \
    -- lsp-add-router-port "external$tenant" "external$tenant-fabric" "fabric-external$tenant" \
    -- lsp-add "external$tenant" "r$tenant" \
    -- lsp-set-addresses "r$tenant" "02:ff:00:00:00:02 172.20.$tenant.10" \
    -- ls-add "transit$tenant" \
    -- lrp-add "fabric$tenant" "fabric-transit$tenant" 02:ff:00:00:00:01 "169.254.$tenant.100/24" \
    -- lrp-set-gateway-chassis "fabric-transit$tenant" peer 100 \
    -- set Logical_Router_Port "fabric-transit$tenant" "options:dynamic-routing-port-name=fabric-transit$tenant" options:dynamic-routing-maintain-vrf=false \
    -- lsp-add-router-port "transit$tenant" "transit$tenant-fabric" "fabric-transit$tenant" \
    -- ls-add "vm$tenant" \
    -- lsp-add "vm$tenant" "v$tenant" \
    -- lsp-set-addresses "v$tenant" "$vm_mac $vm_ip"
  for pair in "transit$tenant:$tenant" "vm$tenant:$vni"; do
    ls=${pair%:*}
    key=${pair#*:}
    ovn-nbctl set Logical_Switch "$ls" "other_config:dynamic-routing-vni=$key" \
      "other_config:dynamic-routing-bridge-ifname=evb$key" "other_config:dynamic-routing-vxlan-ifname=evx$key" \
      "other_config:dynamic-routing-advertise-ifname=eva$key" other_config:dynamic-routing-redistribute=fdb,ip
  done
done
