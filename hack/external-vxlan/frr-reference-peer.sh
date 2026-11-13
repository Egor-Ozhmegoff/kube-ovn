#!/usr/bin/env bash
set -euo pipefail
dir=$(cd "$(dirname "$0")" && pwd)
prefix=${EXTERNAL_VXLAN_LAB:-ko-frr}
image=${EXTERNAL_VXLAN_IMAGE:?Use the same pinned OVN image as the lab}
cluster_prefix=${EXTERNAL_VXLAN_CLUSTER_PREFIX:-10.110.192}
fabric_prefix=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.191}
docker run -d --name "$prefix-peer-central" --label "kube-ovn.external-vxlan-lab=$prefix" \
  --network "$prefix-cluster" --ip "$cluster_prefix.3" \
  --mount "type=bind,source=$dir,target=/lab,readonly" --entrypoint bash "$image" /lab/boot.sh central
docker network connect --ip "$cluster_prefix.100" "$prefix-cluster" "$prefix-peer"
for attempt in {1..30}; do
  if docker exec "$prefix-peer-central" ovn-nbctl --db=tcp:127.0.0.1:6641 show >/dev/null 2>&1; then break; fi
  [[ $attempt != 30 ]] || exit 1
  sleep 1
done
docker exec "$prefix-peer-central" bash /lab/frr-peer-topology.sh
docker exec -e "CLUSTER_PREFIX=$cluster_prefix" -e "FABRIC_PREFIX=$fabric_prefix" "$prefix-peer" bash -c '
  set -e
  for tenant in 220 230; do
    ovs-vsctl set Interface "r$tenant" "external_ids:iface-id=r$tenant"
    ovs-vsctl set Interface "v$tenant" "external_ids:iface-id=v$tenant"
  done
  ovs-vsctl set Open_vSwitch . external_ids:system-id=peer external_ids:hostname=peer \
    "external_ids:ovn-remote=tcp:$CLUSTER_PREFIX.3:6642" external_ids:ovn-encap-type=vxlan \
    "external_ids:ovn-encap-ip=$CLUSTER_PREFIX.100" "external_ids:ovn-evpn-local-ip=$FABRIC_PREFIX.100" \
    external_ids:ovn-evpn-vxlan-ports=4789 \
    external_ids:dynamic-routing-port-mapping=fabric-transit220=evb220,fabric-transit230=evb230
  ovn-controller --detach --pidfile --log-file
'
docker exec "$prefix-peer-central" ovn-nbctl --db=tcp:127.0.0.1:6641 --timeout=60 --wait=hv sync
