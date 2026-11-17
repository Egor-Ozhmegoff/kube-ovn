#!/bin/bash
set -euo pipefail
[[ -f /.dockerenv && $(ip -o link show | wc -l) -eq 1 ]] || exit 2
ip link add tenant-a type vrf table 110001
ip link set tenant-a up
ip netns add pe
ip link add host0 type veth peer name peer0 netns pe
ip link set host0 master tenant-a
ip address add 169.254.16.13/20 dev host0
ip link set host0 up
ip -n pe link set lo up
ip -n pe link add tenant-a type vrf table 110001
ip -n pe link set tenant-a up
ip -n pe link set peer0 master tenant-a
ip -n pe address add 169.254.16.1/20 dev peer0
ip -n pe link set peer0 up
sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.tcp_l3mdev_accept=0
ip netns exec pe sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.tcp_l3mdev_accept=0
mkdir -p /var/run/frr /etc/frr
printf 'service integrated-vtysh-config\n' > /etc/frr/vtysh.conf
touch /etc/frr/frr.conf
chown frr:frr /var/run/frr
cat >/tmp/zebra.conf <<'CONFIG'
frr defaults traditional
hostname lab-peer
log stdout
vrf tenant-a
exit-vrf
CONFIG
cat >/tmp/bgpd.conf <<'CONFIG'
frr defaults traditional
hostname lab-peer
log stdout
router bgp 64514 vrf tenant-a
 bgp router-id 10.255.0.1
 no bgp ebgp-requires-policy
 no bgp network import-check
 neighbor 169.254.16.13 remote-as 64515
 neighbor 169.254.16.13 timers 3 9
 address-family ipv4 unicast
  network 203.0.113.0/24
 exit-address-family
exit
CONFIG
chmod 644 /tmp/zebra.conf /tmp/bgpd.conf
ip netns exec pe /usr/lib/frr/zebra -d -f /tmp/zebra.conf -i /tmp/zebra.pid
ip netns exec pe /usr/lib/frr/bgpd -d -f /tmp/bgpd.conf -i /tmp/bgpd.pid
for attempt in $(seq 1 50); do
 [[ -S /var/run/frr/bgpd.vty ]] && break
 sleep .1
done
vtysh -c 'show version'
KUBE_OVN_OPENPEROUTER_BGP_LAB=true GOMAXPROCS=1 timeout 120 /lab/advertiser.test -test.run '^TestSpeakerLinuxVRFSession$' -test.v
