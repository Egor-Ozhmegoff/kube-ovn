# Native EVPN with host FRR

Host FRR owns BGP EVPN sessions and received routes. OVN owns logical switching,
distributed VM gateways and forwarding policy. OVS performs external workload
VXLAN encapsulation on UDP 4789. Linux bridge, shadow VXLAN, advertise dummy
and VRF devices are control-plane interfaces. They must have no workload,
physical or gateway-Pod ports. Cluster `ovn-encap-ip`/`ovn-encap-type` remain
unchanged; provision the external VTEP address independently in the underlay.

`externalVXLAN.frr` is required for every native EVPN attachment. Remote
VTEPs, MAC/IP entries and external prefixes are supplied exclusively by host
FRR. The previous static `peers`/`routes` API and daemon programming path have
been removed. This is an intentional breaking change for static attachments.
Use separate internal-cluster and external-EVPN VTEP IPs on every node; keep
cluster VXLAN and provision both addresses through their existing owners.

## Version evidence and mechanisms

The supported baseline remains OVN **26.03.3** / OVS **3.7.1** with a consistent
central/controller stack. Pinned published bases in
`hack/external-vxlan/base-images.txt` contain OVN 26.03.4 / OVS 3.7.2. Their
version binaries were executed on amd64 and arm64. The documentation `latest`
describes a development version and is not the source of this version claim.

Checked against the **v26.03.3** sources:

| Function | Versioned evidence |
|---|---|
| VNI, bridge, VXLAN, advertise interface and local MAC/IP redistribution | `ovn-nb.xml`, Logical_Switch EVPN options |
| Remote VTEP/FDB and IP/MAC learning; local exports | `controller/neighbor-exchange.c`, `neighbor-exchange-netlink.c` |
| VRF, table ID, per-port route filtering and connected redistribution | `ovn-nb.xml`, Logical_Router/Logical_Router_Port dynamic-routing options |
| Route import/export and resynchronization | `controller/route-exchange.c`, `route-exchange-netlink.c` |
| EVPN FDB/ARP advertisement, update/withdrawal and IP route exchange | `tests/system-ovn.at`, EVPN switch tests and route-exchange tests |

These are two cooperating mechanisms:

1. **EVPN integration (MAC/IP)**: northd derives local advertisements from OVN
   ports; ovn-controller publishes only locally bound workload identities as
   static FDB entries on the advertise dummy and NOARP neighbors on the bridge.
   Zebra observes these and bgpd announces EVPN type 2. IMET/type 3 supplies BUM
   membership. FRR installs remote VTEP/MACs on the shadow VXLAN with
   `extern_learn` and remote neighbors on the bridge with `NOARP,extern_learn`.
   OVN consumes these through Netlink and programs OVS external tunnels and
   forwarding. MAC/IP updates and withdrawals follow this same path.
2. **IP Route Exchange (prefixes)**: northd exports connected OVN prefixes;
   ovn-controller writes `RTPROT_OVN` routes into the tenant VRF table. FRR must
   redistribute those kernel routes and export the intended EVPN type-5
   prefixes. FRR's imported prefixes must be installed into that table with a
   dynamic protocol (> `RTPROT_STATIC`, e.g. BGP), an actual inner next hop and
   the configured bridge as output interface. OVN imports them on the matching
   DGP, derives learned routes and forwards through the L3VNI. `proto static`
   routes are deliberately not imported. A prefix alone supplies neither its
   next-hop Router MAC nor the VTEP: FRR must also install the matching L3
   neighbor and remote-RMAC FDB entry via EVPN integration.

Kube-OVN never writes controller-owned SB Learned_Route or MAC_Binding rows.
It does not implement a BGP speaker, route reflector, or EVPN MAC mobility engine.

## Ownership and recovery

| Object/state | Owner | Kube-OVN behavior |
|---|---|---|
| External VTEP address, underlay routes/MTU | Host/underlay administration | Validate preexistence; preserve cluster encap |
| Linux VRF, bridge, shadow VXLAN, advertise dummy | Existing host provisioning | Explicit names/VRF table/VNI/local IP/control port/type/master checks; no create, adoption, mutation or delete |
| OVS EVPN source/port and dynamic-routing-port-mapping | Existing host provisioning | Require exact preprovisioned values; never overwrite or remove |
| Remote FDB/neighbors/BGP routes | FRR zebra/bgpd | Never flush, replace or delete |
| Local advertised FDB/neighbors/`RTPROT_OVN` routes | ovn-controller | Enable supported native exchange; native OVN owns withdrawal |
| NB EVPN switch options, DGPs and route-exchange options | Kube-OVN controller | Restore desired API state and remove owned state when withdrawn |
| OVS tunnels/OpenFlow and derived SB state | OVN | No direct programming by this integration |

The host contract permits only the declared VXLAN and dummy as bridge members;
a bridge with physical, workload or other members is rejected. The L3 bridge
MAC must equal the node annotation RMAC. The VM-facing anycast gateway MAC is
configured separately on the Subnet and remains common across nodes. Existing
host L2 bridge MACs may be node-specific and are not the VM gateway identity.

The ownership distinction is essential: in OVN 26.03, a NOARP neighbor without
`extern_learn` is treated as OVN-owned. FRR remote neighbors must retain
`extern_learn`, and remote FDB entries must retain their remote VTEP and
`extern_learn`. Otherwise OVN may remove them or misinterpret ownership. Never
install competing local static entries on the advertise device. This fork does
not repair incorrect FRR flags by copying entries to other tables.

OVN performs initial Netlink dumps and recurring reconciliation, in addition to
event notifications. It restores local exports and consumes retained remote
state after ovn-controller restart. FRR must restore its remote kernel state
and relearn local FDB/neighbors after restart. kube-ovn-daemon restart performs
validation again and leaves both owners' entries intact. A missing or mismatched
host object is reported as an error and is not recreated silently. Host setup
must finish before enabling the API; inspect daemon errors during provisioning.

### Validation before activation

The daemon publishes `ovn.kubernetes.io/external-vxlan-validated` on its Node
only after successful runtime, interface, RMAC and port-mapping checks. Do not
set this annotation manually. It fingerprints the Node UID and boot ID,
external VTEP/RMAC, native resource UIDs, VNIs, host-interface contracts and
this node's routing-port mappings. Controller and daemon use the same desired
resource snapshot format; status and administrative egress eligibility do not
change the fingerprint.

Before enabling NB EVPN or router exchange, the controller requires matching
acknowledgements from declared gateways, externally annotated nodes, and hosts
with existing VPC IP allocations. EVPN switch options are shared across
chassis, so checking only the selected egress node is insufficient. Provision
the complete native interface plan on each externally annotated host. CNI ADD
requires a matching acknowledgement and declared ingress identity for all
subnets of a host-FRR VPC, including ordinary Pod subnets that make the transit
switch local. DEL remains available without an acknowledgement.

Failed validation revokes the acknowledgement. Reconciliation then removes
owned native exchange configuration for the affected VPC, while preserving its
ordinary logical gateways and host-owned interfaces/settings. Incorrect host
configuration therefore blocks external exchange for the VPC rather than
silently activating an unsafe subset of shared switch options. Missing logical
switches are harmless during this cleanup. Node NotReady alone does not revoke
a previously validated interface contract; the separate egress failover logic
still applies.

Deactivation clears the EVPN switch options and router exchange options but
retains owned transit ports, their RMACs and the transit switch's ownership
marker. Revalidation can then recover ingress identities for members removed
while their workloads drain. Deleting these ports during the validation wait
would lose the information required for VM type-2 return traffic.

This acknowledgement is not a dataplane health lease or an atomic lock on Linux
configuration. Rechecks run every 10 seconds and controller resync every 30
seconds; API availability is needed to publish revocation. Withdraw the native
attachment before replacing host interfaces. A reboot, identity or interface
contract change requires revalidation before new activation.
Mapping or membership changes can therefore pause external exchange while
the affected hosts revalidate. Use `egress: false` during evacuation; changing
egress eligibility alone does not invalidate the interface acknowledgement.

For the OVN 26.03 baseline, VRF table IDs must be in `1..4294967294`, excluding
`252..255`. Both admission/Go validation and generated CRDs enforce this. The
broader main/default-table learning described by development documentation is
not enabled by this API.

Withdraw the Kubernetes attachment first and wait for OVN to remove its local
exports and owned NB state before retiring host interfaces through their owner.
The fork never deletes host FRR interfaces or foreign OVS settings on cleanup.
When upgrading an old static attachment, complete withdrawal and cleanup
using the old version, then provision and validate the required host FRR contract.

## Four nodes and two tenants

The [site example](external-vxlan.md#complete-site-example) records external
VTEPs and RMACs for worker-1 through worker-4. Internal VTEP IPs are separate. Annotate each node with
`ovn.kubernetes.io/external-vxlan-ip` and
`ovn.kubernetes.io/external-vxlan-router-mac`. RMAC is shared by a node's
L3VNIs 220/230 and differs between nodes.

Example tenant 220 (tenant 230 uses VNI/table 230, vrf230, cp230/cpvx230/adv230,
and corresponding transit addresses):

```yaml
apiVersion: kubeovn.io/v1
kind: Vpc
metadata: {name: vrf220}
spec:
  defaultSubnet: pods220
  namespaces: [evpn-app220]
  externalVXLAN:
    vni: 220
    frr:
      bridge: cp220
      vxlan: cpvx220
      advertise: adv220
      controlPort: 60001
      vrf: vrf220
      tableID: 220
    gatewayNodes:
    - {node: worker-1, transitCIDR: 169.254.220.11/24, routingPort: native-220-worker-1}
    - {node: worker-2, transitCIDR: 169.254.220.12/24, routingPort: native-220-worker-2}
    - {node: worker-3, transitCIDR: 169.254.220.13/24, routingPort: native-220-worker-3}
    - {node: worker-4, transitCIDR: 169.254.220.14/24, routingPort: native-220-worker-4}
```

Each host's existing OVS `dynamic-routing-port-mapping` must map its listed
`routingPort` token to `cp220`/`cp230`. This is an arbitrary logical mapping key,
not necessarily the new NB port's name. Existing `native-220-worker-1=cp220`
mappings can therefore be reused without changing host FRR provisioning.
The legacy `gatewayNode`, root `transitCIDR` and root `routingPort` remain
supported for a single gateway; do not combine them with `gatewayNodes`.

Multiple DGPs, each bound to its own chassis and RMAC, permit OVN active-active
forwarding when their imported routes provide usable ECMP members. Ready nodes
with `egress` unset or true participate. An entry with `egress: false` remains
an explicitly declared VM-host ingress identity, allowing new VM attachments
while external egress is administratively disabled. NotReady/Unknown,
NetworkUnavailable or deleting nodes also lose egress during periodic
reconciliation (up to 30 seconds plus Kubernetes failure detection).

An inactive FRR member becomes an **ordinary distributed router port**, retaining
its RMAC but removing its Gateway_Chassis binding. OVN's chassis-bound IP Route
Exchange stops there, withdrawing learned egress paths and type-5 prefix exports.
The ingress port remains essential: a VM's more-specific EVPN type-2 host route
still points to that VM's VTEP/RMAC even when the aggregate type-5 route moves.
Deleting the entire port would blackhole return traffic to that VM.
`dynamic-routing-no-learning` alone is insufficient: it does not suppress local
route export. Conversion to an ordinary router port is the supported mechanism.

On membership removal, an owned ingress-only port remains while VPC workload IPs
are allocated to that node. It is cleaned up when the last allocation is
released/moved, or the Node/chassis disappears. New CNI attachments require an
explicit node entry. Use `egress: false` during evacuation, retain that entry
through live migration, and remove it after migration completes. Do not infer
migration completion merely from an IP reservation's NodeName. A new VM host
must be declared before attachment: Kube-OVN does not invent transit IPs or RMAC
identities. Recovery rebinds eligible ports to their own chassis.
`status.externalVXLANReadyNodes` lists eligible Ready egress members, not
ingress-only ports and not verified tunnel health.

The FRR acceptance lab checks four-way egress and wire RMACs, type-2/type-5
withdrawal and reverse traffic through an independent OVN/FRR reference fabric.
See [recorded validation](external-vxlan-validation.md) for tested scenarios
and the remaining hardware/KubeVirt limits.

Kubernetes detects kubelet/node health. OVN BFD/chassis liveness detects selected
OVN tunnel failures. FRR BGP/BFD detects configured routing adjacencies; a live
BGP session does not prove every data tunnel works. Coordinate fabric next-hop
tracking and actual packet tests. This fork cannot force FRR to withdraw its
independent IMET, underlay or unrelated routes based on Kubernetes readiness.

## Secondary KubeVirt networks

Example overlay Subnet (the VPC above must exist):

```yaml
apiVersion: kubeovn.io/v1
kind: Subnet
metadata: {name: vm-net-504221}
spec:
  vpc: vrf220
  provider: vm-net-504221.kubevirt-networks.ovn
  cidrBlock: 172.16.133.0/28
  gateway: 172.16.133.1
  excludeIps: [172.16.133.1]
  gatewayType: distributed
  natOutgoing: false
  mtu: 9000
  externalVXLAN:
    vni: 504221
    gatewayMAC: "02:aa:07:b1:9d:01"
    frr:
      bridge: cp504221
      vxlan: cpvx504221
      advertise: adv504221
      controlPort: 60003
      vrf: vrf220
      tableID: 220
```

Use a Multus NAD with type `kube-ovn`, the matching provider and daemon socket;
no `Vlan`/`ProviderNetwork` attachment. The provider selects an OVN secondary
network independently of a physical VLAN anchor. OVN retains the gateway
IP/MAC on every chassis and routes between networks in the same VPC.
Follow [migration instructions](external-vxlan.md#secondary-ovn-networks-and-migration)
to evacuate old VLAN interfaces and preserve gateway/VM identities. In dynamic
mode FRR/OVN must update MAC/IP advertisements after VM migration. Verify type-2 mobility sequence, old-VTEP
withdrawal, new-VTEP installation and absence of gateway-MAC oscillation.
Pod attachment tests do not validate a KubeVirt live migration.

## Existing FRR configuration: prerequisites, not edits

The requirements repository's generated worker FRR configuration already has
`advertise-all-vni`, `advertise-svi-ip`, per-VNI RTs, VRF VNI 220/230,
`redistribute kernel`, and EVPN `advertise ipv4 unicast`. Its host setup creates
cp/cpvx/adv and VRF interfaces, OVS EVPN settings and native port mappings.
This fork does not change that configuration, daemon or lifecycle.

Dependencies requiring operator review outside this change:

- Its `EXPORT220/230` prefix lists permit only the respective **Pod /24**.
  If external connectivity needs type-5 announcements for the 17 VM prefixes,
  those export policies must allow them, or the fabric must support the intended
  type-2 symmetric IRB path. The fork cannot bypass those FRR policies.
- The old host setup rejects OVN versions other than 26.09. The fork's baseline
  is checked against 26.03 sources; deploying it with that setup requires a
  separately reviewed version-guard change or separately provisioned compatible
  interfaces. Do not rerun the old setup blindly.
- Linux VRF, bridge, VXLAN and OVS kernel support, route/neigh notifications,
  and ovn-controller CAP_NET_ADMIN are required. Linux interface/table/VNI names
  and actual control ports must match the API; provisioning remains external.
- The existing localnet/ProviderNetwork anchors must be retired for these
  overlay VM networks. No workload traffic may enter the CP bridge.
- FRR must preserve remote `extern_learn` flags, install usable L3 next hops
  with RMACs and export OVN kernel routes according to the intended policy.
  Confirm imported route output interfaces match `routingPort` mappings.

## Install, diagnose and test

Use the fork [image workflow and installation procedure](external-vxlan.md#build-and-install-the-fork).
An image must contain the fork controller, daemon, CNI, multicall/webhook and
IPsec cleanup binaries and ovn-controller CAP_NET_ADMIN. Pinned prebuilt bases
avoid recompiling OVN/OVS; a configured workflow is not proof of publication.

Underlay MTU 9216 must cover workload IPv4 MTU 9000 plus VXLAN overhead on every
hop. Shadow ports (60001 etc.) are control-plane sockets; capture must show
workload frames on OVS UDP 4789 and none on shadow ports. Keep cluster VXLAN
source addresses and sockets independent. Inspect:

```sh
vtysh -c 'show bgp l2vpn evpn summary' -c 'show bgp l2vpn evpn route'
vtysh -c 'show evpn vni' -c 'show ip route vrf vrf220'
ip -d link show cpvx220
bridge fdb show dev cpvx220
bridge fdb show dev adv504221
ip neigh show dev cp220
ip route show table 220
ovs-vsctl get Open_vSwitch . external_ids
ovn-sbctl list Learned_Route
ovn-appctl -t ovn-controller evpn/remote-vtep-list
ovn-appctl -t ovn-controller evpn/vtep-arp-list
tcpdump -ni FABRIC_IF -s 0 -vv 'udp port 4789 or udp portrange 60000-60030'
ping -M do -s 8972 EXTERNAL_WORKLOAD_IP
```

`hack/external-vxlan/frr-lab.sh` extends the existing OVN-only lab with separate
test zebra/bgpd instances in the disposable node network namespaces. It never
uses host networking or modifies production FRR. It requires a VRF-capable Linux
Docker host and pinned OVN/OVS and FRR test images. Use `up`, then `check`, and
retain artifacts before `down`. Actual acceptance must cover local and remote
type-2/type-5 update/withdraw, every node's RMAC and VTEP, forward/reverse jumbo
traffic, FRR/controller/daemon restart, membership changes, and migration.

Cisco NX-OS 10.2(4)M compatibility is **unconfirmed**. Establish the Nexus model,
line cards and NVE/VRF capabilities. Check matching VNIs/RTs/RDs, type-2/3/5
import/export, RMAC signaling, recursive L3 next hops, anycast MAC behavior,
MAC mobility, MTU 9216, UDP 4789 and ECMP return paths through all four VTEPs.
No VM-facing gateway on Cisco is required; the VM gateway belongs to OVN.
