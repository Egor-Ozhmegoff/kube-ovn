# Two-node scaling fixture

Run only on the specifically authorized test03/test04 cluster. Requires all
8 site VPCs, 8 Pod Subnets and 26 L2 Subnets, the fixed CNI image on both nodes,
root SSH-agent access and administrative kubectl on test03. It creates plain
Pod endpoints, not KubeVirt launchers or a migration.

1. Apply `pods.json` on test03 with
   `kubectl --kubeconfig=/etc/kubernetes/admin.conf apply -f pods.json`.
   The workload image is the unchanged preloaded tools/base image. The tested
   dataplane is provided by the host CNI/OVN image. Readiness intentionally
   fails: Running/allocated endpoints must still have connectivity and routes.
2. Copy `packet_probe.py` and `cluster_state.py` to
   `/var/tmp/kubeovn-scaling-20261123/` on the appropriate nodes.
   `packet_probe.py` is needed on both nodes; `cluster_state.py` runs on test03.
3. Run `python3 cluster_state.py` on test03. It requires exactly 26 ready L2
   Subnets, 26 barriers, 52 current Active ACKs and all system Pods ready.
4. From the management machine, run
   `python3 two_node_reflection.py --known-hosts /path/to/verified-test-known-hosts`.
   SSH uses the agent and disables SSH config and identity-file loading.
   It injects four reflected frame types through the native PE veth, checks
   nft drop counters, verifies permitted external Ethernet/NA/RARP delivery to
   a local Pod, and verifies outbound RARP in each node's PE namespace.
5. For an external ARP positive control, send `--types 0806 --arp-target POD_IP`
   using the probe's `send` mode on `pe-e-504241` in `perouter`. Capture on the
   same interface with `arp and ether dst 02:aa:ab:ac:ad:ae and arp[6:2] == 2`.
   OVN's ARP responder may answer the query without delivering it to the Pod;
   require the reply for the actual allocated Pod IP/MAC. Do not count a missing
   ARP request in the Pod as evidence of filtering.
6. Run the read-only state snapshot again, then remove only these test resources:
   `kubectl --kubeconfig=/etc/kubernetes/admin.conf delete namespace kubeovn-scale-check`.
   Leave the site's 26 L2 Subnets active. Check withdrawal of the deleted /32s
   from the PE unicast RIB and EVPN Type-5 routes.

Use `nft` from the running CNI container (`crictl exec ... nft`) for operator
checks. On these hosts, nft 1.0.9 crashed while dumping the netdev JSON; the
CNI runtime uses nft 1.1.6. No host packages need to be changed.

These are deployed attachment/OVN component checks with synthetic PE ingress.
They do not exercise a packet arriving from a physical Cisco VTEP, native
Type-2 lifecycle/EVPN MAC Mobility, Multus or KubeVirt live migration.
The disposable kernel regressions must remain in privileged `--network none`
containers; do not run them in a node's host network namespace.
