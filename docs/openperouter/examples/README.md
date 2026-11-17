# Исходные данные двухнодового примера

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
| Host speaker ASN | 64515 | 64515 |
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
через localport, reflection guard описан в implementation.md. Host L3 BGP
peer запускает daemon после fresh API/binding проверки.
Подсети из таблицы не анонсируются из static bootstrap: availability workload
должен сообщать отдельный agent по свежим bindings.

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
образом/версией. Общий host ASN не разрешает без проверки реэкспортировать
удалённые workload routes обратно host speaker; внутренняя доставка должна
оставаться в OVN.

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
