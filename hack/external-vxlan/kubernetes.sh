#!/usr/bin/env bash
set -euo pipefail

# Apply only to a dedicated test cluster. This script does not configure FRR.
: "${KUBECONFIG:?Set KUBECONFIG explicitly to the dedicated test cluster}"
: "${EXTERNAL_VXLAN_GATEWAY_NODE:?Set the first test node name}"
: "${EXTERNAL_VXLAN_SECOND_NODE:?Set the second test node name}"
: "${EXTERNAL_VXLAN_WORKLOAD_IMAGE:?Set an image containing lab-ping, ip and sleep}"
fabric_prefix=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.191}
kc() { kubectl --cache-dir "${EXTERNAL_VXLAN_KUBECTL_CACHE:-/tmp/ko-evx-kubectl}" "$@"; }

case ${1:-configure} in
configure)
  kc annotate node "$EXTERNAL_VXLAN_GATEWAY_NODE" --overwrite \
    "ovn.kubernetes.io/external-vxlan-ip=$fabric_prefix.11" \
    ovn.kubernetes.io/external-vxlan-router-mac=02:ee:0b:00:00:00
  kc annotate node "$EXTERNAL_VXLAN_SECOND_NODE" --overwrite \
    "ovn.kubernetes.io/external-vxlan-ip=$fabric_prefix.12" \
    ovn.kubernetes.io/external-vxlan-router-mac=02:ee:0c:00:00:00
  for tenant in 220 230; do
    for kind in pods vm; do
      kc create namespace "evx-$kind$tenant" --dry-run=client -o yaml | kc apply -f -
    done
    cat <<EOF | kc apply -f -
apiVersion: kubeovn.io/v1
kind: Vpc
metadata:
  name: evx-tenant$tenant
  labels: {external-vxlan-e2e: "true"}
spec:
  namespaces: [evx-pods$tenant, evx-vm$tenant]
  defaultSubnet: evx-pods$tenant
  externalVXLAN:
    vni: $tenant
    gatewayNodes:
    - node: $EXTERNAL_VXLAN_GATEWAY_NODE
      transitCIDR: 169.254.$tenant.11/24
      routingPort: lr-transit$tenant
    - node: $EXTERNAL_VXLAN_SECOND_NODE
      transitCIDR: 169.254.$tenant.12/24
      routingPort: lr-transit$tenant-b
    frr:
      bridge: evb$tenant
      vxlan: evx$tenant
      advertise: eva$tenant
      controlPort: 60000
      vrf: vrf$tenant
      tableID: $tenant
EOF
    second=0
    vm_cidr=172.16.133.0/28
    vm_gateway=172.16.133.1
    vm_remote=172.16.133.10
    vm_vni=504221
    [[ $tenant == 230 ]] && second=1 && vm_cidr=172.16.136.0/24 && vm_gateway=172.16.136.1 && vm_remote=172.16.136.200 && vm_vni=504231
    gateway_mac=$(printf '02:aa:00:%02x:00:01' "$tenant")
    cat <<EOF | kc apply -f -
apiVersion: kubeovn.io/v1
kind: Subnet
metadata:
  name: evx-pods$tenant
  labels: {external-vxlan-e2e: "true"}
spec:
  vpc: evx-tenant$tenant
  namespaces: [evx-pods$tenant]
  cidrBlock: 172.16.$second.0/24
  gateway: 172.16.$second.1
  excludeIps: [172.16.$second.1]
  gatewayType: distributed
  natOutgoing: false
  mtu: 9000
---
apiVersion: kubeovn.io/v1
kind: Subnet
metadata:
  name: evx-vm$tenant
  labels: {external-vxlan-e2e: "true"}
spec:
  vpc: evx-tenant$tenant
  namespaces: [evx-vm$tenant]
  cidrBlock: $vm_cidr
  gateway: $vm_gateway
  excludeIps: [$vm_gateway, $vm_remote]
  gatewayType: distributed
  natOutgoing: false
  mtu: 9000
  externalVXLAN:
    vni: $vm_vni
    gatewayMAC: "$gateway_mac"
    frr:
      bridge: evb$vm_vni
      vxlan: evx$vm_vni
      advertise: eva$vm_vni
      controlPort: 60000
      vrf: vrf$tenant
      tableID: $tenant
EOF
    for index in 1 2; do
      node=$EXTERNAL_VXLAN_GATEWAY_NODE
      [[ $index == 2 ]] && node=$EXTERNAL_VXLAN_SECOND_NODE
      for kind in pods vm; do
        cat <<EOF | kc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: endpoint-$index
  namespace: evx-$kind$tenant
  labels: {external-vxlan-e2e: "true"}
  annotations:
    ovn.kubernetes.io/logical_switch: evx-$kind$tenant
spec:
  nodeName: $node
  tolerations: [{operator: Exists}]
  containers:
  - name: endpoint
    image: $EXTERNAL_VXLAN_WORKLOAD_IMAGE
    imagePullPolicy: IfNotPresent
    command: [sleep, infinity]
EOF
      done
    done
  done
  ;;
check)
  for tenant in 220 230; do
    other=230
    [[ $tenant == 230 ]] && other=220
    for kind in pods vm; do
      for index in 1 2; do
        ns="evx-$kind$tenant"
        kc -n "$ns" wait pod/"endpoint-$index" --for=condition=Ready --timeout=180s
        kc -n "$ns" exec "endpoint-$index" -- lab-ping -n -c 2 -W 2 -M do -s 8972 "172.20.$tenant.10"
        if [[ $kind == vm ]]; then
          remote=172.16.133.10
          [[ $tenant == 230 ]] && remote=172.16.136.200
          kc -n "$ns" exec "endpoint-$index" -- lab-ping -n -c 2 -W 2 -M do -s 8972 "$remote"
        fi
        if kc -n "$ns" exec "endpoint-$index" -- lab-ping -n -c 1 -W 1 "172.20.$other.10"; then
          echo "Unexpected cross-tenant connectivity from $ns/endpoint-$index" >&2
          exit 1
        fi
      done
    done
  done
  ;;
*) echo "Usage: $0 configure|check" >&2; exit 2 ;;
esac
