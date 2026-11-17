#!/usr/bin/env bash
set -euo pipefail
mkdir -p /var/run/openvswitch
ovsdb-tool create /tmp/ovs.db /usr/share/openvswitch/vswitch.ovsschema
ovsdb-server --detach --pidfile=/tmp/ovsdb.pid --unixctl=/tmp/ovsdb.ctl --remote=punix:/var/run/openvswitch/db.sock /tmp/ovs.db
ovs-vsctl --no-wait init
ovs-vswitchd --detach --pidfile=/tmp/vswitchd.pid --unixctl=/tmp/vswitchd.ctl unix:/var/run/openvswitch/db.sock
sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
export GOMAXPROCS=1
timeout --kill-after=10s 20m /runroot/native-hostnetwork.test -test.v -ginkgo.v -ginkgo.timeout=15m -ginkgo.json-report=/runroot/results/native-hostnetwork.json
