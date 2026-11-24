#!/usr/bin/env bash
# Validation only. Every kernel mutation stays in a fresh network-none container.
# ARTIFACTS must contain daemon.test, routes.test, northbound.test, host.test,
# ovs.test, netlink-routes.sh and frr-netlink.conf from the source under review.
set -uo pipefail
if [[ $# != 3 ]]; then
  echo "usage: $0 FRR_IMAGE OVS_NFT_IMAGE ARTIFACTS" >&2
  exit 2
fi
frr_image=$1
ovs_image=$2
artifacts=$(cd "$3" && pwd)
docker_config=$(mktemp -d /tmp/kubeovn-subnet-docker.XXXXXX)
trap 'rm -rf "$docker_config"' EXIT
mkdir -p "$artifacts/results"
status="$artifacts/results/status.tsv"
printf 'test\texit\n' >"$status"
failed=0
run() {
  local name=$1
  shift
  "$@" >"$artifacts/results/$name.log" 2>&1
  local result=$?
  printf '%s\t%s\n' "$name" "$result" >>"$status"
  [[ $result == 0 ]] || failed=1
}
run daemon-unit docker --config "$docker_config" run --rm --network none \
  -v "$artifacts:/lab:ro" --entrypoint /lab/daemon.test "$frr_image" \
  -test.run '^TestOpenPERouter(VPC|NodeUID)' -test.v
run subnet-unit docker --config "$docker_config" run --rm --network none \
  -v "$artifacts:/lab:ro" --entrypoint /lab/routes.test "$frr_image" \
  -test.run '^Test(Routed|Subnet|Legacy)' -test.v
run binding docker --config "$docker_config" run --rm --network none \
  -v "$artifacts:/lab:ro" --entrypoint /lab/ovs.test "$frr_image" \
  -test.run '^TestActiveBindingFollowsRARP$' -test.v
run vpc-isolation docker --config "$docker_config" run --rm --privileged --network none \
  -e KUBE_OVN_SUBNET_ISOLATION_LAB=true -v "$artifacts:/lab:ro" \
  --entrypoint /lab/daemon.test "$frr_image" \
  -test.run '^TestOpenPERouterScopedFailureLinux$' -test.v
run subnet-frr docker --config "$docker_config" run --rm --privileged --network none \
  -v "$artifacts:/lab:ro" --entrypoint /bin/bash "$frr_image" /lab/netlink-routes.sh
for name in TestCleanupInternalPortsAndRecreateVPC TestL2VNIDeleteAndReuseOtherVRF TestRebindKeepsTerminalDropOnPartialFailure TestReleaseInstallsMissingTerminal TestIsolationAcrossVRFLoss TestReflectionKernelLifecycle TestDelayedFilterAdmissionPackets TestL2MACMoveWithReflectionGuard; do
  binary=host.test
  if [[ $name == TestDelayedFilterAdmissionPackets || $name == TestL2MACMoveWithReflectionGuard ]]; then
    binary=northbound.test
  fi
  run "$name" docker --config "$docker_config" run --rm --privileged --network none \
    -e KUBE_OVN_DISPOSABLE_LAB=true -v "$artifacts:/lab:ro" \
    --entrypoint "/lab/$binary" "$ovs_image" -test.timeout 90s -test.run "^$name$" -test.v
done
cat "$status"
exit "$failed"
