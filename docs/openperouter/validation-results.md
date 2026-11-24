# Routed subnet routes и изоляция VPC: 2026-11-23

Рабочая ветка `openperouter`, HEAD `bc3682e5996f959fa0e50e3e0079f0dfe2d9d540`.
Несвязанные изменения прошлого этапа сохранены; per-endpoint часть явно
переработана по новому требованию. Дерево не считалось чистым HEAD.
Index не изменялся. Commit/push, live cluster, deployment automation и другие
репозитории не затрагивались. Подробный контракт:
[netlink-routes.md](netlink-routes.md).

## Изменения текущего этапа

- Удалены per-workload prefix selection, Pod API snapshots и проверки/публикация
  endpoint /32,/128. Старые endpoint-only tests заменены subnet/isolation tests.
  Нормальный pkg/speaker, L2 reflection и штатная KubeVirt обработка сохранены.
- В PE устанавливаются только CIDR готовых OVN Subnet без openPERouterL2.
  Native L2VNI connected routes, Type-2/FDB/ARP/ND/MAC Mobility остаются native.
  Host-side protocol 202 routes уже существовали и сохранены.
- Новый PE protocol 203 с сохраняемым VPC OwnerID в metric, включая IPv6,
  обеспечивает scoped diff/withdraw, restart/recreation и cleanup удалённых VPC.
  Table берётся из фактического VRF; чужие routes/defaults не заменяются.
- VPC-local return/attachment/bootstrap/guard/route errors: Fence UID → Closed
  только после успешного закрытия → scoped withdrawal; другие VPC продолжают.
  Общие snapshot/deadline ошибки сохраняют общий fencing. Ошибка ACK уже
  физически удалённого пути сохраняет registration для retry без закрытия A.
- После startup Fence legacy cleanup удаляет только прежнюю reserved endpoint
  signature (201/42760 с дополнительными признаками). Нужен документированный
  остановленный переход; mixed-version upgrade не поддержан.
- API descriptions/CRD/generated apply configuration обновлены, legacy адресация
  attachment сохранена. Новых route knobs не добавлено. Оба charts сохраняют
  namespace/API endpoint настройки. Документация и FRR fixture обновлены.

## Фактические результаты

Linux/dataplane component tests выполнены на **10.64.244.143** в новых
`--network none` containers; для kernel suites добавлен `--privileged`.
Host network namespace и рабочие ноды не использовались для network mutations.
Воспроизводимый runner: [subnet-regressions.sh](../../hack/openperouter/subnet-regressions.sh).
Raw results, версии и source hashes локально:
[results/subnet-routing-20261123](results/subnet-routing-20261123/),
под существующим gitignore; это не новые отслеживаемые repository fixtures.

| Проверка | Фактический результат |
| --- | --- |
| Linux runner | **13/13 групп PASS**, status.tsv содержит exit 0 каждой группы |
| Routed subnet unit | PASS: L2VNI исключены, IPv4/IPv6 CIDR, deletion/not-ready/provider, NAT rejection |
| Planner/ownership unit | PASS: scoped diff, other VPC/default preservation, exact-prefix conflict, 26 subnet batch, partial failure/cancellation, legacy signature controls |
| Daemon isolation unit | PASS: порядок A/B, unprepared/previously-open B, closure error, общий cancellation |
| VPC isolation kernel component | PASS: production Fence/guard Close/Withdraw B при потере return binding и самого PE attachment, A link/guard/routes остаются; ошибка fencing не отменяет попытку withdrawal. Только OVS row snapshot stubbed |
| PE subnet routes + FRR 10.6.0 | PASS: реальные IPv4 /24 и IPv6 /64 в table 23001, отличной от VNI 10001, корректный local next-hop; BGP RIB redistribution/withdrawal |
| Pod/VM metadata churn | PASS component: fake API source/target launcher creation/deletion при реальном kernel route reconcile не меняют CIDR RIB; не создают endpoint routes/L2 CIDR через L3 |
| FRR negative controls | PASS: foreign/service/default/synthetic protocol-186 host routes не экспортированы этим route-map. Это не реальный external BGP import test |
| Actual recreation/startup cleanup | PASS: VRF/veth/namespace generation, ifindex/table change, repeated Sweep/PurgeLegacy; stale endpoint /32,/128 удалены, foreign controls сохранены |
| Process SIGKILL/restart | PASS как подтверждение, что kernel subnet routes сохраняются после SIGKILL; restart Sweep их удаляет. Standalone child test skipped, но child выполнен и убит parent test |
| Existing kernel regressions | 7/7 PASS: internal Cleanup/recreate, sequential L2VNI reuse, partial rebind/terminal guards, VRF loss, nft lifecycle, delayed admission packets |
| MAC movement packet component | PASS: RARP перемещает Linux FDB source→target, silent prepared target не переносит FDB; reflection Ethernet/ARP/ND/RARP блокируется; external ND и исходящий RARP проходят; cancellation/re-activation и late source deletion сохраняют актуальный FDB |
| OVN binding unit | PASS: primary/additional activation через RARP, отмена и переключение primary. Это не запуск KubeVirt |
| Portable race suites | PASS `go test -race ./pkg/openperouter/...`; Linux-only tests выполнены отдельно runner |
| CRD/client generation | Выполнены make gen-crd и update-codegen.sh; relevant CRD/apply configuration/deepcopy stability PASS |
| **make lint** | **НЕ ПРОШЁЛ**: verify-crd сравнивает незакоммиченные generated CRD с index/HEAD. Commit или staging для обхода не делались |
| Отдельные lint/modernize | PASS с 0 issues через `CI=true GOOS=linux make -o verify-crd lint`; это не успешный полный make lint. Для modernize используется host-native executable, анализирует Linux packages |
| Helm 4.3.2 | PASS default/opt-in lint обоих charts, render namespace override; прежние SemVer/icon warnings остаются |
| Audit harness | 4/4 PASS; не считается native API/dataplane audit |
| Formatting/syntax/diff | gofmt, shell syntax, git diff --check и git diff --cached --check PASS |

FRR показал существенное отличие metric encoding: `0x4cXXXXXX` отображается
как distance **76**, metric **младшие 24 бита**. Это учтено в lab route-map.
Production export/import policy должен обеспечить native FRR owner; нельзя
реэкспортировать конфликтующие fabric-копии локально доступных OVN CIDR.

Первые прогоны не считались успешными: исправлены устаревший /32 lookup в
fixture, FRR metric filter и отменённый testing.Context в cleanup. Полный
codegen создал несвязанный baseline drift 62 clients/informers. Перед его
исключением проверено отсутствие этих diff в снимке начала этапа, сделаны
backups и подтверждено совпадение bytes при повторном generator run. Существующая
openperouterintegration.go доработка сохранена. Автоматическая проверка сначала
отклонила массовое восстановление без такого доказательства; оно не выполнялось.

## Внешние зависимости и непроверенные сценарии

**Совместного deployment с MAC-веткой ещё нет.** В закреплённом OpenPERouter
`60a1285935b10eb0b1cf07e4533faefedde3d4d3` L3 veth по-прежнему связан с
hostSession: conversion без HostSession даёт LinkIPs=nil, SetupL3VNI пропускает
setupHostVeth. Нужен non-BGP native attachment lifecycle. Metadata Kube-OVN
не выдаётся за существующий native API. Код OpenPERouter не менялся.

Не выполнены новым runtime: два совместимых OpenPERouter PE с настоящим
EVPN Type-2 MAC/IP Mobility, Cisco Type-5/export/import/ECMP, двухнодовый полный
8 VRF/26 L2 OVN+PE dataplane и внешний BGP fault, Kubernetes Services/NetworkPolicy
E2E, KubeVirt success/cancel/source cleanup с реальным трафиком. Linux FDB movement
и binding tests не заменяют эти проверки. Полные make ut/build-go не запускались;
собраны релевантные Linux test binaries и выполнены перечисленные suites.

Не гарантирован bounded withdrawal при смерти/зависании daemon до restart:
subnet routes сохраняются в kernel. PE unreachable defaults остаются отдельной
зависимостью OpenPERouter, production Kube-OVN их не устанавливает. Defaults в
lab — только foreign preservation controls, не обход native reconciler.
Полная tenant→underlay изоляция и Cisco/KubeVirt readiness не заявляются.

---

Предыдущий per-endpoint netlink runtime удалён. Его исторические логи сохранены
локально в results/netlink-20261123 и не считаются проверкой текущей архитектуры.

## Исторические результаты прежней BGP-реализации

# Scaling reconciliation: test03/test04, 2026-11-23

Исправление развёрнуто и проверено на настоящем двухнодовом кластере,
только `10.64.244.68/.69`, root через SSH-agent. На завершение активны
**8 tenant VPC, 8 Pod Subnet, 26 L2 Subnet, 26 barriers / 52 current Active ACK**.
Тестовый namespace удалён; отзыв workload `/32` и EVPN Type-5 проверен.
Все системные Pod Ready, management default и API `.112:6443` сохранены.

Kube-OVN: `openperouter@4601dcca47fb3a7a4e7f904a616fc9f7dd1cd1f9` плюс
незакоммиченные изменения. Существовавшие ASN uint32/int64 annotations и CRD
сохранены. OpenPERouter рабочая копия `unreachable-route` с изменениями не
переключалась/не редактировалась; контракт MAC-ветки —
`60a1285935b10eb0b1cf07e4533faefedde3d4d3`. Deployment repo:
`openperouter_kubeovn@88bb76f21492194c268fec33b429229d77ad4beb` плюс
сохранённые изменения пользователя и обновлённые image tag/документация.
В рамках задачи commit/push не выполнялись.

## Причина и изменения

[Подробный разбор и inter-operation safety](reconcile-scaling.md).
После применения 26 L2 test04 воспроизводимо завершал проход в 4s после
22–24 attachments, включая `apply reflection filter: signal: killed`.
Примеры test03: 3.4–3.98s; nft ensure около 2.3s, API barrier reads
0.25–0.30s, OVS ensure 0.32–0.40s. Timings inclusive, не суммируются.
Зафиксирован CPU throttling; лимит 1 CPU не менялся, единственной причиной
он не объявляется.

- Безусловные 27 nft table listings + 26 apply заменены свежей инспекцией
  каждой целевой таблицы и заменой только при drift. Stable state: 27 reads,
  0 apply; кэш успешных применений не добавлялся.
- Проверяются owner, MAC set, device/hook/priority/policy и drop rule;
  lost hook после veth recreation обнаруживается даже при том же MAC set.
  Ошибки не обходят barrier/ACK/Open или fencing/withdraw.
- Deadline 4s, ticker 5s и lease 30s сохранены. Новых OVN routers/policies,
  внешних RIB copies или владельцев native FDB/neighbors/FRR нет.
- В Cleanup исправлен подтверждённый ENODEV между OVS internal-port deletion
  и закрытием старого VRF slave. Other errors не разрешают detach.
- Packet fixture отправляет IPv6 frame по объявленной длине payload:
  прежний тест ошибочно требовал padding, который удалял br_netfilter.
  Сравнение содержимого кадров и deny/positive assertions не ослаблены.

## Образ и итоговое состояние

`localhost/kube-ovn-openperouter:scaling-4601dcca47fb-dirty-63444922e812`

Обе containerd image записи имеют manifest digest
`sha256:15060bcd1d6b0b44f215737c7140f20e8b78e6c08ff33de25d573172a2125f12`.
Runtime image ID обоих CNI:
`sha256:fd12fa557fe8e41154e8205623430391aad79bfde3c0b025473fbc7edab71995`.
Пересобран daemon, остальные binaries/tools переиспользованы из исходного
`git-4601dcca47fb`; этот tag не перезаписан. Code/config source copy
сверена с локальным SHA256 manifest перед сборкой. Образ импортирован в CRI
обоих узлов; применены проверенные generated values, Helm revision **4**.
Для отката оставлены revisions 2/3, исходные values/manifest и старые образы.
Revision 3 уже содержит scaling fix; 2 возвращает известную проблему при 26 L2.
OS/systemd/PE bootstrap в этой задаче не менялись.

## Финальный steady state

Отдельное 180s окно **после** рестартов/churn, до удаления тестовых workload:

| Нода | Проходов | Min / median / max | Ошибки/fencing | nft apply | Возраст local BGP route |
| --- | --- | --- | --- | --- | --- |
| test03 | 36 | 1.549 / 1.805 / 1.974s | 0 | 0 | 424s |
| test04 | 36 | 1.569 / 1.717 / 1.928s | 0 | 0 | 243s |

На каждом проходе подтверждены 26 nft inspections, один cleanup inventory,
78 barrier reads и 42 OVS ensures. Нулевые nft apply не означают устранения
всех OVS/netlink операций. Расчёт API стоимости и границы 250–1000 нод —
в scaling report; такой нагрузочный тест не выполнялся.

## Проверки этой задачи

| Проверка | Фактический результат |
| --- | --- |
| `make gen-crd`, повторная генерация / SHA256 обоих charts и install.sh | PASS, идемпотентно, пользовательские ASN annotations сохранены |
| Обычный `make lint` | BLOCKED на verify-crd: generated dirty CRD отличаются от HEAD; commit для обхода не делался |
| Linux `make -o verify-crd lint`, CI=true | PASS: golangci-lint 2.15.0, 0 issues; modernize без замечаний; пропущен только verify-crd, генерация проверена отдельно |
| Linux `go test -race ./pkg/openperouter/...` | PASS, включая host/barrier/admission; opt-in kernel/BGP cases отдельно |
| daemon OpenPERouter и выбранные controller KubeVirt migration regressions | PASS; не настоящая VM migration |
| 8 VRF / 26 filter unit fixture | PASS: 3 cycles / 78 fresh inspections, нет повторных apply, MAC churn/lost state/partial failure/timeout/retry/foreign ownership |
| Kernel Cleanup/new UID, VNI A→B reuse, partial rebind, missing terminal, host VRF loss, nft lifecycle | PASS, 6/6 в fresh privileged network-none containers |
| Дополнительные Cleanup/new UID повторения | PASS, 8/8 fresh containers после ENODEV fix |
| Delayed-filter production barrier + kernel packet test | PASS, 3/3 fresh containers после исправления IPv6 padding в harness; Ethernet/ARP/NA/RARP deny + positive controls + outbound RARP |
| Ansible render/verify | PASS, 14/12 tasks, 0 failures; изолированный config/temp, KUBECONFIG=/dev/null |
| `git diff --check`, `git diff --cached --check` | PASS для обоих изменённых репозиториев |

Kernel mutations выполнялись только внутри одноразовых контейнеров. Go 1.27.4,
Linux 6.8.0-149, runtime nft 1.1.6, OVS 3.7.2+g8657d8d2ba99,
OVN 26.03.4+ge49fc5c5dfd8, FRR 10.6.0_git, Podman 5.8.1.
Ни один kernel regression не запускался
в host network namespace действующего узла.

## Реальный кластер и dataplane

- Два новых L2 endpoint/MAC допущены на каждой ноде, включая отдельную пару
  после final deployment. Running/allocated Pod имеют намеренно ложный
  application readiness; связность и host /32 announcements работают.
- Pod-only inter-node и L2 inter-node: 10/10 replies каждый, TTL 64.
  Scoped capture содержит tenant packet `.64.2 → .64.3` во внутреннем
  VXLAN **Node IP `.112 → .113`**. Это не packet через внешний VTEP.
- Local PE echo: по 3/3 каждой ноды. Scoped captures на обоих PE показывают
  `.64.2` только на test03 и `.64.3` только на test04, без SNAT и другого
  Kubernetes hop. Это доказательство входа в локальный PE, не Cisco E2E.
- После final restart на обеих нодах введены отражённые Ethernet/ARP/NA/RARP
  с MAC удалённого Pod: drop counter вырос ровно на 4, в локальный Pod они
  не пришли. Положительные external Ethernet/NA/RARP пришли по одному;
  outbound RARP с настоящим Pod MAC пришёл в PE. Для ARP получены реальные
  replies к внешнему MAC на обеих нодах: OVN ARP responder не обязан
  доставлять сам request в Pod.
- Повторный live delete/recreate `pci-etc-z504` на финальном образе:
  новый Subnet UID, native veth сохранён, private bridge удалён, старый lookup
  отсутствует, terminal drop обоих families сохранён, затем 26/52 восстановлены.
  Live VRF A→B смена native OpenPERouter config не выполнялась; это kernel case.
- Последовательные daemon restarts обеих нод: новый Pod UID, сменились все
  26 локальных sessions, все ACK актуальны. Межнодовый ping через restart:
  **1200/1200, 0% потерь за 240s**. Это внутренняя OVN-связность, не обещание
  нулевой потери внешнего трафика при перезапуске BGP speaker.
- Bounded API load: 794 успешных CM reads за 40s, максимум 20/s, без ошибок;
  это не нагрузочный тест большого кластера.
- 9000-byte inner IPv4 (`-M do -s 8972`): 3/3 между L2 workload, без фрагментации.
  Default-network DNS через `10.99.0.10` вернул Service `10.99.0.1`.
- На обеих PE проверены 8 L3 bridge с единым node RMAC (разные между нодами)
  и 26 L2 bridge с `02:00:05:00:03:00`. EVPN peer только `10.222.252.5`,
  Established; фактические TCP source — собственные VTEP `.68/.69`.
- Workload `/32` присутствует в PE unicast RIB при PodReady=false и в EVPN
  Type-5 с RD `10.110.191.68:270`. После удаления test namespace локальные
  /32 и Type-5 обоих mgmt workload отсутствуют на обеих PE. Все 26 L2
  Subnet и исходные site VPC/Pod Subnets оставлены активными.
- Management default обоих узлов остаётся `10.64.244.1/enp26s0f1`;
  только явный Node underlay prefix идёт через infra-host. API readyz `.112`
  и TCP из worker проверены. Старые OVN/etcd базы не трогались.

## Непрошедшие первоначальные запуски и границы

Первоначальные component packet runs падали на положительном NA control:
принят frame 86 bytes с совпадающим содержимым, тест ждал 96 с padding.
Оригинальные failed/debug логи сохранены. Первые Cleanup runs поймали ENODEV
(исправлен) и NLM_F_DUMP_INTR; harness теперь повторяет только неполный dump,
не принимает partial result и не скрывает другие ошибки. Операторский host
nft 1.0.9 упал на netdev JSON dump; использован CNI runtime nft 1.1.6,
host packages не изменялись. Эти попытки не засчитываются как успешные проверки.
Начальный Ansible запуск с /dev/null в качестве config отклонён по расширению;
последующий запуск с отдельным .cfg прошёл.

Не выполнялись full make ut/e2e suite, 250–1000-node load test, cold boot,
недоступность API при OS boot, физический Cisco endpoint/ECMP/MTU/inter-VRF
policy E2E, Multus/KubeVirt success/cancel/live migration, IPv6 L3 workloads,
native Type-2 lifecycle/EVPN MAC Mobility или исчерпывающая NetworkPolicy/tenant
Services matrix. Synthetic PE ingress и RARP не являются VM migration.
PE unreachable defaults и native endpoint lifecycle lease/epoch остаются
внешними зависимостями OpenPERouter. Полная tenant→PE underlay fallback
защита и готовность всей интеграции не заявляются.

Reproducer: [examples/scaling](examples/scaling/README.md).
Логи и source manifest: ignored `results/linux-scaling-20261123/`, на test03 —
`/var/tmp/kubeovn-scaling-20261123/results/`. В артефакты не включены kubeconfig,
Helm Secret manifests, auth files и image tar.

Ниже — **исторические** результаты предыдущих задач, не повтор текущей.

---

# L2 reflection barrier membership, 2026-11-19

База: `openperouter@b7257133dd552bda1a05f59b2b716fadd8bf8223` плюс
существовавшие изменения рабочего дерева. Ветка не переключалась, staged
изменений на старте не было. Предыдущие исправления Cleanup/L2VNI reuse
сохранены. До редактирования сделана отдельная копия dirty/untracked файлов.
Прочитаны AGENTS.md, CODE_STYLE.md и Development and Contribution Guide
по ссылке README. CRD/API типы не менялись; отдельная генерация не нужна,
verify-crd выполняется внутри make lint. Commit/push и deployment отсутствуют.

OpenPERouter в этой задаче не изменялся и его тесты не запускались.
Контракт остаётся MAC-веткой
`Mac_setings_for_l2vni_and_l3vni@60a1285935b10eb0b1cf07e4533faefedde3d4d3`.

## Причина и исправления

Controller передавал все Kubernetes Nodes в Publish. Для неизвестных UID
создавались Pending с пустой Session; Ready отвергал их, а удаление Node
не убирало запись. Нода без daemon блокировала новый MAC на здоровых нодах.

- Nodes.List и аргумент nodes удалены из controller/admission/Publish.
  Publish обновляет revision и сохраняет зарегистрированных участников.
  Он удаляет только старые **sessionless Pending placeholders** прежней
  реализации: такие записи никогда не разрешали открытие. Pending/Active
  с session не исключаются по Node Ready/deletion/таймауту.
- Единственный способ вступления — daemon JoinAfterClose → JoinClosed:
  host flock уже удерживается, local path сначала закрывается. Ошибка
  закрытия не публикует Closed и не заменяет session.
- Production ProtectAndOpen выполняет CAS InstallIntent → install → ACK
  и свежую проверку UID/Node UID/session/revision/Active перед Open.
  Стабильный Active не переписывает Pending/ACK каждый reconcile.
- При смене Node UID в работающем процессе сначала Fence и Close старой
  identity, затем новая регистрация. Старая запись не стирается. Ошибка
  закрытия/API сохраняет прежнюю identity и запрещает переход.
- NB generation pin, монотонные MAC sets и отдельный admitted set сохранены.
  Постоянный MAC VM не ждёт несвязанных новых MAC при migration.

Publish после последнего API read и до LinkSetUp нельзя атомарно объединить
с Linux операцией. В этом interleaving сохраняется предыдущий установленный
фильтр, а Active ACK старой revision блокирует admission новых MAC следующей.
Если смена revision замечена до финальной проверки, Open отклоняется.
CAS conflicts и эти interleavings проверяются production barrier/admission
кодом. Полный разбор: [implementation.md](implementation.md#внешний-l2).

## Проверки этой задачи

Изолированная source copy `/tmp/kubeovn-membership/source` на разрешённом
Linux lab. Kernel mutations выполняются только в fresh privileged
network-none containers; живые node namespaces не меняются. CI=true
отключает мутирующий lint в scratch copy. Среда прежняя: Linux 6.8.0-147,
OVS 3.7.1, OVN 26.03.0, nftables 1.0.9, Go 1.27.4, golangci-lint 2.15.0.

| Проверка | Последний результат |
| --- | --- |
| `make lint` (verify-crd, golangci-lint, modernize) | PASS, exit 0: golangci-lint 0 issues, modernize без замечаний |
| `go test ./pkg/openperouter/...` | PASS, финальный повтор exit 0 |
| daemon OpenPERouter и controller migration tests | PASS, финальный повтор exit 0 |
| `go test -race ./pkg/openperouter/...` | PASS, exit 0 |
| Helm/API endpoint/source-audit | PASS, 9/9 на исходниках этой задачи; chart файлы не менялись |
| OVS active binding/activation tests | PASS; соответствующий код не менялся |
| Kernel Cleanup/same L3VNI/new UID, L2VNI A→B/recreation, partial rebind, missing terminal, VRF loss, nft lifecycle | PASS, 6/6 в отдельных disposable containers |
| Delayed-filter component packet test (Ethernet/ARP/ND/RARP + positive controls/outbound RARP) | PASS после обработки EINTR в receiver; ещё 3/3 повтора в свежих контейнерах |
| `git diff --check`, `git diff --cached --check` | PASS, обе проверки exit 0 |

Новые regression cases:

- Новый Node без daemon и его удаление через fake Kubernetes API не меняют
  membership. NB admission на здоровом участнике проходит. Проверяется
  пустое начальное членство и удаление прежнего sessionless placeholder.
- Принудительные CAS conflicts Publish/Join сохраняют нового участника;
  InstallIntent старой revision после conflict retry отклоняется.
- Publish во время install и между ACK/pre-Open запрещает Open старой
  revision. Publish в финальном API/kernel gap блокирует новый MAC через
  production AdmitMACs до нового install+ACK.
- Зарегистрированный старый Active остаётся после публикации следующей
  revision и смены Node UID; только Closed снимает его блокировку.
- Failed close, failed install, потеря API/ConfigMap и stale session не
  вызывают Open. Новый процесс не подтверждает старой session.
- Постоянный уже допущенный MAC VM проходит admission при задержке нового
  MAC другого workload. Generation loss по-прежнему блокирует admission.

NB tests используют настоящий in-memory NB DB и Kubernetes API test double.
CAS tests используют API double с resourceVersion conflict enforcement.
Daemon Node UID test подставляет результат host Fence; реальные host lifecycle
проверяются отдельными kernel tests. Это unit/component coverage, не полный
Kubernetes/daemon/process restart или KubeVirt/Cisco E2E.

Первый packet run завершился ошибкой `unix.Poll: interrupted system call`
в ARP-подтесте. Это был сбой receiver harness; он сохранён как
`delayed-filter-packets-first-EINTR.txt`. Receiver теперь повторяет Poll/Recvfrom
при EINTR, остальные ошибки и packet assertions сохраняются. После исправления
получены PASS и три дополнительных PASS в новых disposable контейнерах;
последний make lint/package/race цикл также выполнен после этой поправки.

SHA256 всех 69 изменённых code/config/test файлов совпали с итоговой Linux
копией: `tested-source-manifest.json`, `source-verification.txt`. Сырые логи
не добавляются в Git. Логи этой задачи: ignored `docs/openperouter/results/linux-membership-20261119/`
и `/tmp/kubeovn-membership/results/` на Linux. Команды воспроизведения:
[dataplane-validation.md](dataplane-validation.md#membership-barrier-regression).

## Исторические проверки и skips

Полный make ut и двухнодовая OVN matrix 25/25 предыдущей задачи сохранены в
`results/linux-review-three-fixes-20261119/` с её source manifest. **Они не
повторялись в membership-задаче** и не выдаются за её результаты. Native
OpenPERouter kernel/BGP tests также не повторялись. Kernel/BGP opt-in tests
без своих marker/fixture в обычном go test явно SKIP; kernel regressions
запускаются отдельно в disposable containers.

## Ограничения

- Legacy attachments нужно предварительно закрыть на всех нодах до перехода
  на протокол. Незарегистрированный путь безопасен только потому, что новый
  daemon не может открыть его без JoinClosed/InstallIntent/install/ACK.
  Mixed-version rolling upgrade не реализован и не заявляется.
- Потерянный зарегистрированный Active/Pending продолжает блокировать новые
  MAC, пока не подтверждены актуальная защита или закрытие. Node deletion,
  NotReady и таймаут не являются таким подтверждением. Пустой список Members
  относится к сети без зарегистрированных путей, а не к забытым старым краям.
- MAC history сохраняется до удаления Subnet. Внешнее повторное использование
  бывшего OVN MAC этим протоколом не поддерживается. Масштаб 250/1000 нод и
  throughput CAS ACK не измерены; новая стоимость чтений описана в архитектуре.
- PE unreachable defaults — отдельная отложенная зависимость OpenPERouter.
  FRR Null0, PE netlink guards и обходные scripts не добавлялись.
- Native Type-2 endpoint lifecycle API по-прежнему отсутствует. Полные Cisco
  RT/RD/EVPN/link-local interoperability, KubeVirt migration, systemd boot/API
  outage и Kubernetes Service/NetworkPolicy E2E здесь не испытаны. Готовность
  всей интеграции и бесшовная внешняя миграция не заявляются.
