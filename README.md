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

Этапы 1–7 (архитектура, ядро домена, миграция core-схемы, mock-биржа, сканер, стратегия,
портфель/риск, исполнение) — реализованы и покрыты тестами. Этапы 8–12 (реальные адаптеры
бирж, слой БД, Telegram Mini App, ребалансировка, наблюдаемость, деплой) — в работе;
актуальный статус по пакетам — в [`docs/ARCHITECTURE.md`](fandos1/docs/ARCHITECTURE.md).

⚠️ **Не подключать реальные ключи и не включать LIVE-режим** до прохождения DRY_RUN
и чек-листов раздела 21 мастер-промпта.
