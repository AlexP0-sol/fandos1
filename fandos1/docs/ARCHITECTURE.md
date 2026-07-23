# Архитектура системы funding-арбитража (Go)

**Версия:** 1.0 (соответствует промпту v2)
**Связанный промпт:** `master_prompt_funding_arbitrage_go_v2.md`, разделы 15, 23–29.

## 1. Парадигма

**Модульный монолит** для одного пользователя (ADR-0001). Не микросервисы.
Домены разделены в коде так, чтобы их можно было вынести в сервисы в будущем.
PostgreSQL — единственный источник персистентной бизнес-истины. Go-каналы — не источник истины.

## 2. Процессная модель

Два процесса (могут быть одним бинарником с разными подкомандами):

- **`cmd/server`** — HTTP API, Telegram webhook/long-polling, раздача Mini App frontend, health endpoints.
- **`cmd/worker`** — market data, scanner, strategy, execution, risk loop, rebalance.

Они разделяют одну БД и общаются через таблицы + transactional outbox, не через сетевые вызовы. Это упрощает эксплуатацию на одной машине.

## 3. Слои и домены

Легенда: **[есть]** — пакет/директория присутствует в репозитории сегодня;
**[план]** — запланировано, в репозитории ещё нет.

```
cmd/
  server/   — точка входа HTTP/Telegram API                                [план]
  worker/   — точка входа торгового движка                                 [план]

webapp/     — Mini App frontend (Telegram WebApp)                          [план]
deploy/     — конфигурация деплоя (docker-compose, systemd и т.п.)         [план]

internal/
  app/          — сборка DI, wiring процессов                              [план]
  config/       — конфиг-модель (HOT/COLD категории, см. 15.3)             [есть]
  domain/       — доменные типы: ExchangeID, Side, InstrumentType, состояния и т.д. [есть]
  decimal/      — двухконтурная арифметика (ADR-0002): Decimal (risk) + Fixed64 (hot) [есть]
  exchange/     — adapter.go (интерфейс) + реализации per-биржа            [есть]
  marketdata/   — Level 2 all-market мониторинг, coalescing, backpressure  [есть]
  orderbook/    — локальные стаканы, VWAP, sequence validation              [есть]
  scanner/      — Level 3 кандидаты, первичные фильтры, ranking             [есть]
  strategy/     — funding calendar, ExpectedNetPnL, candidate score         [есть]
  allocation/   — распределение капитала, cross-pair contention (раздел 25) [есть]
  execution/    — coordinator, slices, ack-timeout, partial-fill repair, coordinated close [есть]
  risk/         — risk limits, margin, delta, ADL реакция, counterparty risk [есть]
  portfolio/    — агрегированный портфель, PnL, exposure                    [есть]
  rebalance/    — state machine, test/main barrier, circuit breaker         [план]
  credentials/  — envelope encryption, AEAD, в памяти только на подпись    [есть]
  instrument/   — реестр инструментов, InstrumentRegistry                  [есть] (не перечислен выше)
  keyrotation/  — жизненный цикл ключей, kill switch, 2FA-gate мутаций (раздел 27) [план]
  telegram/     — bot backend, initData validation, Mini App API            [план]
  auth/         — sessions, allowlist Admin IDs, replay-защита (13.5)       [есть]
  clocks/       — NTP/server-time sync, clock offset, recvWindow (раздел 24) [план]
  lifecycle/    — graceful shutdown, DB-unavailable handling (раздел 28)    [план]
  repository/   — PostgreSQL-persisted сущности                             [план]
  outbox/       — transactional outbox                                      [план]
  notifications/— Telegram-уведомления, redaction                           [план]
  audit/        — immutable audit log                                        [план]
  observability/— Prometheus/OTel метрики, structured logs                  [план]
```

## 4. Поток данных (_levels_ 1–4 из раздела 7)

```
Level 1 (30–60 мин): exchange.GetInstruments → InstrumentRegistry
Level 2 (WS/bulk):   marketdata → coalesce → MarketSnapshot cache (BBO, mark, funding predicted/realized)
Level 3 (только кандидаты): orderbook depth 10–50 → VWAP, slippage, fees, ADL queue
Level 4 (preflight): перед ордером — повторная сверка всего
```

Поток принятия решения:
```
scanner → candidates → strategy(funding calendar + ExpectedNetPnL + score)
  → allocation(risk-бюджет, cross-pair contention) → execution plan
  → execution(slices, QUERY_THEN_DECIDE, repair) → position state machine
  → risk loop (P0–P1 monitor) → coordinated close
```

## 5. Глобальные состояния и блокировки

Системные состояния (раздел 4.3): `STARTING, READY, PAUSED_BY_USER, SAFE_HALT, TRADING_LOCKED, REBALANCE_LOCKED, RECOVERY_REQUIRED`.
Хранятся в `system_locks` (БД), не только в памяти. Переходы атомарны и логируются в audit.

Триггеры `SAFE_HALT`: critical incident, `MaxDailyLoss` (если `RiskSnapAfterMaxDailyLoss`), недоступность БД, clock skew > лимита, потеря приватного канала открытой позиции без REST-reconciliation.

## 6. Безопасность (кратко, см. threat model)

- Секреты: envelope encryption, master key — вне БД (env/secret manager), расшифровка только в памяти на подпись.
- Авторизация: Telegram initData validation на backend + allowlist Admin IDs + короткоживущая сессия.
- Критичные мутации (адрес вывода, разблокировка rebalance, переключение LIVE/AUTO, fee caps) — второй фактор (раздел 27).
- Replay-защита мутаций: nonce/timestamp + idempotency key (13.5).
- IP whitelist + whitelist addresses на стороне биржи для withdrawal key.

## 7. Отказоустойчивость (раздел 28)

- **Graceful shutdown:** прекратить входы → дождаться in-flight ордеров в пределах timeout → финальная сверка → persist состояния → exit. При превышении timeout — флаг `RECOVERY_REQUIRED`.
- **Недоступность БД:** `SAFE_HALT`, запрет входов, in-memory состояние in-flight ордеров сохраняется, аварийные закрытия по ликвидационному риску через private WS с логированием в durable-side channel; после восстановления — обязательная сверка с биржей, период «blind» помечается в audit.
- **Partial outage биржи:** degraded mode — остальные продолжают, затронутая только monitor/close.
- **RTO/RPO:** цели в ADR, PITR через WAL archiving, зашифрованные backup вне сервера.

## 8.ADL (раздел 23)

При обнаружении ADL на одном leg → экстренная нейтрализация второго leg координированным закрытием → `DEGRADED` → CRITICAL Telegram alert. `ADLExposureLimitPercent` per-exchange, связан с `CounterpartyRiskTier`.

## 9. Версионирование и миграции

Миграции — пронумерованные SQL-файлы в `migrations/`, применяются при старте (должным образом — через инструмент миграций, например `golang-migrate`). Каждая миграция идемпотентна где возможно; необратимые — с `DOWN` только для разработки.

## 10. Открытые вопросы (будущие ADR)

- ADR-0003: выбор конкретной decimal-библиотеки (`shopspring/decimal` vs `cosmos-sdk`, см. property-тесты).
- ADR-0004: выбор мигратора (golang-migrate vs goose vs dbmate).
- ADR-0005: strategy для Telegram Bot — long polling vs webhook (зависит от network egress).
- ADR-0006: backtest-данных источник (собственный сбор vs сторонний provider) — раздел 29.
