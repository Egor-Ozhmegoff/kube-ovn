#!/usr/bin/env bash
set -euo pipefail
export OVN_NB_DB=tcp:127.0.0.1:6641

if [[ ${1:-} == sync ]]; then
  ovn-nbctl --timeout=60 --wait=hv sync
  exit
fi

for tenant in 220 230; do
  second=0
  vm_vni=504221
  vm_cidr=172.16.133.1/28
  [[ $tenant == 230 ]] && second=1 && vm_vni=504231 && vm_cidr=172.16.136.1/24
  ovn-nbctl lr-add "tenant$tenant"
  for kind in pods vm; do
    cidr="172.16.$second.1/24"
    [[ $kind == vm ]] && cidr=$vm_cidr
    # Distributed VM-facing gateway identity is common on both chassis.
    mac=$(printf '02:aa:00:%02x:00:01' "$tenant")
    ovn-nbctl -- ls-add "$kind$tenant" \
      -- lrp-add "tenant$tenant" "lr-$kind$tenant" "$mac" "$cidr" \
      -- lsp-add "$kind$tenant" "$kind$tenant-lr" \
      -- lsp-set-type "$kind$tenant-lr" router \
      -- lsp-set-addresses "$kind$tenant-lr" router \
      -- lsp-set-options "$kind$tenant-lr" "router-port=lr-$kind$tenant"
    for index in $(seq 1 "${EXTERNAL_VXLAN_NODES:-2}"); do
      ip="172.16.$second.$((10 + index))"
      [[ $kind == vm && $tenant == 220 ]] && ip="172.16.133.$((index + 1))"
      [[ $kind == vm && $tenant == 230 ]] && ip="172.16.136.$((index + 1))"
      mac=$(printf '02:00:%02x:%02x:00:%02x' "$tenant" "$index" "$([[ $kind == vm ]] && echo 2 || echo 1)")
      ovn-nbctl -- lsp-add "$kind$tenant" "$kind$tenant-hv$index" \
        -- lsp-set-addresses "$kind$tenant-hv$index" "$mac $ip"
    done
  done
  ovn-nbctl -- ls-add "transit$tenant" \
    -- lrp-add "tenant$tenant" "lr-transit$tenant" 02:ee:0b:00:00:00 "169.254.$tenant.11/24" \
    -- lrp-set-gateway-chassis "lr-transit$tenant" hv1 100 \
    -- lsp-add "transit$tenant" "transit$tenant-lr" \
    -- lsp-set-type "transit$tenant-lr" router \
    -- lsp-set-addresses "transit$tenant-lr" router \
    -- lsp-set-options "transit$tenant-lr" "router-port=lr-transit$tenant"
  for pair in "vm$tenant:$vm_vni" "transit$tenant:$tenant"; do
    ls=${pair%:*}
    vni=${pair#*:}
    ovn-nbctl set Logical_Switch "$ls" \
      "other_config:dynamic-routing-vni=$vni" \
      "other_config:dynamic-routing-bridge-ifname=evb$vni" \
      "other_config:dynamic-routing-vxlan-ifname=evx$vni" \
      "other_config:dynamic-routing-advertise-ifname=eva$vni"
  done

done
