# Kube-OVN-helm

> [!CAUTION]
> This chart is maintained as best effort, the v2 chart is the recommended way of deploying Kube-OVN.  
> Please head [to this Helm Chart](../kube-ovn-v2) instead.
> There is not yet a guide to upgrade from one chart to another, be careful if converting from v1 to v2.

## Installing the Chart

### From OCI Registry

The Helm chart is available from GitHub Container Registry:

```bash
helm install kube-ovn oci://ghcr.io/kubeovn/charts/kube-ovn --version v1.15.0
```

### From Source

Installation :

```bash
$ kubectl label node -lbeta.kubernetes.io/os=linux kubernetes.io/os=linux --overwrite
$ kubectl label node -lnode-role.kubernetes.io/control-plane  kube-ovn/role=master --overwrite
$ kubectl label node -lovn.kubernetes.io/ovs_dp_type!=userspace ovn.kubernetes.io/ovs_dp_type=kernel  --overwrite

# standard install 
$ helm install --debug kubeovn ./charts/kube-ovn --set MASTER_NODES=${Node0}

# high availability install
$ helm install --debug kubeovn ./charts/kube-ovn --set MASTER_NODES=${Node0},${Node1},${Node2}

# upgrade to this version
$ helm upgrade --debug kubeovn ./charts/kube-ovn --set MASTER_NODES=${Node0},${Node1},${Node2}
```

If `MASTER_NODES` unspecified Helm will take internal IPs of nodes with `kube-ovn/role=master` label

### Talos Linux

To install Kube-OVN on Talos Linux, declare openvswitch module in machine config:

```
machine:
  kernel:
    modules:
    - name: openvswitch
```

and use the following options to install this Helm-chart:

```
--set cni_conf.MOUNT_LOCAL_BIN_DIR=false
--set OPENVSWITCH_DIR=/var/lib/openvswitch
--set OVN_DIR=/var/lib/ovn
--set OVN_IPSEC_KEY_DIR=/var/lib/ovs_ipsec_keys
--set DISABLE_MODULES_MANAGEMENT=true
```

`OVN_IPSEC_KEY_DIR` only becomes a host mount when `func.ENABLE_OVN_IPSEC` is enabled, but setting it up
front keeps IPSEC from failing later with `mkdir /etc/origin: read-only file system`.


## Experimental OpenPERouter integration

`openperouter.enabled` defaults to `false`. Enabling it requires the internal
OVN network type to be `vxlan`, a systemd-managed OpenPERouter bootstrap at
`openperouter.bootstrapDir` (default `/var/lib/openperouter`), and node index
and router MAC annotations. The bootstrap directory is mounted read-only;
this chart does not install OpenPERouter or manage its FRR/VXLAN namespace.
See [the integration design and release blockers](../../docs/openperouter/implementation.md)
and [disposable lab examples](../../docs/openperouter/examples/README.md).
The selected OpenPERouter commit and remaining dataplane limitations must be
reviewed before enabling this experimental mode.
