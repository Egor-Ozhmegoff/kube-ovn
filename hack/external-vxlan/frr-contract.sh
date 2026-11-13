#!/usr/bin/env bash
set -euo pipefail
# Safety acceptance ONLY for frr-kubernetes.sh's disposable kind fixture.
: "${KUBECONFIG:?Set the dedicated test kubeconfig}"
: "${EXTERNAL_VXLAN_LAB:?Set the dedicated kind cluster}"
: "${EXTERNAL_VXLAN_GATEWAY_NODE:?Set the first kind node}"
node=$EXTERNAL_VXLAN_GATEWAY_NODE
fabric=${EXTERNAL_VXLAN_FABRIC_PREFIX:-10.110.193}
out=${EXTERNAL_VXLAN_ARTIFACTS:-/tmp/$EXTERNAL_VXLAN_LAB-contract}
mkdir -p "$out"
kc() { kubectl --cache-dir "${EXTERNAL_VXLAN_KUBECTL_CACHE:-/tmp/ko-evx-kubectl}" "$@"; }
[[ $(docker inspect --format '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$node") == "$EXTERNAL_VXLAN_LAB" ]] || exit 1
[[ $(kc get vpc evx-tenant220 -o 'jsonpath={.spec.externalVXLAN.frr.bridge}') == evb220 ]] || exit 1
if docker exec "$node" ip link show evx-proof-test >/dev/null 2>&1; then echo 'Refusing preexisting test interface' >&2; exit 1; fi
central=$(kc -n kube-system get pods -l app=ovn-central -o 'jsonpath={.items[0].metadata.name}')
nb() { kc -n kube-system exec "$central" -c ovn-central -- ovn-nbctl "$@"; }
retry() { for attempt in {1..120}; do if "$@"; then return; fi; sleep 1; done; return 1; }
disabled() {
  local config
  config=$(nb --bare --columns=other_config find Logical_Switch name=evx-transit-evx-tenant220) || return
  [[ $config != *dynamic-routing-vni* ]] || return 1
  config=$(nb --bare --columns=other_config find Logical_Switch name=evx-secondary220) || return
  [[ $config != *dynamic-routing-vni* ]]
}
revoked() { [[ -z $(kc get node "$node" -o 'jsonpath={.metadata.annotations.ovn\.kubernetes\.io/external-vxlan-validated}') ]]; }
forwarding() { kc -n evx-pods220 exec secondary-1 -- lab-ping -I net1 -n -c 2 -W 2 -M do -s 8972 172.20.220.10; }
fdb() { docker exec "$node" bridge fdb show dev evx220 | awk -v peer="$fabric.100" '$0 ~ "dst "peer" " && /extern_learn/' | sort; }
created=false
restore() {
  if [[ $created == true ]]; then docker exec "$node" ip link delete evx-proof-test; created=false; fi
  kc patch vpc evx-tenant220 --type=json -p='[{"op":"replace","path":"/spec/externalVXLAN/frr/bridge","value":"evb220"}]'
}
trap restore EXIT
retry forwarding
fdb > "$out/fdb-before.txt"
[[ -s $out/fdb-before.txt ]] || { echo 'Expected learned external peer entries' >&2; exit 1; }
echo 'A changed API contract must not activate an unvalidated bridge'
kc patch vpc evx-tenant220 --type=json -p='[{"op":"replace","path":"/spec/externalVXLAN/frr/bridge","value":"evb230"}]'
retry disabled
retry revoked
restore
retry forwarding
echo 'An invalid existing host bridge must revoke approval and disable exchange'
docker exec "$node" ip link add evx-proof-test type dummy
created=true
docker exec "$node" ip link set evx-proof-test master evb220
retry revoked
retry disabled
fdb > "$out/fdb-after.txt"
diff -u "$out/fdb-before.txt" "$out/fdb-after.txt"
docker exec "$node" ip link show evb220
restore
retry forwarding
trap - EXIT
echo 'PASS: API changes and failed host revalidation block native exchange; external FRR FDB survives; recovery restores forwarding'
