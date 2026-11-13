# Host FRR native EVPN lab

This directory tests dynamic FRR -> Linux FDB/neighbors/VRF routes -> Netlink ->
OVN -> OVS VXLAN. There is no static-mode bootstrap. It uses separate external
and cluster VTEP IPs and UDP 4789 for workload VXLAN. Linux control-plane
interfaces have no workload ports. Production FRR and unrelated labs are never
changed; use a disposable Linux host with kernel OVS/VRF support.

## Native four-node reference lab

Set `EXTERNAL_VXLAN_IMAGE` to the pinned base or a built fork image,
`EXTERNAL_VXLAN_LAB` to an unused lab prefix and
`EXTERNAL_VXLAN_ARTIFACTS` to an output directory. The default external and
cluster subnets are 10.110.191.0/24 and 10.110.192.0/24; ensure no overlap with
existing Docker networks before starting. `FRR_TEST_IMAGE` defaults to a pinned
published test FRR. No OVN/OVS or BGP implementation is built locally.

```sh
bash hack/external-vxlan/frr-lab.sh up
bash hack/external-vxlan/frr-lab.sh check
bash hack/external-vxlan/frr-acceptance.sh
bash hack/external-vxlan/frr-mobility.sh
```

`lab.sh`, `boot.sh`, `topology.sh` and `endpoints.sh` are helper stages used by
`frr-lab.sh`, not a second control-plane mode. Native topology originates local
MAC/IP/prefixes; remote state is learned only from the separate test FRR.
`frr-reference-peer.sh` starts another OVN instance for the reference fabric;
no manual tunnel ports or OpenFlow forwarding rules are installed.

Use `frr-lab.sh down` to remove only resources carrying this lab's owner label.
Keep captures and command exit statuses before cleanup. Check both directions,
external VNIs 220/230/504221/504231, per-node RMAC, cluster VXLAN and MTU 9000.
Type-2/Type-5 withdrawal, restart and mobility checks require convergence before
forwarding is asserted. VIF movement is not KubeVirt live migration.

## Dedicated Kubernetes/Multus fixture

Install the fork CRDs, fork image and Multus on an isolated two-node kind
cluster. Set an explicit `KUBECONFIG`, `EXTERNAL_VXLAN_GATEWAY_NODE`,
`EXTERNAL_VXLAN_SECOND_NODE`, `EXTERNAL_VXLAN_WORKLOAD_IMAGE` (lab-ping/ip/sleep),
`EXTERNAL_VXLAN_LAB` (kind cluster name), `FRR_TEST_CONFIGS` (generated native
lab FRR configs), `EXTERNAL_VXLAN_IMAGE` and separate fabric/cluster prefixes.
Provision a fresh reference peer with `boot.sh peer` and `endpoints.sh peer`,
connected to the chosen fabric with external IP .100. Bind this directory as
`/lab` in that disposable peer. The two kind nodes need separate external VTEP
addresses .11/.12 and the existing cluster VXLAN addresses.

```sh
bash hack/external-vxlan/frr-kubernetes.sh
bash hack/external-vxlan/kubernetes.sh configure
bash hack/external-vxlan/secondary.sh configure
bash hack/external-vxlan/kubernetes.sh check
bash hack/external-vxlan/secondary.sh check
bash hack/external-vxlan/frr-membership.sh
bash hack/external-vxlan/frr-readiness.sh
bash hack/external-vxlan/frr-contract.sh
KUBE_OVN_EXTERNAL_VXLAN_E2E=true go test ./test/e2e/kube-ovn \
  -args --kubeconfig="$KUBECONFIG" --ginkgo.focus='group:external-vxlan'
```

Provisioning requires a fresh fixture and refuses duplicate devices/containers.
It creates only test FRR; it does not replace a host FRR service. API manifests
contain required FRR contracts from their first application. The membership,
readiness and contract scripts verify kind ownership before changing test
state. They cover egress removal with retained ingress, kubelet-driven NotReady,
validation revocation and recovery while preserving FRR entries.

`secondary.sh cleanup` verifies IP cleanup before subnet removal. Retire host
interfaces through their original owner after native OVN withdrawal. See
[architecture/install](../../docs/native-evpn-frr.md) and
[validation history](../../docs/external-vxlan-validation.md).
