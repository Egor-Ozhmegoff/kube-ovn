#!/usr/bin/env bash
set -euo pipefail
export EXTERNAL_VXLAN_FABRIC_PREFIX=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.191}
export EXTERNAL_VXLAN_CLUSTER_PREFIX=${EXTERNAL_VXLAN_CLUSTER_PREFIX:-10.110.192}

# Isolated OVN-only lab. No Kubernetes, FRR or host network mounts.
lab_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
image=${EXTERNAL_VXLAN_IMAGE:-}
if [[ -z $image ]]; then
  case $(docker info --format '{{.Architecture}}') in
    aarch64|arm64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo 'Set EXTERNAL_VXLAN_IMAGE for this architecture' >&2; exit 2 ;;
  esac
  image=$(awk -v arch="$arch" '$1 == arch {print $2}' "$lab_dir/base-images.txt")
  [[ $image == *@sha256:* ]] || { echo "Missing pinned lab image" >&2; exit 2; }
fi
prefix=${EXTERNAL_VXLAN_LAB:-ko-evx}
nodes=${EXTERNAL_VXLAN_NODES:-2}
[[ $nodes == 2 || $nodes == 4 ]] || { echo 'EXTERNAL_VXLAN_NODES must be 2 or 4' >&2; exit 2; }
[[ $prefix =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || exit 2

dc() { docker "$@"; }
inside() { dc exec -e "EXTERNAL_VXLAN_NODES=$nodes" -e "EXTERNAL_VXLAN_FABRIC_PREFIX=$EXTERNAL_VXLAN_FABRIC_PREFIX" -e "EXTERNAL_VXLAN_CLUSTER_PREFIX=$EXTERNAL_VXLAN_CLUSTER_PREFIX" "$prefix-$1" bash /lab/"$2" "${@:3}"; }

case ${1:-up} in
up)
  for name in central hv1 hv2 peer; do
    if dc container inspect "$prefix-$name" >/dev/null 2>&1; then
      echo "Container $prefix-$name exists; use check or down first." >&2
      exit 1
    fi
  done
  dc network create --label kube-ovn.external-vxlan-lab="$prefix" \
    --subnet ${EXTERNAL_VXLAN_FABRIC_PREFIX}.0/24 --opt com.docker.network.driver.mtu=9216 "$prefix-fabric"
  dc network create --label kube-ovn.external-vxlan-lab="$prefix" \
    --subnet ${EXTERNAL_VXLAN_CLUSTER_PREFIX}.0/24 --opt com.docker.network.driver.mtu=9216 "$prefix-cluster"
  dc run -d --name "$prefix-central" --label kube-ovn.external-vxlan-lab="$prefix" \
    --network "$prefix-cluster" --ip ${EXTERNAL_VXLAN_CLUSTER_PREFIX}.2 \
    --mount "type=bind,source=$lab_dir,target=/lab,readonly" \
    --entrypoint bash "$image" /lab/boot.sh central
  for index in $(seq 1 "$nodes"); do
    dc run -d --privileged --name "$prefix-hv$index" \
      --label kube-ovn.external-vxlan-lab="$prefix" \
      --network "$prefix-fabric" --ip "${EXTERNAL_VXLAN_FABRIC_PREFIX}.1$index" \
      --mount "type=bind,source=$lab_dir,target=/lab,readonly" \
      --entrypoint sleep "$image" infinity
    dc network connect --ip "${EXTERNAL_VXLAN_CLUSTER_PREFIX}.1$index" "$prefix-cluster" "$prefix-hv$index"
    inside "hv$index" boot.sh worker "$index"
  done
  dc run -d --privileged --name "$prefix-peer" \
    --label kube-ovn.external-vxlan-lab="$prefix" \
    --network "$prefix-fabric" --ip ${EXTERNAL_VXLAN_FABRIC_PREFIX}.100 \
    --mount "type=bind,source=$lab_dir,target=/lab,readonly" \
    --entrypoint sleep "$image" infinity
  inside peer boot.sh peer
  inside central topology.sh
  for index in $(seq 1 "$nodes"); do inside "hv$index" endpoints.sh worker "$index"; done
  inside peer endpoints.sh peer
  inside central topology.sh sync
  EXTERNAL_VXLAN_LAB=$prefix bash "$lab_dir/tools.sh"
  echo "Lab configured. Run: bash $lab_dir/lab.sh check"
  ;;
check)
  inside central check.sh control
  for index in $(seq 1 "$nodes"); do inside "hv$index" check.sh worker "$index"; done
  inside peer check.sh peer
  ;;
wire)
  artifacts=${EXTERNAL_VXLAN_ARTIFACTS:-$(mktemp -d)}
  mkdir -p "$artifacts"
  for name in hv1 hv2; do
    dc exec "$prefix-$name" bash -c 'tcpdump -Z root -i eth0 -U -s 0 -w /tmp/external-vxlan.pcap "udp port 4789 or udp port 60000" >/tmp/capture.log 2>&1 & echo $! >/tmp/capture.pid'
  done
  capture_cleanup() {
    for name in hv1 hv2; do
      dc exec "$prefix-$name" bash -c 'kill -INT "$(cat /tmp/capture.pid)"' || true
      dc cp "$prefix-$name:/tmp/external-vxlan.pcap" "$artifacts/$name.pcap"
    done
  }
  trap capture_cleanup EXIT
  bash "$lab_dir/lab.sh" check > "$artifacts/check.log"
  capture_cleanup
  trap - EXIT
  go run "$lab_dir/wirecheck" "$artifacts/hv1.pcap" "$artifacts/hv2.pcap"
  echo "Artifacts: $artifacts"
  ;;
tools)
  EXTERNAL_VXLAN_LAB=$prefix bash "$lab_dir/tools.sh"
  ;;
logs)
  for name in central hv1 hv2 peer; do
    echo "Container $prefix-$name"
    dc logs "$prefix-$name"
    dc exec "$prefix-$name" bash -c 'for f in /var/log/ovn/*.log /var/log/openvswitch/*.log; do if test -f "$f"; then tail -n 60 "$f"; fi; done'
  done
  ;;
down)
  for name in central hv1 hv2 hv3 hv4 peer tools; do
    container="$prefix-$name"
    if dc container inspect "$container" >/dev/null 2>&1; then
      owner=$(dc inspect --format '{{index .Config.Labels "kube-ovn.external-vxlan-lab"}}' "$container")
      [[ $owner == "$prefix" ]] || { echo "Refusing to delete unowned $container" >&2; exit 1; }
      dc rm -f "$container"
    fi
  done
  for network in "$prefix-fabric" "$prefix-cluster"; do
    if dc network inspect "$network" >/dev/null 2>&1; then
      owner=$(dc network inspect --format '{{index .Labels "kube-ovn.external-vxlan-lab"}}' "$network")
      [[ $owner == "$prefix" ]] || { echo "Refusing to delete unowned $network" >&2; exit 1; }
      dc network rm "$network"
    fi
  done
  ;;
*) echo "Usage: $0 up|tools|check|wire|logs|down" >&2; exit 2 ;;
esac
