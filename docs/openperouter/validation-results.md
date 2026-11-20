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
