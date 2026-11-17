# Результаты Linux-проверок, 2026-11-16

Проверки исполнены на разрешённом хосте `10.64.244.143` в новых изолированных
контейнерах. Ветка `openperouter`, исходный HEAD
`8bd912fefaa0a497384bde2ad2c42906e3a14c9c`; изменения остаются незакоммиченными.
OpenPERouter без изменений: ветка `Mac_setings_for_l2vni_and_l3vni`, SHA
`60a1285935b10eb0b1cf07e4533faefedde3d4d3`.

**Цикл проверок репозитория завершён. Есть проваленный dataplane-тест
и ошибка `make lint`; интеграция не готова к полной приёмке.**
Основные результаты: полный `make ut`, сборка Linux binaries, race tests,
70 native kernel specs OpenPERouter и реальная GoBGP↔FRR session проходят.
В двухнодовом OVN component lab проходят 16/17 packet checks; отражённый
IPv6 NA обходит L2 reflection ACL. Полная Cisco/Kubernetes/KubeVirt E2E
приёмка не выполнена.

## Изменение после Linux-прогона

По решению о владении fail-closed удалены FRR Null0 defaults из
`examples/openpe_fabric.yaml`, их разбор и обязательная проверка в Kube-OVN
bootstrap validator. Защитные IPv4/IPv6 defaults должны обеспечиваться
kernel reconciler OpenPERouter; привязка VRF к L3VNI сохраняется.
После изменения выполнен `go test ./pkg/openperouter -count=1` — PASS.
Полный Linux-прогон ниже относится к зафиксированному manifest до этого
изменения. Блокер native VRF lifecycle остаётся открытым.

## Среда и артефакты

Host Ubuntu 24.04.5, kernel `6.8.0-147-generic`, x86_64, 4 CPU / 8 GB RAM.
Disposable worker image `kubeovn-openperouter-full-checks:20261116`, ID
`43e7d6fefd46`: Ubuntu 26.04, OVS 3.7.1, OVN 26.03.0.
Go 1.27.4 linux/amd64, golangci-lint 2.15.0, Helm 4.3.2; checksums загрузок
проверены. Go загружен напрямую с очищенными proxy env; модули используют
`https://proxy.golang.org,direct`. Системные инструменты host не заменены.

Итоговый прогон: **12:50:45–12:57:13 UTC, 2026-11-16**.
Код на Linux сверен с локальной рабочей веткой по SHA256.
Manifest проверенных файлов
фиксирует состояние на момент испытаний; последующие редакционные изменения
документации в него не входят.
Итоговые exit codes
содержат время начала и завершения каждой проверки.
Сырые логи, manifests, host snapshots и PCAP не включаются в Git.
Локальная копия хранится в ignored `docs/openperouter/results/`.
Полные raw logs, coverage и captures сохранены на Linux в
`/tmp/kubeovn-openperouter-full/results/`, source — в соседнем `source/`.

Пользователь разрешил остановку существующих контейнеров. Остановлены 39,
не удалены; inventory/restart policies сохранены в
`/tmp/kubeovn-openperouter-stop.rUcUih`. Старые контейнеры не перенастраивались.
После испытаний тестовые контейнеры и сеть удалены. На контрольной точке
12:57:56 UTC работающих Docker-контейнеров — **0**, доступная память —
**7.1 GiB**; прежние 39 контейнеров остаются остановленными.
Состояние хоста.
Commit, push и deployment
интеграции на существующие ноды не выполнялись.

## Repository checks

| Проверка | Результат |
| --- | --- |
| Полный `make ut` (cmd/pkg и natgateway shell tests) | PASS, 37 пакетов с выполненными тестами |
| Race: OpenPERouter, OVSDB table, IPsec, controller IPsec | PASS |
| Шесть CI shell/Python contracts: IPsec container, CNI security, install syntax, OVN DB mode, egress init и primary CNI | PASS |
| `make gen-crd`, повторная генерация, SHA stability четырёх outputs | PASS |
| `make lint` | FAIL: verify-crd сравнивает intentional generated diff с HEAD и требует commit |
| Отдельный golangci-lint для всего Linux tree | PASS, 0 issues |
| `modernize -test` | PASS |
| `make build-go` | PASS, Linux binaries; tidy классифицировал уже используемый google/uuid как direct dependency |
| Семь Python source-audit / Helm behavior tests | PASS |
| Helm обоих charts: defaults, enabled, render/actual VXLAN args | PASS; исходные SemVer/icon warnings |
| Kube-OVN example structural schemas | PASS; CEL/webhook runtime этим не проверяется |
| `git diff --check` | PASS |

`verify-crd` не обходился изменением index/HEAD; commit запрещён задачей.
Отдельный lint выполнен с
`GOGC=10 GOMEMLIMIT=3GiB GOMAXPROCS=1 GOFLAGS=-p=1 --concurrency=1`
и завершился без замечаний. Ошибка `make lint` остаётся.

Основные журналы: unit tests,
race,
make lint,
отдельный lint,
сборка,
схемы примера.

## Реальные сетевые component tests

**Native OpenPERouter:** неизменённый выбранный SHA, binary с `runasroot`,
70/70 specs, 0 failures / 0 skips. Реальные kernel/netlink/OVS tests проверяют
shared L3 RMAC, L2 gateway MAC, drift/recreate, VNI/veth/MTU lifecycle и
hostMaster OVS. Журнал 70 тестов.
Это не запуск полного OpenPERouter/FRR EVPN reconciler.

**GoBGP ↔ FRR 10.6.0:** реальная TCP BGP session между host VRF и отдельным
PE netns. /32 виден в FRR RIB; внешний /24 получен в Adj-RIB-In, отсутствует
в global GoBGP RIB. Withdraw виден в FRR; после cold restart и повторного
Established старые workload paths не появились. Запуск:
`hack/openperouter/bgp-session.sh` с Linux test binary advertiser.test.
Журнал BGP-сессии.

**Linux VRF isolation:** `hack/test-openperouter-vrf-fallback.sh` воспроизводит
main-table escape без terminal default: 2 tenant ICMP packets на management
endpoint; с unreachable default — 0. Это не воспроизведение точного окна
native PE restart, а доказательство необходимости fail-closed contract.
Результат.

**Systemd bootstrap:** native Podman Quadlet generator с pinned units и
`controller.container.d/10-kubeovn.conf` успешно генерирует service с
согласованным `/etc/openperouter` configuration directory. Фактический boot
и EVPN Established без Kubernetes API не испытаны.

**Два OVN chassis, реальные packet captures:** fixture вызывает текущие
`EnsureTransit`/`EnsureL2`; OVN сохраняет внутренний VXLAN. В этом lab нет
полного native PE/Cisco, внешние L2/return legs синтетические.

| Проверка | Результат |
| --- | --- |
| Same-VRF IPv4 и IPv6/ND через внутренний VXLAN | PASS |
| Local egress каждой ноды | PASS: local 2 packets, соседний egress 0, tunnel egress 0 |
| Return delivery к remote workload, IPv4 и IPv6 | PASS, без повторного egress |
| Возвратный external destination, IPv4 и IPv6 | PASS, drop |
| BUM: один fabric copy у origin, ноль у remote node; external BUM local-only | PASS |
| Отражённый известный workload MAC, custom EtherType | PASS, drop |
| Отражённый IPv6 NA от того же MAC | **FAIL: remote workload получил 1 packet** |
| Workload MTU 1400 IPv4 и IPv6 | PASS; полный внешний tunnel PMTUD не проверен |
| OVN LB VIP → remote HTTP backend, ACL deny/allow VIP | PASS; не Kubernetes controller E2E |
| ovn-controller restart | PASS, внутренняя связность восстановлена |

Провал ND не исключён и не заменён слабым тестом. OVN `build_acls()`
устанавливает reserved `IPV6_CT_OMIT_MATCH` allow priority 65532 выше user
ACL во всех stages. Проба `apply-after-lb` тоже провалилась; настройка
откачена. Нужен Ethernet ingress filter до OVN ACL либо upstream API.
Точные источники и другие блокеры: [implementation.md](implementation.md).

В процессе packet tests исправлены небезопасные negative-inport и mixed
IPv4/IPv6 return ACL: положительный return Port_Group и family-specific
allowlists прошли повторные реальные injection tests. Helm теперь проверяет
фактический `TUNNEL_TYPE=vxlan`; NETWORK_TYPE задаёт overlay/vlan mode.

Полные 17 результатов,
журнал packet tests и
PCAP сохранены отдельно от исходников, в каталоге результатов на Linux.

## Текущие блокеры

| Блокер | Подтверждение и необходимое действие |
| --- | --- |
| IPv6 ND split-horizon | Реальный отражённый NA доставлен remote workload. Нужен Ethernet ingress filter до OVN ACL либо upstream API; исправление пока отсутствует. |
| Fail-closed при пересоздании native PE VRF | В выбранном OpenPERouter нет гарантии установки terminal IPv4/IPv6 defaults до открытия ingress. Нужен контракт у native владельца и fault/restart test. |
| Удаление и перенос Type-2 endpoint | Native L2VNI API не предоставляет endpoint lease/delete. Нужно подтвердить aging/mobility; для гарантированного срока отзыва требуется расширение native API. |

Проверена история [upstream #227](https://github.com/openperouter/openperouter/issues/227):
закрывший его PR #242 впоследствии откачен commit
`73c34a2b13ff25f20cecff505d72c51ca6041450` из-за SRv6;
[продолжение #516](https://github.com/openperouter/openperouter/issues/516) открыто.
В выбранной MAC-ветке исправления нет; статус native fail-closed не меняется.

`make lint/verify-crd` — отдельная ошибка проверки незакоммиченных CRD,
а не исправленный архитектурный блокер. Точные пути кода и минимальные
изменения приведены в [implementation.md](implementation.md#блокеры-и-неподтверждённые-свойства).

## Покрытие исходной 13-пунктной приёмки

| № | Что подтверждено / что осталось |
| --- | --- |
| 1–2 | Локальный OVN handoff без соседнего gateway подтверждён captures; полный PE→Cisco путь не испытан |
| 3 | OVN return injection подтверждён; fabric EVPN reverse route не испытан |
| 4 | Внешний ECMP не испытан |
| 5 | Host VRF terminal-drop component подтверждён; native PE withdrawal/restart/GR/BFD timing не испытан |
| 6 | Same-VRF внутренний VXLAN IPv4/IPv6 подтверждён |
| 7 | Cisco inter-VRF policy не испытана |
| 8 | BUM/custom reflection подтверждены; IPv6 ND reflection FAIL, native Type-2 export/delete не испытаны |
| 9 | KubeVirt live migration не испытана: нет KVM/runtime |
| 10 | Native kernel recreation specs, BGP cold restart, ovn-controller restart подтверждены; full daemon/router namespace reboot не испытан |
| 11 | Quadlet generation и static bootstrap validation подтверждены; boot/API outage с EVPN session не испытаны |
| 12 | Внутренний MTU 1400 подтверждён; внешние encapsulation/PMTUD не испытаны |
| 13 | OVN LB/ACL component подтверждены; Kubernetes Services/NetworkPolicy controller E2E не испытаны |

На хосте нет Cisco image и `/dev/kvm`. FRR component test не доказывает
Cisco interoperability. Расчёты 250/1000 нод в implementation.md — модель,
нагрузочный тест не проводился. Режим остаётся экспериментальным; ND
split-horizon, native fail-closed lifecycle и Type-2 cleanup блокируют готовность.

## Воспроизведение

Tool image собирается из `hack/openperouter/Dockerfile`. В disposable Linux
worker source монтируется в `/work`, verified tools в `/opt/checktools`,
отдельный run root в `/runroot`; repository runner:
`hack/openperouter/repository-suite.sh`. Просматривать все exit codes в TSV:
runner продолжает сбор доказательств после отдельных ошибок.

Пакетный lab:

```sh
CGO_ENABLED=0 go build -tags openperouterlab -o "$RUN_ROOT/dataplane-helper" ./test/integration/openperouter/fixture
sudo KUBE_OVN_OPENPEROUTER_RUN_ROOT="$RUN_ROOT" python3 hack/openperouter/dataplane.py
```

`RUN_ROOT` должен содержать `source/`, helper и `results/`; tool image tag
зафиксирован в runner. Нужен отдельный disposable host с Docker/privileges.
Не запускать на действующем кластере. Для native tests дополнительно собрать
pinned upstream `internal/hostnetwork` с `-tags runasroot`, затем использовать
`hack/openperouter/native-hostnetwork.sh`. Для BGP binary:
`CGO_ENABLED=0 go test -c -o "$RUN_ROOT/advertiser.test" ./pkg/openperouter/advertiser`.
Runners используют новые namespaces/контейнеры; они не устанавливают ручные
маршруты/FDB в managed OpenPERouter namespace действующей интеграции.
