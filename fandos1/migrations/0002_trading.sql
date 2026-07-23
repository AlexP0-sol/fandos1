-- 0002_trading.sql — торговые сущности (раздел 16, 16.1 промпта v2):
-- инструменты, позиции, ордера, fills, funding, снимки счетов, аллокации,
-- ADL-события, position mode, статус подключений, уведомления.
-- Все денежные/количественные величины — NUMERIC (принцип 1.2.1: никакого float).

BEGIN;

-- Статус подключения к бирже (для dashboard 14.1 и partial outage 28).
CREATE TABLE exchange_connection_status (
    exchange       TEXT PRIMARY KEY,
    rest_healthy   BOOLEAN NOT NULL DEFAULT FALSE,
    ws_public_ok   BOOLEAN NOT NULL DEFAULT FALSE,
    ws_private_ok  BOOLEAN NOT NULL DEFAULT FALSE,
    last_rest_ok   TIMESTAMPTZ,
    last_ws_msg    TIMESTAMPTZ,
    details        JSONB,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Канонический реестр инструментов (6.1); персистентная копия для рестартов и UI.
CREATE TABLE exchange_instruments (
    instrument_id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    exchange          TEXT NOT NULL,
    exchange_symbol   TEXT NOT NULL,
    canonical_asset   TEXT NOT NULL,            -- BTC, ETH, ...
    quote_asset       TEXT NOT NULL DEFAULT 'USDT',
    instrument_type   TEXT NOT NULL DEFAULT 'LINEAR_USDT_PERP',
    status            TEXT NOT NULL,            -- active / reduce_only / delisted / suspended
    contract_multiplier NUMERIC NOT NULL DEFAULT 1,
    qty_step          NUMERIC NOT NULL,
    min_qty           NUMERIC NOT NULL,
    tick_size         NUMERIC NOT NULL,
    max_leverage      NUMERIC,
    funding_interval_sec BIGINT NOT NULL,
    refreshed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (exchange, exchange_symbol)
);
CREATE INDEX ON exchange_instruments (canonical_asset);

-- Соответствие канонический актив ↔ биржевой символ (6.1);
-- отдельно от instruments: правки маппинга не должны трогать рыночные атрибуты.
CREATE TABLE symbol_mappings (
    mapping_id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    canonical_asset TEXT NOT NULL,
    exchange        TEXT NOT NULL,
    exchange_symbol TEXT NOT NULL,
    is_manual       BOOLEAN NOT NULL DEFAULT FALSE,  -- ручное исключение нормализации
    note            TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (exchange, exchange_symbol)
);
CREATE INDEX ON symbol_mappings (canonical_asset);

-- Позиция (пара ног) — persistent state machine (10, 16.1).
CREATE TABLE positions (
    position_id      TEXT PRIMARY KEY,               -- совпадает с domain.PositionID
    strategy         TEXT NOT NULL DEFAULT 'funding_arbitrage',
    canonical_asset  TEXT NOT NULL,
    state            TEXT NOT NULL,                  -- domain.PositionState
    entry_reason     TEXT,
    long_exchange    TEXT NOT NULL,
    short_exchange   TEXT NOT NULL,
    target_qty       NUMERIC NOT NULL,
    actual_delta     NUMERIC NOT NULL DEFAULT 0,
    position_mode    TEXT NOT NULL DEFAULT 'one_way',
    counterparty_tier_snapshot JSONB,                -- tier обеих бирж на момент входа
    entry_at         TIMESTAMPTZ,
    exit_at          TIMESTAMPTZ,
    exit_reason      TEXT,
    realised_pnl     NUMERIC NOT NULL DEFAULT 0,
    funding_pnl      NUMERIC NOT NULL DEFAULT 0,
    fees_paid        NUMERIC NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (long_exchange <> short_exchange)
);
CREATE INDEX ON positions (state);
CREATE INDEX ON positions (canonical_asset, state);

-- История переходов state machine (10; отладка и audit).
CREATE TABLE position_transitions (
    transition_id  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    position_id    TEXT NOT NULL REFERENCES positions(position_id),
    from_state     TEXT NOT NULL,
    to_state       TEXT NOT NULL,
    reason         TEXT,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON position_transitions (position_id, occurred_at);

-- Нога позиции (16.1).
CREATE TABLE position_legs (
    leg_id             TEXT PRIMARY KEY,             -- domain.LegID
    position_id        TEXT NOT NULL REFERENCES positions(position_id),
    exchange           TEXT NOT NULL,
    exchange_symbol    TEXT NOT NULL,
    side               TEXT NOT NULL CHECK (side IN ('long','short')),
    contract_qty       NUMERIC NOT NULL DEFAULT 0,
    base_qty           NUMERIC NOT NULL DEFAULT 0,
    contract_multiplier NUMERIC NOT NULL DEFAULT 1,
    entry_vwap         NUMERIC,
    mark_price         NUMERIC,
    liquidation_price  NUMERIC,
    margin_mode        TEXT,
    margin_ratio       NUMERIC,
    isolated_margin    NUMERIC,
    status             TEXT NOT NULL,
    adl_queue_position NUMERIC,                      -- [0,1], если биржа публикует
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (position_id, side)
);
CREATE INDEX ON position_legs (position_id);

-- Immutable execution plan (7 этап; план набора slices).
CREATE TABLE execution_plans (
    plan_id       TEXT PRIMARY KEY,
    position_id   TEXT NOT NULL REFERENCES positions(position_id),
    kind          TEXT NOT NULL CHECK (kind IN ('ENTRY','EXIT','REPAIR','REBALANCE_CLOSE')),
    payload       JSONB NOT NULL,                    -- slices, лимиты, protection ticks
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_by TEXT REFERENCES execution_plans(plan_id)
);
CREATE INDEX ON execution_plans (position_id);

-- Ордер (16.1). raw_response_ref — ссылка на redacted JSONB, не сырой ответ.
CREATE TABLE orders (
    order_pk          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    exchange          TEXT NOT NULL,
    exchange_order_id TEXT,
    client_order_id   TEXT NOT NULL,
    position_id       TEXT REFERENCES positions(position_id),
    leg_id            TEXT REFERENCES position_legs(leg_id),
    side              TEXT NOT NULL CHECK (side IN ('long','short')),
    reduce_only       BOOLEAN NOT NULL DEFAULT FALSE,
    order_mode        TEXT NOT NULL,                 -- market / limit / marketable_limit_ioc
    time_in_force     TEXT,
    requested_qty     NUMERIC NOT NULL,
    price             NUMERIC,
    filled_qty        NUMERIC NOT NULL DEFAULT 0,
    avg_fill_price    NUMERIC,
    fees              NUMERIC NOT NULL DEFAULT 0,
    status            TEXT NOT NULL,
    ack_state         TEXT NOT NULL DEFAULT 'acked'  -- acked / queried / timed_out (10.2)
        CHECK (ack_state IN ('acked','queried','timed_out')),
    exchange_ts       TIMESTAMPTZ,
    raw_response_ref  JSONB,                         -- ТОЛЬКО после redaction (16.1)
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (exchange, client_order_id)
);
CREATE INDEX ON orders (position_id);
CREATE INDEX ON orders (exchange, exchange_order_id);
CREATE INDEX ON orders (status) WHERE status IN ('new','acknowledged','partially_filled');

-- Fill (16.1).
CREATE TABLE fills (
    fill_pk           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    exchange          TEXT NOT NULL,
    exchange_fill_id  TEXT,
    exchange_order_id TEXT,
    client_order_id   TEXT NOT NULL,
    position_id       TEXT REFERENCES positions(position_id),
    leg_id            TEXT REFERENCES position_legs(leg_id),
    side              TEXT NOT NULL,
    base_qty          NUMERIC NOT NULL,
    price             NUMERIC NOT NULL,
    fee               NUMERIC NOT NULL DEFAULT 0,
    fee_asset         TEXT,
    is_maker          BOOLEAN NOT NULL DEFAULT FALSE,
    exchange_ts       TIMESTAMPTZ NOT NULL,
    recorded_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (exchange, exchange_fill_id)
);
CREATE INDEX ON fills (position_id);
CREATE INDEX ON fills (client_order_id);

-- Фактические funding-платежи (3.2: подтверждение обязательно).
CREATE TABLE funding_payments (
    payment_pk    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    exchange      TEXT NOT NULL,
    exchange_symbol TEXT NOT NULL,
    position_id   TEXT REFERENCES positions(position_id),
    leg_id        TEXT REFERENCES position_legs(leg_id),
    amount        NUMERIC NOT NULL,                  -- со знаком: + получено, − уплачено
    rate          NUMERIC NOT NULL,
    notional      NUMERIC,
    funding_time  TIMESTAMPTZ NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (exchange, exchange_symbol, funding_time, position_id)
);
CREATE INDEX ON funding_payments (position_id);

-- Периодические снимки балансов/маржи по биржам (14.1, reconciliation).
CREATE TABLE account_snapshots (
    snapshot_pk    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    exchange       TEXT NOT NULL,
    equity         NUMERIC NOT NULL,
    available      NUMERIC NOT NULL,
    used_margin    NUMERIC NOT NULL DEFAULT 0,
    unrealised_pnl NUMERIC NOT NULL DEFAULT 0,
    details        JSONB,
    taken_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON account_snapshots (exchange, taken_at DESC);

-- Распределение risk-бюджета по позициям (25).
CREATE TABLE capital_allocations (
    allocation_id  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    position_id    TEXT REFERENCES positions(position_id),
    canonical_asset TEXT NOT NULL,
    long_exchange  TEXT NOT NULL,
    short_exchange TEXT NOT NULL,
    notional_usdt  NUMERIC NOT NULL,
    granted_qty    NUMERIC NOT NULL,
    reason         TEXT,
    allocated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at    TIMESTAMPTZ
);
CREATE INDEX ON capital_allocations (position_id);
CREATE INDEX ON capital_allocations (released_at) WHERE released_at IS NULL;

-- ADL-события и реакция (23, 16.1).
CREATE TABLE adl_events (
    adl_event_id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    exchange       TEXT NOT NULL,
    exchange_symbol TEXT NOT NULL,
    leg_id         TEXT REFERENCES position_legs(leg_id),
    position_id    TEXT REFERENCES positions(position_id),
    detected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    queue_position NUMERIC,
    action_taken   TEXT NOT NULL,                    -- halt_entries / coordinated_close / rebuild / accept
    resulting_state TEXT,
    pnl_impact     NUMERIC
);
CREATE INDEX ON adl_events (position_id);

-- Требуемый/фактический position mode по биржам (16).
CREATE TABLE position_mode_state (
    exchange       TEXT PRIMARY KEY,
    required_mode  TEXT NOT NULL DEFAULT 'one_way',
    actual_mode    TEXT,
    margin_mode    TEXT,
    verified_at    TIMESTAMPTZ,
    is_consistent  BOOLEAN NOT NULL DEFAULT FALSE
);

-- Уведомления Telegram: дедупликация и rate-limit (14.5).
CREATE TABLE notifications (
    notification_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind           TEXT NOT NULL,                    -- position_opened / adl_warning / safe_halt / ...
    severity       TEXT NOT NULL DEFAULT 'INFO' CHECK (severity IN ('INFO','WARN','ERROR','CRITICAL')),
    dedup_key      TEXT,                             -- kind+entity: подавление повторов
    payload        JSONB NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at        TIMESTAMPTZ,
    suppressed     BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX ON notifications (sent_at) WHERE sent_at IS NULL AND NOT suppressed;
CREATE INDEX ON notifications (dedup_key, created_at DESC);

COMMIT;
