#!/usr/bin/env bash
set -euo pipefail
ping_command=${LAB_PING:-lab-ping}

retry() {
  local attempt
  for attempt in {1..30}; do
    if "$@"; then return; fi
    sleep 1
  done
  return 1
}

if [[ $1 == control ]]; then
  ovn-sbctl --db=tcp:127.0.0.1:6642 show
  ovn-nbctl --db=tcp:127.0.0.1:6641 --timeout=60 --wait=hv sync
  exit
fi

for tenant in 220 230; do
  if [[ $1 == worker ]]; then
    index=${2:?worker index}
    for kind in pods vm; do
      retry ip netns exec "$kind$tenant" "$ping_command" -n -c 1 -W 1 "172.20.$tenant.10"
      ip netns exec "$kind$tenant" "$ping_command" -n -c 2 -W 2 -M do -s 8972 "172.20.$tenant.10"
    done
    vm_remote=172.16.133.10
    [[ $tenant == 230 ]] && vm_remote=172.16.136.200
    retry ip netns exec "vm$tenant" "$ping_command" -n -c 1 -W 1 "$vm_remote"
    ip netns exec "vm$tenant" "$ping_command" -n -c 2 -W 2 -M do -s 8972 "$vm_remote"
    # Explicit negative tenant isolation check (no inter-tenant route).
    other=230
    [[ $tenant == 230 ]] && other=220
    if ip netns exec "pods$tenant" "$ping_command" -n -c 1 -W 1 "172.20.$other.10"; then
      echo 'Unexpected cross-tenant connectivity' >&2
      exit 1
    fi
    ip -n "vm$tenant" neigh show
  else
    second=0
    [[ $tenant == 230 ]] && second=1
    for index in $(seq 1 "${EXTERNAL_VXLAN_NODES:-2}"); do
      retry ip netns exec "r$tenant" "$ping_command" -n -c 1 -W 1 "172.16.$second.$((10 + index))"
      ip netns exec "r$tenant" "$ping_command" -n -c 1 -W 2 -M do -s 8972 "172.16.$second.$((10 + index))"
      vm_ip="172.16.133.$((index + 1))"
      [[ $tenant == 230 ]] && vm_ip="172.16.136.$((index + 1))"
      ip netns exec "v$tenant" "$ping_command" -n -c 1 -W 2 -M do -s 8972 "$vm_ip"
    done
  fi
done
if [[ $1 == worker ]]; then
  ovn-appctl -t ovn-controller evpn/remote-vtep-list
  ovs-vsctl --columns=name,type,options,error list Interface
fi
