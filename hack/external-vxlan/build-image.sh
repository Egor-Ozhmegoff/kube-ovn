#!/usr/bin/env bash
set -euo pipefail

# No push and no OVN/OVS compilation. Set DOCKER_CONFIG explicitly when needed.
root=$(cd "$(dirname "$0")/../.." && pwd)
mode=${1:-image}
arch=${ARCH:-$(go env GOARCH)}
case "$arch" in amd64|arm64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 2 ;; esac
base=$(awk -v arch="$arch" '$1 == arch {print $2}' "$root/hack/external-vxlan/base-images.txt")
[[ $base == *@sha256:* ]] || { echo "Missing pinned base for $arch" >&2; exit 2; }
case "$mode" in binaries|image) ;; *) echo "Usage: $0 binaries|image" >&2; exit 2 ;; esac
out=${OUTPUT_DIR:-$(mktemp -d)}
mkdir -p "$out"
out=$(cd "$out" && pwd)
cd "$root"
revision=${SOURCE_REVISION:-$(git rev-parse HEAD)}
version=${VERSION:-v1.17.0}
build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
ldflags="-s -w -X github.com/kubeovn/kube-ovn/versions.COMMIT=$revision -X github.com/kubeovn/kube-ovn/versions.VERSION=$version -X github.com/kubeovn/kube-ovn/versions.BUILDDATE=$build_date"
for component in cni controller daemon cmd ipsec; do
  binary=kube-ovn-$component
  path=./cmd/$component
  [[ $component != cni ]] || binary=kube-ovn
  [[ $component != cmd ]] || path=./cmd
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$out/$binary" "$path"
done
cp dist/images/Dockerfile.external-vxlan "$out/Dockerfile"
echo "Built fork binaries in $out; prebuilt base: $base"
if [[ $mode == image ]]; then
  : "${IMAGE:?Set IMAGE to a local tag; this script never publishes}"
  docker build --platform "linux/$arch" --build-arg "BASE_IMAGE=$base" -t "$IMAGE" "$out"
fi
