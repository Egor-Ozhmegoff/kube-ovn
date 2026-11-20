# Интеграция с OpenPERouter: выбранная версия

Выбрана ветка
[`Egor-Ozhmegoff/openperouter:Mac_setings_for_l2vni_and_l3vni`](https://github.com/Egor-Ozhmegoff/openperouter/tree/Mac_setings_for_l2vni_and_l3vni),
закреплён commit **`60a1285935b10eb0b1cf07e4533faefedde3d4d3`**.
Kube-OVN: ветка `openperouter`, база текущей доработки
`b7257133dd552bda1a05f59b2b716fadd8bf8223`.
Локальная MAC-ветка OpenPERouter проверена без переключения его рабочей копии.

**Добавлен экспериментальный runtime Kube-OVN**, API/CRD, apply configurations,
валидация, opt-in Helm и тесты. По умолчанию режим выключен. Обязательные
гарантии dataplane пока не подтверждены; fail-closed при пересоздании native
PE VRF имеет конкретный пробел в API/реализации выбранной версии.

Текущий datapath, владельцы, lifecycle, ограничения Services/L2 и оценка
250/1000 нод: [implementation.md](implementation.md).
Результаты проверок: [validation-results.md](validation-results.md).
Предыдущая доработка разделила Fence/Cleanup и добавила распределённый допуск
нового MAC до активации endpoint и безопасное повторное использование L2VNI.
Текущая доработка membership убирает зависимость admission от регистрации
Kubernetes Nodes: участники вступают только через daemon после закрытия пути.
Проверки предыдущей доработки включали repository suite, OVS/kernel regressions,
delayed-filter packet test и двухнодовый OVN component lab. Предыдущие native
OpenPERouter kernel и host-VRF BGP tests привязаны к своим исходникам; они
не считаются повтором текущей задачи. Актуальные результаты и skips —
в validation-results.md.

## Подтверждённые изменения MAC

| Требование | Настройка | Реализация выбранной ветки |
| --- | --- | --- |
| Общий anycast MAC для L2VNI | `L2VNISpec.gatewayMAC: '02:00:05:00:03:00'` вместе с gatewayIPs | CRD/CEL и webhook validation → conversion → `L2VNIParams.GatewayMAC` → `SetupL2VNI` → Linux bridge MAC |
| Один RMAC всех L3VNI ноды | `routerMAC` в локальном node-config.yaml или CLI `--router-mac` | startup config → `KernelDatapathConfigurator.RouterMAC` → все L3VNIParams → `l3VNIBridgeOptions` → Linux bridge MAC |
| Разные RMAC между нодами | Различные routerMAC в node-config.yaml | Значения задаёт Ansible; проверки глобальной уникальности в OpenPERouter нет |
| Восстановление MAC | Штатный reconciler OpenPERouter | Повторно применяет MAC через bridge options; в исходниках есть Linux-тест drift/recreation. 70 native kernel specs выбранного SHA прошли на Linux |

Gateway MAC immutable: добавление, изменение и удаление поля существующего
L2VNI запрещены. Нужен план пересоздания при переходе с прежнего значения.
Router MAC поддерживается **только kernel datapath**, не grout. Node-config
читается при старте; изменение routerMAC считать требующим рестарта controller,
а не обещать hot reload. Изменение активного MAC и EVPN convergence нужно
испытывать отдельно.

## Артефакты

* [Двухнодовый пример](examples/README.md): один VRF/L3VNI, два L2VNI с общим
  gateway MAC, разные routerMAC нод, отдельная Pod-сеть без L2VNI. Добавлены Kube-OVN manifests и Helm overrides для disposable lab.
* [Подробная сверка MAC-ветки](mac-settings-review.md): пути API/reconciler,
  ограничения и результаты проверок.
* [Datapath, владельцы, lifecycle и расчёт масштаба](implementation.md).
* [План 13 dataplane-проверок](dataplane-validation.md): критерии полной приёмки.
* [Последние результаты Linux-проверок](validation-results.md): логи, PCAP,
  текущие блокеры и непроверенные сценарии.

## Воспроизводимый source audit

```sh
curl -fL https://codeload.github.com/Egor-Ozhmegoff/openperouter/tar.gz/60a1285935b10eb0b1cf07e4533faefedde3d4d3 -o /tmp/openperouter-mac-audit.tar.gz
mkdir -p /tmp/openperouter-mac-audit
tar -xzf /tmp/openperouter-mac-audit.tar.gz --strip-components=1 -C /tmp/openperouter-mac-audit
python3 hack/check-openperouter.py /tmp/openperouter-mac-audit
```

Профиль по умолчанию теперь `mac-settings`. Ожидается exit **0** и
`status: api-reviewed`, `blockers: []`, `dataplaneTested: false`.
Это успешное воспроизведение source review, а не готовность dataplane.
Отдельное поле `integrationBlockers` фиксирует пробелы fail-closed lifecycle
и удаления Type-2; пустые MAC API `blockers` не означают готовую интеграцию.
Сохраняются явные profiles `--profile upstream` и `--profile egor-fork` для
исторических commits; они возвращают exit 2 с соответствующими блокерами.
Exit 1 — отсутствующие/отличающиеся bytes, требующие новой проверки.

`mac-settings-evidence.json` фиксирует SHA-256 исследованных API, CRD,
conversion, startup, reconciler и тестов. Аудит ничего не развёртывает и
не обращается к Kubernetes API или Linux networking.

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s test/unittest -p 'test_openperouter_audit.py' -v
```

## Проверки и границы готовности

См. [результаты](validation-results.md) и [13 сценариев](dataplane-validation.md).
Go и Helm скачаны напрямую в `/private/tmp` с проверкой SHA256; системный Go
и настройки proxy не менялись. Source audit не является dataplane test.

Отложенная зависимость: kernel defaults при native PE VRF recreate. Она
не исправляется в Kube-OVN. Native Type-2 lifecycle API отсутствует;
допуск новых MAC теперь ждёт подтверждения host reflection guards; throughput при churn требует измерений. Cisco, Kubernetes
Service/NetworkPolicy и KubeVirt migration требуют полной E2E проверки.
Минимальные необходимые изменения native owner перечислены в
[implementation.md](implementation.md#блокеры-и-неподтверждённые-свойства).
Deployment, изменения действующих нод, commit и push не выполнялись.
