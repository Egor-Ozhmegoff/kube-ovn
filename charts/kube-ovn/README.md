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

## Direct Kubernetes API endpoint

By default, Kubernetes supplies the API Service ClusterIP through the in-cluster
configuration. To reach the API directly (for example, while bootstrapping the
CNI), configure a stable control-plane load balancer or VIP:

```yaml
networking:
  API_SERVER_HOST: "api.example.com"
  API_SERVER_PORT: "6443"
```

Use a bare IP/DNS name without a scheme or port. The endpoint must be reachable
from the nodes and Pods running API clients, and present in the API server
certificate SANs. ServiceAccount token and CA authentication remain unchanged.
With an empty host the chart emits neither override variable; with a host and
an empty port it uses 6443.

The override applies to controller, CNI, IPsec agent/cleanup, OVS startup,
OVN Central leader checker, IC controller, pinger and upgrade/delete hooks.
It changes neither Service CIDRs nor OVN database addresses (`MASTER_NODES`).
Explicit kubeconfigs, when used by a client, retain their own server endpoint.
This setting belongs to `charts/kube-ovn`; it does not configure chart v2.
