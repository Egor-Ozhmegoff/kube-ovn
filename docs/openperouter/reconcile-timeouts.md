# Бюджеты reconciliation и изоляция VPC

В `openperouter@1af7a97729db` общий 4s context охватывал подготовку и все VPC.
Закрытие B использовало отдельные 3s, но общий deadline продолжал истекать.
Возвращённый `ctx.Err()` запускал общий Fence/Sweep, включая исправный A.

## Контексты

| Этап | Родитель | Ограничение | Последствие ошибки |
| --- | --- | --- | --- |
| Daemon | stop channel | До остановки | Прекратить обычную обработку, финальная cleanup |
| Подготовка: API, Sweep, host Cleanup, guards, SB/OVS | Daemon | 4s | Общий Fence/Closed/Sweep |
| Свежий общий snapshot перед VPC и независимая проверка после timeout | Daemon | 4s каждый | Общий Fence/Closed/Sweep |
| Один VPC: attachment, subnet routes, L2 guards/Open | Daemon | 4s каждый | Закрыть только его UID |
| Закрытие одного UID | WithoutCancel daemon | 3s каждый | Логировать и повторять; остальные VPC продолжают |
| Общая/final cleanup | WithoutCancel daemon | 3s | Логировать реальную ошибку, не утверждать успешный отзыв |

Начальное fencing и очистка legacy routes имеют отдельный startup budget 10s.
Контекст общей подготовки отменяется до обработки VPC и не является её родителем.
Бюджеты внутренние; новых flags/values нет. Последовательная обработка сохранена.

Fence получает не более половины оставшегося closure budget, максимум 1.5s.
Closed публикуется только после успешного Fence. Withdrawal вызывается независимо
от ошибок Fence/ACK и использует оставшийся исходный closure context. Это резерв
для попытки withdrawal, а не гарантия её успеха при недоступном kernel/API.
Failed closure UID хранится для повторной попытки; до её успеха обычный reconcile
этого UID пропускается. После restart startup fencing повторяет общую очистку.

## Свежесть и область отказа

Перед каждым VPC заново читаются VPC/Subnet, bootstrap/Node identity, живые
SB bindings и установленные OVS ports; проверяется доступность PE namespace.
Изменение конфигурации между подготовкой и обработкой не маскируется прежним
успехом. Перед L3 Open повторяется полный snapshot и сравниваются UID/Spec,
готовность/удаление Subnet, Node/bootstrap и поколение источника SB. Изменение
только resourceVersion или счётчиков IP не закрывает неизменившуюся сеть.

Перед каждым L2 Open внутри barrier callback повторяется живое чтение SB/OVS
и проверяется return binding/installed port. Также проверяются namespace inode,
VRF/table/veth/ifindex/address generation и host attachment ownership. Barrier
session/revision и установка актуального reflection filter остаются обязательными.
Полные API Lists не выполняются для каждого L2VNI: конфигурация перепроверена
перед L3 Open в этой же ограниченной попытке. Это не атомарный snapshot API,
OVS и Linux; внешнее изменение после последней проверки остаётся возможным.

Общие API/SB/OVS ошибки и потеря/замена PE namespace запускают общий fail-closed
путь. Отсутствие конкретного return port/VRF/veth, local route/guard error и
timeout VPC закрывают только его UID. Если общий read вернул deadline/cancellation
(включая timeout до исчерпания всего VPC budget), после scoped closure выполняется
независимая проверка общего состояния.
Она отличает исчерпание бюджета B от продолжающегося общего сбоя. Известная
ошибка общего поколения не становится локальной из-за совпавшего timeout.

## Подписки и ошибки reflection

Идентичность источника — настроенный OVN endpoint, имя Chassis, UUID реального
Chassis и поколение соединения. Адрес Go-объекта не является идентичностью OVN.
Изменение только guardNames создаёт новый reader, не закрывая старый до успешной
инициализации нового. До и после создания проверяется живое старое соединение
(Echo, Chassis UUID). Только при том же endpoint/name/UUID новый reader наследует
поколение; иначе snapshot отклоняется как общий сбой. Неуспешная проверка старого
reader удаляет его из live reference, чтобы следующий цикл мог восстановиться.

Snapshot хранит значение поколения, а не указатель reader. После проверки
неизменности конфигурации текущего VPC и свежего return binding L3 проверка
принимает новый snapshot/guardNames. Последующие MAC reads используют live
reference reader; закрытая копия в аргументах VPC не сохраняется. Перед L2 Open
повторно проверяются поколение и живые SB/OVS данные. Добавление/удаление сети B
не закрывает исправный A. Изменение сети самого A по-прежнему отклоняет попытку A.

| Источник ошибки | Область и действие |
| --- | --- |
| VPC/Subnet Lists, Node/bootstrap identity, SB connection/monitor/Echo, Chassis Get/UUID, OVS rows/appctl | Общая инфраструктура: общий Fence/Closed/Sweep; deadline чтения сначала проходит независимую проверку |
| Новый endpoint/chassis/неплановое поколение reader | Общий отказ перед Open; плановая смена подписок при доказанной живости и той же идентичности не относится к нему |
| AddressSet Get: отсутствует конкретная подписанная строка или подписка на неё | Локальная `ErrReflectionAddressSetNotFound`, также совместимая с `table.ErrNotFound`: закрыть VPC UID, guards Closed только после Fence, withdrawal/retry |
| Отсутствует Chassis при проверке ReflectionMACs | Общий сбой, даже если исходная ошибка также `table.ErrNotFound` |
| Barrier ConfigMap/session/revision/digest/ACK, nft/host attachment, return binding/installed VIF, PE VRF/veth/routes/ownership | Ошибка конкретного VPC: scoped close/withdraw/retry; ошибки общей подготовки/cleanup не переводятся в этот scope |
| PE namespace потерян/заменён | Общий отказ; потеря отдельного VRF/veth остаётся локальной |
| Ошибка закрытия/Closed ACK/withdrawal | Сохранить UID для retry; попытаться выполнить withdrawal независимо от Fence/ACK; не закрывать другие VPC только из-за этой ошибки |

Перед чтением MAC выполняется проверка живого SB соединения и Chassis. Отсутствие
AddressSet не превращается в пустой MAC-набор: ProtectAndOpen не вызывается.
Подтверждённый существующий пустой AddressSet по-прежнему отличается от missing.
После появления строки обычный reconcile может восстановить защиту и пути.
Guard ConfigMap error сам по себе не доказывает недоступность всех API ресурсов;
общие API snapshots проверяются независимо. После ambiguous shared timeout
успешный probe разрешает дальнейшую обработку других VPC, но не открывает
провалившийся VPC без новой успешной попытки.

OVS appctl, initial OVSDB connection и monitor теперь наследуют контекст
операции. Netlink сохраняет свои socket/context проверки; deadline не может
принудительно прервать произвольный синхронный syscall. Время цикла растёт с
числом VPC и retries; 5s ticker не означает завершение цикла за 5s. Стоимость
дополнительных API/SB/OVS snapshots под нагрузкой этим исправлением не измерена.

При stop отменяются подготовка/обычный VPC reconcile, следующий VPC не запускается.
Финальный Fence/Closed/Sweep получает независимый context даже после отмены stop.
SIGKILL не выполняет final cleanup; kernel subnet routes сохраняются до restart
или внешнего withdrawal механизма. Новая endpoint lease не вводится.

Routed subnet protocol 203, исключение OpenPERouterL2 и host protocol 202 не
изменены. Per-endpoint routes и локальный BGP не возвращаются. PE defaults,
native attachment-only lifecycle и внешний FRR export остаются отдельными
контрактами, описанными в [netlink-routes.md](netlink-routes.md).
