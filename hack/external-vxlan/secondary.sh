#!/usr/bin/env bash
set -euo pipefail
# Uses kubernetes.sh's disposable two-tenant fixture and a separately installed Multus.
: "${KUBECONFIG:?Set the dedicated test kubeconfig}"
: "${EXTERNAL_VXLAN_GATEWAY_NODE:?Set the first node}"
: "${EXTERNAL_VXLAN_SECOND_NODE:?Set the second node}"
: "${EXTERNAL_VXLAN_WORKLOAD_IMAGE:?Set an image containing lab-ping, ip and sleep}"
kc() { kubectl --cache-dir "${EXTERNAL_VXLAN_KUBECTL_CACHE:-/tmp/ko-evx-kubectl}" "$@"; }
for tenant in 220 230; do
  subnet="evx-secondary$tenant"
  provider="$subnet.kubevirt-networks.ovn"
  ns="evx-pods$tenant"
  case ${1:-configure} in
  configure)
    cidr=172.16.133.32/27
    gateway=172.16.133.33
    vni=504223
    vm_cidr=172.16.133.0/28
    [[ $tenant != 230 ]] || { cidr=172.16.137.0/26; gateway=172.16.137.1; vni=504232; vm_cidr=172.16.136.0/24; }
    kc create namespace kubevirt-networks --dry-run=client -o yaml | kc apply -f -
    cat <<EOF | kc apply -f -
apiVersion: kubeovn.io/v1
kind: Subnet
metadata: {name: $subnet, labels: {external-vxlan-secondary: "true"}}
spec:
  vpc: evx-tenant$tenant
  provider: $provider
  cidrBlock: $cidr
  gateway: $gateway
  excludeIps: [$gateway]
  gatewayType: distributed
  natOutgoing: false
  mtu: 9000
  externalVXLAN:
    vni: $vni
    gatewayMAC: "02:ab:00:00:00:$([[ $tenant == 220 ]] && echo 20 || echo 30)"
    frr:
      bridge: evb$vni
      vxlan: evx$vni
      advertise: eva$vni
      controlPort: 60000
      vrf: vrf$tenant
      tableID: $tenant
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata: {name: $subnet, namespace: kubevirt-networks}
spec:
  config: >-
    {"cniVersion":"0.3.1","type":"kube-ovn","provider":"$provider","server_socket":"/run/openvswitch/kube-ovn-daemon.sock","routes":[{"dst":"172.20.$tenant.0/24","gw":"$gateway"},{"dst":"$vm_cidr","gw":"$gateway"}]}
EOF
    for index in 1 2; do
      node=$EXTERNAL_VXLAN_GATEWAY_NODE
      [[ $index != 2 ]] || node=$EXTERNAL_VXLAN_SECOND_NODE
      cat <<EOF | kc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: secondary-$index
  namespace: $ns
  labels: {external-vxlan-secondary: "true"}
  annotations:
    ovn.kubernetes.io/logical_switch: evx-pods$tenant
    k8s.v1.cni.cncf.io/networks: '[{"name":"$subnet","namespace":"kubevirt-networks","interface":"net1"}]'
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
    ;;
  check)
    for index in 1 2; do kc -n "$ns" wait pod/secondary-$index --for=condition=Ready --timeout=180s; done
    ips=()
    for index in 1 2; do
      ips+=("$(kc -n "$ns" get pod secondary-$index -o "go-template={{index .metadata.annotations \"$provider.kubernetes.io/ip_address\"}}")")
      kc -n "$ns" exec secondary-$index -- ip -d link show net1
      kc -n "$ns" exec secondary-$index -- ip route
      kc -n "$ns" exec secondary-$index -- lab-ping -I net1 -n -c 2 -W 2 -M do -s 8972 "172.20.$tenant.10"
      vm=$(kc -n "evx-vm$tenant" get pod endpoint-1 -o 'jsonpath={.status.podIP}')
      kc -n "$ns" exec secondary-$index -- lab-ping -I net1 -n -c 2 -W 2 -M do -s 8972 "$vm"
    done
    kc -n "$ns" exec secondary-1 -- lab-ping -I net1 -n -c 3 -W 2 -M do -s 8972 "${ips[1]}"
    kc -n "$ns" exec secondary-2 -- lab-ping -I net1 -n -c 3 -W 2 -M do -s 8972 "${ips[0]}"
    ;;
  cleanup)
    for index in 1 2; do
      kc -n "$ns" delete pod secondary-$index --ignore-not-found --wait=true
      kc wait --for=delete "ip/secondary-$index.$ns.$provider" --timeout=90s
    done
    kc -n kubevirt-networks delete network-attachment-definition "$subnet" --ignore-not-found
    kc delete subnet "$subnet" --ignore-not-found
    ;;
  *) echo "Usage: $0 configure|check|cleanup" >&2; exit 2 ;;
  esac
done
