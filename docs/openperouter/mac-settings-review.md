# Проверка Mac_setings_for_l2vni_and_l3vni

Repository: `https://github.com/Egor-Ozhmegoff/openperouter`.
Branch: `Mac_setings_for_l2vni_and_l3vni`.
Commit: [`60a1285935b10eb0b1cf07e4533faefedde3d4d3`](https://github.com/Egor-Ozhmegoff/openperouter/tree/60a1285935b10eb0b1cf07e4533faefedde3d4d3).
Commit получен через GitHub commits API, архив загружен по полному SHA.
Это выбранная версия вместо ранее исследованных upstream и main форка.

## L2 gateway MAC

В `api/v1alpha1/l2vni_types.go` добавлено optional `GatewayMAC *string`.
Schema ограничивает длину и colon-формат 48-bit MAC, CEL проверяет unicast,
nonzero, зависимость от gatewayIPs и неизменность поля. Webhook повторяет
проверки значения и неизменности. `zz_generated.deepcopy.go` копирует pointer;
поле присутствует в generated CRD/chart manifests этой ветки.

`internal/conversion/validate_vni.go:ParseGatewayMAC` проверяет colon format,
nonzero и unicast; `host_conversion.go:l2vniToHost` передаёт значение в
`L2VNIParams.GatewayMAC`. `hostnetwork/bridge.go:GatewayMAC` выбирает явный
MAC либо прежнее вычисление по VNI, если поле отсутствует.

`hostnetwork/vni.go:SetupL2VNI` устанавливает MAC через bridge options до
присоединения VXLAN/port. Комментарий в коде объясняет порядок: поздняя смена
MAC bridge может заменить remote FDB entry локальной за спиной zebra.
`ensureBridgeMACAddress` идемпотентен, не создаёт netlink update при равном MAC.

Значение `02:00:05:00:03:00` соответствует валидатору. Теперь OpenPERouter
может быть единственным владельцем IRB gateway MAC. Это не означает, что
разрешено включить в OVN второй gateway с тем же IP без проверки ARP/ND/IRB.

## Node-wide L3 router MAC

`api/static/config.go:NodeConfig.RouterMAC` задаёт локальное значение.
`cmd/hostcontroller/main.go:overrideHostMode` переносит его в параметры,
после чего `validateParameters` вызывает `hostnetwork.ParseRouterMAC`.
Node-config загружается до validation. Есть CLI `--router-mac` и явный
отказ для grout datapath. Значение из файла при наличии имеет приоритет над CLI.

`KernelDatapathConfigurator.Configure` присваивает `k.RouterMAC` каждой
L3VNI перед вызовом `SetupL3VNI`. `l3VNIBridgeOptions` валидирует значение
и добавляет `ensureBridgeMACAddress`; `setupBridge` применяет options до
поднятия bridge. Так один параметр распространяется на все L3VNI ноды,
независимо от количества VRF.

В `internal/hostnetwork/vni_test.go` есть тест двух VNI, drift и bridge
recreation; также проверяется применение до link-up. Эти kernel/netlink тесты исполнены на Linux в составе native suite:
70/70 specs прошли, включая MAC drift/recreation. Проверка RMAC в EVPN
advertisement до Cisco остаётся частью полной E2E приёмки.

Router MAC не находится в L3VNISpec и не обязан там находиться: целевой
systemd bootstrap читает node-local configuration до Kubernetes API.
Глобальную уникальность между нодами обеспечивает Ansible/config validation.
Чтение node-config происходит при startup; hot reload routerMAC не подтверждён.

## Что не изменилось автоматически

* Наличие MAC настроек не создаёт management LSP/host VRF и локальный egress
  для Kube-OVN shared distributed router.
* OpenPERouter сам не получает OVN workload prefixes. Kube-OVN пишет routed subnet routes (без L2VNI и per-endpoint routes),
  но совместимый L3 attachment без локального BGP пока не подтверждён:
  hostSession + RawFRRConfig провалил штатный reload
  (см. [netlink-routes.md](netlink-routes.md)).
* Не найден готовый контракт local MAC/IP ownership и split-horizon между
  внутренним OVN VXLAN и внешним EVPN. Наивное подключение localnet на всех
  нодах остаётся непроверенным и потенциально создаёт двойной BUM путь.
* Требуются тесты unreachable default/изоляции VRF, route withdrawal, MTU,
  KubeVirt mobility и Services/NetworkPolicy.
* FRR этой ветки — 10.6.0. Нельзя переносить результаты от 10.7.1 без проверки.

В `examples/` используются новые поля и workload attachments через
экспериментальную интеграцию Kube-OVN (localport, без hostMaster).
IRB connected prefix может присутствовать в RIB/export независимо от наличия
workload; его нельзя считать свидетельством доступности Pod/VM. Нужно
согласовать отдельно policy Type-5 export и per-workload announcements.

## Выполненные проверки

Source/API audit с точными SHA-256 проходит, прежние MAC-блокеры отсутствуют.
Четыре native specs примера проходят structural schema validation по CRD этой
ветки; отдельно проверены разные node routerMAC. CEL/webhook runtime не запускались.
Offline gate tests проверяют, что статус `api-reviewed` не превращается в
заявление `dataplaneTested: true`.

Native `internal/hostnetwork` выбранного SHA исполнен на Linux с
`-tags runasroot`: **70/70 specs, 0 failures, 0 skips**. Проверены применение
MAC в kernel, общий RMAC для нескольких L3VNI, drift/recreation, veth и MTU.
Журнал `native-hostnetwork.txt` хранится в каталоге результатов,
описанном в [отчёте](validation-results.md).

Kube-OVN API/CRD, generated configurations, Helm и runtime реализованы.
Предыдущий OVN component lab 2026-11-16 выявил ND reflection через reserved
allow. Текущая доработка ставит nft netdev ingress filter до OVN ACL;
новые packet/kernel результаты — в validation-results.md.
Реальная host-VRF GoBGP↔FRR 10.6.0 session проходит анонс, отзыв и restart.
MAC-настройки не закрывают ND split-horizon, native fail-closed lifecycle и
native non-BGP L3 attachment; Type-2 aging/mobility остаются предметом E2E проверки. Актуальные результаты и границы проверок — в
[отчёте](validation-results.md), минимальные изменения — в
[implementation.md](implementation.md#блокеры-и-неподтверждённые-свойства).
