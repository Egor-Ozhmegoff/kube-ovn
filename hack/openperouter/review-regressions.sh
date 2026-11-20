#!/usr/bin/env bash
# Run only on a lab machine. All kernel mutations stay in fresh network-none containers.
set -euo pipefail
if [[ $# != 2 ]]; then
  echo "usage: $0 LAB_IMAGE DIRECTORY_WITH_host.test_AND_northbound.test" >&2
  exit 2
fi
image=$1
artifacts=$(cd "$2" && pwd)
docker_config=$(mktemp -d /tmp/kubeovn-opr-review-docker.XXXXXX)
trap 'rm -rf "$docker_config"' EXIT
for name in TestCleanupInternalPortsAndRecreateVPC TestL2VNIDeleteAndReuseOtherVRF TestRebindKeepsTerminalDropOnPartialFailure TestReleaseInstallsMissingTerminal TestIsolationAcrossVRFLoss TestReflectionKernelLifecycle TestDelayedFilterAdmissionPackets; do
  binary=host.test
  if [[ "$name" == TestDelayedFilterAdmissionPackets ]]; then
    binary=northbound.test
  fi
  docker --config "$docker_config" run --rm --privileged --network none \
    -e KUBE_OVN_DISPOSABLE_LAB=true \
    --mount "type=bind,source=$artifacts,target=/tests,readonly" \
    --entrypoint "/tests/$binary" "$image" \
    -test.v -test.timeout 90s -test.run "^$name$"
done
