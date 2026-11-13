# Native EVPN external VXLAN

Host FRR supplies the dynamic control plane. OVN performs logical switching,
routing and distributed anycast gateways; OVS encapsulates workloads into
external VXLAN on UDP 4789. Linux bridge/VXLAN/VRF/advertise interfaces are
host-owned control-plane devices. Kube-OVN validates them and never creates,
changes or deletes their FDB/neighbors/routes. Cluster VXLAN is retained with
a separate internal VTEP IP on each node.

See [architecture, API, installation and diagnostics](native-evpn-frr.md) and
[validation evidence](external-vxlan-validation.md). Published bases are pinned
in `hack/external-vxlan/base-images.txt`; building the fork replaces its Go
binaries, without compiling OVN/OVS.

## Removal of the static prototype

`externalVXLAN.frr` is required. Static `peers`, MAC lists and external `routes`
are no longer part of the API. Missing FRR contracts fail validation; they do
not select a fallback mode. The daemon has no static Netlink write path and
never overwrites host FRR OVS external IDs. Ordinary Kube-OVN static routing
features outside this fork's externalVXLAN integration are unchanged.

Before upgrading a node with the old prototype, withdraw its old attachments
using the old version and wait for its owned NB state and Linux control-plane
devices to be removed. Then provision the host FRR interfaces and settings
through their owner, install the new CRDs/image and apply dynamic attachments.
Do not assume deleting CRD fields cleans host state. An old
`kube-ovn-external-vxlan` OVS owner or interface alias is rejected, rather than
adopted or automatically removed. The remaining NB migration cleanup only
removes the former attachment's tagged routes and per-port MAC bindings; it
never creates static remote state. Retain a backup of original manifests and
coordinate withdrawal with the fabric before migrating live workloads.

## Secondary OVN networks and migration

A secondary provider `<nad>.<namespace>.ovn` selects an OVN overlay, independently
of VLAN/ProviderNetwork. Create the matching Multus NAD using `type: kube-ovn`
and the daemon socket. Remove `spec.vlan` from the replacement overlay Subnet.

1. Record guest MAC/IP and existing gateway MAC/IP. Evacuate or stop VMs before
   switching their attachment; do not attach both old and replacement networks.
2. Provision explicit FRR bridge/VXLAN/advertise/VRF contracts for each L2VNI
   and tenant L3VNI. Validate reverse routes, RMAC and BGP export policies.
3. Create the replacement Subnet/NAD with a custom VPC, distributed gateway,
   no NAT and the recorded anycast gateway MAC/IP. No Vlan/ProviderNetwork is
   needed. Retire old physical attachments only through their owner after all
   users have drained.
4. Start endpoints with reserved identities. Verify same-network traffic,
   same-VPC routing, external L2/L3 traffic and jumbo packets in both directions.
   Check type-2 withdrawal/new-VTEP learning after actual VM live migration.

Pod/Multus attachment and VIF moves are not proof of KubeVirt live migration.
Cisco interoperability still requires the actual Nexus model and hardware tests.

## Complete site example

`hack/external-vxlan/site-networks.tsv` contains all 17 source VNIs/CIDRs/gateways
with neutral names `vm-net-<VNI>`. Run:

```sh
bash hack/external-vxlan/render-site.sh > /tmp/native-vxlan-site.yaml
```

The renderer emits only dynamic host FRR mode: VPCs/L3VNIs 220 and 230, Pod
CIDRs 172.16.0.0/24 and 172.16.1.0/24, and 17 Subnet/NAD pairs. Provision its
explicit interface names through the host owner before applying manifests.
Use `GATEWAY_MAC_FILE` (name/TAB/MAC) to retain existing VM-facing gateway MACs;
the generated locally administered defaults are for new networks.

Example external identities (internal cluster VTEPs remain separate):

| Node | Management | External VTEP | L3 Router MAC |
| --- | --- | --- | --- |
| worker-1 | 10.64.244.65 | 10.110.191.11 | 02:ee:0b:00:00:00 |
| worker-2 | 10.64.244.66 | 10.110.191.12 | 02:ee:0c:00:00:00 |
| worker-3 | 10.64.244.68 | 10.110.191.13 | 02:ee:0d:00:00:00 |
| worker-4 | 10.64.244.69 | 10.110.191.14 | 02:ee:0e:00:00:00 |

Replace example node names/IPs with the actual site inventory. Each node uses
one RMAC for both L3VNIs; it differs between nodes and from the VM-facing
anycast gateway MAC. Workload MTU is 9000; the complete underlay path must
support 9216. FRR BGP reachability is not proof of jumbo VXLAN reachability.
