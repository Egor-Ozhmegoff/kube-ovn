#!/usr/bin/env bash
set -euo pipefail

# Only test utilities are installed. The OVN/OVS image is not rebuilt.
lab_prefix=${EXTERNAL_VXLAN_LAB:-ko-evx}
helper="$lab_prefix-tools"
if docker container inspect "$helper" >/dev/null 2>&1; then
  owner=$(docker inspect --format '{{index .Config.Labels "kube-ovn.external-vxlan-lab"}}' "$helper")
  [[ $owner == "$lab_prefix" ]] || { echo "Unowned tools container: $helper" >&2; exit 1; }
else
  docker run -d --name "$helper" --label "kube-ovn.external-vxlan-lab=$lab_prefix" \
    --entrypoint bash "${EXTERNAL_VXLAN_TOOLS_IMAGE:-golang:1.26.4}" -c \
    'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends iputils-ping tcpdump && touch /tmp/tools-ready && sleep infinity'
fi
for attempt in {1..120}; do
  docker exec "$helper" test -f /tmp/tools-ready && break
  if [[ $(docker inspect --format '{{.State.Running}}' "$helper") != true || $attempt == 120 ]]; then
    docker logs "$helper"
    exit 1
  fi
  sleep 1
done
task_tmp=$(mktemp -d)
trap 'rm -rf -- "$task_tmp"' EXIT
docker cp "$helper:/usr/bin/ping" "$task_tmp/lab-ping"
docker cp "$helper:/usr/bin/tcpdump" "$task_tmp/tcpdump"
pcap_library=$(docker exec "$helper" bash -c "ldd /usr/bin/tcpdump | awk '/libpcap.so.0.8/ {print \$3}'")
[[ -n $pcap_library ]] || { echo 'libpcap not found in tools image' >&2; exit 1; }
docker cp -L "$helper:$pcap_library" "$task_tmp/libpcap.so.0.8"
workers="hv1 hv2"
[[ ${EXTERNAL_VXLAN_NODES:-2} != 4 ]] || workers="$workers hv3 hv4"
for name in $workers peer; do
  container="$lab_prefix-$name"
  docker cp "$task_tmp/lab-ping" "$container:/usr/local/bin/lab-ping"
  docker cp "$task_tmp/tcpdump" "$container:/usr/local/bin/tcpdump"
  docker cp "$task_tmp/libpcap.so.0.8" "$container:/usr/local/lib/libpcap.so.0.8"
  docker exec "$container" ldconfig
done
