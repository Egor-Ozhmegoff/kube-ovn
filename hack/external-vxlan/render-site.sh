#!/usr/bin/env bash
set -euo pipefail
# Render only: never modifies the requirements repository or applies resources.
[[ ${CONTROL_PLANE:-frr} == frr ]] || { echo "Only host FRR control plane is supported" >&2; exit 2; }
dir=$(cd "$(dirname "$0")" && pwd)
cat <<EOF
apiVersion: v1
kind: Namespace
metadata: {name: kubevirt-networks}
EOF
for tenant in 220 230; do
  second=0
  [[ $tenant != 230 ]] || second=1
  cat <<EOF
---
apiVersion: v1
kind: Namespace
metadata: {name: evpn-app$tenant}
---
apiVersion: kubeovn.io/v1
kind: Vpc
metadata: {name: vrf$tenant}
spec:
  namespaces: [evpn-app$tenant]
  defaultSubnet: pods$tenant
  externalVXLAN:
    vni: $tenant
EOF
    control_port=60001
    [[ $tenant != 230 ]] || control_port=60002
    cat <<EOF
    frr:
      bridge: cp$tenant
      vxlan: cpvx$tenant
      advertise: adv$tenant
      controlPort: $control_port
      vrf: vrf$tenant
      tableID: $tenant
EOF
  cat <<EOF
    gatewayNodes:
EOF
  for index in 1 2 3 4; do
    cat <<EOF
    - node: worker-$index
      transitCIDR: 169.254.$tenant.$((index+10))/24
EOF
    echo "      routingPort: native-$tenant-worker-$index"
  done
  cat <<EOF
---
apiVersion: kubeovn.io/v1
kind: Subnet
metadata: {name: pods$tenant}
spec:
  vpc: vrf$tenant
  namespaces: [evpn-app$tenant]
  cidrBlock: 172.16.$second.0/24
  gateway: 172.16.$second.1
  excludeIps: [172.16.$second.1]
  gatewayType: distributed
  natOutgoing: false
  mtu: 9000
EOF
done
control_port=60002
while read -r name tenant vni cidr gateway; do
  control_port=$((control_port+1))
  mac=$(printf '02:aa:%02x:%02x:%02x:01' "$((vni >> 16))" "$(((vni >> 8) & 255))" "$((vni & 255))")
  # Supply a name<TAB>MAC file to preserve preexisting VM-facing gateway MACs.
  if [[ -n ${GATEWAY_MAC_FILE:-} ]]; then
    mac=$(awk -v name="$name" '$1 == name {print $2}' "$GATEWAY_MAC_FILE")
    [[ -n $mac ]] || { echo "Missing gateway MAC for $name" >&2; exit 2; }
  fi
  cat <<EOF
---
apiVersion: kubeovn.io/v1
kind: Subnet
metadata: {name: $name}
spec:
  vpc: vrf$tenant
  provider: $name.kubevirt-networks.ovn
  cidrBlock: $cidr
  gateway: $gateway
  excludeIps: [$gateway]
  gatewayType: distributed
  natOutgoing: false
  enableDHCP: false
  mtu: 9000
  externalVXLAN:
    vni: $vni
    gatewayMAC: "$mac"
EOF
    cat <<EOF
    frr:
      bridge: cp$vni
      vxlan: cpvx$vni
      advertise: adv$vni
      controlPort: $control_port
      vrf: vrf$tenant
      tableID: $tenant
EOF
  cat <<EOF
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata: {name: $name, namespace: kubevirt-networks}
spec:
  config: >-
    {"cniVersion":"0.3.1","type":"kube-ovn","server_socket":"/run/openvswitch/kube-ovn-daemon.sock","provider":"$name.kubevirt-networks.ovn"}
EOF
done < "$dir/site-networks.tsv"
