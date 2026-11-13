#!/usr/bin/env bash
set -euo pipefail
export EXTERNAL_VXLAN_FABRIC_PREFIX=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.191}
export EXTERNAL_VXLAN_CLUSTER_PREFIX=${EXTERNAL_VXLAN_CLUSTER_PREFIX:-10.110.192}
# Isolated test FRR only. Production FRR is never read or changed by this script.
dir=$(cd "$(dirname "$0")" && pwd)
export EXTERNAL_VXLAN_LAB=${EXTERNAL_VXLAN_LAB:-ko-frr}
export EXTERNAL_VXLAN_NODES=${EXTERNAL_VXLAN_NODES:-4}
workers="hv1 hv2"
[[ $EXTERNAL_VXLAN_NODES != 4 ]] || workers="$workers hv3 hv4"
prefix=$EXTERNAL_VXLAN_LAB
frr_image=${FRR_TEST_IMAGE:-quay.io/frrouting/frr@sha256:eae3340ca578bb588d92ab3d1e77deb806f80d1da2bc0053cb1dfcfef2aa236f}
artifacts=${EXTERNAL_VXLAN_ARTIFACTS:-/tmp/$prefix-artifacts}
dc() { docker "$@"; }
nb() { dc exec -e OVN_NB_DB=tcp:127.0.0.1:6641 "$prefix-central" ovn-nbctl "$@"; }
case ${1:-up} in
up)
  # Fail before creating a lab on kernels lacking VRF support (Docker Desktop).
  dc run --rm --privileged --network none --entrypoint sh "$frr_image" -c 'ip link add vrf-test type vrf table 222'
  bash "$dir/lab.sh" up
  mkdir -p "$artifacts"
  for name in $workers peer; do
    role=worker
    index=${name#hv}
    local_ip=${EXTERNAL_VXLAN_FABRIC_PREFIX}.1$index
    [[ $name != peer ]] || { role=peer; index=1; local_ip=${EXTERNAL_VXLAN_FABRIC_PREFIX}.100; }
    dc exec -e "EXTERNAL_VXLAN_FABRIC_PREFIX=$EXTERNAL_VXLAN_FABRIC_PREFIX" "$prefix-$name" bash /lab/frr-host.sh "$role" "$index"
    mkdir -p "$artifacts/$name"
    config="$artifacts/$name/frr.conf"
    cat > "$config" <<EOF
frr defaults traditional
hostname $prefix-$name
log stdout informational
service integrated-vtysh-config
vrf vrf220
 vni 220
exit-vrf
vrf vrf230
 vni 230
exit-vrf
router bgp 65000
 bgp router-id $local_ip
 no bgp ebgp-requires-policy
 no bgp default ipv4-unicast
EOF
    neighbors="${EXTERNAL_VXLAN_FABRIC_PREFIX}.100"
    if [[ $name == peer ]]; then
      neighbors="${EXTERNAL_VXLAN_FABRIC_PREFIX}.11 ${EXTERNAL_VXLAN_FABRIC_PREFIX}.12"
      [[ $EXTERNAL_VXLAN_NODES != 4 ]] || neighbors="$neighbors ${EXTERNAL_VXLAN_FABRIC_PREFIX}.13 ${EXTERNAL_VXLAN_FABRIC_PREFIX}.14"
    fi
    for neighbor in $neighbors; do
      echo " neighbor $neighbor remote-as 65000" >> "$config"
    done
    cat >> "$config" <<EOF
 address-family l2vpn evpn
  advertise-all-vni
  advertise-svi-ip
EOF
    for neighbor in $neighbors; do echo "  neighbor $neighbor activate" >> "$config"; done
    for vni in 504221 504231; do
      cat >> "$config" <<EOF
  vni $vni
   rd $local_ip:$((vni % 1000))
   route-target import 65000:$vni
   route-target export 65000:$vni
  exit-vni
EOF
    done
    printf ' exit-address-family\nexit\n' >> "$config"
    for tenant in 220 230; do
      redistribution=kernel
      cat >> "$config" <<EOF
router bgp 65000 vrf vrf$tenant
 bgp router-id $local_ip
 no bgp default ipv4-unicast
 address-family ipv4 unicast
  redistribute $redistribution
 exit-address-family
 address-family l2vpn evpn
  rd $local_ip:$tenant
  route-target import 65000:$tenant
  route-target export 65000:$tenant
  advertise ipv4 unicast
 exit-address-family
exit
EOF
    done
    chmod 644 "$config"
    touch "$artifacts/$name/vtysh.conf"
    dc run -d --privileged --name "$prefix-frr-$name" \
      --label "kube-ovn.external-vxlan-lab=$prefix" --network "container:$prefix-$name" \
      --mount "type=bind,source=$artifacts/$name,target=/etc/frr" \
      --entrypoint /bin/sh "$frr_image" -c \
      'set -e; /usr/lib/frr/mgmtd -d; /usr/lib/frr/zebra -d -f /dev/null; /usr/lib/frr/bgpd -d -f /dev/null; vtysh -b; exec sleep infinity'
  done
  for tenant in 220 230; do
    for index in $(seq 2 "$EXTERNAL_VXLAN_NODES"); do
      case "$index" in 2) suffix=-b ;; 3) suffix=-c ;; 4) suffix=-d ;; esac
      mac=$(printf '02:ee:%02x:00:00:00' "$((index+10))")
      nb -- lrp-add "tenant$tenant" "lr-transit$tenant$suffix" "$mac" "169.254.$tenant.$((index+10))/24" \
        -- lrp-set-gateway-chassis "lr-transit$tenant$suffix" "hv$index" 100 \
        -- lsp-add "transit$tenant" "transit$tenant-lr$suffix" \
        -- lsp-set-type "transit$tenant-lr$suffix" router \
        -- lsp-set-addresses "transit$tenant-lr$suffix" router \
        -- lsp-set-options "transit$tenant-lr$suffix" "router-port=lr-transit$tenant$suffix"
    done
    nb set Logical_Router "tenant$tenant" options:dynamic-routing=true \
      "options:dynamic-routing-vrf-name=vrf$tenant" "options:dynamic-routing-vrf-id=$tenant" options:dynamic-routing-redistribute=connected
    suffixes="base -b"
    [[ $EXTERNAL_VXLAN_NODES != 4 ]] || suffixes="$suffixes -c -d"
    for suffix in $suffixes; do
      [[ $suffix != base ]] || suffix=""
      nb set Logical_Router_Port "lr-transit$tenant$suffix" "options:dynamic-routing-port-name=lr-transit$tenant$suffix" options:dynamic-routing-maintain-vrf=false
    done
    for ls in "transit$tenant" "vm$tenant"; do
      nb set Logical_Switch "$ls" other_config:dynamic-routing-redistribute=fdb,ip
    done
  done
  bash "$dir/frr-reference-peer.sh"
  echo "FRR lab configured. Artifacts/configs: $artifacts. Run check after BGP converges."
  ;;
check)
  for name in $workers peer; do
    dc exec "$prefix-frr-$name" vtysh -c 'show bgp l2vpn evpn summary'
    dc exec "$prefix-frr-$name" vtysh -c 'show bgp l2vpn evpn route'
  done
  dc exec -e OVN_SB_DB=tcp:127.0.0.1:6642 "$prefix-central" ovn-sbctl list Learned_Route
  for name in $workers; do
    dc exec "$prefix-$name" ip route show table 220
    dc exec "$prefix-$name" ovn-appctl -t ovn-controller evpn/remote-vtep-list
    dc exec "$prefix-$name" ip netns exec pods220 lab-ping -n -c 3 -W 2 -M do -s 8972 172.20.220.10
    dc exec "$prefix-$name" ip netns exec vm230 lab-ping -n -c 3 -W 2 -M do -s 8972 172.16.136.200
  done
  ;;
down)
  for name in frr-hv1 frr-hv2 frr-hv3 frr-hv4 frr-peer peer-central; do
    if dc container inspect "$prefix-$name" >/dev/null 2>&1; then
      owner=$(dc inspect --format '{{index .Config.Labels "kube-ovn.external-vxlan-lab"}}' "$prefix-$name")
      [[ $owner == "$prefix" ]] || exit 1
      dc rm -f "$prefix-$name"
    fi
  done
  bash "$dir/lab.sh" down
  ;;
*) echo "Usage: $0 up|check|down" >&2; exit 2 ;;
esac
