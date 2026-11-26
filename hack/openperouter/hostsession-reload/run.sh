#!/bin/bash
set -euo pipefail

fixtureDir=$(cd "$(dirname "$0")" && pwd)
outputDir=${1:?usage: bash run.sh NEW_RESULTS_DIR}
mkdir "$outputDir"
outputDir=$(cd "$outputDir" && pwd)
dockerConfig=$(mktemp -d "${TMPDIR:-/tmp}/hostsession-docker.XXXXXX")
image=quay.io/frrouting/frr:10.6.0
for mode in cold reload; do
    mkdir "$outputDir/$mode"
    docker --config "$dockerConfig" run --rm --privileged --network none \
        --entrypoint /bin/bash \
        -v "$fixtureDir:/lab:ro" -v "$outputDir/$mode:/out" \
        "$image" /lab/probe.sh "$mode" >"$outputDir/$mode.log" 2>&1
done
python3 "$fixtureDir/check.py" "$outputDir"
