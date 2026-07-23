# fandos — funding-rate arbitrage system (Go)

Дельта-нейтральный арбитраж funding rate между криптобиржами (Binance USDT-M / Bybit V5 Linear,
далее — остальные): long perpetual на одной бирже, short на другой, доход — разница funding-платежей
и basis, при жёстком контроле рисков.

Проект строится строго по спецификации
[`fandos1/master_prompt_funding_arbitrage_go_v2.md`](fandos1/master_prompt_funding_arbitrage_go_v2.md)
(12 этапов, stage gates, Definition of Done).

## Структура репозитория

```
fandos1/                 — корень Go-модуля (github.com/thecd/fundarbitrage)
  cmd/                   — точки входа: server (API/Telegram), worker (торговый движок)
  internal/
    decimal/             — двухконтурная арифметика: Decimal (риск) + Fixed64 (hot path); float64 запрещён
    domain/              — доменные типы: биржи, инструменты, ордера, состояния
    config/              — COLD (env, immutable) / HOT (БД, atomic swap) конфигурация
    credentials/         — envelope encryption секретов бирж (AES-256-GCM, DEK + master key)
    auth/                — проверка Telegram WebApp initData (HMAC, allowlist)
    exchange/            — интерфейс адаптера + httpclient, mock, binance, bybit
    instrument/          — канонический реестр инструментов, нормализация символов
    marketdata/          — snapshot-кэш, WS reconnect/backoff/circuit breaker
    orderbook/           — VWAP по стакану, basis, проверка глубины
    scanner/             — Level 3 сканер кандидатов, первичные фильтры, ранжирование
    strategy/            — funding calendar, ExpectedNetPnL, candidate scores
    allocation/          — распределение капитала, cross-pair contention
    portfolio/           — персистентная state machine позиции
    risk/                — риск-лимиты: margin, дельта, daily loss, ADL, counterparty
    execution/           — исполнение: QUERY_THEN_DECIDE, slices, repair, coordinated close
  migrations/            — PostgreSQL схема (0001 — core; 0002 trading; 0003 transfers)
  docs/                  — ARCHITECTURE, CONFIG_MODEL, THREAT_MODEL, ADR, контракты бирж
```

## Ключевые принципы (непереговорные)

- **Никакого float64 в финансовой логике** — только `internal/decimal` (ADR-0002).
- **Дельта-нейтральность** — одна нога никогда не живёт без другой; дисбаланс чинится немедленно (repair).
- **QUERY_THEN_DECIDE** — при таймауте ack состояние ордера запрашивается, никаких слепых retry.
- **PostgreSQL — единственный источник истины**; state machine позиций персистентна.
- **Секреты** — envelope encryption, master key только из env/KMS, никогда в логах и БД открытым текстом.
- **Coordinated close** — обе ноги закрываются синхронно; независимые TP/SL запрещены как основной механизм.

## Разработка

```bash
cd fandos1
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
```

Go ≥ 1.26. Зависимости: `shopspring/decimal` (арифметика). Схема БД — PostgreSQL ≥ 14.

## Статус

Реализованы и покрыты тестами (go test + go test -race зелёные):

- **Этапы 1–7** — архитектура/ADR, домен, decimal, конфиг, секреты, mock-биржа,
  instrument registry, market data, сканер, стратегия, аллокация, портфель/риск,
  исполнение (QUERY_THEN_DECIDE, repair, coordinated close);
- **Этап 8** — реальные адаптеры ВСЕХ 7 бирж (REST + contract-тесты): Binance USDT-M,
  Bybit V5, OKX V5, Bitget V2, KuCoin Futures, MEXC Contract, Gate.io V4. WebSocket —
  у Binance/Bybit готов, у остальных REST-поллинг (WS помечен TODO в коде);
- **Слой данных** — миграции 0001–0003, `internal/repository` (pgx, атомарный Persister),
  transactional outbox с retry/SKIP LOCKED (интеграционные тесты на PostgreSQL 16);
- **Этап 9** — Telegram bot, сессии, Mini App API + dashboard (`webapp/`);
- **Этап 10** — ребалансировка: планировщик 50/50, state machine c test-transfer barrier,
  fee-cap, withdrawal circuit breaker (plan-only режим);
- **Этап 11** — observability (slog+redaction, Prometheus-метрики, health), `cmd/server`
  и `cmd/worker` с graceful shutdown, SAFE_HALT, DB-watchdog, NTP clock-sync,
  Makefile / Dockerfile / docker-compose / CI / RUNBOOK.

Быстрый старт (DRY_RUN, без реальных бирж — mock-контур с демо-данными):

```bash
make migrate DATABASE_URL=postgres://...   # применить миграции
make run-worker-dry                        # сканер печатает кандидатов, /metrics на :9090
make run-server                            # Mini App на :8080
```

- **Этап 12** — движок исполнения (`internal/engine`): полный цикл кандидат → аллокация →
  persistent state machine → preflight → параллельный вход обеих ног → repair дельта-дисбаланса
  → HEDGED → MONITORING → выход (запрос оператора / смена знака funding) → coordinated close;
  ввод API-ключей всех 7 бирж через Mini App (envelope-шифрование, claim владельца),
  live-wiring реальных адаптеров из зашифрованных ключей. Проверено сквозным smoke на 7
  mock-биржах: движок сам выбрал long binance / short gate (максимальный funding-спред),
  открыл дельта-нейтральную позицию и закрыл её по запросу через transactional outbox.

Не реализовано (следующие шаги): keyrotation/kill switch и 2FA-гейт критичных мутаций
(раздел 27); WebSocket для OKX/Bitget/KuCoin/MEXC/Gate (сейчас REST-поллинг); persist
per-leg объёмов в position_legs (сейчас в БД хранится только net delta — точный сплит ног
теряется при рестарте); снапшот кандидатов для серверного API; сверка withdrawal/deposit
эндпоинтов (помечены TODO:VERIFY); load/chaos-тесты; финальный production rollout.
Актуальный статус по пакетам — в [`docs/ARCHITECTURE.md`](fandos1/docs/ARCHITECTURE.md).

⚠️ **Не подключать реальные ключи и не включать LIVE-режим** до прохождения DRY_RUN
и чек-листов раздела 21 мастер-промпта (см. [`docs/RUNBOOK.md`](docs/RUNBOOK.md)).
