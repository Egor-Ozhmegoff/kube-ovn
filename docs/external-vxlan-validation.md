# Dynamic native EVPN validation — 2026-11-13

This record covers the dynamic-only worktree based on `67d22b6c`, before any
commit or publication. The first fork commit is `1100f1f3`; its upstream parent
is `3fa1c2a0`. Changes remove the fork's static external VXLAN implementation.
Existing upstream routing APIs and unrelated Helm/API endpoint changes remain
intact. See [architecture/install](native-evpn-frr.md) and
[lab reproduction](../hack/external-vxlan/README.md).

## Environment and image

- Authorized isolated Linux host: `10.64.244.143`, kernel OVS and VRF support.
  Production FRR and unrelated containerlabs were not changed.
- Go 1.27.3; golangci-lint 2.15.0, matching the repository workflow.
- Published base:
  `kubeovn/kube-ovn-dev@sha256:26db119b4dd42fdccdd25d046039c48b39ed69be578456f573978ce45f3ab0db`.
- Executed OVN `26.03.4+g0f5e2ddd977b`, OVS `3.7.2+g84744f1be139`.
- Built test image: `local/kube-ovn-native-vxlan:dynamic-only-20261113`, ID
  `sha256:5498cef657826b57c078c94735c9c6c38745501738ce4a2ceceeb4301a08e1b2`.
  The five fork Go binaries were built; OVN/OVS were not rebuilt. This image
  exists on the test host and was imported into the dedicated kind cluster.
  No registry publication was performed.
- Separate test FRR 10.6.0_git:
  `quay.io/frrouting/frr@sha256:eae3340ca578bb588d92ab3d1e77deb806f80d1da2bc0053cb1dfcfef2aa236f`.
- Fresh four-node lab `ko-dynamic1113`: external VTEPs `10.118.215.11–14`,
  internal cluster VXLAN VTEPs `10.118.216.11–14`; independent OVN/FRR fabric
  reference peer `.100`. Both transports remain VXLAN.
- Existing dedicated two-node kind fixture `ko-frr143-k8s`: Kubernetes 1.36.1,
  Multus, external fabric `10.110.193.0/24`, separate cluster transport.
  Controller and CNI daemon were upgraded to the new image. Existing host FRR
  contracts were retained; this was not a new Kubernetes installation.

## Executed checks

| Check | Result |
| --- | --- |
| Kubernetes API code generation | deepcopy/applyconfiguration/client generation completed; removed peer/route types have no generated leftovers |
| `make gen-crd`, `make lint` | Completed; CRD verification, golangci-lint and Go modernize ran in an isolated Linux source copy; real local Git index was not modified |
| Targeted ExternalVXLAN tests | util/controller/daemon/OVS/webhook passed |
| Full package suites | controller, daemon, OVS, webhook, IPsec and util passed; util needed no exclusion |
| Privileged daemon Netlink test | `TestExternalVXLANFRRPreservesHostState` executed and passed with actual Linux VRF; no root/VRF skip |
| Disabled-IPsec runtime regression | `TestExternalVXLANDisabledIPsecCleanup` executed and passed in a separate OVS container |
| API server dry runs | Accept table IDs 220, 230, 4294967294; reject 0, 252, 254, 255, 4294967295; reject missing FRR and removed peers/routes fields |
| Fresh dynamic bootstrap | Four-node native lab starts without static remote FDB/MAC/routes or manual workload OpenFlow setup |
| Native L2/L3 forwarding | Both tenants, forward and independently initiated reverse traffic, tenant isolation, DF MTU 9000 passed |
| Four-node egress | Wire captures show all four VTEPs with correct per-node RMAC for each L3VNI |
| Dynamic control plane | Type-2/Type-5 withdrawal/restoration, egress withdrawal with retained RMAC, FRR and ovn-controller restart passed |
| VIF mobility | Actual VIF move hv1 → hv2 → hv1 retained guest IP/MAC and OVN gateway; FRR remote FDB followed the new VTEP |
| Broadcast sample | Tenant-220 guest UDP broadcast reached the external tenant-220 guest and was absent on tenant 230 |
| Kubernetes/Multus traffic | Existing primary, VM-subnet and secondary endpoints passed intra-VPC and external jumbo checks |
| Gateway membership | `egress:false`, removal with live allocations, retained ingress and restoration passed |
| Kubernetes NodeNotReady | Kubelet-driven withdrawal, retained ingress and forwarding recovery passed; kubelet restored and both nodes Ready |
| Host contract safety | Invalid API bridge/unexpected bridge member revoked validation and disabled exchange; external FRR FDB survived; recovery passed |
| Actual opt-in e2e execution | 3 external-vxlan specs passed: forwarding/anycast/MTU, NB VNI drift recovery, CNI daemon restart. Other 720 specs were outside the selected group |
| Fresh CNI ADD/DEL | Four new primary+Multus Pods across both nodes/tenants passed forward/reverse jumbo traffic; deletion left no matching IPAM or logical ports |
| No preloaded peer ARP shortcut | Removed permanent local reference-gateway neighbor entries; full native forwarding check passed with ordinary OVN ARP resolution |
| Renderer/shell | `bash -n` passed; generated 2 VPCs, 19 Subnets and 17 NADs with required FRR and no peers/routes; static mode explicitly rejected |

For CRD verification the disposable Linux copy uses its own Git index with the
generated bundles tracked, including normally ignored chart files. No commit
was created. The successful full OVS package rerun followed a correction to the
new cleanup test fixture: static route seed rows require named UUIDs before
attachment. The tested cleanup preserves an ordinary foreign route.

## Final native wire evidence

Captures from all four chassis were checked by the repository's wirecheck
binary. UDP 4789, correct VNI, DF MTU 9000 in both directions and per-node
Router MAC were verified; no shadow UDP 60000 packet was accepted.

| VNI | Outbound / inbound | Egress VTEPs for L3VNI |
| --- | --- | --- |
| 220 | 109 / 109 | `.11`, `.12`, `.13`, `.14` |
| 230 | 96 / 96 | `.11`, `.12`, `.13`, `.14` |
| 504221 | 93 / 82 | L2 probes, both directions |
| 504231 | 87 / 84 | L2 probes, both directions |

The initial fresh-lab capture failed strict wirecheck because existing IPv6
link-local addresses on control-plane dummy devices generated Router
Solicitation over shadow UDP 60000. That failure is retained in the results.
The lab's interface owner now disables IPv6 on its explicitly created IPv4-only
control-plane interfaces; underlay and BGP unnumbered links are untouched.
A second clean bootstrap and complete acceptance/mobility run passed. The
production daemon remains read-only with respect to host devices and FRR state.

The first e2e attempt found missing selector labels on previously recreated
fixture Pods. Labels were restored to the fixture's declared values and the
entire three-spec group was executed again successfully. Initial failed logs
were not replaced by the successful logs. Recovery probes may observe transient
loss while routing converges; Ready/acknowledgement alone is not a traffic test.

## Evidence and limits

Detailed commands, successful and initial failed logs, exit files and captures
are kept under `/tmp/ko-dynamic-only-20261113` on the test host. A local result
copy is stored under `/private/tmp/kubeovn-dynamic-only-20261113`. Scratch Go
binaries and test images are outside the repository; no upstream PR or image
publication was performed.

This does not certify actual KubeVirt VM attachment/live migration, all 17 VM
networks simultaneously, every unknown-unicast/multicast/BUM case, physical
link/VTEP reachability failures, or Cisco Nexus interoperability. Pod attachment
and VIF mobility are distinguished from a VM live migration. The Nexus model,
NX-OS 10.2(4)M capabilities, return-path/RMAC behavior, EVPN export policies and
MTU must still be verified on the real fabric. Production FRR configuration was
not changed or replaced by static inputs.
