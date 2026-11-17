#!/usr/bin/env bash
# Full repository checks in a disposable Linux worker; source is a task copy.
set -uo pipefail
export PATH=/opt/checktools/go/bin:/opt/checktools/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export GOTOOLCHAIN=local GOPROXY=https://proxy.golang.org,direct
export GOPATH=/runroot/cache/gopath GOMODCACHE=/runroot/cache/mod GOCACHE=/runroot/cache/build
export GOLANGCI_LINT_CACHE=/runroot/cache/lint GOMAXPROCS=2 GOFLAGS=-p=2 CI=true
export HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= https_proxy= http_proxy= all_proxy=
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0=/work
export PYTHONDONTWRITEBYTECODE=1
cd /work
mkdir -p /runroot/results /tmp/cni-ovsdb-bin
ln -sf "$(command -v ovsdb-tool)" /tmp/cni-ovsdb-bin/ovsdb-tool
ln -sf "$(command -v ovsdb-server)" /tmp/cni-ovsdb-bin/ovsdb-server
export OVSDB_CNI_TEST_BIN_DIR=/tmp/cni-ovsdb-bin
status=/runroot/results/status.tsv
printf 'check\texit\tstartedUTC\tfinishedUTC\n' > "$status"
run() {
 local name=$1
 shift
 local started finished result
 started=$(date -u +%FT%TZ)
 printf '%s START %s\n' "$started" "$name"
 timeout --signal=TERM --kill-after=30s 90m "$@" >"/runroot/results/$name.log" 2>&1
 result=$?
 finished=$(date -u +%FT%TZ)
 printf '%s\t%s\t%s\t%s\n' "$name" "$result" "$started" "$finished" >> "$status"
 printf '%s END %s exit=%s\n' "$finished" "$name" "$result"
}
{
 uname -a
 go version
 golangci-lint version
 helm version --short
 ovs-vswitchd --version
 ovn-controller --version
 git branch --show-current
 git rev-parse HEAD
 git status --short
} >/runroot/results/environment.log 2>&1
run ci-ipsec-container python3 hack/test-ipsec-container.py
run ci-cni-security bash dist/images/install-cni-security_test.sh
run ci-install-syntax bash -n dist/images/install.sh
run ci-ovn-db-mode python3 hack/test-ovn-db-mode.py
run ci-egress-init bash dist/images/init-vpc-egress-gateway_test.sh
run ci-egress-primary-cni bash dist/images/init-vpc-egress-gateway-primary-cni_test.sh
run make-ut make ut
run ipsec-race go test -race ./pkg/ipsec
run controller-ipsec-race go test -race ./pkg/controller -run TestIPsec -count=1
run openperouter-race go test -race ./pkg/openperouter/... ./pkg/ovsdb/table
run openperouter-audit-tests python3 -m unittest discover -s test/unittest -p test_openperouter_audit.py -v
run gen-crd make gen-crd
sha256sum charts/kube-ovn/templates/kube-ovn-crd.yaml charts/kube-ovn-v2/crds/kube-ovn-crd.yaml dist/images/install.sh yamls/gen/kube-ovn-crd.yaml >/runroot/results/crd-before.sha256
run gen-crd-repeat make gen-crd
run gen-crd-stability sha256sum -c /runroot/results/crd-before.sha256
run make-lint make lint
run standalone-lint golangci-lint run --timeout=30m
run modernize bash -o pipefail -c "go list ./... | grep -vE 'github.com/kubeovn/kube-ovn/mocks|github.com/kubeovn/kube-ovn/pkg/apis/kubeovn|github.com/kubeovn/kube-ovn/pkg/client' | xargs go tool modernize -test"
run build-go make build-go
run helm-default helm lint charts/kube-ovn charts/kube-ovn-v2
run helm-v1-openperouter helm lint charts/kube-ovn -f docs/openperouter/examples/helm-v1.yaml
run helm-v2-openperouter helm lint charts/kube-ovn-v2 -f docs/openperouter/examples/helm-v2.yaml
run helm-render-v1 bash -c 'helm template lab charts/kube-ovn -f docs/openperouter/examples/helm-v1.yaml > /runroot/results/helm-v1.yaml'
run helm-render-v2 bash -c 'helm template lab charts/kube-ovn-v2 -f docs/openperouter/examples/helm-v2.yaml > /runroot/results/helm-v2.yaml'
run diff-check git diff --check
git diff -- go.mod go.sum >/runroot/results/module-diff.patch
printf '%s\n' 'Repository suite finished; inspect every status, including make-lint/verify-crd.'
