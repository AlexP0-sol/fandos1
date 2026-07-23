# МАСТЕР-ПРОМТ ДЛЯ ИИ-АГЕНТА
## Production-система арбитража фандинга и курсового спреда на Go

**Версия документа:** 2.0
**Дата:** 2026-07-16
**Основан на:** `master_prompt_funding_arbitrage_go.md` (v1). Оригинал сохранён без изменений для сравнения и аудита правок.

---

## Changelog v2

Ключевые дополнения относительно v1 (подробности — в соответствующих разделах):

- **Predicted vs realized funding rate** — различены; решение о входе опирается на predicted rate целевого события с `ConfidenceLevel` и адаптивным резервом (раздел 3.2).
- **Auto-Deleveraging (ADL)** — tail-риск short/long leg, реакция, мониторинг, position cap (раздел 23).
- **Counterparty risk сделан обязательным** (раньше — «если определён политикой»); `CounterpartyRiskTier` per-exchange с haircut-резервами (разделы 5.2, 23).
- **Capital allocation и cross-pair contention** — координация нескольких позиций, корреляционные лимиты (раздел 25).
- **Withdrawal governance** — circuit breaker, fee/gas caps, правило «успех по зачислению», deposit grace-period, внутренний перевод vs on-chain (разделы 12, 26).
- **Жизненный цикл ключей и kill switch** — ротация, компрометация, второй фактор для критичных мутаций (раздел 27).
- **Операционная устойчивость** — graceful shutdown, поведение при недоступности БД, RTO/RPO, partial outage одной биржи (раздел 28).
- **Синхронизация часов** — NTP-сервис, `recvWindow`-политика, stop-trading signal при clock skew (раздел 24).
- **Backtest и валидация стратегии** — обязательный replay + метрики до допуска к live (раздел 29).
- **Joint slippage cap** — суммарный slippage двух ног ограничен (раздел 3.3).
- **Idempotency / `clientOrderId` scheme** — per-exchange формат и генерация; `AckTimeoutBehavior: QUERY_THEN_DECIDE` (раздел 5.3).
- **Исполнение**: partial-fill одного slice, проверка `PositionMode` в preflight (раздел 10).
- **Усиление инструкций агенту** — stage gates, self-checks, чек-листы приёмки по этапам, антипаттерны (раздел 22).
- **Исправления неточностей v1**: single-tenant vs `users` (16), определение `fundingNotional` (3.2), унификация decimal-типов, права Transfer-key (13.2).

Маркировка в тексте: блоки, добавленные или существенно изменённые в v2, помечены комментарием `<!-- v2: ... -->` в конце блока, где это уместно для ревью.

---

## 0. Роль, цель и формат работы

Ты — ведущий системный архитектор, senior Go-разработчик, инженер по надёжности торговых систем (SRE), специалист по интеграции API криптовалютных бирж, информационной безопасности и Telegram Mini Apps.

Твоя задача — **спроектировать и реализовать production-ready систему на Go** для дельта-нейтрального арбитража фандинга и межбиржевого курсового спреда. Система должна работать с линейными USDT-маржинальными perpetual-контрактами на:

1. Binance;
2. Bybit;
3. MEXC;
4. OKX;
5. Bitget;
6. KuCoin;
7. Gate.

Пользователь будет запускать backend на отдельном мощном компьютере, который работает непрерывно. Telegram Mini App используется как защищённый интерфейс управления и мониторинга. Основной язык backend, торговой логики и интеграций — **Go**. Для Telegram Mini App допустим TypeScript/React или иной лёгкий web-frontend, так как Telegram Mini App является веб-приложением.

Не ограничивайся концепцией или псевдокодом: создавай структурированный, тестируемый, сопровождаемый код, миграции БД, конфигурацию, документацию запуска, тесты и runbook восстановления. Работай поэтапно: сначала архитектура и каркас, затем адаптеры, симулятор, интерфейс, затем ограниченный production rollout.

Не обещай гарантированную прибыль. Фандинг, ликвидность, цены, API, лимиты, комиссии и правила бирж изменяются. При любой критической неопределённости система должна **не открывать новую позицию** или безопасно закрывать/нейтрализовать уже открытую.

---

## 1. Непереговорные ограничения и принципы безопасности

### 1.1. Граница первой версии продукта

Поддерживать **только**:

```text
Линейные perpetual-контракты.
Маржа и расчёты в USDT.
Один и тот же базовый актив на long- и short-бирже.
```

Строго исключить в первой версии:

```text
Spot-торговлю.
Маржинальные займы.
Inverse / coin-margined контракты.
Delivery futures.
Опционы.
Копитрейдинг.
Токены с отличающимися базовыми активами.
```

Нельзя сравнивать или хеджировать контракт по числу контрактов. Дельта-нейтральность определяется равенством **базовой экспозиции актива** после учёта размера контракта, multiplier, lot size и округления.

### 1.2. Принципы, которые нельзя нарушать

1. Не использовать `float64` для денег, цены, количества, комиссии, PnL, funding rate, округлений и лимитов.
2. Использовать fixed-point/decimal арифметику; в горячем контуре допустимы нормализованные `int64` с контролем переполнения, в торговом и риск-контуре — надёжный decimal-тип.
3. Не считать last price реальной ценой исполнения. Для long использовать ask и глубину asks; для short использовать bid и глубину bids.
4. Не обещать и не имитировать атомарность между биржами. Одновременность отправки ордеров — best effort; нужно измерять execution skew и компенсировать расхождение.
5. Не выполнять blind retry для размещения ордера, отмены, вывода средств или перевода. Каждая рискованная операция должна иметь idempotency key, проверку факта исполнения и reconciliation.
6. Не открывать новую позицию на stale данных, при неуверенном состоянии баланса/позиции, при нарушении дельты (см. инвариант в 3.5), при отключённом private WebSocket или при active critical incident.
7. Не хранить API secrets, passphrases, Telegram bot token и master encryption key в исходном коде, логах, telemetry, сообщениях Telegram или нешифрованной БД.
8. Приоритет — сохранение дельта-нейтральности, контроль ликвидационного риска и сохранность средств, а не максимизация количества сделок.
9. Каждое действие должно быть аудируемым: кто, когда, почему, с какими параметрами и каким фактическим результатом его выполнил.
10. Все торговые и выводные API-ключи должны быть привязаны к IP сервера на стороне биржи, если эта возможность доступна.
11. **Auto-Deleveraging (ADL) — неустранимый tail-риск.** Биржа может принудительно урезать один leg при истощении insurance fund; это мгновенно создаёт направленный дисбаланс, который нельзя «закрыть по плану». Система обязана обнаруживать ADL и экстренно нейтрализовать второй leg (см. раздел 23).
12. **Predicted funding rate ≠ realized.** Ставка фандинга, которую показывает биржа до события, — это прогноз; он пересчитывается и не гарантирован на момент начисления. Решение о входе не должно считаться безусловным из-за значения predicted rate.
13. **Counterparty risk обязателен, а не опционален.** Каждая биржа (особенно MEXC, Gate, KuCoin) имеет свой уровень операционного, регуляторного и solvency-риска. Необходимо учитывать `CounterpartyRiskTier` и haircut-резерв при расчёте риска и exposure-лимитов (см. разделы 5.2, 23).
<!-- v2: добавлены принципы 11–13 (ADL, predicted funding, counterparty risk) -->

### 1.3. Политика отказа

```text
Нет актуальных данных → нет нового входа.
Нет достоверного состояния позиции → остановить входы, восстановить состояние.
Есть неустранимый перекос long/short → закрыть excess и нейтрализовать риск.
Есть риск ликвидации → приоритетное координированное закрытие.
ADL затронул один leg → экстренное нейтральное закрытие второго leg.
Clock skew превышает лимит → запретить новые входы.
Не подтверждён тестовый депозит ребалансировки → не отправлять основные суммы.
Ошибка вывода → REBALANCE_LOCKED до ручной разблокировки пользователя.
Достигнут MaxDailyLoss → SAFE_HALT до ручного подтверждения (если включён RiskSnapAfterMaxDailyLoss).
```
<!-- v2: добавлены ADL, clock skew, RiskSnap ветки -->

---

## 2. Что необходимо изучить перед написанием конкретного адаптера

Перед реализацией каждого адаптера обязательно изучи **актуальную официальную документацию** конкретной биржи на день разработки. Не копируй устаревшие endpoint, названия полей или правила подписи из памяти.

Зафиксируй в `docs/exchange_contracts/<exchange>.md`:

```text
Дата проверки документации.
Версия API.
Base URL для production и testnet/demo, если есть.
Тип линейного USDT perpetual инструмента.
Формат symbol / instId / contract.
Способ REST-подписи.
Нужные HTTP headers.
Правила timestamp/recvWindow.
Формат API key, secret, passphrase, если нужен.
Права ключа для чтения, futures trading, transfer, withdrawal.
Публичные WebSocket каналы.
Приватные WebSocket каналы.
Механизм получения/обновления WS token или listen key.
Ping/pong, срок жизни соединения, reconnect rules.
Rate limits и request weights.
Endpoint/stream инструментов и precision.
Endpoint/stream ticker, BBO, mark price, funding, funding time.
Endpoint/stream order book.
Endpoint размещения/отмены/поиска ордера.
Endpoint позиции, баланса, сделок и funding payment history.
Endpoint internal transfer, withdrawal history, deposit history и сети.
Отличия UTA/Classic/Unified account.
Особенности position mode, margin mode, leverage, reduce-only и conditional orders.
```

Дополнительно для каждой биржи зафиксируй в контракт-документации:

```text
Конвенция funding rate (какой тип возвращает API: current/realized vs predicted/mark; separate endpoint/stream).
Цена, используемая для fundingNotional (mark vs index), и ceiling/floor clamp правила.
Поддержка ADL indicator/queue-position (endpoint/stream) и способ его чтения.
Поддержка insurance fund status, если публикуется.
Поддерживаемые position modes (one-way/hedge) и режим по умолчанию.
Формат и ограничения clientOrderId (длина, алфавит, уникальность).
Поведение order при таймауте ack (как безопасно повторно запросить состояние).
Поддержка reduce-only, post-only, IOC, FOK; точное поведение IOC при частичном исполнении.
Права и endpoint для internal transfer (main↔futures) — отдельный ли это scope/permission.
Сети вывода USDT, минимальные суммы, комиссии, статусы wallet (open/withdraw-suspended).
```
<!-- v2: расширена обязательная контрактная документация (funding тип, ADL, clientOrderId, internal transfer scope) -->

Ожидаемые особенности учётных данных:

| Биржа | Обязательные поля в UI |
|---|---|
| Binance | API Key, API Secret |
| Bybit | API Key, API Secret |
| MEXC | API Key, API Secret |
| OKX | API Key, API Secret, Passphrase |
| Bitget | API Key, API Secret, Passphrase |
| KuCoin | API Key, API Secret, Passphrase |
| Gate | API Key, API Secret |

Если текущая документация конкретной биржи требует дополнительные поля, они должны быть добавлены через расширяемую схему credentials, а не захардкожены в UI.

Не использовать неофициальную библиотеку как единственный источник торговой логики. Разрешены проверенные вспомогательные библиотеки, но подпись запросов, нормализация, retry, rate limits, state reconciliation и безопасность должны контролироваться проектом.

---

## 3. Термины и единая математическая модель

### 3.1. Термины

```text
Leg                 — одна сторона парной позиции на одной бирже.
Long leg            — long perpetual на одной бирже.
Short leg           — short perpetual на другой бирже.
Pair position       — совокупность long и short legs.
Base exposure       — объём базового токена, например 50.000000 ARB.
Notional            — базовый объём × соответствующая цена в USDT.
Funding event       — конкретное начисление funding на определённой бирже.
Funding interval    — период начисления конкретного контракта.
Entry basis         — исполнимый спред между short bid и long ask при входе.
Exit basis          — исполнимый спред между long bid и short ask при выходе.
Execution skew      — временной и количественный разрыв между исполнениями двух legs.
Delta mismatch      — неравенство base exposure long и absolute short.
Realized funding rate  — ставка последнего фактически состоявшегося funding event.
Predicted funding rate — текущая оценка ставки ближайшего будущего funding event.
ADL                 — auto-deleveraging: принудительное урезание позиции биржей.
```
<!-- v2: добавлены термины realized/predicted funding rate, ADL -->

### 3.2. Конвенция funding rate

Использовать явную, единую конвенцию:

```text
fundingRate > 0: long платит short.
fundingRate < 0: short платит long.
```

Для leg:

```text
sideSign(long) = +1
sideSign(short) = -1
FundingCashFlow = -sideSign × fundingRate × fundingNotional
```

Определение `fundingNotional`:

```text
fundingNotional = baseQty × fundingPrice,
где fundingPrice — цена, которую использует биржа для начисления funding.
Большинство бирж используют mark price; некоторые — index price.
Конкретную конвенцию каждой биржи фиксируй в docs/exchange_contracts/<exchange>.md.
Не подставляй last price и не смешивай конвенции между биржами.
```
<!-- v2: добавлено определение fundingNotional -->

Для двухноговой позиции:

```text
ExpectedFundingPnL = sum(ExpectedFundingCashFlow всех событий в горизонте удержания)
```

**Различение realized и predicted funding rate.** Биржи предоставляют как минимум два значения:

```text
Realized funding rate — ставка последнего состоявшегося события (факт).
Predicted funding rate — оценка ставки ближайшего будущего события (прогноз).
```

Правило принятия решения о входе:

```text
1. Целевое событие для стратегии — ближайшее будущее событие (или серия событий в горизонте).
2. ExpectedFundingCashFlow считается на основе predicted rate этого события.
3. Каждому predicted rate присваивается ConfidenceLevel в зависимости от:
   - дистанции до события (чем ближе — тем выше уверенность);
   - стабильности ставки за последнее время;
   - качества данных и sequence book;
   - исторической дисперсии ставок этого инструмента.
4. FundingUncertaintyReserve тем больше, чем ниже ConfidenceLevel и чем дальше событие.
5. Нельзя открывать позицию, если ConfidenceLevel ниже минимально допустимого.
```

Фактический funding PnL не должен вычисляться только формулой: после расчёта он обязан быть подтверждён по private account event, funding history, ledger или изменению баланса, в зависимости от возможностей конкретной биржи. При расхождении между формулой и фактом — фактическое значение имеет приоритет, расхождение логируется как incident.
<!-- v2: добавлены различение realized/predicted, правило решения по predicted + ConfidenceLevel -->

### 3.3. Реальный ценовой спред

Для long на бирже `L` и short на бирже `S`:

```text
EntryBasisRaw = ShortBestBid / LongBestAsk - 1
```

Для целевого объёма нужно использовать не только BBO, а VWAP по стакану:

```text
LongEntryVWAP  = стоимость покупки требуемого объёма / объём покупки.
ShortEntryVWAP = стоимость продажи требуемого объёма / объём продажи.
EntryBasisVWAP = ShortEntryVWAP / LongEntryVWAP - 1.
```

При выходе:

```text
LongExitVWAP  = реальная VWAP продажи long leg.
ShortExitVWAP = реальная VWAP выкупа short leg.
```

**Joint slippage cap.** Slippage ограничивается не только для каждой ноги отдельно, но и суммарно:

```text
ActualEntrySlippageLong  + ActualEntrySlippageShort <= JointSlippageCapBps.
ActualExitSlippageLong   + ActualExitSlippageShort  <= JointSlippageCapBps.
```

При системном шоке обе ноги склонны скользить одновременно в невыгодную сторону, поэтому индивидуальные лимиты не защищают от совместного ухудшения. Если фактический суммарный slippage превысил cap во время набора — остановить дальнейшие slices и пересчитать edge; при выходе — продолжать координированное закрытие согласно `UltimateEmergencyClosePolicy`, но зафиксировать нарушение.
<!-- v2: добавлен joint slippage cap и учёт системного шока -->

### 3.4. Критерий ожидаемой чистой доходности

Не открывать позицию на основе одного funding rate. Рассчитывать:

```text
ExpectedNetPnL =
    ExpectedFundingPnL
  + ExpectedBasisPnL
  - EstimatedEntryFees
  - EstimatedExitFees
  - EstimatedEntrySlippage
  - EstimatedExitSlippage
  - FundingUncertaintyReserve
  - BasisDivergenceReserve
  - CounterpartyRiskReserve
  - RebalanceCostReserve, если применимо
  - SafetyReserve.
```

В UI и конфигурации должны существовать:

```text
MinFundingEdge
MinEntryBasis
MaxAdverseEntryBasis
MinExpectedNetPnLUSDT
MinExpectedNetROI
MinConfidenceLevel
FundingUncertaintyReserve (или формула пересчёта от ConfidenceLevel)
JointSlippageCapBps
MaxAllowedSlippageBps
CounterpartyRiskReserve по tier
```
<!-- v2: добавлены CounterpartyRiskReserve, MinConfidenceLevel, JointSlippageCapBps -->

Позиция eligible только если все обязательные фильтры пройдены и `ExpectedNetPnL >= MinExpectedNetPnLUSDT`.

### 3.5. Дельта-нейтральность

```text
DeltaBase = LongBaseQty - abs(ShortBaseQty)
DeltaUSD  = DeltaBase × conservativeMarkPrice
```

Пользователь настраивает:

```text
DeltaToleranceBase
DeltaToleranceUSD
MaxDeltaRepairTime
```

Инвариант активной парной позиции:

```text
abs(DeltaBase) <= DeltaToleranceBase
AND
abs(DeltaUSD) <= DeltaToleranceUSD.
```

### 3.6. Унификация decimal-типов

```text
Риск-контур и торговая логика: shopspring/decimal (или эквивалентный проверенный decimal-пакет).
  Используется для всех расчётов ExpectedNetPnL, funding, basis, fees, дельты, лимитов.
Горячий контур (нормализация WS-сообщений, агрегация market data): нормализованный int64
  с явно заданной scale (например, price scale = 8, qty scale = 8) и контролем переполнения.
Граница между контурами — явная: преобразование int64↔decimal только в одном месте
  с проверкой lossless-конверсии и логированием потерь точности.
Запрещено неявное приведение decimal→float64 и обратно в финансовой логике.
```
<!-- v2: добавлен раздел 3.6 — унификация decimal -->

---

## 4. Режимы работы

### 4.1. Полуавтоматический режим

В полуавтоматическом режиме система:

```text
Сканирует рынок.
Нормализует и ранжирует кандидатов.
Показывает полный расчёт expected net PnL.
Показывает risk score и причину допуска/отклонения.
Не открывает позицию без подтверждения пользователя.
Не запускает основной вывод средств без установленной политики подтверждения.
Позволяет пользователю вручную закрыть позицию.
```

Подтверждение пользователя не должно открывать сделку по устаревшему snapshot. После нажатия «Открыть» backend обязан заново выполнить preflight. Если edge, ликвидность, funding, баланс или риск изменились, сделка отменяется с понятной причиной.

### 4.2. Полностью автоматический режим

В автоматическом режиме система может самостоятельно:

```text
Выбирать eligible candidates.
Открывать, сопровождать и закрывать позиции.
Прекращать набор позиции при ошибке.
Выполнять настройку leverage/margin mode в пределах разрешённого профиля.
Выполнять ребалансировку только при явном включении этой функции.
```

Автоматический режим не отменяет ни одного риск-лимита, ограничителя капитала, requirement свежести данных или global lock.

### 4.3. Глобальные состояния системы

```text
STARTING
READY
PAUSED_BY_USER
SAFE_HALT
TRADING_LOCKED
REBALANCE_LOCKED
RECOVERY_REQUIRED
```

`SAFE_HALT` означает: запретить новые входы; открытые позиции продолжать мониторить и закрывать по risk policy. Переход в `SAFE_HALT` происходит, в частности, при: критическом incident, MaxDailyLoss (если включён `RiskSnapAfterMaxDailyLoss`), недоступности БД, превышении clock skew, потере приватного канала для открытой позиции без возможности REST-reconciliation.

`REBALANCE_LOCKED` означает: запретить новый ребаланс и новые входы до ручной разблокировки; все активные позиции сопровождать в соответствии с risk policy.
<!-- v2: расширено описание SAFE_HALT -->

---

## 5. Настройки, которые пользователь должен видеть и менять через Mini App

Все значения валидируются backend-ом. В UI должны быть допустимые диапазоны, значения по умолчанию и предупреждения об опасных настройках.

### 5.1. Стратегия поиска

```text
FundingSearchMode:
  SAME_INTERVAL
  DIFFERENT_INTERVAL
  BOTH

RequireAlignedFundingTimes: bool
MaxFundingTimeSkewMinutes
MinFundingSpread
MinEntryBasis
MaxAdverseEntryBasis
MinExpectedNetPnLUSDT
MinExpectedNetROI
MinConfidenceLevel
MinQuoteVolume24hUSDT
MinOrderBookDepthUSDT
MinOpenInterestUSDT, если данные доступны
MaxDataAgeMs
MaxFundingDataAgeMs
MinSecondsBeforeFundingToEnter
AllowedExchanges
AllowedAssets / blocked assets
RequireBacktestPass: bool (по умолчанию true) — не допускать пару к live без пройденного backtest
```
<!-- v2: добавлены MinConfidenceLevel, MinSecondsBeforeFundingToEnter, RequireBacktestPass -->

### 5.2. Риск и капитал

```text
Leverage
MarginMode: ISOLATED по умолчанию
PositionMode: HEDGE / ONE_WAY (требуемый режим; проверяется в preflight)
MaxInitialMarginPercentPerExchange
MaxPortfolioMarginPercent
MaxNotionalPerPositionUSDT
MaxNotionalPerAssetUSDT
MaxOpenPositions
FreeMarginReservePercent
MaxDailyLossUSDT
MaxDailyLossPercent
MaxPositionLossUSDT
MaxPositionLossPercent
MinimumDistanceToLiquidationPercent
EmergencyMarginRatio
DeltaToleranceBase
DeltaToleranceUSD
MaxDeltaRepairTimeMs
MaxHoldingTime
MaxFundingEventsToHold
JointSlippageCapBps
MaxExposurePerExchangeUSDT
CounterpartyRiskTier per-exchange: A / B / C
CounterpartyHaircutPercent per tier
CorrelationLimitBetweenPositions
MaxCorrelatedNotionalUSDT
MarginWarningThresholdPercent
MarginWarningAction: ALERT / REDUCE_ONLY / CLOSE
RiskSnapAfterMaxDailyLoss: bool (по умолчанию true)
ADLExposureLimitPercent per-exchange
MinConfidenceLevel
```
<!-- v2: добавлены joint slippage, counterparty tier/haircut, correlation limits, margin warning, risk snap, ADL exposure limit, PositionMode -->

### 5.3. Исполнение

```text
OrderMode:
  MARKETABLE_LIMIT_IOC — по умолчанию
  MARKET — только при явном разрешении пользователя

SlicesCount
SliceIntervalMs
MaxExecutionSkewMs
OrderAckTimeoutMs
FillConfirmationTimeoutMs
AckTimeoutBehavior: QUERY_THEN_DECIDE (по умолчанию)
PartialFillSlicePolicy: COMPLETE_OR_REPAIR (по умолчанию)
CompensationAttempts
CloseProtectionTicks
MaxCloseRequotes
CloseDeadlineMs
UltimateEmergencyClosePolicy
ClientOrderIdScheme: per-exchange формат и правила генерации
```

`AckTimeoutBehavior: QUERY_THEN_DECIDE` — при истечении `OrderAckTimeoutMs` без ack **запросить состояние ордера через REST**, затем принять решение (отменить/оставить/дозакрыть). **Никогда не переотправлять ордер вслепую** при таймауте ack — это создаёт риск дублирующей заявки.

`PartialFillSlicePolicy` — поведение при частичном исполнении одного slice в пределах slice-window: дождаться остатка, затем при недопустимой дельте выполнить repair (см. 10.2–10.3).

`ClientOrderIdScheme` — каждая биржа имеет свои ограничения на `clientOrderId` (длина, алфавит, уникальность, срок хранения). Backend генерирует детерминированные idempotent идентификаторы по схеме, включающей: position_id, leg, slice index, nonce. Это позволяет при рестарте/таймауте однозначно сопоставить биржевой ордер с внутренним планом и избежать дублирования.

`UltimateEmergencyClosePolicy` должен быть осознанной настройкой. Например:

```text
STRICT_PRICE_GUARD: не ухудшать цену за лимит, но повышается риск не закрыться.
ESCALATING_PRICE_GUARD: постепенно расширять защиту в заданном лимите.
EMERGENCY_MARKET_CLOSE: при риске ликвидации закрыть reduce-only market order.
```

По умолчанию: `ESCALATING_PRICE_GUARD`; при критическом ликвидационном риске — `EMERGENCY_MARKET_CLOSE` только если пользователь явно включил этот режим.
<!-- v2: добавлены AckTimeoutBehavior, PartialFillSlicePolicy, ClientOrderIdScheme -->

### 5.4. Условия выхода

```text
TakeProfitNetPnLUSDT
TakeProfitNetROIPercent
StopLossNetPnLUSDT
StopLossNetROIPercent
ExitFundingThreshold
ExitExpectedNetPnLThreshold
MaxAdverseBasisPercent
TargetBasisExitPercent
MaxHoldingTime
ExitAfterFundingEvents
ExitIfFundingSignChanges
ExitIfFundingIntervalChanges
ExitIfDataBlindTimeExceeded
ExitIfADLDetected: bool (по умолчанию true) — при ADL на одном leg экстренно нейтрализовать второй
ExitIfConfidenceBelowMin: bool
```
<!-- v2: добавлены ADL-выход и confidence-выход -->

### 5.5. Ребалансировка

```text
RebalanceEnabled
RebalanceMode: MANUAL_PLAN / AUTO
TargetBalancePercentPerPair: по умолчанию 50/50
AllowedBalanceImbalancePercent
RebalanceMinAmountUSDT
RebalanceMaxAmountUSDT
RebalanceDailyLimitUSDT
TestTransferAmount
TestDepositTimeout
MainTransferTimeout
RequireNoOpenPositionsForRebalance: true по умолчанию
TransferableReservePercent
WithdrawalFeeCapUSDT
GasPriceCap, если применимо к сети
DepositGracePeriodMs
WithdrawalFailureThreshold (circuit breaker)
```
<!-- v2: добавлены fee/gas caps, deposit grace, circuit breaker threshold -->

---

## 6. Нормализация инструментов и рыночных данных

### 6.1. Канонический реестр

Создай таблицу и in-memory registry, в котором один инструмент представлен как:

```go
type CanonicalInstrument struct {
    Exchange             ExchangeID
    CanonicalBaseAsset   string
    ExchangeSymbol       string
    InstrumentType       InstrumentType // LINEAR_USDT_PERPETUAL
    SettlementCurrency   string         // USDT
    ContractMultiplier   Decimal
    QtyStep              Decimal
    MinQty               Decimal
    MinNotional          Decimal
    TickSize             Decimal
    MaxLeverage          Decimal
    MaxMarketOrderQty    Decimal
    PositionLimit        Decimal
    FundingIntervalSec   int64
    FundingPriceType     FundingPriceType // MARK / INDEX (см. 3.2)
    SupportsADLIndicator bool
    Status               InstrumentStatus
}
```

Символы нельзя строить простым конкатенированием `asset + USDT`; нужны явные mappings. Примеры форматов различаются: `BTCUSDT`, `BTC_USDT`, `BTC-USDT-SWAP`, `XBTUSDTM` и другие.
<!-- v2: добавлены FundingPriceType, SupportsADLIndicator -->

### 6.2. Нормализованный market snapshot

```go
type MarketSnapshot struct {
    Exchange             ExchangeID
    CanonicalBaseAsset   string
    ExchangeSymbol       string

    BestBid              Decimal
    BestAsk              Decimal
    MarkPrice            Decimal
    IndexPrice           Decimal
    LastPrice            Decimal

    QuoteVolume24h       Decimal
    OpenInterest         Decimal
    BidDepthForTargetQty Decimal
    AskDepthForTargetQty Decimal

    RealizedFundingRate  Decimal       // ставка последнего состоявшегося события
    PredictedFundingRate Decimal       // оценка ближайшего будущего события
    FundingRateCap       *Decimal
    FundingRateFloor     *Decimal
    FundingIntervalSec   int64
    NextFundingTime      time.Time
    FundingConfidence    ConfidenceLevel

    ADLQueuePosition     *ADLQueuePosition // если биржа публикует

    ExchangeTimestamp    time.Time
    LocalReceiveTime     time.Time
    SequenceValid        bool
    IsFresh              bool
}
```
<!-- v2: добавлены realized/predicted funding, FundingConfidence, ADLQueuePosition -->

### 6.3. Слой свежести и качества данных

Любой snapshot должен иметь:

```text
Источник.
Время биржи.
Время получения локальным сервером.
Возраст.
Статус sequence book.
Статус WebSocket соединения.
Смещение локальных часов относительно биржи (clock offset), если измеримо.
```

Правила:

```text
Public data stale → не открывать новые позиции по затронутому источнику.
Private data stale → не открывать/не увеличивать позицию; начать recovery.
Order book sequence gap → snapshot недействителен до REST resync.
Mark/BBO, отличающиеся от разумного диапазона, → quality alert и исключение кандидата.
Clock offset выше MaxAllowedClockOffsetMs → запрет новых входов; см. раздел 24.
При reconnect приватного WS обязательна REST-reconciliation позиции и ордеров,
  даже если WebSocket-сообщения не сообщали об ошибке.
```
<!-- v2: добавлены clock offset и обязательная REST-recon при reconnect -->

---

## 7. Высокоэффективный сканер без перегрузки компьютера

### 7.1. Архитектура по уровням

Не загружай полный стакан каждого токена на всех биржах. Используй четыре уровня.

#### Level 1 — Instrument registry

Обновлять редко: каждые 30–60 минут и при системных событиях.

```text
Инструменты, статусы, шаги, multiplier, лимиты, funding interval, funding price type.
```

#### Level 2 — лёгкий all-market мониторинг

По всем допустимым инструментам получать по WebSocket или bulk endpoint:

```text
BBO / ticker.
Mark price.
Realized и predicted funding rate.
Next funding time.
24h quote volume.
```

Не делать REST-запрос на каждый тик и не хранить полный history каждого сообщения.

#### Level 3 — подробный анализ только кандидатов

Только для активных кандидатов получать/поддерживать:

```text
Depth 10–50 уровней.
VWAP на целевой объём.
Реальную оценку slippage.
Точные trading fees аккаунта.
ADL indicator/queue-position, если доступен.
```

#### Level 4 — preflight перед торговлей

Непосредственно перед заявками повторно проверить:

```text
Актуальность BBO и depth.
Expected Net PnL.
Funding schedule и predicted rate.
Свободную маржу.
Статус инструмента.
Лимиты позиции.
Отсутствие конфликтующих ордеров.
Состояние WebSocket и API.
Position mode на обеих биржах.
Clock offset в допуске.
```

### 7.2. Адаптивная частота

```text
До funding > 60 минут:           анализ кандидата раз в 3–10 секунд.
До funding 15–60 минут:          1–3 секунды.
До funding 5–15 минут:           250–1 000 мс для лучших кандидатов.
До funding < 5 минут:            100–250 мс для Top-кандидатов.
Открытие/закрытие позиции:       критические private события обрабатываются сразу.
Открытая позиция:                непрерывный private WS + быстрый risk loop.
UI:                              агрегированные обновления 1–4 раза в секунду.
```

### 7.3. Coalescing, backpressure и очереди

```text
Public market data:
  Можно coalesce: хранить последнее состояние символа, не каждое старое обновление.
  При переполнении очереди сохранять последний snapshot и метрику drops/coalesces.

Private order/position/balance events:
  Терять нельзя.
  Обрабатывать упорядоченно, дедуплицировать и при сомнении сверять REST.
  При критическом переполнении — остановить новые входы и выполнить reconciliation.
```

Не создавать goroutine на каждое сообщение. Использовать:

```text
Ограниченное количество goroutines на соединение.
Bounded worker pools.
Context cancellation.
Очереди с лимитами.
Отдельные очереди по приоритетам.
```

### 7.4. Приоритеты API и вычислений

```text
P0 — аварийное закрытие и ликвидационный риск, реакция на ADL.
P1 — устранение дельта-дисбаланса, мониторинг открытой позиции.
P2 — размещение/отмена/подтверждение ордеров.
P3 — reconciliation позиции и баланса.
P4 — ребалансировка.
P5 — новый scan кандидатов.
P6 — UI, история, отчёты.
```

Каждый exchange adapter обязан иметь независимый rate limiter, request queue, circuit breaker и reconnect backoff с jitter.
<!-- v2: P0 дополнен реакцией на ADL -->

---

## 8. Логика отбора и ранжирования кандидатов

### 8.1. Первичные фильтры

Кандидат создаётся только если:

```text
Один canonical base asset.
Оба инструмента — LINEAR_USDT_PERPETUAL.
Оба инструмента active/tradable.
Есть достаточная глубина для целевого объёма.
24h volume выше порога.
Нет stale data.
Есть funding data и funding time.
Объём можно выразить на обеих биржах без недопустимого rounding residue.
У пользователя достаточно свободной маржи на обеих биржах.
Нет другой конфликтующей позиции по этому активу.
До следующего funding события осталось не менее MinSecondsBeforeFundingToEnter.
Predicted funding ConfidenceLevel >= MinConfidenceLevel.
Пара прошла backtest (если включён RequireBacktestPass).
Ни одна из бирж не находится в circuit-breaker/withdrawal-suspend, влияющем на выход.
```
<!-- v2: добавлены MinSecondsBeforeFunding, ConfidenceLevel, backtest, circuit-breaker проверки -->

### 8.2. Режимы funding period

#### SAME_INTERVAL

```text
FundingInterval(long) == FundingInterval(short)
```

При включённом `RequireAlignedFundingTimes` дополнительно:

```text
abs(NextFundingTime(long) - NextFundingTime(short)) <= MaxFundingTimeSkew.
```

#### DIFFERENT_INTERVAL

```text
FundingInterval(long) != FundingInterval(short)
```

Обязательно построить event calendar каждого funding event в пределах horizon удержания.

#### BOTH

Показывать оба типа с явной меткой:

```text
SAME_INTERVAL_ALIGNED
SAME_INTERVAL_UNALIGNED
DIFFERENT_INTERVAL
```

### 8.3. Funding event calendar

Для каждого кандидата создать список ожидаемых событий:

```go
type FundingEvent struct {
    Exchange          ExchangeID
    Symbol            string
    LegSide           Side
    ScheduledAt       time.Time
    FundingRate       Decimal  // predicted rate на момент расчёта
    RateType          FundingRateType // PREDICTED / REALIZED (после свершения)
    EstimatedNotional Decimal
    EstimatedCashFlow Decimal
    Confidence        ConfidenceLevel
}
```

Не ранжировать разные интервалы только по annualized проценту. Annualized ROI допустимо показывать в UI, но решение о входе должно опираться на реальный календарь событий, прогноз cash flow и риск изменения ставки.
<!-- v2: добавлены RateType, Confidence в структуру -->

### 8.4. Candidate score

В дополнение к ExpectedNetPnL рассчитывай:

```text
LiquidityScore.
FundingConfidenceScore.
BasisStabilityScore.
ExecutionRiskScore.
CounterpartyRiskScore (обязательно).
DataQualityScore.
ADLRiskScore.
```

Выводить пользователю объяснение:

```text
Почему кандидат принят.
Почему отклонён.
Какие лимиты стали блокирующим фактором.
Какой funding event является целевым.
Какие комиссии и резервы вычтены (включая counterparty haircut).
```
<!-- v2: CounterpartyRiskScore сделан обязательным, добавлен ADLRiskScore -->

---

## 9. Расчёт объёма, плеча и маржи

### 9.1. Плечо

Плечо не является разрешением использовать весь депозит. По умолчанию использовать `ISOLATED` margin, если биржа и инструмент это поддерживают. Не смешивать средства стратегии с неизвестными ручными позициями.

### 9.2. Предельный объём

Для каждой биржи проверить:

```text
InitialMargin.
Open/close fees.
Worst-case slippage (с учётом joint cap).
Maintenance buffer.
Funding uncertainty reserve.
Counterparty haircut reserve.
Position limit.
Max market/IOC quantity.
User max initial margin percentage.
User max notional.
Max exposure per exchange (counterparty-концентрация).
ADL exposure limit.
```

Условие:

```text
InitialMargin + Fees + WorstCaseSlippage + SafetyReserve + CounterpartyReserve
<= FreeCollateral × MaxInitialMarginPercentPerExchange.
```

Итоговый target base quantity:

```text
targetBaseQty = minimum(
  maxByLongMargin,
  maxByShortMargin,
  maxByLongLiquidity,
  maxByShortLiquidity,
  maxByExchangeLimits,
  maxByUserNotional,
  maxByPortfolioRisk,
  maxByCounterpartyExposure,
  maxByADLLimit,
  maxByRemainingRiskBudget
)
```

После вычисления объём нужно привести к допустимому шагу **на обеих биржах**. Если полученный общий кратный объём даёт недопустимую дельту — кандидат отклоняется или объём понижается.
<!-- v2: добавлены counterparty/ADL/risk-budget ограничения -->

### 9.3. Распределение капитала между кандидатами

См. подробнее раздел 25. Если одновременно eligible несколько кандидатов, конкурирующих за капитал на одной бирже (cross-pair contention), система не открывает их независимо. Сначала решается портфельная задача: какие позиции и какого размера открыть в пределах общего risk-бюджета, лимитов per-exchange и корреляционных ограничений. Только после этого формируются execution plans.
<!-- v2: добавлен раздел 9.3 -->

---

## 10. State machine позиции

Каждая позиция должна иметь персистентный state machine. Не управлять состоянием только в памяти.

```text
DISCOVERED
QUALIFIED
AWAITING_USER_APPROVAL
PREPARING
OPENING
PARTIALLY_HEDGED
HEDGED
MONITORING
EXIT_REQUESTED
EXITING
RECONCILING
CLOSED
DEGRADED
FAILED
```

Разрешённые переходы должны быть явно описаны и протестированы. Все transition записываются транзакционно в БД и в audit log.

### 10.1. Preflight перед открытием

Перед каждой новой позицией:

```text
1. Получить/проверить свежие position и balance snapshots обеих бирж.
2. Проверить отсутствие неизвестных open orders.
3. Проверить margin mode, leverage и position mode (HEDGE/ONE_WAY) на обеих биржах.
4. Проверить instrument status, quantity step, min qty, multiplier, funding price type.
5. Пересчитать funding event calendar и predicted rates с ConfidenceLevel.
6. Пересчитать VWAP, joint slippage и expected net PnL.
7. Проверить capital/risk limits, включая counterparty exposure и корреляционные лимиты.
8. Проверить доступность private WS и обязательную REST-recon после reconnect.
9. Проверить data age, clock offset (см. 24) и API health.
10. Создать immutable execution plan с уникальным ID.
```
<!-- v2: добавлены position mode, predicted/Confidence, joint slippage, counterparty, clock offset -->

### 10.2. Набор позиции slices

Для каждого slice:

```text
1. Проверить, что план ещё выгоден и не устарел.
2. Рассчитать точную допустимую base quantity.
3. Конвертировать её в contract quantity отдельно для каждой биржи.
4. Сформировать уникальные clientOrderId для каждого leg по ClientOrderIdScheme.
5. Одновременно отправить ордера на long и short legs.
6. По умолчанию использовать reduce-safe marketable limit IOC с price protection.
7. Дождаться private WS подтверждения исполнения; при необходимости проверить REST.
8. При таймауте ack — AckTimeoutBehavior: QUERY_THEN_DECIDE (запросить состояние, не переотправлять вслепую).
9. При partial fill одного slice — действовать по PartialFillSlicePolicy:
   дождаться остатка в пределах slice-window, затем repair при недопустимой дельте.
10. Сравнить фактические filled base quantities, а не requested quantities.
11. Проверить joint slippage cap по фактическим исполнениям.
12. Если дельта допустима — перейти к следующему slice.
13. Если дельта недопустима — остановить дальнейший набор и выполнить repair.
```
<!-- v2: добавлены ack timeout policy, partial fill policy, joint slippage check -->

### 10.3. Сценарий 60 токенов против 50 токенов

Если при шестом slice планировалось открыть 10 токенов на каждой бирже, но получилось:

```text
Биржа A: 60 TOKEN.
Биржа B: 50 TOKEN.
```

Алгоритм:

```text
1. Немедленно остановить следующие slices.
2. Отменить pending orders текущего slice.
3. Сверить фактические позиции через private WS и REST.
4. Однократно попытаться открыть недостающий объём на меньшей ноге: 10 TOKEN.
5. Если получили 60/60 — продолжить план.
6. Если попытка не исполнилась, частично исполнилась с недопустимой дельтой или вернула ошибку:
   a. закрыть reduce-only лишнюю экспозицию на большей ноге;
   b. привести legs к минимальному общему фактически доступному base quantity;
   c. зафиксировать DEGRADED;
   d. отправить Telegram-уведомление;
   e. по умолчанию остановить дальнейший набор до новой preflight-проверки или подтверждения пользователя.
```

Никогда не отправлять бесконечные попытки компенсирующего ордера.

---

## 11. Сопровождение позиции и условия выхода

### 11.1. Постоянно рассчитывать

```text
Фактический base qty каждой ноги.
DeltaBase и DeltaUSD.
Текущий executable exit basis.
Unrealized basis PnL.
Realized trading PnL.
Confirmed funding PnL.
Expected future funding PnL (на основе predicted rate и ConfidenceLevel).
Total net PnL after estimated closing fees/slippage.
Margin ratio, available collateral и liquidation distance каждой ноги.
ADL queue-position обеих ног (если публикуется).
Data freshness, connection health и clock offset.
```
<!-- v2: добавлены predicted funding в expected PnL, ADL queue, clock offset -->

### 11.2. Приоритетные причины выхода

#### P0 — аварийный выход

Немедленно запросить координированное закрытие при:

```text
Нажатии пользователем «Принудительно закрыть».
Аварийном margin ratio.
Опасном приближении к liquidation price.
Невосстановимом delta mismatch.
Критическом order rejection на одной ноге.
Длительной невозможности узнать позицию.
Длительном отключении private WS + невозможности REST reconciliation.
Delisting, halt, reduce-only или ином опасном status инструмента.
Обнаружении несанкционированной ручной позиции/ордера.
Обнаружении ADL на одном leg (см. раздел 23).
```

#### P1 — выход по funding risk

```text
Funding rate сменил знак.
ConfidenceLevel упал ниже MinConfidenceLevel.
Expected future net funding ниже ExitFundingThreshold.
ExpectedNetPnL ниже ExitExpectedNetPnLThreshold.
Funding interval изменился.
Funding event не подтверждён в configurable timeout.
Funding prediction слишком нестабилен по заданной policy.
```

#### P2 — выход по basis и PnL

```text
Достигнут TakeProfitNetPnL/ROI.
Достигнут TargetBasisExitPercent.
Достигнут StopLossNetPnL/ROI.
Basis расширился выше MaxAdverseBasisPercent.
Joint slippage при выходе превысил cap сверх допустимой policy.
Ожидаемая стоимость выхода ухудшилась за пределы risk policy.
```

#### P3 — выход по времени

```text
Достигнут MaxHoldingTime.
Собрано MaxFundingEventsToHold.
Достигнут пользовательский planned horizon.
```
<!-- v2: добавлены ADL в P0, ConfidenceLevel в P1, joint slippage в P2 -->

### 11.3. Координированное закрытие

**Не использовать независимые TP/SL на двух биржах как основной механизм выхода.** Они могут закрыть один leg раньше второго и создать направленную экспозицию.

Основной алгоритм закрытия:

```text
1. Перевести позицию в EXIT_REQUESTED и заблокировать новые slices.
2. Отменить все entry/pending/несовместимые conditional orders.
3. Считать актуальные позиции обеих ног.
4. Вычислить общий base quantity, который можно закрыть синхронно.
5. Одновременно отправить reduce-only marketable IOC на обеих биржах.
6. Для long leg при продаже:
   limit = bestBid - CloseProtectionTicks × tickSize.
7. Для short leg при выкупе:
   limit = bestAsk + CloseProtectionTicks × tickSize.
8. Дождаться executions, не только order acknowledgements.
9. При таймауте ack — QUERY_THEN_DECIDE; не закрывать «вслепую» повторным ордером.
10. При частичном исполнении сначала выровнять legs до минимального общего остатка.
11. Повторять закрытие оставшейся парной части с bounded количеством requotes.
12. Контролировать joint slippage cap; при превышении — escalate по UltimateEmergencyClosePolicy.
13. При критическом риске применять UltimateEmergencyClosePolicy.
14. После закрытия обязательно REST+WS reconciliation, отмена остаточных ордеров, проверка нулевых позиций.
15. Зафиксировать итоговый PnL, fees, funding, basis, slippage, причину выхода.
```

Нативные биржевые conditional reduce-only stop orders могут использоваться только как дополнительный аварийный слой, если они поддерживаются и явно отмечены в UI. Они не заменяют основной coordinated exit engine.
<!-- v2: добавлены QUERY_THEN_DECIDE и joint slippage в закрытие -->

---

## 12. Ребалансировка капитала

### 12.1. Общая политика

Ребалансировка выключена по умолчанию и включается пользователем отдельно. Для автоматического вывода требуются отдельные withdrawal API keys с минимальными необходимыми правами, IP whitelist и заранее добавленными whitelist-адресами на самих биржах.

Не выводить средства, задействованные как margin открытой позиции. По умолчанию:

```text
RequireNoOpenPositionsForRebalance = true.
```

### 12.2. Различение видов перемещения средств

```text
Internal transfer (main↔futures на одной бирже):
  Мгновенно, без on-chain комиссии, через отдельный endpoint/permission.
  Требует scope Transfer у ключа (см. 13.2).
  У некоторых бирж — отдельный permission или отдельный ключ.

On-chain withdrawal (с биржи A на адрес биржи B):
  Комиссия сети (gas/fee), время подтверждения, риск задержки/потери.
  Считать успешным только по зачислению на destination, не по ack вывода.
  Подчиняется fee/gas caps и circuit breaker.
```

В UI и в планах ребалансировки вид перемещения должен быть явно отмечен и учитываться в расчётах и таймаутах.
<!-- v2: добавлен раздел 12.2 — различение internal transfer vs on-chain -->

### 12.3. Данные маршрута

Пользователь через Mini App задаёт для каждого маршрута:

```text
Source exchange.
Destination exchange.
Asset: в первой версии USDT.
Network на source.
Network на destination.
Destination address.
Memo/tag, если требуется.
Название маршрута.
Разрешённый лимит.
```

Перед сохранением:

```text
Проверить формат address.
Проверить необходимость memo/tag.
Сохранить fingerprint/hash адреса.
Потребовать явное подтверждение изменения адреса + второй фактор (см. 27).
После изменения адреса отключать AUTO rebalance до повторного подтверждения.
```

### 12.4. Расчёт цели 50/50

Целевой баланс для конкретной рабочей пары бирж рассчитывается не от полного депозита, а от доступного к перемещению капитала:

```text
NetTransferableEquity =
  TotalEquity
  - LockedMargin
  - MaintenanceReserve
  - PendingWithdrawals
  - MinimumOperationalReserve.
```

Цель:

```text
TargetA ≈ 50% ± AllowedBalanceImbalancePercent.
TargetB ≈ 50% ± AllowedBalanceImbalancePercent.
```

Учитывать withdrawal fee, minimum withdrawal, minimum deposit, network fee, precision и уже находящиеся в обработке суммы. Если суммарная комиссия маршрута превышает `WithdrawalFeeCapUSDT` (или gas превышает `GasPriceCap`) — маршрут откладывается с понятной причиной.
<!-- v2: добавлены fee/gas caps в расчёт цели -->

### 12.5. State machine ребалансировки

```text
IDLE
PLANNED
PRECHECKING
TEST_TRANSFERS_SENT
WAITING_TEST_CREDITS
TESTS_CONFIRMED
MAIN_TRANSFERS_SENT
WAITING_MAIN_CREDITS
INTERNAL_TRANSFERS_PENDING
COMPLETED
FAILED
REBALANCE_LOCKED
```

### 12.6. Обязательный двухэтапный механизм

Пример маршрутов:

```text
Binance → Bybit.
Gate → OKX.
```

Алгоритм:

```text
1. Создать единый transfer plan и global rebalance lock.
2. Выполнить precheck всех маршрутов:
   сети, адреса, memo, whitelist, minimum withdrawal/deposit, fee (vs cap),
   wallet status, доступный баланс, отсутствие позиции, лимиты.
3. Отправить тестовую сумму по каждому маршруту.
4. Для каждого теста записать request ID, withdrawal ID, txid, сумму, сеть, адрес fingerprint и timestamps.
5. Ждать подтверждённого зачисления на каждой destination-бирже.
6. Сопоставлять депозит по withdrawal ID/txid; при невозможности — по строго заданным признакам сети, времени, суммы и адреса.
7. Пока все тесты не имеют TEST_CREDITED, не отправлять ни одной основной суммы по любому маршруту.
8. Если все тесты зачислены — отправить основные суммы согласно plan.
9. Подтвердить основные депозиты, выполнить внутренние transfer в trading/futures wallet, если это требуется конкретной биржей.
10. Выполнить финальную сверку целевых балансов.
```

Мониторинг депозита в течение `DepositGracePeriodMs`: если средства не зачислены в срок — эскалация в incident без слепой повторной отправки.

### 12.7. Ошибка теста или основного перевода

Если тестовый депозит не зачислен за `TestDepositTimeout`, возникла ошибка API, транзакция не сопоставлена, сеть недоступна или возникла иная критическая ошибка:

```text
1. Не отправлять основные суммы ни по одному маршруту.
2. Не выполнять автоматический повторный withdrawal.
3. Перевести систему в REBALANCE_LOCKED.
4. Запретить новые входы в сделки.
5. Продолжать безопасно сопровождать существующие позиции.
6. Отправить критическое уведомление в Telegram.
7. Показать пользователю ID, txid, сеть, сумму, статус и точную причину.
8. Разрешить повторное включение ребалансировки только пользователю через Mini App (со вторым фактором, см. 27).
```

**Withdrawal circuit breaker.** При достижении `WithdrawalFailureThreshold` последовательных неудач/таймаутов по бирже или маршруту — автоматический перевод в `REBALANCE_LOCKED` без ожидания следующей попытки.

Если ошибка произошла после отправки основной суммы, также не делать blind retry и не пытаться автоматически «компенсировать» перевод обратным выводом. Нужны audit trail, reconciliation и ручное решение пользователя.
<!-- v2: добавлены DepositGracePeriod и withdrawal circuit breaker -->

---

## 13. API-ключи, секреты и доступы

### 13.1. Ввод ключей

Пользователь вводит ключи в Mini App, но browser/Telegram client не является хранилищем секретов. Mini App передаёт секреты по HTTPS в backend один раз. После успешного сохранения secret и passphrase никогда не возвращаются через API и не показываются в интерфейсе.

В UI показывать только:

```text
Exchange.
Masked API key fingerprint.
Тип ключа: Trade / Withdrawal.
Права, обнаруженные при проверке.
IP whitelist status, если доступен.
Время последней успешной проверки.
Возраст ключа и дата плановой ротации (см. 27).
Статус подключения.
```
<!-- v2: добавлен возраст ключа и дата ротации -->

### 13.2. Разделение ключей

#### Trade key

Минимальные права:

```text
Read.
Futures/Contract Trade.
```

Не выдавать право Withdraw этому ключу.

#### Withdrawal/Rebalance key

Отдельный ключ, активный только если ребалансировка включена:

```text
Read.
Transfer, если нужен (internal main↔futures). ВАЖНО: на ряде бирж это отдельный scope/permission;
   его наличие обязательно проверить в контракт-документации и в runtime при precheck.
Withdraw.
```

Для него обязательны:

```text
IP whitelist.
Whitelist withdrawal addresses на стороне биржи.
Минимальные дневные и разовые лимиты.
Отдельный audit trail.
```
<!-- v2: уточнён scope Transfer — отдельный permission, обязательная проверка -->

### 13.3. Хранение

Использовать envelope encryption:

```text
Mini App → TLS → Backend.
Backend → шифрование секретов AEAD.
Encrypted secret blobs → PostgreSQL.
Master key → внешний secret manager или защищённая environment variable на сервере.
Расшифрование — только в памяти на время подписания запроса.
```

Никогда не логировать:

```text
API key полностью.
API secret.
Passphrase.
Telegram bot token.
Подписи запросов.
Полные withdrawal addresses, если это не требуется для защищённого audit UI.
```

Telegram Bot Token хранить только в `TELEGRAM_BOT_TOKEN` или secret manager; не включать его в репозиторий.

### 13.4. Telegram авторизация

```text
Проверять Telegram WebApp initData на backend по официальному алгоритму.
Не доверять telegram user ID, присланному фронтендом без validation.
Использовать allowlist Telegram Admin IDs.
Ограничить все торговые и выводные endpoint только авторизованному владельцу.
Использовать короткоживущую server-side session.
```

### 13.5. Replay-защита мутаций

Все мутации UI→backend (открытие/закрытие позиции, изменение настроек, изменение адреса вывода, разблокировка rebalance, переключение режима) должны:

```text
Содержать nonce/timestamp и idempotency key.
Проверяться на сервере на повтор/устаревание timestamp.
Иметь явный audit-запись с correlation ID.
```
<!-- v2: добавлен раздел 13.5 — replay-защита -->

Ротация ключей, kill switch и второй фактор — см. раздел 27.

---

## 14. Telegram Mini App: обязательный UI/UX

### 14.1. Главный dashboard

Показывать:

```text
Текущий режим: Semi-auto / Auto.
Состояние системы.
Состояние каждой биржи (включая counterparty tier).
WebSocket/API health.
Общий equity.
Свободная и использованная маржа по каждой бирже.
Суммарный realized/unrealized PnL.
Confirmed funding PnL.
Количество открытых позиций.
Текущий использованный risk-бюджет по биржам.
Активные incident/locks.
Clock offset индикатор.
```
<!-- v2: добавлены counterparty tier, risk-бюджет, clock offset -->

### 14.2. Scanner

Для каждого кандидата показать:

```text
Токен.
Long exchange/symbol.
Short exchange/symbol.
Realized и predicted funding rate обеих ног + ConfidenceLevel.
Funding interval и next funding time обеих ног.
Класс режима: SAME_INTERVAL_ALIGNED / DIFFERENT_INTERVAL и т.д.
Best ask long и best bid short.
Entry VWAP basis.
24h volume и доступную глубину.
Комиссии.
Ожидаемый funding PnL.
Expected basis PnL.
Все резервы (включая counterparty haircut).
Expected Net PnL и ROI.
Risk score (включая ADL risk).
Backtest-статус пары.
Причины eligibility/rejection.
```

В полуавтоматическом режиме дать кнопки:

```text
Открыть.
Игнорировать.
Добавить актив в blacklist.
Просмотреть полный execution plan.
```
<!-- v2: добавлены predicted/Confidence, counterparty haircut, ADL risk, backtest статус -->

### 14.3. Экран позиции

```text
Статус позиции.
Причина входа.
Фактические long/short base quantities.
DeltaBase / DeltaUSD.
Contract quantities и multiplier.
Leverage, margin mode, position mode, margin used.
Entry prices и current executable exit prices.
Mark price, liquidation price, margin ratio.
ADL queue-position обеих ног (если доступно).
Funding events: expected (predicted) и confirmed.
Funding PnL, basis PnL, fees, net PnL.
Open orders.
Последние ошибки и actions.
Кнопка «Принудительно закрыть» с подтверждением.
```
<!-- v2: добавлены position mode, ADL queue -->

### 14.4. Настройки и ребалансировка

Реализовать все настройки из раздела 5, с пояснениями. Для ребалансировки показать:

```text
Toggle RebalanceEnabled.
Маршруты и сети.
Masked address + address fingerprint.
Memo/tag.
Вид перемещения: internal transfer / on-chain.
План перевода до запуска с оценкой комиссий (vs caps).
Тестовые переводы и статусы.
Основные переводы и txid.
Deposit grace-period мониторинг.
Текущий rebalance lock и withdrawal circuit breaker status.
Кнопку ручной разблокировки после явного подтверждения + второго фактора.
```
<!-- v2: добавлены вид перемещения, caps, grace-period, circuit breaker, 2FA -->

### 14.5. Уведомления Telegram

Отправлять уведомления минимум при:

```text
Старт/остановка бота.
Потеря или восстановление exchange connection.
Открытие позиции.
Частичное исполнение/repair.
Полное хеджирование позиции.
Получение funding.
Выход и итоговый PnL.
Обнаружение ADL.
Любой SAFE_HALT / REBALANCE_LOCKED.
Срабатывание withdrawal circuit breaker.
Превышение clock skew.
Ошибка withdrawal/test deposit.
Приближение к risk limits / margin warning.
Достижение MaxDailyLoss.
Ручная операция пользователя.
Требование второго фактора для критичной мутации.
```

Не включать секреты в уведомления.
<!-- v2: добавлены ADL, circuit breaker, clock skew, margin warning, MaxDailyLoss, 2FA -->

---

## 15. Архитектура Go-проекта

Для одного пользователя использовать **модульный монолит**, а не избыточную микросервисную архитектуру. Разделять домены в коде так, чтобы в будущем их можно было вынести в сервисы.

Пример структуры:

```text
cmd/
  server/                 HTTP API, Telegram webhook/long polling, frontend serving
  worker/                 market data, strategy, execution, rebalance

internal/
  app/
  config/
  domain/
  decimal/
  exchange/
    adapter.go
    binance/
    bybit/
    mexc/
    okx/
    bitget/
    kucoin/
    gate/
  marketdata/
  orderbook/
  scanner/
  strategy/
  allocation/             распределение капитала между кандидатами (см. 25)
  execution/
  risk/
  portfolio/
  rebalance/
  credentials/
  keyrotation/            жизненный цикл ключей и kill switch (см. 27)
  telegram/
  auth/
  clocks/                 сервис синхронизации часов (см. 24)
  lifecycle/              graceful shutdown, DB-unavailable handling (см. 28)
  repository/
  outbox/
  notifications/
  audit/
  observability/

migrations/
webapp/
docs/
tests/
deploy/
```
<!-- v2: добавлены allocation, keyrotation, clocks, lifecycle -->

### 15.1. Exchange adapter interface

Создать строгий интерфейс, не позволяющий стратегии зависеть от конкретной биржи:

```go
type ExchangeAdapter interface {
    ID() ExchangeID

    GetServerTime(ctx context.Context) (time.Time, error)
    GetInstruments(ctx context.Context) ([]Instrument, error)
    GetFunding(ctx context.Context, symbol string) (FundingInfo, error)
    GetTicker(ctx context.Context, symbol string) (Ticker, error)
    GetOrderBookSnapshot(ctx context.Context, symbol string, depth int) (OrderBookSnapshot, error)

    SubscribePublic(ctx context.Context, subscriptions []PublicSubscription) (<-chan PublicEvent, error)
    SubscribePrivate(ctx context.Context, credentials CredentialRef) (<-chan PrivateEvent, error)

    GetBalances(ctx context.Context) ([]Balance, error)
    GetPositions(ctx context.Context) ([]Position, error)
    GetOpenOrders(ctx context.Context, symbol string) ([]Order, error)

    SetLeverage(ctx context.Context, req SetLeverageRequest) error
    SetMarginMode(ctx context.Context, req SetMarginModeRequest) error
    SetPositionMode(ctx context.Context, req SetPositionModeRequest) error
    PlaceOrder(ctx context.Context, req PlaceOrderRequest) (OrderAck, error)
    CancelOrder(ctx context.Context, req CancelOrderRequest) error
    GetOrder(ctx context.Context, req OrderQuery) (Order, error)

    GetADLState(ctx context.Context, symbol string) (ADLState, error) // если поддерживается

    InternalTransfer(ctx context.Context, req InternalTransferRequest) (TransferResult, error)
    Withdraw(ctx context.Context, req WithdrawalRequest) (WithdrawalResult, error)
    GetWithdrawalHistory(ctx context.Context, query TransferQuery) ([]Withdrawal, error)
    GetDepositHistory(ctx context.Context, query TransferQuery) ([]Deposit, error)
    GetNetworkInfo(ctx context.Context, asset string) ([]NetworkInfo, error)
}
```

Все структуры должны быть нормализованными. Особенности exchange API должны оставаться внутри конкретного адаптера. `FundingInfo` должен явно содержать как realized, так и predicted rate, тип funding price и ConfidenceLevel.
<!-- v2: добавлены SetPositionMode, GetADLState, уточнение про FundingInfo -->

### 15.2. Надёжность состояния

Использовать PostgreSQL как источник персистентного бизнес-состояния. Внутренние каналы Go не являются источником истины.

Для критических действий применять transactional outbox:

```text
Изменение состояния в БД.
Создание audit event.
Создание command/outbox event.
Отправка внешнего действия.
Подтверждение фактического результата.
```

При рестарте backend обязан:

```text
1. Загрузить все неоконченные позиции и transfer plans.
2. Запросить актуальные position, orders, balances у бирж.
3. Сопоставить их с БД.
4. Запретить новые входы при расхождении.
5. Создать incident и потребовать/выполнить recovery policy.
```

### 15.3. Управление жизненным циклом процесса

```text
Graceful shutdown:
  При получении сигнала остановки — прекратить новые входы,
  дождаться подтверждения in-flight ордеров/transfer в пределах shutdown timeout,
  выполнить финальную сверку позиций/балансов, зафиксировать состояние, закрыться.
  См. подробнее раздел 28.

Конфигурация — две категории:
  HOT: перезагружается без рестарта (пороговые значения, лимиты, blacklists, режимы).
  COLD: требует рестарта (структура бирж, БД-параметры, мастер-ключ).
  Категория каждого параметра зафиксирована в config-модели.
```
<!-- v2: добавлен раздел 15.3 — graceful shutdown и hot/cold config -->

---

## 16. Схема данных PostgreSQL

> **Single-tenant v1.** В первой версии система обслуживает одного пользователя. Таблица `users` (и связанные `telegram_sessions`, `exchange_credentials`) сохраняется для будущего мульти-тенантного расширения, но все risk/execution-запросы работают в рамках единственного tenant_id. Это решение зафиксировать в ADR; не вводить UI мульти-аккаунта. <!-- v2: разрешено противоречие single-user vs users -->

Создать миграции как минимум для:

```text
users
telegram_sessions
exchange_credentials
exchange_connection_status
wallet_addresses
strategy_settings
exchange_instruments
symbol_mappings
positions
position_legs
execution_plans
orders
fills
funding_payments
account_snapshots
transfer_plans
transfer_routes
transfer_attempts
incidents
notifications
idempotency_keys
audit_log
outbox_events
system_locks
capital_allocations          распределение risk-бюджета по позициям (25)
adl_events                   факты ADL и реакция системы (23)
withdrawal_circuit_breaker   состояние circuit breaker по биржам/маршрутам (26)
key_rotation_log             история ротации и компрометаций ключей (27)
clock_sync_state             измеренный clock offset и статус NTP (24)
position_mode_state          требуемый/фактический position mode по биржам
```
<!-- v2: добавлены новые таблицы -->

### 16.1. Важные свойства таблиц

`positions`:

```text
position_id, strategy, canonical_asset, state, entry reason,
long exchange, short exchange, target qty, actual delta,
entry/exit timestamps, exit reason, realised pnl, funding pnl, fees,
position mode, counterparty tier snapshot.
```

`position_legs`:

```text
exchange, symbol, side, contract qty, base qty, multiplier,
entry vwap, mark price, liquidation price, margin info, status,
adl queue position (если применимо).
```

`orders` и `fills`:

```text
exchange order id, client order id, position id, leg id,
side, reduce only, requested qty, filled qty, average price,
fees, status, exchange timestamps, raw response reference,
ack state (acked/queried/timed-out).
```

`transfer_attempts`:

```text
plan ID, route ID, phase TEST/MAIN, kind (INTERNAL / ONCHAIN),
source/destination, asset, network, address fingerprint, memo fingerprint,
request ID, withdrawal ID, txid, gross amount, fee, net amount,
status, timeout, failure reason, fee-cap-check result.
```

`adl_events`:

```text
event id, exchange, symbol, leg, detected at, queue position,
affected position id, action taken, resulting state, pnl impact.
```

Секреты не дублировать в audit log или raw JSON. Raw API responses сохранять только после redaction.
<!-- v2: добавлены position mode, counterparty tier, adl queue, ack state, transfer kind, fee-cap-check, таблица adl_events -->

### 16.2. RTO/RPO и резервное копирование

```text
Цель RTO/RPO зафиксировать в ADR (например, RTO <= 15 минут, RPO <= 5 минут).
Регулярные зашифрованные backups PostgreSQL + point-in-time recovery (PITR) через WAL archiving.
Backup хранится отдельно от сервера; процедура восстановления отрепетирована и задокументирована.
Master encryption key имеет отдельную стратегию резервирования (не в той же БД).
Шифрование backup at rest.
```
<!-- v2: добавлен раздел 16.2 — RTO/RPO и backup -->

---

## 17. Наблюдаемость, логирование и health checks

### 17.1. Метрики

Экспортировать Prometheus/OpenTelemetry-метрики:

```text
CPU, RAM, GC pause, goroutine count.
WebSocket connected state и reconnect count по бирже.
WebSocket event lag.
REST latency/error/rate-limit по endpoint и бирже.
Queue length и dropped/coalesced public events.
Private event processing lag.
Market snapshot age.
Order ack latency.
Fill confirmation latency.
Execution skew между legs.
Delta mismatch duration.
Position PnL, margin ratio, liquidation distance.
ADL queue-position и число ADL-событий.
Predicted funding ConfidenceLevel-распределение.
Capital utilisation по биржам и общий risk-бюджет.
Clock offset и число stop-trading signals по clock skew.
Withdrawal fee, gas, число circuit-breaker срабатываний.
Transfer state duration.
DB pool utilisation и outbox lag.
```
<!-- v2: добавлены метрики ADL, confidence, capital, clock offset, withdrawal -->

### 17.2. Логи

Использовать structured JSON logs с correlation IDs:

```text
system_id
position_id
execution_plan_id
leg_id
client_order_id
exchange
transfer_plan_id
withdrawal_id
incident_id
adl_event_id
```

Запрещено логировать secrets. Уровни:

```text
DEBUG — только локально и с redaction.
INFO — штатные state transitions.
WARN — stale data, retry, partial fill, degradation.
ERROR — операция не выполнена.
CRITICAL — риск ликвидации, unknown position, transfer failure, ADL, compromise.
```

### 17.3. Health endpoint

`/healthz` и `/readyz` должны различать:

```text
Процесс жив.
БД доступна.
Ключи могут быть расшифрованы.
Clock offset в допуске.
Основные exchange adapters healthy.
Private streams для открытых позиций живы.
Нет блокирующего recovery incident.
Circuit breakers (trading/withdrawal) не активны.
```

### 17.4. Поведение при недоступности БД

```text
При недоступности БД (connection errors, timeout):
  Перейти в SAFE_HALT.
  Запретить новые входы.
  Сохранять in-memory состояние in-flight ордеров и позиций для последующего восстановления.
  Продолжать мониторинг открытых позиций через private WS и принимать аварийные решения
    (закрытие при ликвидационном риске) даже без записи в БД, логируя факт в отдельный
    durable-side channel (файл + уведомление), чтобы при восстановлении выполнить reconciliation.
  Не доверять in-memory состоянию как источнику истины после восстановления БД —
    обязательная сверка с биржей и пометка периода «blind» в audit log.
```
<!-- v2: добавлен раздел 17.4 — поведение при недоступности БД -->

---

## 18. Тестирование

### 18.1. Unit tests

Покрыть минимум:

```text
Funding sign и cash-flow calculations (realized и predicted).
Расчёт basis/VWAP.
Joint slippage cap.
Комиссии и slippage.
Decimal rounding и quantity conversion (включая границы int64).
Target quantity constraints (включая counterparty/ADL/risk-budget).
Delta calculations.
Candidate eligibility (включая MinSecondsBeforeFunding, ConfidenceLevel, backtest-флаг).
Funding calendar для равных/разных периодов.
State transition validation.
Exit conditions (включая ADL-выход).
Rebalance 50/50 calculation с учётом fee caps.
Address/network validation.
Encryption/decryption secrets.
clientOrderId генерация и коллизии.
Capital allocation (разрешение contention).
```
<!-- v2: расширены unit tests -->

### 18.2. Adapter contract tests

Для каждой биржи использовать fixtures из актуальной официальной документации и безопасные sandbox/testnet вызовы, если доступны. Тестировать:

```text
REST signature.
Timestamp.
WebSocket authentication.
Parsing instruments.
Parsing funding (realized + predicted + ConfidenceLevel).
Parsing BBO/depth.
Parsing private order/fill/position events.
Создание корректного order payload с clientOrderId.
Ack timeout → query → decide path.
Ошибка/limit response mapping.
ADL indicator parsing, если поддерживается.
SetPositionMode.
```

### 18.3. Симулятор биржи

Реализовать exchange simulator/replay harness, который способен моделировать:

```text
Полное исполнение.
Partial fill.
One-leg rejection.
Задержку ответа.
Таймаут ack (без ответа) для проверки QUERY_THEN_DECIDE.
Дубликаты WebSocket events.
Out-of-order events.
WebSocket disconnect.
REST timeout.
Rate limit.
Funding sign change.
Predicted funding rate drift и ConfidenceLevel decay.
Stale order book.
Liquidation-risk event.
ADL-событие на одном leg.
Withdrawal accepted, но deposit timeout.
Withdrawal с превышением fee cap.
Joint slippage при системном шоке.
```
<!-- v2: расширены сценарии симулятора -->

### 18.4. Нагрузочные и хаос-тесты

Проверить:

```text
7 бирж, полный поток ticker/BBO.
50–100 detailed order books.
Несколько открытых позиций одновременно с contention за капитал.
Рестарт worker во время позиции.
Потеря сети одной биржи (degraded mode остальных).
Замедление PostgreSQL и полный отказ БД.
Очереди на границе лимита.
Clock skew injection.
Graceful shutdown во время набора позиции.
```

Обязательная команда:

```text
go test ./...
go test -race ./...
```

Также выполнить fuzz-тесты парсеров входящих данных и property tests для округлений и дельта-инвариантов.
<!-- v2: добавлены contention, DB-fail, clock skew, graceful shutdown -->

---

## 19. Режимы запуска и rollout

Реализовать режимы:

```text
DRY_RUN:
  Получать реальные market data, но не отправлять реальные ордера/withdrawals.
  Симулировать fills консервативно по BBO/VWAP.

PAPER:
  Вести виртуальный портфель и funding schedule.

TESTNET/DEMO:
  Использовать официальные sandbox среды там, где они существуют.

LIVE:
  Разрешён только после явного включения (со вторым фактором, см. 27) и всех health checks.
```

Порядок production rollout:

```text
1. DRY_RUN на всех семи биржах.
2. Проверить symbol mapping, funding calendar, predicted/Confidence и UI.
3. Подключить одну торговую пару на двух биржах с минимальным notional.
4. Протестировать opening, partial fill repair, ack timeout path, exit и restart recovery.
5. Смоделировать ADL-сценарий и убедиться в корректной нейтрализации.
6. Включить остальные биржи по одной.
7. Ребалансировку сначала в режиме plan-only.
8. Проверить тестовые переводы минимальными суммами, включая fee-cap-check.
9. Только затем разрешить AUTO rebalance при явном подтверждении пользователя.
```
<!-- v2: добавлены predicted/Confidence, ack timeout, ADL-сценарий, fee-cap-check в rollout -->

---

## 20. Развёртывание на отдельном компьютере

Рекомендуемая среда:

```text
Linux (Ubuntu Server/Debian) на отдельном постоянно работающем компьютере.
4+ CPU cores, 8+ GB RAM, SSD.
Стабильный проводной интернет или надёжный резервный канал.
UPS при возможности.
Docker Compose или systemd services.
PostgreSQL с регулярными зашифрованными backup (см. 16.2).
Reverse proxy с TLS для Telegram Mini App API.
Firewall: открыть только необходимые порты.
NTP-сервис с мониторингом (см. 24).
```

Не запускать торговый backend на телефоне. Телефон предназначен для Telegram Mini App, уведомлений и emergency control.

В production установить resource limits и мониторинг:

```text
GOMAXPROCS с запасом для ОС и PostgreSQL.
GOMEMLIMIT.
Bounded queues.
DB connection pool limits.
Log rotation.
Автоперезапуск process manager-ом.
NTP/clock sync и alert при рассинхронизации.
```
<!-- v2: добавлен NTP-сервис и alert -->

---

## 21. Definition of Done и критерии приёмки

Система считается готовой к ограниченному live запуску только если:

```text
1. Все 7 адаптеров имеют документированные актуальные API contracts.
2. Поддержаны только разрешённые LINEAR_USDT_PERPETUAL инструменты.
3. Нет float64 в критической финансовой и торговой логике.
4. Все ключи шифруются и redacted в логах.
5. Mini App валидирует Telegram initData на backend.
6. Полуавтоматический режим требует revalidation перед ордером.
7. Автоматический режим соблюдает все risk limits.
8. Система корректно ведёт funding calendar равных и разных периодов.
9. Различены current/realized и predicted funding; решение о входе опирается на predicted с ConfidenceLevel.
10. Набор позиции обрабатывает partial fill, ack timeout (QUERY_THEN_DECIDE) и 60/50-подобный сценарий.
11. Координированное закрытие является основным механизмом выхода; joint slippage cap контролируется.
12. После рестарта выполняется reconciliation и не допускается blind trading.
13. Ребалансировка реализует test-transfer barrier для всех маршрутов; внутренний перевод vs on-chain различены.
14. Ошибка тестового депозита блокирует основной вывод и требует ручной разблокировки (со вторым фактором).
15. Withdrawal circuit breaker, fee/gas caps и правило «успех по зачислению» реализованы.
16. ADL-сценарий покрыт симулятором и recovery; реакция задокументирована.
17. Counterparty risk активен (не опционален); capital allocation разрешает cross-pair contention.
18. Clock offset монитóрится и останавливает торговлю при превышении лимита.
19. Жизненный цикл ключей (ротация), kill switch и второй фактор для критичных мутаций реализованы.
20. Graceful shutdown и поведение при недоступности БД протестированы.
21. Historical backtest-фреймворк готов; критерии допуска пары к live определены.
22. Все ключевые действия отражаются в Telegram и audit log.
23. Проходят unit, race, integration, replay и chaos tests.
24. DRY_RUN отработал заданное время без stale-data, memory leak и неконтролируемого роста очередей.
```
<!-- v2: DoD расширен с 16 до 24 пунктов (E) -->

---

## 22. Порядок работы ИИ-агента

Следуй этому порядку и после каждого этапа предоставляй краткий отчёт, список файлов, тестов, рисков и следующих действий.

```text
Этап 1. Создать ADR, подробную архитектуру, конфигурационную модель и threat model.
Этап 2. Создать Go skeleton, БД-миграции, конфиг, secrets storage, audit/outbox, health checks.
Этап 3. Реализовать общий exchange adapter interface и mock exchange.
Этап 4. Реализовать instrument registry, symbol normalization, market-data cache, WS reconnect/backpressure.
Этап 5. Реализовать scanner, funding calendar (realized/predicted + ConfidenceLevel), formulas, candidate ranking и DRY_RUN.
Этап 6. Реализовать portfolio/risk engine, capital allocation и persistent position state machine.
Этап 7. Реализовать execution coordinator (включая ack timeout, partial fill repair), coordinated close и recovery.
Этап 8. Последовательно интегрировать реальные биржи, начиная с двух; после contract tests добавить остальные.
Этап 9. Реализовать Telegram Bot backend и Mini App dashboard.
Этап 10. Реализовать rebalance plan-only, затем test-transfer barrier, затем guarded live mode.
Этап 11. Добавить observability, load/chaos tests, deployment и runbooks.
Этап 12. Провести DRY_RUN и поэтапный production rollout.
```

### 22.1. Stage gates (обязательно)

Между каждым этапом — **gate**, который нельзя пропустить:

```text
1. Отчёт по этапу в формате 22.4.
2. Пройден self-check чек-лист этапа (22.3).
3. Запуск go test ./... и go test -race ./... без падений.
4. Зафиксированы открытые вопросы и риски.
5. Явное решение «готов продолжать» — без подписанных артефактов текущего этапа
   не приступать к следующему.
```

### 22.2. Антипаттерны (что НЕ делать)

```text
Не копировать endpoint, названия полей или правила подписи из памяти — только актуальная документация.
Не использовать float64 в финансовой/торговой логике.
Не имитировать атомарность между биржами.
Не выполнять blind retry ордера/вывода при таймауте.
Не расширять edge через TODO-заглушки в production-пути.
Не стартовать live без пройденных stage gates и DRY_RUN.
Не считать predicted funding rate гарантированным.
Не игнорировать ADL-риск.
Не доверять in-memory состоянию как источнику истины после рестарта/отказа БД.
Не хардкодить формат symbol конкатенацией.
Не обходить второй фактор для критичных мутаций.
```

### 22.3. Self-check чек-листы по этапам (примеры)

```text
После Этапа 3 (adapter interface + mock):
  - Интерфейс не позволяет стратегии импортить конкретную биржу?
  - Mock проходит все contract-тесты из 18.2?
  - FundingInfo явно различает realized/predicted и содержит ConfidenceLevel?
  - GetADLState и SetPositionMode в интерфейсе?

После Этапа 5 (scanner):
  - Funding calendar строится и для равных, и для разных интервалов?
  - ConfidenceLevel вычисляется и влияет на резерв?
  - MinSecondsBeforeFundingToEnter применяется?

После Этапа 6 (risk/allocation):
  - Capital allocation разрешает contention за капитал на одной бирже?
  - CounterpartyRiskTier учитывается в haircut и exposure-лимитах?
  - Корреляционные лимиты между позициями работают?

После Этапа 7 (execution):
  - Ack timeout обрабатывается как QUERY_THEN_DECIDE?
  - Partial fill одного slice ведёт к repair, а не к слепой повторной отправке?
  - Joint slippage cap проверяется по фактическим исполнениям?
  - ADL-сценарий приводит к экстренной нейтрализации второго leg?

После Этапа 10 (rebalance):
  - Internal transfer и on-chain различены?
  - Fee/gas caps отклоняют невыгодные маршруты?
  - Успех вывода определяется по зачислению, а не по ack?
  - Circuit breaker срабатывает при пороге неудач?
```

### 22.4. Формат отчёта по этапу

```text
Этап: <номер и название>
Артефакты: список созданных/изменённых файлов (с путями).
Тесты: go test ./... и go test -race ./... — статус, покрытие ключевых сценариев.
Self-check: статус чек-листа этапа (22.3).
Риски и открытые вопросы: список с приоритетом.
Следующее действие: конкретный шаг следующего этапа.
Решение gate: готов продолжать / нужны доработки.
```

Если требование противоречит безопасности, API-возможностям биржи или дельта-нейтральности, не реализуй его молча. Объясни конфликт, предложи безопасный вариант и отрази решение в ADR.

Главная цель: не максимальное количество сделок, а контролируемая, измеримая, отказоустойчивая дельта-нейтральная система, которая использует funding и курсовой spread только после учёта реальной исполнимости, комиссий, ликвидности, рисков и ограничений каждой биржи.
<!-- v2: раздел 22 усилен (D): stage gates, антипаттерны, self-checks, формат отчёта -->

---

## 23. Auto-Deleveraging (ADL) и tail-риски короткой ноги

### 23.1. Суть риска

Auto-Deleveraging — механизм биржи: при экстремальных движениях рынка и истощении insurance fund биржа принудительно урезает позиции прибыльных трейдеров по банкротной цене убыточных. Для дельта-нейтральной пары это означает: один leg может быть урезан биржей в одностороннем порядке, мгновенно создав направленную экспозицию, которую нельзя «закрыть по плану», потому что урезанная часть уже реализована. Это один из опаснейших tail-рисков стратегии.

### 23.2. Мониторинг

```text
Там, где биржа публикует ADL indicator / queue-position — опрашивать или подписываться.
Записывать ADL queue-position каждой ноги в MarketSnapshot и в adl_events.
Insurance fund depletion (если публикуется) — отдельная метрика и alert.
ADLRiskScore участвует в candidate score и в принятии решения о входе.
```

### 23.3. Реакция на ADL

```text
1. При обнаружении факта ADL на одном leg (через приватное событие, position change или reconciliation):
   a. Немедленно экстренно нейтрализовать второй leg координированным закрытием.
   b. Перевести позицию в DEGRADED, затем в выход.
   c. Зафиксировать pnl-воздействие и разницу между плановой и фактической ценой ADL.
   d. Отправить CRITICAL-уведомление в Telegram.
2. Не пытаться «восстановить» хедж переоткрытием урезанной ноги автоматически — это новое направленное решение.
```

### 23.4. Лимиты exposure по ADL-риску

```text
ADLExposureLimitPercent per-exchange ограничивает максимальный notional на бирже
с учётом её CounterpartyRiskTier.
Биржам с высоким ADL-риском / низким tier соответствует меньший лимит и больший haircut.
При росте ADL queue-position по открытой позиции — снижение приоритета новых входов
на этой бирже и опционально proactive reduce.
```
<!-- v2: новый раздел 23 -->

---

## 24. Синхронизация часов и таймстампы

### 24.1. NTP-сервис

```text
Сервер обязан поддерживать синхронизацию часов (chrony/ntpd/systemd-timesyncd).
Внутренний компонент clocks/ периодически измеряет смещение относительно биржи
(GetServerTime) и относительно NTP.
MaxAllowedClockOffsetMs — порог; при превышении — stop-trading signal (запрет новых входов).
Open позиции продолжают мониториться; при возможности — закрытие по risk policy.
```

### 24.2. recvWindow и подпись

```text
recvWindow-политика per-exchange фиксируется в контракт-документации.
Перед подписью запроса проверять, что локальный timestamp в допуске;
при рассинхронизации — повторно синхронизировать и только потом подписывать.
Таймстамп в подписи — в часовом поясе/формате конкретной биржи (мс/сек, UTC/local).
```

### 24.3. Clock skew как причина отказа

```text
Превышение clock offset → запрет новых входов, SAFE_HALT при тяжёлом отклонении.
Clock skew учитывается в freshness-логике (6.3): данные с большим skew считаются менее надёжными.
Метрика clock_offset экспортируется в observability.
```
<!-- v2: новый раздел 24 -->

---

## 25. Распределение капитала и координация нескольких позиций

### 25.1. Cross-pair contention

Несколько eligible кандидатов могут одновременно требовать капитал на одной и той же бирже (например, два long leg на Binance). Независимое открытие каждого на максимальный объём превысит лимиты. Поэтому:

```text
Открытие позиций решается как портфельная задача, а не как независимое решение per-candidate.
Входы приоритизируются по Candidate score и ExpectedNetPnL с учётом ConfidenceLevel.
Общий risk-бюджет по бирже и по портфелю — общий ресурс.
```

### 25.2. Риск-бюджет и лимиты

```text
Суммарный notional по бирже <= MaxExposurePerExchangeUSDT (с учётом CounterpartyRiskTier).
Суммарный risk по портфелю != сумма индивидуальных лимитов (диверсификация/концентрация).
CorrelationLimitBetweenPositions и MaxCorrelatedNotionalUSDT ограничивают совместный exposure
по скоррелированным парам (один базовый актив, тесно связанные активы).
Остаток risk-бюджета вычисляется перед каждым новым execution plan (в preflight, 10.1).
```

### 25.3. Алгоритм (минимальный)

```text
1. Собрать текущие eligible кандидаты и их score.
2. Отфильтровать по hard limits (per-exchange exposure, correlation, counterparty).
3. Жадно или по оптимизационному критерию выбрать набор позиций и размеров
   в пределах общего risk-бюджета.
4. Сформировать execution plans только для отобранного набора.
5. Зафиксировать распределение в capital_allocations.
```
<!-- v2: новый раздел 25 -->

---

## 26. Безопасность on-chain вывода и governance трансферов

### 26.1. Принципы

```text
Внутренние переводы (main↔futures) и on-chain выводы — разные операции с разным риском.
On-chain вывод считается успешным только по зачислению на destination, не по ack.
Вывод подчиняется fee/gas caps и circuit breaker.
Двухэтапный test/main барьер (см. 12.6) обязателен.
```

### 26.2. Fee/gas caps

```text
Перед отправкой вывода — оценить комиссию сети.
Если fee > WithdrawalFeeCapUSDT или gas > GasPriceCap — отложить маршрут с понятной причиной.
Не снижать caps автоматически «чтобы провести сейчас» — изменение caps — критичная мутация (второй фактор).
```

### 26.3. Deposit grace-period monitoring

```text
После отправки вывода мониторить зачисление в течение DepositGracePeriodMs.
При истечении — эскалация в incident без слепой повторной отправки.
Сопоставление депозита по withdrawal ID/txid; fallback — по строго заданным признакам.
```

### 26.4. Withdrawal circuit breaker

```text
При WithdrawalFailureThreshold последовательных неудач/таймаутов по бирже/маршруту —
автоматический перевод в REBALANCE_LOCKED без ожидания следующей попытки.
Разблокировка — только пользователем через Mini App со вторым фактором.
```

### 26.5. Replay protection и audit

```text
Все мутации трансферов — с nonce/timestamp и idempotency key (см. 13.5).
Каждый шаг маршрута — отдельная запись в transfer_attempts и audit_log.
Адреса хранятся только как fingerprint; полное значение — только в защищённом audit UI.
```
<!-- v2: новый раздел 26 -->

---

## 27. Жизненный цикл ключей и kill switch

### 27.1. Ротация ключей

```text
Плановая ротация: каждый ключ имеет целевой возраст ротации; UI напоминает о ротации.
Процедура: завести новый ключ → проверить права/IP whitelist → зашифровать и сохранить →
переключить адаптеры на новый ключ → проверить health → инвалидировать старый ключ на бирже.
Аварийная ротация (при компрометации): немедленно инвалидировать старый ключ на бирже,
завести новый, переключить, зафиксировать инцидент и audit.
```

### 27.2. Kill switch

```text
Мгновенное отключение trade- и/или withdrawal-ключей по команде пользователя.
Приводит к SAFE_HALT: запрет новых входов; открытые позиции продолжают мониториться
и могут закрываться по risk policy (если ключ ещё активен для reduce-only).
Полная инвалидация withdrawal-ключа блокирует ребалансировку до ручного восстановления.
```

### 27.3. Второй фактор для критичных мутаций

```text
Изменение адреса вывода.
Разблокировка rebalance после lock.
Переключение в LIVE/AUTO режим.
Снятие RiskSnapAfterMaxDailyLoss.
Изменение fee/gas caps.
Эти мутации требуют второго фактора (Telegram-подтверждение/OTP) в дополнение к initData-авторизации.
```

### 27.4. Audit trail

```text
Все ротации, компрометации, kill-switch-команды и 2FA-мутации — в key_rotation_log и audit_log
с correlation ID, временем, инициатором и результатом.
```
<!-- v2: новый раздел 27 -->

---

## 28. Операционная устойчивость

### 28.1. Graceful shutdown

```text
При сигнале остановки:
  Прекратить новые входы.
  Дождаться подтверждения in-flight ордеров/transfer в пределах shutdown timeout.
  Выполнить финальную сверку позиций/балансов.
  Зафиксировать состояние (positions, transfer plans) в БД.
  Закрыться.
Если shutdown timeout превышен — оставить явный флаг RECOVERY_REQUIRED для следующего старта.
```

### 28.2. Недоступность БД

```text
SAFE_HALT, запрет новых входов.
In-memory состояние in-flight ордеров сохраняется для последующего восстановления.
Аварийные решения по открытым позициям (ликвидационный риск) допускаются через private WS
с логированием в durable-side channel; после восстановления БД — обязательная сверка.
Период «blind» явно помечается в audit log.
```

### 28.3. Partial outage одной биржи

```text
При частичной потере связи с одной биржей — degraded mode:
остальные биржи продолжают работу; затронутая — только monitor и close по risk policy.
Новые позиции, затрагивающие недоступную биржу, блокируются.
Circuit breaker адаптера активируется по правилам 7.4.
```

### 28.4. Деградация latency / сети

```text
При росте REST/WS latency или потере пакетов — снижать частоту сканера,
отключать автоматические входы, усиливать reconciliation.
Не открывать позиции на деградированном канале.
```

### 28.5. RTO/RPO и восстановление

```text
Цели RTO/RPO из 16.2.
Процедура восстановления отрепетирована: restore backup/PITR, восстановление ключей,
reconciliation с биржами, SAFE_HALT до подтверждения一致性.
```
<!-- v2: новый раздел 28 -->

---

## 29. Historical backtest и валидация стратегии

### 29.1. Назначение

До допуска пары/актива к live необходимо убедиться, что стратегевый край устойчив на истории. Backtest-фреймворк — обязательный компонент; `RequireBacktestPass` по умолчанию включён.

### 29.2. Replay-движок

```text
Replay исторических данных: funding (realized), BBO/depth (снимки), predicted funding,
комиссии аккаунта, лимиты инструментов.
Симуляция исполнения консервативно по VWAP/depth с joint slippage.
Учёт ADL-событий по истории там, где данные доступны.
```

### 29.3. Минимальный набор метрик

```text
Hit-rate (доля позиций с положительным net PnL).
Средний entry basis и exit basis.
Funding variance по инструменту.
Распределение ExpectedNetPnL и фактического net PnL.
Max drawdown.
Worst-case joint slippage на истории.
Частота ADL-событий и их pnl-воздействие.
Чувствительность к ConfidenceLevel-порогу.
```

### 29.4. Критерий допуска к live

```text
Пара допускается к live, если:
  hit-rate >= порога,
  средний net PnL положителен с запасом,
  max drawdown в пределах risk-аппетита,
  нет неприемлемой частоты ADL-событий,
  backtest проведён на репрезентативном окне (включая стресс-периоды).
Результат backtest хранится и показывается в UI (14.2) рядом с кандидатом.
```
<!-- v2: новый раздел 29 -->

---

## Источники фактологии (v2)

- Predicted funding rate (форвард-оценка на конец интервала, пересчитывается, не гарантирована): CoinMetrics Product Docs — Predicted Funding Rates.
- Funding rate cap/floor и корректировка следующей ставки при достижении cap: Binance Futures Funding Rates (official FAQ).
- Auto-Deleveraging (ADL) и risk для short/long позиций: Binance Auto-Deleveraging explainer (official FAQ).
```
