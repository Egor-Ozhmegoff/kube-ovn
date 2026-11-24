#!/bin/bash
# Run ONLY inside a new --privileged --network none disposable FRR container.
set -euo pipefail
[[ -f /.dockerenv && $(ip -o link show | wc -l) -eq 1 ]] || exit 2
ip netns add pe
ip -n pe link set lo up
ip -n pe link add tenant-a type vrf table 23001
ip -n pe link set tenant-a up
ip link add host-e-10001 type veth peer name pe-e-10001 netns pe
ip address add 169.254.16.13/20 dev host-e-10001
ip -6 address add fd00:16::d/64 dev host-e-10001 nodad
ip link set host-e-10001 up
ip -n pe link set pe-e-10001 master tenant-a
ip -n pe address add 169.254.16.1/20 dev pe-e-10001
ip -n pe -6 address add fd00:16::1/64 dev pe-e-10001 nodad
ip -n pe link set pe-e-10001 up
ip -n pe route add unreachable default table 23001 metric 32767
ip -n pe -6 route add unreachable default table 23001 metric 32767
ip -n pe route add 192.0.2.1/32 via 169.254.16.13 dev pe-e-10001 table 23001 proto 201 metric 100
# Synthetic imported BGP FIB entry; this is not a real external fabric session.
ip -n pe route add 198.51.100.1/32 via 169.254.16.13 dev pe-e-10001 table 23001 proto 186 metric 42760
ip -n pe route add 10.101.3.12/32 via 169.254.16.13 dev pe-e-10001 table 23001 proto 201 metric 100
ip -n pe route add 10.101.3.13/32 via 169.254.16.13 dev pe-e-10001 table 23001 proto 186 metric 42760
mkdir -p /var/run/frr /etc/frr
printf 'service integrated-vtysh-config\n' >/etc/frr/vtysh.conf
touch /etc/frr/frr.conf
chown frr:frr /var/run/frr
ip netns exec pe /usr/lib/frr/zebra -d -f /lab/frr-netlink.conf -i /tmp/zebra.pid
ip netns exec pe /usr/lib/frr/bgpd -d -f /lab/frr-netlink.conf -i /tmp/bgpd.pid
for attempt in $(seq 1 50); do
 [[ -S /var/run/frr/bgpd.vty ]] && break
 sleep .1
done
vtysh -c 'show version'
KUBE_OVN_NETLINK_LAB=true timeout 120 /lab/routes.test -test.run '^TestLinux' -test.v
