-- 0001_core.sql — идентичность (single-tenant, ADR-0001), секреты, настройки,
-- системные состояния, audit/outbox. Торговые сущности — 0002, трансферы — 0003.
-- Соответствует разделу 16 промпта v2.

BEGIN;

-- ADR-0001: таблица подготовлена под multi-tenant, runtime — единственный tenant.
CREATE TABLE users (
    user_id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id    TEXT NOT NULL UNIQUE DEFAULT 'default',
    telegram_id  BIGINT NOT NULL UNIQUE,
    is_admin     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE telegram_sessions (
    session_id   UUID PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(user_id),
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON telegram_sessions (expires_at);

-- Секреты: только AEAD-blob (internal/credentials), AAD связывает строку с контекстом.
CREATE TABLE exchange_credentials (
    credential_id  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(user_id),
    exchange       TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('trade','withdrawal')),
    key_fingerprint TEXT NOT NULL,          -- masked, для UI (13.1)
    blob_version   SMALLINT NOT NULL,
    enc_dek        BYTEA NOT NULL,
    ciphertext     BYTEA NOT NULL,
    rotated_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at     TIMESTAMPTZ,
    UNIQUE (user_id, exchange, kind)
);

-- HOT settings: singleton-строка с JSONB payload; NOTIFY для hot-reload (15.3).
CREATE TABLE strategy_settings (
    singleton    BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    version      BIGINT NOT NULL DEFAULT 1,
    payload      JSONB NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE FUNCTION notify_hot_settings() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('hot_settings', NEW.version::text);
    RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER trg_hot_settings AFTER INSERT OR UPDATE ON strategy_settings
    FOR EACH ROW EXECUTE FUNCTION notify_hot_settings();

-- Глобальные состояния (4.3): персистентно, не только в памяти.
CREATE TABLE system_locks (
    lock_name    TEXT PRIMARY KEY,   -- SAFE_HALT, TRADING_LOCKED, REBALANCE_LOCKED, ...
    engaged      BOOLEAN NOT NULL DEFAULT FALSE,
    reason       TEXT,
    engaged_at   TIMESTAMPTZ,
    released_at  TIMESTAMPTZ
);

-- Append-only audit (1.2.9). UPDATE/DELETE запрещены на уровне прав роли приложения.
CREATE TABLE audit_log (
    audit_id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor          TEXT NOT NULL,          -- user:<id> | system:<component>
    action         TEXT NOT NULL,
    correlation_id TEXT,
    params         JSONB,                  -- ОБЯЗАТЕЛЬНО после redaction
    result         TEXT NOT NULL
);
CREATE INDEX ON audit_log (occurred_at);
CREATE INDEX ON audit_log (correlation_id);

-- Transactional outbox (15.2).
CREATE TABLE outbox_events (
    event_id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind         TEXT NOT NULL,
    payload      JSONB NOT NULL,
    processed_at TIMESTAMPTZ
);
CREATE INDEX ON outbox_events (processed_at) WHERE processed_at IS NULL;

-- Idempotency мутаций UI→backend (13.5) и внешних операций.
CREATE TABLE idempotency_keys (
    idem_key    TEXT PRIMARY KEY,
    scope       TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    result_hash TEXT
);

CREATE TABLE incidents (
    incident_id  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    severity     TEXT NOT NULL CHECK (severity IN ('WARN','ERROR','CRITICAL')),
    kind         TEXT NOT NULL,
    details      JSONB,
    opened_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ
);

-- Раздел 24: состояние clock sync.
CREATE TABLE clock_sync_state (
    source       TEXT PRIMARY KEY,          -- ntp | exchange:<id>
    offset_ms    BIGINT NOT NULL,
    measured_at  TIMESTAMPTZ NOT NULL,
    within_limit BOOLEAN NOT NULL
);

-- Раздел 27: журнал ротаций/kill switch.
CREATE TABLE key_rotation_log (
    rotation_id  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    credential_id BIGINT REFERENCES exchange_credentials(credential_id),
    action       TEXT NOT NULL CHECK (action IN ('PLANNED_ROTATION','EMERGENCY_ROTATION','KILL_SWITCH')),
    initiator    TEXT NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    details      JSONB
);

-- Seed: ровно один владелец (критерий приёмки ADR-0001).
-- telegram_id=0 — placeholder; первичная привязка владельца (setup) обновит его.
INSERT INTO users (tenant_id, telegram_id) VALUES ('default', 0) ON CONFLICT DO NOTHING;
INSERT INTO system_locks (lock_name, engaged) VALUES
 ('SAFE_HALT', FALSE), ('TRADING_LOCKED', FALSE), ('REBALANCE_LOCKED', FALSE)
 ON CONFLICT (lock_name) DO NOTHING;

COMMIT;
