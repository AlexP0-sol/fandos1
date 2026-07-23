# Конфигурационная модель

**Связанные разделы промпта:** 5 (настройки), 15.3 (HOT/COLD), 24 (clocks), 27 (key rotation).

## 1. Принципы

1. **Два источника конфигурации:**
   - **Process config** (стартовый): `config.yaml` + environment variables. COLD параметры — только здесь.
   - **User settings** (БД, `strategy_settings`): параметры стратегии/риска, меняемые через Mini App. HOT параметры.
2. **Категории (раздел 15.3):**
   - **HOT** — перезагружается без рестарта worker. Реализовано через подписку на изменения в БД (LISTEN/NOTIFY или polling) + atomic pointer swap.
   - **COLD** — требует рестарта: структура бирж, параметры БД, мастер-ключ, listen-адреса.
3. **Валидация:** backend валидирует каждое значение (диапазоны, обязательность, опасные комбинации). UI показывает диапазоны, defaults и предупреждения.
4. **Defaults безопасности:** все risk-лимиты — консервативные; AUTO-режимы и ребаланс выключены по умолчанию.

## 2. COLD (process config: config.yaml + env)

| Параметр | Env | Default | Описание |
|---|---|---|---|
| `server.http_addr` | `HTTP_ADDR` | `:8080` | HTTP listen |
| `server.public_base_url` | `PUBLIC_BASE_URL` | — | Внешний URL для Telegram webhook / Mini App |
| `db.dsn` | `DATABASE_URL` | — | PostgreSQL DSN |
| `db.max_open_conns` | `DB_MAX_OPEN_CONNS` | 25 | |
| `db.max_idle_conns` | `DB_MAX_IDLE_CONNS` | 5 | |
| `secrets.master_key_env` | `MASTER_KEY_ENV` | `MASTER_KEY` | Имя env-переменной с master key |
| `secrets.kms_provider` | `KMS_PROVIDER` | `env` | `env` \| `aws` \| `gcp` (внешний secret manager) |
| `exchanges[].id` | — | — | Список включённых бирж (COLD: добавление новой биржи — рестарт) |
| `exchanges[].rest_base_url` | — | — | |
| `exchanges[].ws_base_url` | — | — | |
| `exchanges[].testnet` | — | false | |
| `clocks.ntp_servers` | `NTP_SERVERS` | `pool.ntp.org` | |
| `clocks.max_offset_ms` | `MAX_CLOCK_OFFSET_MS` | 500 | Превышение → stop-trading |
| `clocks.sync_interval` | `CLOCK_SYNC_INTERVAL` | 30s | |
| `telemetry.prometheus_addr` | `PROM_ADDR` | `:9090` | |
| `telemetry.otlp_endpoint` | `OTLP_ENDPOINT` | — | |
| `lifecycle.shutdown_timeout` | `SHUTDOWN_TIMEOUT` | 30s | |
| `logging.level` | `LOG_LEVEL` | `info` | |
| `run_mode` | `RUN_MODE` | `dry_run` | `dry_run` \| `paper` \| `testnet` \| `live` |

**Master encryption key** — ТОЛЬКО в env/secret manager, никогда не в config.yaml и не в БД.

## 3. HOT (user settings: БД → Mini App)

Полный список из раздела 5 промпта v2. Сгруппированы по категориям UI.

### 3.1. Стратегия поиска (5.1)
`FundingSearchMode`, `RequireAlignedFundingTimes`, `MaxFundingTimeSkewMinutes`, `MinFundingSpread`, `MinEntryBasis`, `MaxAdverseEntryBasis`, `MinExpectedNetPnLUSDT`, `MinExpectedNetROI`, `MinConfidenceLevel`, `MinQuoteVolume24hUSDT`, `MinOrderBookDepthUSDT`, `MinOpenInterestUSDT`, `MaxDataAgeMs`, `MaxFundingDataAgeMs`, `MinSecondsBeforeFundingToEnter`, `AllowedExchanges`, `AllowedAssets`/`blocked`, `RequireBacktestPass`.

### 3.2. Риск и капитал (5.2)
`Leverage`, `MarginMode`, `PositionMode`, `MaxInitialMarginPercentPerExchange`, `MaxPortfolioMarginPercent`, `MaxNotionalPerPositionUSDT`, `MaxNotionalPerAssetUSDT`, `MaxOpenPositions`, `FreeMarginReservePercent`, `MaxDailyLossUSDT`, `MaxDailyLossPercent`, `MaxPositionLossUSDT`, `MaxPositionLossPercent`, `MinimumDistanceToLiquidationPercent`, `EmergencyMarginRatio`, `DeltaToleranceBase`, `DeltaToleranceUSD`, `MaxDeltaRepairTimeMs`, `MaxHoldingTime`, `MaxFundingEventsToHold`, `JointSlippageCapBps`, `MaxExposurePerExchangeUSDT`, `CounterpartyRiskTier` (per-exchange), `CounterpartyHaircutPercent` (per tier), `CorrelationLimitBetweenPositions`, `MaxCorrelatedNotionalUSDT`, `MarginWarningThresholdPercent`, `MarginWarningAction`, `RiskSnapAfterMaxDailyLoss`, `ADLExposureLimitPercent` (per-exchange).

### 3.3. Исполнение (5.3)
`OrderMode`, `SlicesCount`, `SliceIntervalMs`, `MaxExecutionSkewMs`, `OrderAckTimeoutMs`, `FillConfirmationTimeoutMs`, `AckTimeoutBehavior`, `PartialFillSlicePolicy`, `CompensationAttempts`, `CloseProtectionTicks`, `MaxCloseRequotes`, `CloseDeadlineMs`, `UltimateEmergencyClosePolicy`, `ClientOrderIdScheme`.

### 3.4. Условия выхода (5.4)
`TakeProfitNetPnLUSDT`, `TakeProfitNetROIPercent`, `StopLossNetPnLUSDT`, `StopLossNetROIPercent`, `ExitFundingThreshold`, `ExitExpectedNetPnLThreshold`, `MaxAdverseBasisPercent`, `TargetBasisExitPercent`, `MaxHoldingTime`, `ExitAfterFundingEvents`, `ExitIfFundingSignChanges`, `ExitIfFundingIntervalChanges`, `ExitIfDataBlindTimeExceeded`, `ExitIfADLDetected`, `ExitIfConfidenceBelowMin`.

### 3.5. Ребалансировка (5.5)
`RebalanceEnabled`, `RebalanceMode`, `TargetBalancePercentPerPair`, `AllowedBalanceImbalancePercent`, `RebalanceMinAmountUSDT`, `RebalanceMaxAmountUSDT`, `RebalanceDailyLimitUSDT`, `TestTransferAmount`, `TestDepositTimeout`, `MainTransferTimeout`, `RequireNoOpenPositionsForRebalance`, `TransferableReservePercent`, `WithdrawalFeeCapUSDT`, `GasPriceCap`, `DepositGracePeriodMs`, `WithdrawalFailureThreshold`.

## 4. Опасные комбинации (обязательные предупреждения в UI)

- `OrderMode = MARKET` → предупреждение о неконтролируемом slippage.
- `UltimateEmergencyClosePolicy = EMERGENCY_MARKET_CLOSE` → предупреждение о риске неконтролируемой цены закрытия; включается только осознанно.
- `RiskSnapAfterMaxDailyLoss = false` → предупреждение об отсутствии авто-останова после дневного убытка.
- `RequireBacktestPass = false` → предупреждение о допуске пар без backtest.
- `MinSecondsBeforeFundingToEnter` < 5 → предупреждение о риске входа в момент обнуления edge.
- `MaxDailyLossUSDT`/`MaxDailyLossPercent` оба пустые → отсутствие дневного стопа.

## 5. Категоризация каждого HOT-параметра

Все HOT-параметры перезагружаются без рестарта. Ни один из них не является COLD. COLD — только параметры процесса (раздел 2). Это разграничение зафиксировано в `internal/config` через два типа: `ColdConfig` (immutable после старта) и `HotSettings` (atomic, обновляемый через БД).
