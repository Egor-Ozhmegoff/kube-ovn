> Историческое исследование BGP runtime на базе 4601dcca47fb.
> В текущей доработке GoBGP/lease заменены netlink PE routes:
> [контракт](netlink-routes.md), [новые результаты](validation-results.md).
> Deployment и измерения этого документа не повторялись для новой реализации.

# Reconciliation scaling: 8 VRF / 26 L2

База Kube-OVN: `openperouter@4601dcca47fb3a7a4e7f904a616fc9f7dd1cd1f9`
плюс dirty-изменения, включая сохранённые ASN-аннотации/CRD пользователя.
OpenPERouter не изменялся; контракт — MAC-ветка
`60a1285935b10eb0b1cf07e4533faefedde3d4d3`, не `unreachable-route`.

## Измеренная причина

Общий context deadline 4s распространялся на последовательный проход всех
VRF/L2 attachments. Ошибка измерена на настоящих test03/test04: Kubernetes
1.36.5, Linux 6.8.0-149, containerd 2.1.5, лимит daemon **1 CPU** сохранён.
Измерения — inclusive wall time и число вызовов за проход; вложенные этапы
пересекаются и их нельзя складывать как независимые интервалы.

- Без L2, с 8 VPC/Pod Subnet: примеры steady state test03 0.28–0.36s,
  первый проход около 0.84s.
- С 26 L2: примеры test03 3.4–3.98s. На test04 повторялись обрывы в 4s
  после 22–24 L2, в том числе `apply reflection filter: signal: killed`.
- Пример полного прохода: nft ensure около 2.3s (примерно 1.0–1.15s listings
  и 1.25–1.35s apply), barrier reads около 0.25–0.30s, OVS ensure около
  0.32–0.40s. Host stages включают остальные netlink/validation операции;
  отдельное время каждого netlink syscall не измерялось.
- Старый steady state запускал **53 nft процесса**: 27 `list tables`
  и 26 безусловных apply. После timeout Fence/Closed/withdraw приводили
  к повторному открытию, CAS ACK и тем же apply в следующем проходе.
  Это объясняет устойчивый цикл ошибок test04, а не только одиночный пик.
- CPU throttling зафиксирован через cgroup cpu.stat; это не доказательство,
  что он был единственной причиной. Ресурсы/CPU limit не повышались.

## Исправление и safety

`EnsureReflection` читает **фактическую целевую nft table** на каждом вызове.
Проверяются owner comment/Subnet UID, family/name, единственные set/chain/rule,
полный MAC set, ingress device, hook/type/priority/policy и drop expression.
Handles и текущие counter values не считаются drift. Другие отличия приводят
к атомарной замене только собственной таблицы. Чужой/неполный ownership dump
и ошибки чтения не разрешают destructive replacement.

Отсутствие таблицы подтверждается отдельным успешным inventory: ошибка
inspect сама по себе не означает, что таблицу можно создать. При гонке между
read и apply nft transaction либо выполняется целиком, либо ошибка возвращается
в прежний fail-closed путь. Нет persistent/in-memory cache установленных правил.
Потеря rule/set/table обнаруживается свежим read. После удаления veth kernel
оставляет цепочку без `dev`; это проверено в disposable lab и требует repair
даже при **неизменном MAC-наборе** и повторном использовании имени устройства.

Deadline **4s**, ticker **5s**, доступность по свежему API Pod snapshot и lease
**30s** не менялись. Ошибки по-прежнему вызывают fencing, Closed только после
успешного закрытия и отзыв маршрутов. Filter verification/install остаётся
внутри production `ProtectAndOpen` до ACK и финальной проверки session/revision
перед Open. Stable Active ACK не заменяет проверку kernel state.

Нормальный проход теперь делает 26 targeted inspections + 1 cleanup inventory,
**0 apply** при неизменной защите. На обеих нодах измерены примеры 1.6–1.8s
с полными 8 VRF / 26 L2; задержка не маскируется большим timeout.

## Обнаруженная гонка Cleanup

Kernel regression поймал `close VRF slave opre…: no such device`: после OVS
Port transaction internal interface мог исчезнуть между LinkList и LinkSetDown.
Cleanup теперь считает ENODEV доказательством отсутствия именно старого slave
при close/detach. Другие ошибки запрещают дальнейшее отсоединение. Terminal
RPDB guards уже установлены перед этим шагом; native veth не удаляются.
Есть deterministic operation-order tests, полный old UID → cleanup → new UID
kernel test и восемь дополнительных повторов в fresh containers.

Отдельный NLM_F_DUMP_INTR — не доказательство успешного cleanup. Production
по-прежнему отвергает неполный dump. Kernel harness повторяет только этот
документированный transient result; любой другой error сразу завершает тест.

## Измерения и масштаб

Одна строка `OpenPERouter reconciliation timing` содержит total duration,
error и агрегированные counts/durations: `api.pods`, `barrier.read/update`,
`guard.register/protect-open`, `ovs.bindings/ensure-port`, `host.cleanup/l2/l3`,
`nft.inspect/list/apply/ensure/cleanup`, `bgp.ready`. Не суммируйте nested stages.

Остались 78 barrier GET, один Node GET и один Pods LIST на healthy цикл
этого набора, а также 42 OVS port ensure. Stable barrier ACK не переписывается;
оптимизация не заявляет устранение всех OVS/netlink транзакций.
При одинаковых 26 L2 на каждой ноде это примерно 4k/16k API requests/s для
250/1000 нод без informer/retries. Ответы CM включают всех зарегистрированных
участников; размер и CAS contention также растут. Это **расчёт, не нагрузочный
тест**. Глобальная масштабируемость admission на 250–1000 нод не подтверждена.
Сохранены внутренний OVN VXLAN, локальный PE и отсутствие копий внешних RIB
в OVN/SB; новых routers/policies на node×VRF не добавлялось.

## Сборка, deployment и воспроизведение

Final image: `localhost/kube-ovn-openperouter:scaling-4601dcca47fb-dirty-63444922e812`.
Manifest digest на обеих нодах:
`sha256:15060bcd1d6b0b44f215737c7140f20e8b78e6c08ff33de25d573172a2125f12`.
Пересобран daemon; tools/OVS/OVN и остальные бинарники переиспользованы из
`localhost/kube-ovn-openperouter:git-4601dcca47fb`. Этот commit tag не перезаписан.
Code/config source copy сверена по SHA256; dirty manifest и build recipe сохранены.
Новый образ импортирован в containerd обеих нод и применён Helm revision 4
с generated site values. Полный OCI archive хранится на test03; test04 получил
новые blobs с использованием уже имевшихся immutable родительских слоёв.

Deployment values обновлены, render/verify прошли. Сохранены Helm revisions
2 (pre-task) и 3 (первое scaling fix без Cleanup ENODEV correction), старые
образы и pre-task values/manifest. Rollback 3 сохраняет работающий масштаб
26 L2; rollback 2 возвращает воспроизведённое ограничение 4s при этом масштабе.
OS/systemd/FRR bootstrap не менялся. Default обоих хостов через management,
API `10.110.191.112:6443`, host Node IP и отдельные PE VTEP IP сохранены.

[Воспроизводимый cluster fixture](examples/scaling/README.md) и
[результаты/ограничения](validation-results.md). Kernel tests запускать только
в disposable privileged network-none containers. На этих хостах операторский
nft 1.0.9 падал при JSON dump; runtime nft 1.1.6 из CNI работал. Это не повод
менять host packages или обходить проверку защиты.

Cluster packet checks используют synthetic PE ingress, а не физический Cisco
endpoint. KubeVirt/Multus live migration, native Type-2 lifecycle/MAC Mobility,
external ECMP, physical fabric MTU и cold boot не подтверждены этой задачей.
PE unreachable defaults остаются отложенной зависимостью OpenPERouter:
полная защита tenant→PE underlay fallback не заявляется.
