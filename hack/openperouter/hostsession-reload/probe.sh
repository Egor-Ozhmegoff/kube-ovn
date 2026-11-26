#!/bin/bash
# Only inside a fresh privileged network-none disposable FRR container.
set -euo pipefail
[[ -f /.dockerenv && $(ip -o link show | wc -l) -eq 1 ]] || exit 2
mode=${1:?cold or reload}
[[ $mode == cold || $mode == reload ]] || exit 2
mkdir -p /out /etc/frr /var/run/frr
: >/out/status.tsv
printf 'service integrated-vtysh-config\n' >/etc/frr/vtysh.conf
chown frr:frr /var/run/frr
ip link set lo up
ip link add tenant-a type vrf table 23001
ip link set tenant-a up
ip link add attachment type dummy
ip link set attachment master tenant-a
ip address add 169.254.16.1/20 dev attachment
ip -6 address add fd00:16::1/64 dev attachment nodad
ip link set attachment up
file=/lab/before.conf
[[ $mode == cold ]] && file=/lab/after.conf
cp "$file" /etc/frr/frr.conf
chown frr:frr /etc/frr/frr.conf
/usr/lib/frr/zebra -f /dev/null -i /tmp/zebra.pid >>/out/zebra.log 2>&1 &
zebra_pid=$!
/usr/lib/frr/bgpd -f /dev/null -i /tmp/bgpd.pid >>/out/bgpd.log 2>&1 &
bgp_pid=$!
for i in $(seq 1 100); do
 if vtysh -d zebra -c 'show version' >/dev/null 2>&1 && vtysh -d bgpd -c 'show version' >/dev/null 2>&1; then break; fi
 sleep .1
done
vtysh -b > /out/boot-load.log 2>&1
wait_config() {
 local pattern=$1
 for i in $(seq 1 100); do
  vtysh -c 'show running-config' >/out/wait-running.conf 2>/dev/null || true
  if grep -q "$pattern" /out/wait-running.conf; then return; fi
  sleep .1
 done
 echo "configuration did not become ready: $pattern" >&2
 exit 1
}
if [[ $mode == cold ]]; then wait_config 'redistribute kernel route-map OVN_SUBNET6'; else wait_config 'network fd00:16::d/128'; fi
vtysh -c 'show version' > /out/version.txt
vtysh -c 'show running-config' > /out/start-running.conf
vtysh -c 'show bgp vrf tenant-a ipv4 unicast json' > /out/start-rib4.json
vtysh -c 'show bgp vrf tenant-a ipv6 unicast json' > /out/start-rib6.json
for attempt in 1 2 3; do
 cp /lab/after.conf /etc/frr/frr.conf
 set +e
 python3 /usr/lib/frr/frr-reload.py --test --logfile /dev/null /etc/frr/frr.conf >"/out/reload-$attempt-test.log" 2>&1
 test_rc=$?
 python3 /usr/lib/frr/frr-reload.py --reload --logfile /dev/null /etc/frr/frr.conf >"/out/reload-$attempt.log" 2>&1
 reload_rc=$?
 set -e
 printf '%s\t%s\t%s\n' "$attempt" "$test_rc" "$reload_rc" >>/out/status.tsv
 vtysh -c 'show running-config' >"/out/reload-$attempt-running.conf"
 vtysh -c 'show bgp vrf tenant-a summary json' >"/out/reload-$attempt-summary.json"
 vtysh -c 'show bgp vrf tenant-a ipv4 unicast json' >"/out/reload-$attempt-rib4.json"
 vtysh -c 'show bgp vrf tenant-a ipv6 unicast json' >"/out/reload-$attempt-rib6.json"
done
kill "$bgp_pid"
wait "$bgp_pid" || true
kill "$zebra_pid"
wait "$zebra_pid" || true
rm -f /var/run/frr/zebra.vty /var/run/frr/bgpd.vty
cp /etc/frr/frr.conf /out/restart-input.conf
chown frr:frr /etc/frr/frr.conf
/usr/lib/frr/zebra -f /dev/null -i /tmp/zebra.pid >>/out/zebra.log 2>&1 &
zebra_pid=$!
/usr/lib/frr/bgpd -f /dev/null -i /tmp/bgpd.pid >>/out/bgpd.log 2>&1 &
bgp_pid=$!
for i in $(seq 1 100); do
 if vtysh -d zebra -c 'show version' >/dev/null 2>&1 && vtysh -d bgpd -c 'show version' >/dev/null 2>&1; then break; fi
 sleep .1
done
vtysh -b > /out/boot-load.log 2>&1
wait_config 'redistribute kernel route-map OVN_SUBNET6'
vtysh -c 'show bgp vrf tenant-a summary json' >/out/restart-summary.json
vtysh -c 'show running-config' >/out/restart-running.conf
vtysh -c 'show bgp vrf tenant-a ipv4 unicast json' >/out/restart-rib4.json
vtysh -c 'show bgp vrf tenant-a ipv6 unicast json' >/out/restart-rib6.json
cat /out/status.tsv
