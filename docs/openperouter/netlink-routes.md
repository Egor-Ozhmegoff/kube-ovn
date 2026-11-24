# Маршруты OVN-подсетей и native EVPN L2

Текущая доработка Kube-OVN: `openperouter@bc3682e5996f` плюс изменения рабочего
дерева. Она заменяет предыдущий незакоммиченный per-endpoint netlink runtime.
Kube-OVN не определяет положение Pod/VM для IP-анонсов и не создаёт по Pod IP
дополнительных /32 или /128. Локальный BGP speaker интеграции не возвращается;
независимый `pkg/speaker` сохранён.

## Разделение L2 и L3

| Сеть | Возвратный путь из PE | Внешний анонс |
| --- | --- | --- |
| Subnet с `openPERouterL2` | Native L2VNI bridge/IRB и host L2 path | Native OpenPERouter/FRR: Type-2, FDB/ARP/ND, MAC Mobility |
| Готовый OVN Subnet без `openPERouterL2` в интегрированном VPC | Маршрут CIDR через свой L3 veth → host VRF → OVN return VIF | FRR redistribution/export по внешней policy |

Routed subnet routes определяются конфигурацией/сетевой готовностью Subnet,
а не PodReady, allocated IP, местом Pod или владельцем VM. Исключаются
удаляемые/неготовые сети и не-OVN providers; несовместимые gateway/NAT настройки
отклоняются существующей валидацией. Смена/удаление launcher не меняет CIDR routes.
Все ноды с готовым локальным attachment могут иметь маршрут одной подсети:
входящий пакет может попасть в OVN на этой ноде и далее к удалённому workload
через внутренний OVN VXLAN. Исходящий пакет использует только локальный PE.
Remote Kubernetes gateway fallback не добавляется.

Host-side `host.Attachment.routes` уже устанавливал protocol 202: default к
локальному PE и subnet CIDR через обычный OVN return VIF. Это **host** VRF,
не таблица PE. Этот механизм сохранён. Native OpenPERouter создаёт connected
routes L2VNI в PE, но не знает CIDR Pod-only сетей за OVN router. Поэтому PE
получает только недостающие routed subnet routes, без дублирования L2 connected
prefixes. Нативная policy экспорта IRB connected prefixes остаётся внешней
ответственностью и не должна ошибочно считаться анонсом доступности endpoint.

## PE route ownership

Daemon открывает netlink socket в namespace из `--openperouter-namespace`
(оба charts: `openperouter.namespacePath`, default `/var/run/netns/perouter`).
Он не перемещает interfaces и не меняет FRR/FDB/neighbors/EVPN state.
Проверяются реальный UP VRF и его table ID, уникальность таблицы среди PE VRF,
UP native veth/master/peer ifindex, ожидаемые local IP/prefixes. Table ID не VNI.
Next-hop — host-side адрес этого локального L3 attachment.

| Поле | Контракт |
| --- | --- |
| Table | фактический PE tenant VRF, не 0/253/254/255 |
| Prefix | нормализованный CIDR выбранного OVN Subnet, не default |
| Protocol | 203, отдельно от host 202 и retired endpoint 201 |
| Metric / OwnerID | `0x4c000000 OR (CRC32(VPC UID) AND 0xffffff)` |
| IPv4 realm | равен OwnerID |
| IPv6 realm | 0: Linux IPv6 FIB не сохраняет RTA_FLOW |
| Type/scope | unicast/universe, не multipath |

Protocol/metric/realm signature зарезервирована во всём PE namespace только
за этой интеграцией. Она даёт operational ownership, не криптографическую
аутентификацию. Коллизии OwnerID текущих VPC проверяются перед reconciliation;
при коллизии общий snapshot не используется и выполняется fencing. Kernel
metric сохраняет владельца и для IPv6, позволяя withdraw VPC без наличия
старого VRF/veth или памяти прошлого процесса. Старую reservation нельзя
переиспользовать сторонним владельцем при сохранённых старых routes.

Каждый VPC reconcile читает actual state и применяет diff только своего OwnerID.
Другой OwnerID, connected/BGP/service/host routes и terminal defaults сохраняются.
Любой чужой exact-prefix conflict вызывает отказ этого VPC. Добавления используют
EXCL, не Replace. Удалённая/переименованная сеть удаляет старый собственный CIDR;
повторное применение — no-op. После partial error повтор читает kernel заново.
После recreate VRF/table/ifindex старые own routes удаляются, новые проверяются
readback перед Open. Namespace generation проверяется по inode. Нет cache lease.

## Изоляция ошибок VPC

Общий этап читает свежие VPC/Subnet snapshots, bootstrap/node identity,
SB/OVS state. Pod snapshot больше не требуется. Ошибка этих общих источников,
истечение отдельного бюджета общей подготовки или невозможность выполнить общую cleanup/Sweep
сохраняют полный fencing и withdrawal. Ошибка ACK удалённой сети после
успешной физической Cleanup сохраняет registration для retry; рабочие VPC
из-за неё не закрываются.

Bootstrap validation конкретного VPC, отсутствие его native attachment или
bound/installed return port, route conflict/netlink error, L2 guard registration
или preparation/open error закрывают **только этот VPC**. Обработчик:

1. Вызывает Fence с точным VPC UID, закрывая L2 и L3 owned ingress.
2. Только после успешного Fence публикует Closed его зарегистрированным guards.
3. Независимо от ошибок fencing/API пытается отозвать routes этого OwnerID.
4. Логирует ошибки и продолжает другие VPC. Неуспешное закрытие не выдаёт Closed.

Daemon stop context не имеет deadline всего цикла. Общая подготовка и каждое
обновление общего snapshot ограничены 4s; каждый VPC получает отдельные 4s,
а его закрытие — независимые 3s. Истечение VPC budget не вызывает общий
Fence/Sweep. Неуспешное закрытие сохраняется для повторной попытки до обычного
reconcile этого UID. Половина оставшегося closure budget резервируется для
withdrawal после fencing/ACK. Ticker остаётся 5s, обработка последовательная;
это не гарантия периода или общего времени цикла.

Перед следующим VPC snapshot обновляется; перед Open повторяются проверки
конфигурации и живого dataplane. Настоящая ошибка общего источника остаётся
общим отказом. Неоднозначный timeout общего чтения внутри VPC проверяется
независимым общим budget. Подробности и ограничения:
[reconcile-timeouts.md](reconcile-timeouts.md).
Native PE namespace/link retirement остаётся обязанностью native owner.

Удалённые VPC routes очищаются Sweep по live UID. Host Cleanup сохраняет
свой ownership/lifecycle, включая internal OVS Port/Interface и terminal guards.
После успешного удаления L2 path закрываются и забываются соответствующие guard
registrations. Sequential L2VNI reuse A → cleanup → B сохраняется; горячий перенос
действующего subnet между VPC не реализуется.

Reflection barrier/membership/session и nft guard не упрощаются. Новые MAC
по-прежнему допускаются только после защиты потенциально открытых L2 edges.
Обработка ARP/IPv6 ND/RARP, primary/additional binding, requested-chassis и
миграция KubeVirt остаются штатными. CIDR route не меняет владельца при миграции,
а Type-2/MAC Mobility принадлежит только OpenPERouter. Успешный Linux FDB move
или binding unit test не является доказательством внешней EVPN/KubeVirt миграции.

## Переход с endpoint routes

До запуска новой версии остановить все прежние publishers и предварительно
закрыть legacy attachments. Mixed-version rolling upgrade не поддержан.
Старый BGP runtime не оставляется как совместимый параллельный путь.

После startup Fence, до API cache sync и любого Open:

- Sweep удаляет собственные routes нового формата от прошлого процесса.
- PurgeLegacy удаляет только retired host routes: protocol 201, metric 42760,
  unicast/universe /32 или /128, tenant table, local gateway/device, без multipath;
  IPv4 дополнительно требует realm с верхним байтом `0x4b`, IPv6 — realm 0.
- Ничего в retired формате больше не создаётся и не продлевается.

Это безопасно **только при соблюдении прежнего reservation contract**: вся
совпадающая signature принадлежала старой интеграции. Если reservation когда-либо
использовали сторонние процессы (особенно IPv6 без realm), автоматическая
атрибуция невозможна: до запуска daemon провести offline inventory, подтвердить
каждый старый route по сохранённым данным и вручную завершить переход в закрытом
стенде. Не стартовать runtime поверх неразобранного конфликтующего ownership.
PurgeLegacy нельзя заменить широким `flush proto 201`.

При аварии процесса subnet routes сохраняются в ядре. Их желаемость связана с
конфигурацией подсети, не с жизнью Pod. Однако умерший daemon не гарантирует
своевременный отзыв при потере attachment или удалении сети; нужен restart либо
внешний liveness/withdrawal механизм для ограниченного срока. PreStop недостаточен.

## Bootstrap и внешний FRR контракт

Адресация `transitCIDRs`/`hostSessionCIDRs`, namespace value и read-only metadata
`kube-ovn-attachments.yaml` нужны L3 attachment и сохраняются. Legacy название
hostSessionCIDRs не включает BGP. ASN поля предыдущей доработкой удалены из API.
После обновления descriptions выполнены CRD/client generators; оба charts
сохраняют API_SERVER_HOST/PORT overrides и прежние defaults вне opt-in режима.

OpenPERouter `Mac_setings_for_l2vni_and_l3vni@60a1285935b10eb0b1cf07e4533faefedde3d4d3`
по-прежнему не имеет независимого от hostSession non-BGP L3 veth lifecycle:
`internal/conversion/host_conversion.go:l3vniToHost` без HostSession не задаёт
LinkIPs, `internal/hostnetwork/vni.go:SetupL3VNI` пропускает setupHostVeth.
Текущий runtime отвергает legacy hostSession. Local metadata не создаёт veth
и не является предлагаемым native API. Нужен отдельный внешний контракт;
его имя/наличие здесь не выдумываются. Другие репозитории не меняются.

FRR 10.6.0 классифицирует protocol 203 как `kernel`. Реальный lab показал:
Linux priority `0x4cXXXXXX` zebra отображает как **distance 76**, metric
`0xXXXXXX`. Поэтому `match metric` должен использовать младшие 24 бита, не
полную kernel priority. Внешний владелец FRR должен установить allowlist точных
routed CIDR **и** соответствующую metric текущего VPC UID; при пересоздании VPC
policy должна обновиться. Нельзя включать bare redistribute kernel или host
prefix-list от старой реализации. Фабричные копии routed CIDR, уже доступных через локальный OVN, не должны
вытеснять локальный путь: внешний import policy должен исключать такие
конфликтующие копии. Если foreign exact-prefix уже установлен, runtime
закроет только этот VPC и не будет перезаписывать чужой маршрут.
Imported BGP, connected/service/defaults и
native L2VNI не должны повторно экспортироваться этим путём.

[hack/openperouter/frr-netlink.conf](../../hack/openperouter/frr-netlink.conf)
проверяет этот контракт для lab-vpc, IPv4 /24 и IPv6 /64. Это только fixture,
не конфигурация, устанавливаемая Kube-OVN. Реальный Cisco Type-5/export/import
не проверен. Native FRR owner должен сохранять policy после reconcile/restart
через собственный поддержанный механизм; Kube-OVN не редактирует FRR.

PE unreachable defaults остаются отложенной доработкой OpenPERouter; этот
код их не устанавливает. Host-side terminal defaults/guards сохраняются.
Не заявляется полная tenant→underlay изоляция.

## Воспроизводимые проверки

Сначала создать `/tmp/subnet-lab` и пустой Docker config directory. Только на
отдельном lab host, не в host namespace рабочей ноды:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o /tmp/subnet-lab/routes.test ./pkg/openperouter/routes
cp hack/openperouter/{netlink-routes.sh,frr-netlink.conf} /tmp/subnet-lab/
docker --config /tmp/empty-docker-config run --rm --privileged --network none \
  --entrypoint /bin/bash -v /tmp/subnet-lab:/lab:ro \
  quay.io/frrouting/frr:10.6.0 /lab/netlink-routes.sh
```

Полный component runner (артефакты собираются из текущего исходного дерева):

```sh
for spec in daemon:pkg/daemon host:pkg/openperouter/host northbound:pkg/openperouter/northbound routes:pkg/openperouter/routes ovs:pkg/ovs; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c \
    -o "/tmp/subnet-lab/${spec%%:*}.test" "./${spec#*:}"
done
bash hack/openperouter/subnet-regressions.sh \
  quay.io/frrouting/frr:10.6.0 YOUR_OVS_NFT_LAB_IMAGE /tmp/subnet-lab
```

OVS/NFT image должен содержать iproute2, nftables и работающий kernel OVS.
Runner сохраняет `results/status.tsv` и отдельные логи; nonzero exit любого
suite даёт nonzero итог. Никакого подключения к Kubernetes в runner нет.

Linux unit selection: `routes.test -test.run 'TestRouted|TestSubnet|TestLegacy'`.
`TestOpenPERouterScopedFailureLinux` требует отдельного network-none container
и `KUBE_OVN_SUBNET_ISOLATION_LAB=true`; OVS row snapshot stubbed, но закрытие
Linux links, guards и route withdrawal проходят production code.
Host/reflection packet suites запускаются через
[hack/openperouter/review-regressions.sh](../../hack/openperouter/review-regressions.sh).
Новый `TestL2MACMoveWithReflectionGuard` также требует disposable container.
Фактические результаты и skips: [validation-results.md](validation-results.md).
