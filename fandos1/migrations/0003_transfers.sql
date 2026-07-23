-- 0003_transfers.sql — ребалансировка и трансферы (разделы 12, 26 промпта v2):
-- адреса, планы, маршруты, попытки (двухэтапный TEST/MAIN), circuit breaker вывода.
-- Все суммы — NUMERIC.

BEGIN;

-- Белый список адресов (26.1): ТОЛЬКО pre-approved, добавление — через 2FA (27.3).
CREATE TABLE wallet_addresses (
    address_id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    exchange          TEXT NOT NULL,                 -- владелец адреса (куда депозит)
    asset             TEXT NOT NULL,
    network           TEXT NOT NULL,                 -- TRC20 / ERC20 / BEP20 / ...
    address           TEXT NOT NULL,
    memo              TEXT,
    address_fingerprint TEXT NOT NULL,               -- усечённый hash для audit (без полного адреса в логах)
    label             TEXT,
    approved_at       TIMESTAMPTZ,                   -- NULL = не одобрен, использовать НЕЛЬЗЯ
    approved_by       TEXT,
    revoked_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (exchange, asset, network, address)
);

-- План ребалансировки (12.5): state machine PLAN → TEST → MAIN → DONE/FAILED.
CREATE TABLE transfer_plans (
    plan_id        TEXT PRIMARY KEY,
    state          TEXT NOT NULL CHECK (state IN
        ('DRAFT','PLANNED','AWAITING_APPROVAL','TEST_SENT','TEST_CONFIRMED',
         'MAIN_SENT','MAIN_CONFIRMED','COMPLETED','FAILED','CANCELLED')),
    reason         TEXT NOT NULL,                    -- rebalance_50_50 / margin_topup / manual
    from_exchange  TEXT NOT NULL,
    to_exchange    TEXT NOT NULL,
    asset          TEXT NOT NULL,
    gross_amount   NUMERIC NOT NULL,
    est_fee        NUMERIC NOT NULL DEFAULT 0,
    net_amount     NUMERIC,
    route_id       BIGINT,                           -- FK ниже, после создания transfer_routes
    dry_run        BOOLEAN NOT NULL DEFAULT TRUE,    -- plan-only режим (этап 10)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ,
    failure_reason TEXT,
    CHECK (from_exchange <> to_exchange)
);
CREATE INDEX ON transfer_plans (state)
    WHERE state NOT IN ('COMPLETED','FAILED','CANCELLED');

-- Маршрут перемещения (12.3): asset+network+адрес назначения+лимиты.
CREATE TABLE transfer_routes (
    route_id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    from_exchange  TEXT NOT NULL,
    to_exchange    TEXT NOT NULL,
    asset          TEXT NOT NULL,
    network        TEXT NOT NULL,
    address_id     BIGINT NOT NULL REFERENCES wallet_addresses(address_id),
    min_amount     NUMERIC NOT NULL DEFAULT 0,
    max_amount     NUMERIC,                          -- NULL = без лимита маршрута
    fee_cap        NUMERIC,                          -- 26.2: максимум допустимой комиссии
    est_duration_min SMALLINT,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (from_exchange, to_exchange, asset, network)
);
ALTER TABLE transfer_plans
    ADD CONSTRAINT transfer_plans_route_fk FOREIGN KEY (route_id) REFERENCES transfer_routes(route_id);

-- Попытка перевода (16.1): двухэтапный механизм TEST → MAIN (12.6).
CREATE TABLE transfer_attempts (
    attempt_id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    plan_id        TEXT NOT NULL REFERENCES transfer_plans(plan_id),
    route_id       BIGINT NOT NULL REFERENCES transfer_routes(route_id),
    phase          TEXT NOT NULL CHECK (phase IN ('TEST','MAIN')),
    kind           TEXT NOT NULL CHECK (kind IN ('INTERNAL','ONCHAIN')),
    source         TEXT NOT NULL,
    destination    TEXT NOT NULL,
    asset          TEXT NOT NULL,
    network        TEXT,
    address_fingerprint TEXT,                        -- 16.1: fingerprint, не полный адрес
    memo_fingerprint    TEXT,
    request_id     TEXT NOT NULL,                    -- наш идемпотентный ID (26.5)
    withdrawal_id  TEXT,                             -- ID биржи
    txid           TEXT,
    gross_amount   NUMERIC NOT NULL,
    fee            NUMERIC,
    net_amount     NUMERIC,
    fee_cap_check  TEXT NOT NULL DEFAULT 'PENDING'   -- 26.2: PASSED / FAILED / PENDING
        CHECK (fee_cap_check IN ('PENDING','PASSED','FAILED')),
    status         TEXT NOT NULL CHECK (status IN
        ('CREATED','SENT','CONFIRMED','TIMED_OUT','FAILED','CANCELLED')),
    timeout_at     TIMESTAMPTZ,
    failure_reason TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (request_id)
);
CREATE INDEX ON transfer_attempts (plan_id);
CREATE INDEX ON transfer_attempts (status)
    WHERE status IN ('CREATED','SENT');

-- Circuit breaker вывода (26.4): состояние по бирже/маршруту.
CREATE TABLE withdrawal_circuit_breaker (
    breaker_id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope          TEXT NOT NULL,                    -- exchange:<id> | route:<route_id> | global
    tripped        BOOLEAN NOT NULL DEFAULT FALSE,
    trip_reason    TEXT,
    failures_count SMALLINT NOT NULL DEFAULT 0,
    tripped_at     TIMESTAMPTZ,
    reset_at       TIMESTAMPTZ,                      -- когда можно пробовать снова (manual reset — NULL)
    manual_reset_required BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scope)
);

COMMIT;
