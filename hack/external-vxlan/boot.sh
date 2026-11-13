#!/usr/bin/env bash
set -euo pipefail
export EXTERNAL_VXLAN_FABRIC_PREFIX=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.191}
export EXTERNAL_VXLAN_CLUSTER_PREFIX=${EXTERNAL_VXLAN_CLUSTER_PREFIX:-10.110.192}

mkdir -p /run/ovn /run/openvswitch /etc/openvswitch /var/log/ovn /var/log/openvswitch
if [[ $1 == central ]]; then
  for db in nb sb; do
    ovsdb-tool create "/run/ovn/$db.db" "/usr/share/ovn/ovn-$db.ovsschema"
    port=6641
    [[ $db == sb ]] && port=6642
    ovsdb-server "/run/ovn/$db.db" --remote="ptcp:$port:0.0.0.0" \
      --remote="punix:/run/ovn/ovn${db}_db.sock" --pidfile="/run/ovn/$db.pid" \
      --unixctl="/run/ovn/$db.ctl" --detach --log-file="/var/log/ovn/$db.log"
  done
  ovn-northd --ovnnb-db=tcp:127.0.0.1:6641 --ovnsb-db=tcp:127.0.0.1:6642 \
    --detach --pidfile --log-file
  exec sleep infinity
fi

ovsdb-tool create /etc/openvswitch/conf.db /usr/share/openvswitch/vswitch.ovsschema
ovsdb-server /etc/openvswitch/conf.db --remote=punix:/run/openvswitch/db.sock \
  --detach --pidfile --log-file
ovs-vsctl --no-wait init
ovs-vswitchd --detach --pidfile --log-file
ovs-vsctl -- add-br br-int -- set Bridge br-int fail_mode=secure
ovs-vsctl --timeout=10 show
if ! ip link show br-int >/dev/null 2>&1; then
  echo 'Kernel OVS datapath unavailable: this lab requires Linux Open vSwitch support.' >&2
  exit 1
fi

if [[ $1 == worker ]]; then
  index=${2:?worker index}
  ovs-vsctl set Open_vSwitch . external_ids:system-id="hv$index" \
    external_ids:hostname="hv$index" external_ids:ovn-remote=tcp:${EXTERNAL_VXLAN_CLUSTER_PREFIX}.2:6642 \
    external_ids:ovn-encap-type=vxlan external_ids:ovn-encap-ip="${EXTERNAL_VXLAN_CLUSTER_PREFIX}.1$index" \
    external_ids:ovn-evpn-local-ip="${EXTERNAL_VXLAN_FABRIC_PREFIX}.1$index" external_ids:ovn-evpn-vxlan-ports=4789
  ovn-controller --detach --pidfile --log-file
fi
ovn-controller --version
ovs-vswitchd --version
