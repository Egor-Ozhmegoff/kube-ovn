# Native hostSession + trailing RawFRRConfig: failed reload contract

This is a negative FRR compatibility reproducer, not a deployable bootstrap.
Source: OpenPERouter `Mac_setings_for_l2vni_and_l3vni`, commit
`60a1285935b10eb0b1cf07e4533faefedde3d4d3`; FRR image
`quay.io/frrouting/frr:10.6.0`, as pinned in that source's Dockerfile.

The candidate keeps native `hostSession` for L3 veth creation and uses the
existing RawFRRConfig suffix to remove local BGP neighbors and attachment
network statements. Kube-OVN does not install or manage this FRR configuration.

## Source contract and fixture provenance

- `internal/ipam/ipam.go:VethIPsFromPool/vethIPsForFamily` computes PE address
  at network offset 0, shifted to 1 when its final byte is zero. Host address
  is at `nodeIndex+1`, or `nodeIndex+2` after that shift; pool masks are retained.
- With pools `169.254.16.0/20`, `fd00:16::/64` and index 11, PE addresses are
  `169.254.16.1/20`, `fd00:16::1/64`; host addresses are
  `169.254.16.13/20`, `fd00:16::d/64`. Index 12 needs a different suffix:
  neighbors `.14` and `::e`, network statements `.14/32` and `::e/128`.
- `internal/conversion/host_conversion.go:l3vniToHost` supplies LinkIPs only
  with hostSession. `internal/hostnetwork/vni.go:SetupL3VNI` skips host veth
  creation without LinkIPs. `setupHostVeth` assigns addresses and the PE VRF.
- `internal/conversion/frr_conversion.go:l3vniToFRR` produces a tenant config
  for each family, with local neighbor and host attachment /32 or /128.
  `localneighbor.tmpl` activates each neighbor in both unicast families.
- `internal/frr/templates/frr.tmpl` appends RawConfig after tenant BGP.
  `internal/frrconfig/frrconfig.go:Update` executes `--test`, then `--reload`,
  propagating nonzero status. Its ISIS pre-clean does not affect this fixture.

`renderer-main.go.txt` invokes the original IPAM and `frr.ApplyConfig` in an
extracted copy of the pinned source. It constructs the minimal dual-stack
L3VNIConfig inputs following the conversion above; it does not invoke the full
controller/conversion pipeline. Only hostname is normalized to
`hostsession-lab`. `before.conf` is generated without RawConfig, `after.conf`
with `raw.conf` appended. No generated positive neighbor commands are edited.
Generated fixtures retain the renderer's whitespace, including trailing spaces.
Explicit RT/RD are omitted in this minimal reproducer to isolate reload ordering.

The native input fragment is in `native-hostsession.yaml`; the corresponding
Kubernetes RawFRRConfig example is `rawfrrconfig-node11.yaml` (node-a/index 11).
It is an external owner's CR, not a claim of offline systemd bootstrap support.
This probe uses a dummy attachment in a disposable namespace solely for FRR
address/RIB controls.
It does **not** test native veth creation, Kube-OVN dataplane, or replace the
native attachment owner with an unmanaged veth.

## Reproduce

With Go meeting the pinned source's go.mod requirement, generate into a new
directory and compare to the checked-in fixtures:

```sh
bash hack/openperouter/hostsession-reload/generate.sh /path/to/openperouter /tmp/hostsession-generated
diff -u hack/openperouter/hostsession-reload/before.conf /tmp/hostsession-generated/before.conf
diff -u hack/openperouter/hostsession-reload/after.conf /tmp/hostsession-generated/after.conf
```

On an isolated Linux lab with Docker, Python 3 and the FRR image available:

```sh
bash hack/openperouter/hostsession-reload/run.sh /tmp/hostsession-new-results
```

Use a new results directory. Privilege is limited to fresh `--network none`
containers, with no host network namespace or daemon sockets mounted. The script
uses its own empty Docker client configuration. Supply Docker access through
the lab's normal sudo mechanism if needed. Never run probe.sh on the host.

Both modes boot via `vtysh -b`, then run three normal test/reload pairs and
restart zebra/bgpd with the same integrated configuration. `cold` boots the
candidate directly; `reload` first boots the generated positive controls.
Probe collection can complete successfully after failed reload commands;
`check.py` makes the overall run fail on any such status. It also checks
positive controls, sampled removal of local neighbors/attachment RIB entries,
and retention of external neighbor configuration and filtered redistribution.
It does not assert an external session is Established.

## Observed result: 2026-11-25

On isolated Linux `10.64.244.143`, both modes yielded:

| Attempt | --test | --reload |
| --- | --- | --- |
| 1 | 0 | 1 |
| 2 | 0 | 1 |
| 3 | 0 | 1 |

Reload logs contain `Specify remote-as or peer-group commands first` for
`neighbor 169.254.16.13 activate` and corresponding route-map/IPv6 commands.
The desired file retains positive generated commands followed by `no neighbor`;
the reloader attempts address-family commands after peer removal. Sampled
runtime after each reload and restart has no local neighbors or attachment
network statements. The external neighbor declaration and filtered kernel
redistribution remain. Partial application with a nonzero reload result does
not satisfy OpenPERouter's reconciliation contract.

A repeat with the checked-in fixtures again failed all six reloads. Its first
cold-start RIB snapshot still contained `169.254.16.13/32` although running-config
already omitted that network statement; later sampled RIBs did not contain it.
This observation does not establish a convergence bound or safe advertisement
behavior during startup. The checker rejects that sample as well.

External BGP establishment, actual kernel route redistribution/withdrawal,
native veth lifecycle, transient re-advertisement during failed reload,
Cisco EVPN and KubeVirt migration were not validated by this experiment.
Do not enable native hostSession acceptance based on these snapshots.
A new attachment-only API is not inherently required, but this tested
configuration-only alternative has failed its mandatory reload gate.

## Redistribution policy

`raw.conf` allowlists only routed subnet `10.101.3.0/24` and `fd00:3::/64`,
excluding L2VNI networks. These are separate from attachment addresses.
Protocol 203 is classified as kernel by this FRR; kernel priority's high byte
`0x4c` becomes distance 76, and only its low 24 bits are matched as metric.
The example metric 8399656 is a fixture value, not every VPC's OwnerID: production
policy must match the actual VPC UID metric and update on VPC recreation.
Do not use bare `redistribute kernel`. The external owner's import policy must
exclude fabric copies of local routed prefixes to preserve local OVN delivery
and avoid conflicting routes. These policies remain outside Kube-OVN ownership.
