#!/usr/bin/env bash
# Linux component test, not an OpenPERouter deployment or an end-to-end test.
# Run ONLY in a fresh disposable, privileged container with --network none.
set -euo pipefail
[[ -f /.dockerenv && ${KUBE_OVN_DISPOSABLE_LAB:-} == true ]] || {
  echo 'Requires a disposable Docker container and KUBE_OVN_DISPOSABLE_LAB=true' >&2
  exit 2
}
[[ $(id -u) == 0 ]] || { echo 'Requires root inside the disposable container' >&2; exit 2; }
for command in ip ping iptables sysctl; do
  command -v "$command" >/dev/null || { echo "Missing dependency: $command" >&2; exit 2; }
done
[[ $(ip -o link show | wc -l) -eq 1 ]] || {
  echo 'Requires --network none and no pre-existing interfaces beyond loopback' >&2
  exit 2
}
for name in opr-source opr-outside; do
  if ip netns list | awk '{print $1}' | grep -Fxq "$name"; then
    echo "Refusing pre-existing namespace $name" >&2
    exit 2
  fi
done
cleanup() {
  ip netns del opr-source 2>/dev/null || true
  ip netns del opr-outside 2>/dev/null || true
  ip link del opr-tenant 2>/dev/null || true
}
trap cleanup EXIT
ip netns add opr-source
ip netns add opr-outside
ip link add opr-tenant type vrf table 110001
ip link set opr-tenant up
ip link add opr-ingress type veth peer name src0 netns opr-source
ip link set opr-ingress master opr-tenant
ip address add 10.250.10.1/24 dev opr-ingress
ip link set opr-ingress up
ip -n opr-source link set lo up
ip -n opr-source address add 10.250.10.2/24 dev src0
ip -n opr-source link set src0 up
ip -n opr-source route add default via 10.250.10.1
ip link add opr-mgmt type veth peer name ext0 netns opr-outside
ip address add 192.0.2.1/24 dev opr-mgmt
ip link set opr-mgmt up
ip route add default via 192.0.2.2 dev opr-mgmt
ip -n opr-outside link set lo up
ip -n opr-outside address add 192.0.2.2/24 dev ext0
ip -n opr-outside address add 203.0.113.10/32 dev lo
ip -n opr-outside link set ext0 up
ip -n opr-outside route add 10.250.10.0/24 via 192.0.2.1
ip netns exec opr-outside sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.ext0.rp_filter=0
sysctl -qw net.ipv4.ip_forward=1 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0 net.ipv4.conf.opr-ingress.rp_filter=0 net.ipv4.conf.opr-mgmt.rp_filter=0 net.ipv4.conf.opr-tenant.rp_filter=0
ip netns exec opr-outside iptables -t raw -N OPRTEST
ip netns exec opr-outside iptables -t raw -A PREROUTING -p icmp --icmp-type echo-request -s 10.250.10.2 -d 203.0.113.10 -j OPRTEST
ip netns exec opr-outside iptables -t raw -A OPRTEST -j ACCEPT
capture() {
  local phase=$1
  ip netns exec opr-outside iptables -t raw -Z OPRTEST
  ip netns exec opr-source ping -n -c 2 -W 1 203.0.113.10 >"/tmp/$phase.ping" 2>&1 || true
  local packets
  packets=$(ip netns exec opr-outside iptables -t raw -L OPRTEST -n -v -x | awk '$3 == "ACCEPT" {print $1; exit}')
  printf '%s matching external ICMP packets: %s\n' "$phase" "$packets"
  [[ "$packets" =~ ^[0-9]+$ && "$packets" -gt 0 ]]
}
uname -r
ip rule show
ip route get 203.0.113.10 from 10.250.10.2 iif opr-ingress || true
if capture unguarded; then
  echo 'CONFIRMED: tenant packet reached management path without terminal VRF default'
else
  echo 'No leak reproduced; investigate before asserting a native PE blocker' >&2
  exit 1
fi
ip route add unreachable default table 110001 metric 32767
if capture guarded; then
  echo 'FAIL: tenant packet escaped despite terminal unreachable default' >&2
  exit 1
fi
ip route show table 110001
printf '%s\n' 'PASS: terminal unreachable default prevents management escape in isolated Linux component fixture'
