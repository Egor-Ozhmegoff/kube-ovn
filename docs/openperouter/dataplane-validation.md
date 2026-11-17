# Изолированный план приёмки

Полная E2E приёмка **не завершена**. На Linux исполнены компонентные
испытания: OVN 16/17 (ND reflection FAIL), native OpenPERouter 70/70,
реальная GoBGP↔FRR session. Границы и оставшиеся пункты — в
[отчёте](validation-results.md). Ниже полная последовательность для
экспериментального runtime. Read-only probe runner
`hack/test-openperouter-lab.sh` проверяет часть пунктов 1/5/6/13 после подготовки
отдельного стенда; остальные пункты требуют указанных ниже captures/операций.
В выбранной ветке `Mac_setings_for_l2vni_and_l3vni` API-блокеры MAC сняты;
L3 handoff проверен в OVN component lab; L2 имеет подтверждённый ND blocker.

## Фиксация стенда

Использовать две отдельные disposable Linux VM `node-a`/`node-b`, два FRR
fabric leaf и внешний endpoint; Cisco virtual switch добавить при наличии
лицензированного образа. FRR leaf проверяет протокол, но не Cisco interoperability.
Закрепить Kube-OVN source/image digest, OVN/OVS version, OpenPERouter commit
и digest, FRR 10.6.0 выбранной ветки, kernel/Podman/systemd и Cisco image/version в отчёте.
Сохранить конфигурацию и вывод `uname -r`, `podman version`,
`ovs-vswitchd --version`, `ovn-controller --version`, `show version` FRR/Cisco.

Management сеть изолирована от fabric; `fabric0` — выделенный NIC каждой VM,
две независимые leaf connectivity обеспечивают внешний ECMP. Для peering
без Kubernetes API нужны локальные images, configuration и working IPv6 LL.
Не брать адреса/учётные данные из действующего стенда.

Использовать сети примера `examples/README.md`. Разместить Pod A
`10.101.3.11` на node-a и Pod B `10.101.3.12` на node-b; VM A
`10.101.1.11`, внешний L2 endpoint `10.101.1.200` и внешний routed endpoint
`203.0.113.10`. Второй VRF tenant-b: L3VNI 20001, RT 64512:20001,
Pod C `10.102.3.11`; без общего import RT с tenant-a. Поднять один Service
с backend на обеих нодах и отдельно Service с `externalTrafficPolicy: Local`.

## Наблюдения

На **каждой** VM собрать синхронные capture на workload interface, OVN tunnel
NIC, host management endpoint, `host-e-10001`, router-side leg, Linux VXLAN
и `fabric0`; записывать ingress/egress ifindex, inner source/destination,
TTL/hop-limit, VXLAN VNI и outer src/dst. Одного traceroute недостаточно:
overlay скрывает промежуточный Kubernetes gateway.

Для snapshot использовать только namespace/контекст изолированного стенда:
`ip -j -d link`, `ip -j rule`, `ip -j route show table all`, IPv6 equivalents,
`bridge -j fdb`, `ip -j neigh`, `ovs-vsctl show`, OVN NB/SB bindings,
`ovn-trace` и FRR `show bgp l2vpn evpn ... json`, `show bgp vrf ... json`,
`show ip route vrf ... json`. Зафиксировать соответствие VNI/RMAC/RT/RD.
Снимать snapshots до события, сразу после и после convergence.

## Последовательность и критерии

| № | Действие в disposable lab | Ожидаемый результат и доказательство |
| --- | --- | --- |
| 1 | С Pod A и Pod B запустить потоки к routed endpoint | На каждой ноде поток проходит только её host-e-10001 и её fabric0. Inner source сохраняется. |
| 2 | Для этих потоков наблюдать node-b при выходе A и наоборот | Нет исходящего external flow через соседний host attachment. На internal overlay нет туннельного переноса этого egress. Повторить с недоступным локальным PE. |
| 3 | С внешнего endpoint обратиться к обоим Pod/VM | У fabric есть корректный Type-5/Type-2 и return destination. Capture подтверждает доставку из local management endpoint к OVN LSP; нет повторного внешнего egress и TTL loop. |
| 4 | Анонсировать одинаковый внешний prefix с двух leaf, создать много 5-tuples | Проверить реальные ECMP FIB nexthops в OpenPERouter и использование обоих путей; выключить один. Если выбранная версия не устанавливает ECMP, явно зафиксировать отсутствие поддержки, а не выводить её из BGP multipath RIB. |
| 5 | Withdraw все external routes, включая default, затем отдельно оборвать BGP/BFD | После convergence поток прекращается локально. Нет выхода через другой Kubernetes node, management NIC и main VRF. Internal Pod A↔B сохраняется. Измерить detection, withdraw и FIB timing; повторить IPv6, с default в main table. |
| 6 | Отключить внешнюю fabric и обмениваться A↔B внутри VRF | Прямой внутренний OVN VXLAN, без host-e/fabric hop. Проверить Pods в разных subnets одного VRF. |
| 7 | Обмен tenant-a↔tenant-b при запрещённом и затем разрешённом Cisco policy | Сначала drop; затем путь виден на fabric в обоих направлениях. В host и perouter нет локального route leaking. Внутренняя OVN доставка между VPC отсутствует. |
| 8 | Внешний L2 endpoint: ARP, IPv6 ND, unknown unicast, broadcast/multicast | Ровно требуемая доставка без повторного BUM и без дублирующих внутренних/внешних путей. Только local MAC/IP экспортируется локальным PE; удалить VM — отозвать Type-2 и очистить FDB/neighbors. Проверить заданный anycast MAC на обоих L2VNI. |
| 9 | Мигрировать VM A на node-b, затем migration rollback и повторную миграцию | IP/MAC сохраняются, анонс переходит только после active binding; MAC mobility sequence корректна. Source PE перестаёт объявлять endpoint, stale FDB/neighbors удалены, policy/LB сохраняются. |
| 10 | По очереди restart agent, OVS, ovn-controller, FRR, OpenPERouter controller; удалить veth/router namespace; reboot VM | Нет анонсов из stale cache, окно отказа измерено. Endpoint переобнаруживается по новому ifindex, без второго владельца. Gateway MAC и общий RMAC восстановлены; нет leaked links/VRF. |
| 11 | С нуля reboot обе VM с недоступным API и очищенным runtime workload состоянием | Bootstrap поднимает underlay и EVPN сессии из локальных файлов. Workload routes отсутствуют до свежей binding/API sync. Для уже работающих workload отдельно проверить истечение freshness lease при API outage. |
| 12 | DF/PMTUD IPv4 и IPv6, размер на границе допустимой MTU и выше | Сохранена доставка допустимого размера, ICMP PTB/fragmentation-needed возвращается корректно. Нет silent blackhole на локальной L3/L2 границе. |
| 13 | ClusterIP, NodePort, LB VIP, session affinity, backend change; NetworkPolicy deny/allow | OVN LB/NAT и ACL действуют в обоих направлениях, включая внешнее L2 ingress и миграцию. Проверить local/remote backend и trafficPolicy. Service NAT не нарушает обещанное сохранение source workload prefix для обычного routed трафика. |

Тест №5 повторить с GR выключенным на обоих peers, затем отдельно с GR/stale
retention для измерения его последствий. В контрольной точке после withdrawal
исключить оставшийся маршрут от второго peer и stale route в FIB; без этого
тест не проверяет отсутствие fallback.

Для №8 считать кадры с уникальным payload/sequence, а не только ping loss.
Принудительно очистить ARP/ND, выключить known-unicast learning и повторить
для двух одновременных отправителей. Наличие двух путей должно обнаруживаться
по duplicate frames и MAC move churn, даже если ping проходит.

## MTU

IPv4 VXLAN добавляет 50 байт относительно inner IP MTU, IPv6 VXLAN — 70.
Если internal OVN VXLAN и external VXLAN используются на разных участках,
workload MTU ограничена минимумом `internal_underlay_MTU - overhead` и
`external_underlay_MTU - overhead`, а не автоматически суммой двух overhead.
Если underlay одного туннеля фактически проходит внутри другого, нужны оба
overhead и соответствующий packet capture. Учесть VLAN tags, IPv6 и иные
инкапсуляции; не считать jumbo MTU стенда универсальным решением.

## Артефакты и граница результата

Для каждого теста сохранить inputs, pinned versions, captures с timestamps,
до/после RIB/FIB/NB/SB snapshots, exit codes и измеренный convergence.
Нельзя помечать «pass» тесты с пропущенными prerequisites. После готовой
реализации перенести последовательность в repository e2e с версионным
`f.SkipVersionPriorTo` и отдельным opt-in для privileged disposable lab.

В текущей поставке нет результатов этих 13 тестов, доказательства ECMP,
Cisco interoperability, L2 split-horizon, live migration или масштабирования.
Offline upstream audit проверяет только закреплённые исходники и блокеры API.

## Команды воспроизведения частичных smoke probes

На **отдельном disposable кластере**, явно передавая его kubeconfig/context,
после установки native bootstrap и experimental Kube-OVN image:

```sh
kubectl --kubeconfig /tmp/lab-kubeconfig --context lab annotate node node-a openperouter.kubeovn.io/node-index=11 openperouter.kubeovn.io/router-mac=02:00:00:01:00:0b
kubectl --kubeconfig /tmp/lab-kubeconfig --context lab annotate node node-b openperouter.kubeovn.io/node-index=12 openperouter.kubeovn.io/router-mac=02:00:00:01:00:0c
kubectl --kubeconfig /tmp/lab-kubeconfig --context lab apply -f docs/openperouter/examples/kube-ovn.yaml
kubectl --kubeconfig /tmp/lab-kubeconfig --context lab label namespace tenant-a-pods openperouter.kubeovn.io/disposable-lab=true
kubectl --kubeconfig /tmp/lab-kubeconfig --context lab apply -f docs/openperouter/examples/probes.yaml
hack/test-openperouter-lab.sh /tmp/lab-kubeconfig lab base 203.0.113.10
```

Затем отозвать external prefix/default на **lab fabric** согласно пункту 5,
дождаться RIB/FIB convergence и выполнить:

```sh
hack/test-openperouter-lab.sh /tmp/lab-kubeconfig lab withdrawn 203.0.113.10
```

Runner ничего не развёртывает и не меняет маршруты. Он требует lab-label,
фиксированное размещение probe-a/node-a и probe-b/node-b, проверяет direct
Pod connectivity, ClusterIP HTTP и успех/неуспех external ICMP по фазе.
Успешный ping сам по себе не доказывает node-local egress: captures пунктов
1/2 обязательны. Withdrawal phase не различает изоляцию и любой другой
отказ — снимки FIB/пакетов и положительный контроль до/после обязательны.
Требование внешнего ICMP endpoint является частью fixture, не предположением
об arbitrary external host. Для MTU добавить IPv4 DF probe с `iputils ping -M do`
и IPv6 PTB test; busybox fixture не заменяет эти проверки.
