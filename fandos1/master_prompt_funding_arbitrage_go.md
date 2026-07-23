# МАСТЕР-ПРОМТ ДЛЯ ИИ-АГЕНТА
## Production-система арбитража фандинга и курсового спреда на Go

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
6. Не открывать новую позицию на stale данных, при неуверенном состоянии баланса/позиции, при нарушении дельты, при отключённом private WebSocket или при active critical incident.
7. Не хранить API secrets, passphrases, Telegram bot token и master encryption key в исходном коде, логах, telemetry, сообщениях Telegram или нешифрованной БД.
8. Приоритет — сохранение дельта-нейтральности, контроль ликвидационного риска и сохранность средств, а не максимизация количества сделок.
9. Каждое действие должно быть аудируемым: кто, когда, почему, с какими параметрами и каким фактическим результатом его выполнил.
10. Все торговые и выводные API-ключи должны быть привязаны к IP сервера на стороне биржи, если эта возможность доступна.

### 1.3. Политика отказа

```text
Нет актуальных данных → нет нового входа.
Нет достоверного состояния позиции → остановить входы, восстановить состояние.
Есть неустранимый перекос long/short → закрыть excess и нейтрализовать риск.
Есть риск ликвидации → приоритетное координированное закрытие.
Не подтверждён тестовый депозит ребалансировки → не отправлять основные суммы.
Ошибка вывода → REBALANCE_LOCKED до ручной разблокировки пользователя.
```

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
```

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

Для двухноговой позиции:

```text
ExpectedFundingPnL = sum(ExpectedFundingCashFlow всех событий в горизонте удержания)
```

Фактический funding PnL не должен вычисляться только формулой: после расчёта он обязан быть подтверждён по private account event, funding history, ledger или изменению баланса, в зависимости от возможностей конкретной биржи.

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
FundingUncertaintyReserve
MaxAllowedSlippageBps
```

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

---

## 4. Режимы работы

### 4.1. Полуавтоматический режим

В полуавтоматическом режиме система:

```text
Сканирует рынок.
Нормализует и ранжирует кандидаты.
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

`SAFE_HALT` означает: запретить новые входы; открытые позиции продолжать мониторить и закрывать по risk policy.

`REBALANCE_LOCKED` означает: запретить новый ребаланс и новые входы до ручной разблокировки; все активные позиции сопровождать в соответствии с risk policy.

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
MinQuoteVolume24hUSDT
MinOrderBookDepthUSDT
MinOpenInterestUSDT, если данные доступны
MaxDataAgeMs
MaxFundingDataAgeMs
AllowedExchanges
AllowedAssets / blocked assets
```

### 5.2. Риск и капитал

```text
Leverage
MarginMode: ISOLATED по умолчанию
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
```

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
CompensationAttempts
CloseProtectionTicks
MaxCloseRequotes
CloseDeadlineMs
UltimateEmergencyClosePolicy
```

`UltimateEmergencyClosePolicy` должен быть осознанной настройкой. Например:

```text
STRICT_PRICE_GUARD: не ухудшать цену за лимит, но повышается риск не закрыться.
ESCALATING_PRICE_GUARD: постепенно расширять защиту в заданном лимите.
EMERGENCY_MARKET_CLOSE: при риске ликвидации закрыть reduce-only market order.
```

По умолчанию: `ESCALATING_PRICE_GUARD`; при критическом ликвидационном риске — `EMERGENCY_MARKET_CLOSE` только если пользователь явно включил этот режим.

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
```

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
```

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
    Status               InstrumentStatus
}
```

Символы нельзя строить простым конкатенированием `asset + USDT`; нужны явные mappings. Примеры форматов различаются: `BTCUSDT`, `BTC_USDT`, `BTC-USDT-SWAP`, `XBTUSDTM` и другие.

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

    FundingRate          Decimal
    FundingRateCap       *Decimal
    FundingRateFloor     *Decimal
    FundingIntervalSec   int64
    NextFundingTime      time.Time

    ExchangeTimestamp    time.Time
    LocalReceiveTime     time.Time
    SequenceValid        bool
    IsFresh              bool
}
```

### 6.3. Слой свежести и качества данных

Любой snapshot должен иметь:

```text
Источник.
Время биржи.
Время получения локальным сервером.
Возраст.
Статус sequence book.
Статус WebSocket соединения.
```

Правила:

```text
Public data stale → не открывать новые позиции по затронутому источнику.
Private data stale → не открывать/не увеличивать позицию; начать recovery.
Order book sequence gap → snapshot недействителен до REST resync.
Mark/BBO, отличающиеся от разумного диапазона, → quality alert и исключение кандидата.
```

---

## 7. Высокоэффективный сканер без перегрузки компьютера

### 7.1. Архитектура по уровням

Не загружай полный стакан каждого токена на всех биржах. Используй четыре уровня.

#### Level 1 — Instrument registry

Обновлять редко: каждые 30–60 минут и при системных событиях.

```text
Инструменты, статусы, шаги, multiplier, лимиты, funding interval.
```

#### Level 2 — лёгкий all-market мониторинг

По всем допустимым инструментам получать по WebSocket или bulk endpoint:

```text
BBO / ticker.
Mark price.
Funding rate.
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
```

#### Level 4 — preflight перед торговлей

Непосредственно перед заявками повторно проверить:

```text
Актуальность BBO и depth.
Expected Net PnL.
Funding schedule.
Свободную маржу.
Статус инструмента.
Лимиты позиции.
Отсутствие конфликтующих ордеров.
Состояние WebSocket и API.
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
P0 — аварийное закрытие и ликвидационный риск.
P1 — устранение дельта-дисбаланса, мониторинг открытой позиции.
P2 — размещение/отмена/подтверждение ордеров.
P3 — reconciliation позиции и баланса.
P4 — ребалансировка.
P5 — новый scan кандидатов.
P6 — UI, история, отчёты.
```

Каждый exchange adapter обязан иметь независимый rate limiter, request queue, circuit breaker и reconnect backoff с jitter.

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
```

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
    FundingRate       Decimal
    EstimatedNotional Decimal
    EstimatedCashFlow Decimal
    Confidence        ConfidenceLevel
}
```

Не ранжировать разные интервалы только по annualized проценту. Annualized ROI допустимо показывать в UI, но решение о входе должно опираться на реальный календарь событий, прогноз cash flow и риск изменения ставки.

### 8.4. Candidate score

В дополнение к ExpectedNetPnL рассчитывай:

```text
LiquidityScore.
FundingConfidenceScore.
BasisStabilityScore.
ExecutionRiskScore.
CounterpartyRiskScore, если определён политикой.
DataQualityScore.
```

Выводить пользователю объяснение:

```text
Почему кандидат принят.
Почему отклонён.
Какие лимиты стали блокирующим фактором.
Какой funding event является целевым.
Какие комиссии и резервы вычтены.
```

---

## 9. Расчёт объёма, плеча и маржи

### 9.1. Плечо

Плечо не является разрешением использовать весь депозит. По умолчанию использовать `ISOLATED` margin, если биржа и инструмент это поддерживают. Не смешивать средства стратегии с неизвестными ручными позициями.

### 9.2. Предельный объём

Для каждой биржи проверить:

```text
InitialMargin.
Open/close fees.
Worst-case slippage.
Maintenance buffer.
Funding uncertainty reserve.
Position limit.
Max market/IOC quantity.
User max initial margin percentage.
User max notional.
```

Условие:

```text
InitialMargin + Fees + WorstCaseSlippage + SafetyReserve
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
  maxByPortfolioRisk
)
```

После вычисления объём нужно привести к допустимому шагу **на обеих биржах**. Если полученный общий кратный объём даёт недопустимую дельту — кандидат отклоняется или объём понижается.

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
3. Проверить margin mode и leverage.
4. Проверить instrument status, quantity step, min qty, multiplier.
5. Пересчитать funding event calendar.
6. Пересчитать VWAP и expected net PnL.
7. Проверить capital/risk limits.
8. Проверить доступность private WS.
9. Проверить data age, clock sync и API health.
10. Создать immutable execution plan с уникальным ID.
```

### 10.2. Набор позиции slices

Для каждого slice:

```text
1. Проверить, что план ещё выгоден и не устарел.
2. Рассчитать точную допустимую base quantity.
3. Конвертировать её в contract quantity отдельно для каждой биржи.
4. Сформировать уникальные clientOrderId для каждого leg.
5. Одновременно отправить ордера на long и short legs.
6. По умолчанию использовать reduce-safe marketable limit IOC с price protection.
7. Дождаться private WS подтверждения исполнения; при необходимости проверить REST.
8. Сравнить фактические filled base quantities, а не requested quantities.
9. Если дельта допустима — перейти к следующему slice.
10. Если дельта недопустима — остановить дальнейший набор и выполнить repair.
```

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
Expected future funding PnL.
Total net PnL after estimated closing fees/slippage.
Margin ratio, available collateral и liquidation distance каждой ноги.
Data freshness и connection health.
```

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
```

#### P1 — выход по funding risk

```text
Funding rate сменил знак.
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
Ожидаемая стоимость выхода ухудшилась за пределы risk policy.
```

#### P3 — выход по времени

```text
Достигнут MaxHoldingTime.
Собрано MaxFundingEventsToHold.
Достигнут пользовательский planned horizon.
```

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
9. При частичном исполнении сначала выровнять legs до минимального общего остатка.
10. Повторять закрытие оставшейся парной части с bounded количеством requotes.
11. При критическом риске применять UltimateEmergencyClosePolicy.
12. После закрытия обязательно REST+WS reconciliation, отмена остаточных ордеров, проверка нулевых позиций.
13. Зафиксировать итоговый PnL, fees, funding, basis, slippage, причину выхода.
```

Нативные биржевые conditional reduce-only stop orders могут использоваться только как дополнительный аварийный слой, если они поддерживаются и явно отмечены в UI. Они не заменяют основной coordinated exit engine.

---

## 12. Ребалансировка капитала

### 12.1. Общая политика

Ребалансировка выключена по умолчанию и включается пользователем отдельно. Для автоматического вывода требуются отдельные withdrawal API keys с минимальными необходимыми правами, IP whitelist и заранее добавленными whitelist-адресами на самих биржах.

Не выводить средства, задействованные как margin открытой позиции. По умолчанию:

```text
RequireNoOpenPositionsForRebalance = true.
```

### 12.2. Данные маршрута

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
Потребовать явное подтверждение изменения адреса.
После изменения адреса отключать AUTO rebalance до повторного подтверждения.
```

### 12.3. Расчёт цели 50/50

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

Учитывать withdrawal fee, minimum withdrawal, minimum deposit, network fee, precision и уже находящиеся в обработке суммы.

### 12.4. State machine ребалансировки

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

### 12.5. Обязательный двухэтапный механизм

Пример маршрутов:

```text
Binance → Bybit.
Gate → OKX.
```

Алгоритм:

```text
1. Создать единый transfer plan и global rebalance lock.
2. Выполнить precheck всех маршрутов:
   сети, адреса, memo, whitelist, minimum withdrawal/deposit, fee,
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

### 12.6. Ошибка теста или основного перевода

Если тестовый депозит не зачислен за `TestDepositTimeout`, возникла ошибка API, транзакция не сопоставлена, сеть недоступна или возникла иная критическая ошибка:

```text
1. Не отправлять основные суммы ни по одному маршруту.
2. Не выполнять автоматический повторный withdrawal.
3. Перевести систему в REBALANCE_LOCKED.
4. Запретить новые входы в сделки.
5. Продолжать безопасно сопровождать существующие позиции.
6. Отправить критическое уведомление в Telegram.
7. Показать пользователю ID, txid, сеть, сумму, статус и точную причину.
8. Разрешить повторное включение ребалансировки только пользователю через Mini App.
```

Если ошибка произошла после отправки основной суммы, также не делать blind retry и не пытаться автоматически «компенсировать» перевод обратным выводом. Нужны audit trail, reconciliation и ручное решение пользователя.

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
Статус подключения.
```

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
Transfer, если нужен.
Withdraw.
```

Для него обязательны:

```text
IP whitelist.
Whitelist withdrawal addresses на стороне биржи.
Минимальные дневные и разовые лимиты.
Отдельный audit trail.
```

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

---

## 14. Telegram Mini App: обязательный UI/UX

### 14.1. Главный dashboard

Показывать:

```text
Текущий режим: Semi-auto / Auto.
Состояние системы.
Состояние каждой биржи.
WebSocket/API health.
Общий equity.
Свободная и использованная маржа по каждой бирже.
Суммарный realized/unrealized PnL.
Confirmed funding PnL.
Количество открытых позиций.
Активные incident/locks.
```

### 14.2. Scanner

Для каждого кандидата показать:

```text
Токен.
Long exchange/symbol.
Short exchange/symbol.
Funding rate обеих ног.
Funding interval и next funding time обеих ног.
Класс режима: SAME_INTERVAL_ALIGNED / DIFFERENT_INTERVAL и т.д.
Best ask long и best bid short.
Entry VWAP basis.
24h volume и доступную глубину.
Комиссии.
Ожидаемый funding PnL.
Expected basis PnL.
Все резервы.
Expected Net PnL и ROI.
Risk score.
Причины eligibility/rejection.
```

В полуавтоматическом режиме дать кнопки:

```text
Открыть.
Игнорировать.
Добавить актив в blacklist.
Просмотреть полный execution plan.
```

### 14.3. Экран позиции

```text
Статус позиции.
Причина входа.
Фактические long/short base quantities.
DeltaBase / DeltaUSD.
Contract quantities и multiplier.
Leverage, margin mode, margin used.
Entry prices и current executable exit prices.
Mark price, liquidation price, margin ratio.
Funding events: expected и confirmed.
Funding PnL, basis PnL, fees, net PnL.
Open orders.
Последние ошибки и actions.
Кнопка «Принудительно закрыть» с подтверждением.
```

### 14.4. Настройки и ребалансировка

Реализовать все настройки из раздела 5, с пояснениями. Для ребалансировки показать:

```text
Toggle RebalanceEnabled.
Маршруты и сети.
Masked address + address fingerprint.
Memo/tag.
План перевода до запуска.
Тестовые переводы и статусы.
Основные переводы и txid.
Текущий rebalance lock.
Кнопку ручной разблокировки после явного подтверждения.
```

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
Любой SAFE_HALT / REBALANCE_LOCKED.
Ошибка withdrawal/test deposit.
Приближение к risk limits.
Ручная операция пользователя.
```

Не включать секреты в уведомления.

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
  execution/
  risk/
  portfolio/
  rebalance/
  credentials/
  telegram/
  auth/
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
    PlaceOrder(ctx context.Context, req PlaceOrderRequest) (OrderAck, error)
    CancelOrder(ctx context.Context, req CancelOrderRequest) error
    GetOrder(ctx context.Context, req OrderQuery) (Order, error)

    InternalTransfer(ctx context.Context, req InternalTransferRequest) (TransferResult, error)
    Withdraw(ctx context.Context, req WithdrawalRequest) (WithdrawalResult, error)
    GetWithdrawalHistory(ctx context.Context, query TransferQuery) ([]Withdrawal, error)
    GetDepositHistory(ctx context.Context, query TransferQuery) ([]Deposit, error)
    GetNetworkInfo(ctx context.Context, asset string) ([]NetworkInfo, error)
}
```

Все структуры должны быть нормализованными. Особенности exchange API должны оставаться внутри конкретного адаптера.

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

---

## 16. Схема данных PostgreSQL

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
```

### 16.1. Важные свойства таблиц

`positions`:

```text
position_id, strategy, canonical_asset, state, entry reason,
long exchange, short exchange, target qty, actual delta,
entry/exit timestamps, exit reason, realised pnl, funding pnl, fees.
```

`position_legs`:

```text
exchange, symbol, side, contract qty, base qty, multiplier,
entry vwap, mark price, liquidation price, margin info, status.
```

`orders` и `fills`:

```text
exchange order id, client order id, position id, leg id,
side, reduce only, requested qty, filled qty, average price,
fees, status, exchange timestamps, raw response reference.
```

`transfer_attempts`:

```text
plan ID, route ID, phase TEST/MAIN, source/destination,
asset, network, address fingerprint, memo fingerprint,
request ID, withdrawal ID, txid, gross amount, fee, net amount,
status, timeout, failure reason.
```

Секреты не дублировать в audit log или raw JSON. Raw API responses сохранять только после redaction.

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
Transfer state duration.
DB pool utilisation и outbox lag.
```

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
```

Запрещено логировать secrets. Уровни:

```text
DEBUG — только локально и с redaction.
INFO — штатные state transitions.
WARN — stale data, retry, partial fill, degradation.
ERROR — операция не выполнена.
CRITICAL — риск ликвидации, unknown position, transfer failure.
```

### 17.3. Health endpoint

`/healthz` и `/readyz` должны различать:

```text
Процесс жив.
БД доступна.
Ключи могут быть расшифрованы.
Основные exchange adapters healthy.
Private streams для открытых позиций живы.
Нет блокирующего recovery incident.
```

---

## 18. Тестирование

### 18.1. Unit tests

Покрыть минимум:

```text
Funding sign и cash-flow calculations.
Расчёт basis/VWAP.
Комиссии и slippage.
Decimal rounding и quantity conversion.
Target quantity constraints.
Delta calculations.
Candidate eligibility.
Funding calendar для равных/разных периодов.
State transition validation.
Exit conditions.
Rebalance 50/50 calculation.
Address/network validation.
Encryption/decryption secrets.
```

### 18.2. Adapter contract tests

Для каждой биржи использовать fixtures из актуальной официальной документации и безопасные sandbox/testnet вызовы, если доступны. Тестировать:

```text
REST signature.
Timestamp.
WebSocket authentication.
Parsing instruments.
Parsing funding.
Parsing BBO/depth.
Parsing private order/fill/position events.
Создание корректного order payload.
Ошибка/limit response mapping.
```

### 18.3. Симулятор биржи

Реализовать exchange simulator/replay harness, который способен моделировать:

```text
Полное исполнение.
Partial fill.
One-leg rejection.
Задержку ответа.
Дубликаты WebSocket events.
Out-of-order events.
WebSocket disconnect.
REST timeout.
Rate limit.
Funding sign change.
Stale order book.
Liquidation-risk event.
Withdrawal accepted, но deposit timeout.
```

### 18.4. Нагрузочные и хаос-тесты

Проверить:

```text
7 бирж, полный поток ticker/BBO.
50–100 detailed order books.
Несколько открытых позиций.
Рестарт worker во время позиции.
Потеря сети одной биржи.
Замедление PostgreSQL.
Очереди на границе лимита.
```

Обязательная команда:

```text
go test ./...
go test -race ./...
```

Также выполнить fuzz-тесты парсеров входящих данных и property tests для округлений и дельта-инвариантов.

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
  Разрешён только после явного включения и всех health checks.
```

Порядок production rollout:

```text
1. DRY_RUN на всех семи биржах.
2. Проверить symbol mapping, funding calendar и UI.
3. Подключить одну торговую пару на двух биржах с минимальным notional.
4. Протестировать opening, partial fill repair, exit и restart recovery.
5. Включить остальные биржи по одной.
6. Ребалансировку сначала в режиме plan-only.
7. Проверить тестовые переводы минимальными суммами.
8. Только затем разрешить AUTO rebalance при явном подтверждении пользователя.
```

---

## 20. Развёртывание на отдельном компьютере

Рекомендуемая среда:

```text
Linux (Ubuntu Server/Debian) на отдельном постоянно работающем компьютере.
4+ CPU cores, 8+ GB RAM, SSD.
Стабильный проводной интернет или надёжный резервный канал.
UPS при возможности.
Docker Compose или systemd services.
PostgreSQL с регулярными зашифрованными backup.
Reverse proxy с TLS для Telegram Mini App API.
Firewall: открыть только необходимые порты.
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
NTP/clock sync.
```

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
9. Набор позиции обрабатывает partial fill и 60/50-подобный сценарий.
10. Координированное закрытие является основным механизмом выхода.
11. После рестарта выполняется reconciliation и не допускается blind trading.
12. Ребалансировка реализует test-transfer barrier для всех маршрутов.
13. Ошибка тестового депозита блокирует основной вывод и требует ручной разблокировки.
14. Все ключевые действия отражаются в Telegram и audit log.
15. Проходят unit, race, integration, replay и chaos tests.
16. DRY_RUN отработал заданное время без stale-data, memory leak и неконтролируемого роста очередей.
```

---

## 22. Порядок работы ИИ-агента

Следуй этому порядку и после каждого этапа предоставляй краткий отчёт, список файлов, тестов, рисков и следующих действий.

```text
Этап 1. Создать ADR, подробную архитектуру, конфигурационную модель и threat model.
Этап 2. Создать Go skeleton, БД-миграции, конфиг, secrets storage, audit/outbox, health checks.
Этап 3. Реализовать общий exchange adapter interface и mock exchange.
Этап 4. Реализовать instrument registry, symbol normalization, market-data cache, WS reconnect/backpressure.
Этап 5. Реализовать scanner, funding calendar, formulas, candidate ranking и DRY_RUN.
Этап 6. Реализовать portfolio/risk engine и persistent position state machine.
Этап 7. Реализовать execution coordinator, partial fill repair, coordinated close и recovery.
Этап 8. Последовательно интегрировать реальные биржи, начиная с двух; после contract tests добавить остальные.
Этап 9. Реализовать Telegram Bot backend и Mini App dashboard.
Этап 10. Реализовать rebalance plan-only, затем test-transfer barrier, затем guarded live mode.
Этап 11. Добавить observability, load/chaos tests, deployment и runbooks.
Этап 12. Провести DRY_RUN и поэтапный production rollout.
```

Если требование противоречит безопасности, API-возможностям биржи или дельта-нейтральности, не реализуй его молча. Объясни конфликт, предложи безопасный вариант и отрази решение в ADR.

Главная цель: не максимальное количество сделок, а контролируемая, измеримая, отказоустойчивая дельта-нейтральная система, которая использует funding и курсовой spread только после учёта реальной исполнимости, комиссий, ликвидности, рисков и ограничений каждой биржи.
