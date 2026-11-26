#!/bin/bash
set -euo pipefail

sourceRepo=${1:?usage: bash generate.sh OPENPEROUTER_REPO OUTPUT_DIR}
outputDir=${2:?output directory required}
fixtureDir=$(cd "$(dirname "$0")" && pwd)
revision=60a1285935b10eb0b1cf07e4533faefedde3d4d3
workDir=$(mktemp -d "${TMPDIR:-/tmp}/openperouter-render.XXXXXX")

# Only the extracted copy receives the harness. The source checkout stays read-only.
git -C "$sourceRepo" archive "$revision" go.mod go.sum api/v1alpha1 \
    internal/frr internal/ipam internal/ipfamily internal/networklayerprotocol |
    tar -xf - -C "$workDir"
mkdir -p "$workDir/cmd/contract-render" "$outputDir"
outputDir=$(cd "$outputDir" && pwd)
cp "$fixtureDir/renderer-main.go.txt" "$workDir/cmd/contract-render/main.go"
cp "$fixtureDir/raw.conf" "$outputDir/raw.conf"
(
    cd "$workDir"
    GOENV=off GOAUTH=off go run ./cmd/contract-render "$outputDir"
)
printf 'Renderer source: %s\nExtracted source and harness: %s\n' "$revision" "$workDir"
