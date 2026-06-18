-- LLMGW V1 schema (design §8). Config tables are edited by hand; runtime-state tables
-- are written by the gateway. The "window" column is quoted because WINDOW is a
-- reserved word in PostgreSQL.

CREATE TABLE project (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE provider (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    type        TEXT NOT NULL CHECK (type IN ('claude_max_oauth', 'anthropic_api', 'openrouter')),
    config_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE budget_limit (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    tag        TEXT,
    dimension  TEXT NOT NULL CHECK (dimension IN ('calls', 'tokens', 'cost_usd')),
    "window"   TEXT NOT NULL CHECK ("window" IN ('hour', 'day')),
    max_value  DOUBLE PRECISION NOT NULL,
    action     TEXT NOT NULL CHECK (action IN ('block', 'warn'))
);

CREATE TABLE route (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id    BIGINT REFERENCES project (id) ON DELETE CASCADE,
    model_pattern TEXT NOT NULL,
    provider_id   BIGINT NOT NULL REFERENCES provider (id) ON DELETE CASCADE
);

CREATE TABLE model_price (
    model               TEXT PRIMARY KEY,
    input_usd_per_mtok  DOUBLE PRECISION NOT NULL,
    output_usd_per_mtok DOUBLE PRECISION NOT NULL
);

CREATE TABLE oauth_token (
    provider_id    BIGINT NOT NULL REFERENCES provider (id) ON DELETE CASCADE,
    account_label  TEXT NOT NULL,
    access_token   TEXT,
    refresh_token  TEXT NOT NULL,
    expires_at     TIMESTAMPTZ,
    cooldown_until TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, account_label)
);

CREATE TABLE usage_event (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts            TIMESTAMPTZ NOT NULL DEFAULT now(),
    project_id    BIGINT NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    tag           TEXT NOT NULL,
    model         TEXT NOT NULL,
    provider      TEXT NOT NULL,
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    cost_usd      DOUBLE PRECISION NOT NULL DEFAULT 0,
    status        TEXT NOT NULL,
    latency_ms    BIGINT,
    error         TEXT
);

CREATE TABLE reservation (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    tag        TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_usage_event_project_tag_ts ON usage_event (project_id, tag, ts);

CREATE INDEX idx_reservation_project_tag ON reservation (project_id, tag);
