# Исходные данные двухнодового примера

**Переход на netlink:** примеры теперь требуют native L3 attachment без BGP.
В закреплённой MAC-ветке такого API нет; удаление `hostSession` само по себе
не создаёт veth. Это целевая конфигурация после внешней доработки, а не готовый
bootstrap. `kube-ovn-attachments.yaml` — только метаданные Kube-OVN.
Читайте [порядок перехода и ограничения](../netlink-routes.md).


Это **экспериментальный lab fixture**. Компонентные проверки и оставшиеся
блокеры описаны в [отчёте](../validation-results.md). Используется
ветка `Mac_setings_for_l2vni_and_l3vni`, commit
`60a1285935b10eb0b1cf07e4533faefedde3d4d3`: она поддерживает gatewayMAC
и node routerMAC. Не применять эти файлы к действующим нодам.
YAML содержит native static OpenPERouter configuration, а не Kubernetes
манифесты. На каждой изолированной Linux VM Ansible устанавливает
свой `node-config.yaml` и общий `openpe_fabric.yaml` в
`/var/lib/openperouter/`. Отдельный `fabric0` передаётся во владение
OpenPERouter; management NIC остаётся в host namespace.

| Объект | Node A | Node B |
| --- | --- | --- |
| node index | 11 | 12 |
| Underlay ASN / TOR ASN | 64514 / 64512 | 64514 / 64512 |
| L3VNI / VRF | 10001 / tenant-a | 10001 / tenant-a |
| L2VNI | 10101, 10102 | 10101, 10102 |
| Host L3 attachment | host-e-10001 | host-e-10001 |
| Host L2 attachments | host-e-10101, host-e-10102 | host-e-10101, host-e-10102 |
| routerMAC из node-config.yaml | 02:00:00:01:00:0b | 02:00:00:01:00:0c |

Workload сети примера:

| Сеть | CIDR | Gateway | Внешний L2VNI |
| --- | --- | --- | --- |
| blue, tenant-a | 10.101.1.0/24 | 10.101.1.1 | 10101 |
| green, tenant-a | 10.101.2.0/24 | 10.101.2.1 | 10102 |
| pods, tenant-a | 10.101.3.0/24 | 10.101.3.1 (OVN) | отсутствует |

MAC IRB gateway обоих L2 сегментов задан как `02:00:05:00:03:00` через
`gatewayMAC`, вместе с соответствующими `gatewayIPs`. Это immutable поля:
в существующий L2VNI gatewayMAC нельзя добавить или изменить обновлением.
`hostMaster` и bridge mappings не заполнены: host leg подключает Kube-OVN
через localport, reflection guard описан в implementation.md. PE получает только CIDR Pod-only сетей через локальный L3 attachment.
L2VNI исключены; endpoint IP и их размещение не являются входами routes.
Подсети из таблицы не анонсируются из static bootstrap: внешний export routed subnet prefixes выполняет FRR согласно policy.

L2 IRB connected prefixes могут быть экспортированы самим OpenPERouter в
зависимости от FRR connected/EVPN policy. Это не анонс наличия конкретного
workload; измерить фактические Type-2/Type-5 и фильтрацию в прототипе.
`routerMAC` действует для всех L3VNI ноды только в kernel datapath, при
старте читается из node-config. Разные значения между нодами обеспечивает
конфигурационное управление, а не глобальная проверка OpenPERouter.

`routerIDCIDR` в этом небольшом примере `/24`. При 1000 нодах нужен больший
уникальный pool (например, согласованный `/20`), как и VTEP pool. IPAM по
node index проверяется по фактическому pinned OpenPERouter; не предполагать,
что host-side transit IP одинаков на всех нодах.

Systemd bootstrap должен устанавливать upstream файлы из
`systemdmode/quadlets/` целиком, включая `.pod` и `.volume`, FRR initial
configuration из `systemdmode/frrconfig/`, а также предварительно загруженный
образ из закреплённого commit с digest вместо `:main`. Процедура upstream
описана в `systemdmode/deploy.sh`; её нужно адаптировать Ansible к выделенной
VM, не запускать на текущем хосте. Здесь deployment tooling не исполнялось.

Внешний peering задаётся интерфейсом `fabric0` и использует IPv6 link-local.
Remote Cisco VTEP addresses в этом примере отсутствуют: next-hop обнаруживает
FRR EVPN. Проверить RFC 5549/extended next-hop и AS_PATH с конкретным Cisco
образом/версией. Локальной BGP-сессии Kube-OVN↔PE нет. FRR redistribution требует явного
фильтра protocol-класса/metric и routed subnet prefixes; внутренняя доставка остаётся в OVN.

## Экспериментальные workload attachments

Добавлены
`kube-ovn.yaml` (VPC, namespaces, три Subnet), `helm-v1.yaml`, `helm-v2.yaml`
и `probes.yaml`. Attachment реализован через localport с MAC reflection ACL,
а не hostMaster/bridge mappings. Текущее состояние и release blockers:
[implementation.md](../implementation.md). Это не готовый production deployment.

Не применять `node-config.yaml` как Kubernetes CR. На каждой lab VM установить
соответствующий node file и общий openpe_fabric.yaml локально через Ansible.
Защитные defaults не задаются через FRR. Terminal IPv4/IPv6 routes должны
принадлежать kernel reconciler OpenPERouter и устанавливаться до открытия
ingress, включая VRF recreate. В выбранной версии эта гарантия отсутствует;
соответствующий release blocker сохраняется.

Helm overrides задают `openperouter.enabled: true` и внутренний VXLAN.
Bootstrap смонтирован read-only. Использовать image, собранный из текущих
изменений Kube-OVN, и PE image закреплённого SHA/digest. Простое включение
flag в старом image не устанавливает новую реализацию.

Порядок команд для disposable lab и probe runner приведены в
[dataplane-validation.md](../dataplane-validation.md). Пример Service содержит
`ovn.kubernetes.io/vpc: tenant-a`. External Service VIP анонсы не реализованы.

## Bootstrap directory и transport

После установки native Quadlets закреплённого SHA установить
[10-kubeovn.conf](controller.container.d/10-kubeovn.conf) в
`/etc/containers/systemd/controller.container.d/10-kubeovn.conf`. Drop-in
согласует `--host-configuration-dir=/etc/openperouter` с native mount
`/var/lib/openperouter:/etc/openperouter`; без override default controller
ищет static specs в подкаталоге `configs`, а daemon читает root bootstrapDir.
Проверен dry-run настоящего Podman Quadlet generator, не фактический boot.

В Helm v1 `NETWORK_TYPE=geneve` означает общий overlay mode, а реальный
transport задаётся `TUNNEL_TYPE=vxlan`; в v2 соответствуют `networkType`
и `tunnelType`. Проверяется реальный аргумент daemon. VLAN mode и Geneve
transport для OpenPERouter integration отклоняются.

## Два инфраструктурных адреса и независимый bootstrap

| Назначение | node-a | node-b | Владелец |
| --- | --- | --- | --- |
| Node IP, host0, host namespace | 192.0.2.11/24 | 192.0.2.12/24 | ОС / Ansible / systemd-networkd |
| Внешний VTEP, PE namespace | 198.18.0.11/32 | 198.18.0.12/32 | OpenPERouter |
| OVN VXLAN source | Node IP | Node IP | Kube-OVN / OVS |
| PE fabric0, IPv6 link-local | выделенный NIC | выделенный NIC | OpenPERouter |

`routerIDCIDR` и `tunnelEndpoint.cidrs` используют одинаковый IPv4 pool:
в выбранном API оба адреса вычисляются как pool + nodeIndex. Router ID
совпадает с внешним VTEP; отдельный третий инфраструктурный IP не нужен.
Host/PE transit link-local адреса — локальные служебные подключения, не VTEP.

Пример внешней настройки systemd-networkd для node-a (`host0.network`):

```ini
[Match]
Name=host0
[Network]
Address=192.0.2.11/24
Gateway=192.0.2.1
```

Для node-b заменить Address на 192.0.2.12/24. Подсеть Node IP должна быть
доступна через underlay с остальных нод и API endpoint. Перед запуском PE
Ansible обязан проверить, что `fabric0` и `host0` — разные устройства, и
host0 не входит в `underlays.interfaces` PE. NetworkDevice в native API
перемещает устройство в PE namespace: host0 передавать ему нельзя.
Если нужны VLAN subinterfaces, их создаёт ОС заранее и разводит владельцев;
Kube-OVN не создаёт bootstrap ОС.

Порядок: host-underlay/Node IP/routes → network-online → native Podman/systemd
FRR/PE из локальных образов и файлов → kubelet/control plane/OVN Central →
OVS/OVN daemon → tenant attachments и workload announcements. Kubelet node-ip
и OVN tunnel source выбирают Node IP; Helm v1 `networking.IFACE=host0`, v2
`agent.interface=host0`. Адреса API задаются явно в обоих charts:

```yaml
networking:
  API_SERVER_HOST: '192.0.2.10'
  API_SERVER_PORT: '6443'
```

Это прямой underlay API endpoint, не MASTER_NODES (OVN Central).
Без overrides сохраняются стандартные Kubernetes Service env.

MAC-ветка `60a1285` запускает static reconciler до API, а после подключения
API передаёт тот же `StaticConfigDir` Kubernetes reconciler и вызывает
`mergeStaticConfig`. Bootstrap-underlay остаётся в локальном файле; его не
следует удалять после join или дублировать конфликтующим dynamic Underlay.
Разные peers для нод/групп задаются их локальными underlay specs либо native
nodeSelector; это не конфигурация Kube-OVN. Static mirrored CR не становятся
вторым источником конфигурации. Реальный cold boot/handover требует отдельной
E2E проверки, source review его не заменяет.

Новая нода получает index/router-mac annotations после установки локальных
файлов и проверки PE. До наличия обеих annotations transit reconciliation
не включает её return VIF и продолжает обслуживать существующие ноды.
Daemon дополнительно сверяет annotations с bootstrap, native veth, OVN
connection и installed bindings; annotations сами не разрешают анонсы.

Для другой группы leaf peers изменяется native underlay в локальном файле
соответствующей ноды, например в node-b вместо соседнего ASN 64512:

```yaml
# Fragment of this node's Underlay, not Kube-OVN Helm values.
neighbors:
  - asn: 64513
    interface: fabric0
    holdTimeSeconds: 9
    keepaliveTimeSeconds: 3
```

Node index, local attachment pools, VNI и RT/RD должны оставаться согласованными с VPC
и политикой fabric. Remote VTEP IP в OVN routes/policies не добавляются.
