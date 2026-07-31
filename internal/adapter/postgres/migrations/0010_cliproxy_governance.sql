ALTER TABLE usage_event RENAME TO legacy_usage_event;
ALTER TABLE budget_limit RENAME TO legacy_budget_limit;
ALTER TABLE model_price RENAME TO legacy_model_price;
DROP TABLE reservation;

CREATE TABLE client_key (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    public_id TEXT NOT NULL UNIQUE,
    digest BYTEA NOT NULL CHECK (octet_length(digest) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);

CREATE TABLE budget_limit (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    dimension TEXT NOT NULL CHECK (dimension IN ('calls','tokens','cost')),
    "window" TEXT NOT NULL CHECK ("window" IN ('hour','day')),
    max_value DOUBLE PRECISION NOT NULL CHECK (
      max_value >= 0
      AND max_value < 'Infinity'::double precision
      AND (dimension = 'cost' OR max_value = trunc(max_value))
    ),
    action TEXT NOT NULL CHECK (action IN ('block','warn')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, dimension, "window", action)
);

CREATE TABLE request_event (
    id UUID PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    client_key_id BIGINT NOT NULL REFERENCES client_key(id),
    operation TEXT NOT NULL CHECK (operation IN ('generation','metadata')),
    requested_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    requested_model TEXT,
    state TEXT NOT NULL CHECK (state IN ('in_flight','completed')),
    accounting_state TEXT NOT NULL CHECK (
      accounting_state IN ('pending','observed','accounting_unknown','resolved_zero','not_applicable')
    ),
    downstream_status INTEGER,
    accounting_resolved_at TIMESTAMPTZ
);

CREATE TABLE usage_attempt (
    id UUID PRIMARY KEY,
    request_id UUID NOT NULL REFERENCES request_event(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    executor_type TEXT NOT NULL,
    resolved_model TEXT NOT NULL,
    requested_alias TEXT NOT NULL,
    upstream_auth_id TEXT NOT NULL,
    upstream_auth_type TEXT NOT NULL,
    input_tokens BIGINT NOT NULL CHECK (input_tokens >= 0),
    output_tokens BIGINT NOT NULL CHECK (output_tokens >= 0),
    reasoning_tokens BIGINT NOT NULL CHECK (reasoning_tokens >= 0),
    cache_read_tokens BIGINT NOT NULL CHECK (cache_read_tokens >= 0),
    cache_creation_tokens BIGINT NOT NULL CHECK (cache_creation_tokens >= 0),
    total_tokens BIGINT NOT NULL CHECK (total_tokens >= 0),
    unclassified_tokens BIGINT NOT NULL CHECK (unclassified_tokens >= 0),
    service_tier TEXT NOT NULL,
    response_service_tier TEXT NOT NULL,
    failed BOOLEAN NOT NULL,
    upstream_status INTEGER,
    latency_ms BIGINT NOT NULL CHECK (latency_ms >= 0),
    ttft_ms BIGINT NOT NULL CHECK (ttft_ms >= 0),
    cost_usd DOUBLE PRECISION CHECK (
      cost_usd >= 0 AND cost_usd < 'Infinity'::double precision
    ),
    pricing_state TEXT NOT NULL CHECK (pricing_state IN ('priced','unknown_pricing')),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE model_price (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider TEXT NOT NULL,
    model_pattern TEXT NOT NULL,
    service_tier TEXT NOT NULL,
    input_per_million DOUBLE PRECISION CHECK (
      input_per_million >= 0 AND input_per_million < 'Infinity'::double precision
    ),
    output_per_million DOUBLE PRECISION CHECK (
      output_per_million >= 0 AND output_per_million < 'Infinity'::double precision
    ),
    cache_read_per_million DOUBLE PRECISION CHECK (
      cache_read_per_million >= 0 AND cache_read_per_million < 'Infinity'::double precision
    ),
    cache_creation_per_million DOUBLE PRECISION CHECK (
      cache_creation_per_million >= 0 AND cache_creation_per_million < 'Infinity'::double precision
    ),
    effective_from TIMESTAMPTZ NOT NULL,
    UNIQUE (provider, model_pattern, service_tier, effective_from)
);

-- The legacy schema constrained neither uniqueness nor values, and a failed
-- import means the gateway never starts on that database again. Sanitize
-- fail-closed instead of aborting: truncate fractional integer caps and clamp
-- negatives to a full block (both more restrictive than the stored garbage),
-- keep the most restrictive duplicate, and drop only caps no arithmetic can
-- use (NaN, Infinity — excluded by max_value < 'Infinity', above which NaN sorts).
INSERT INTO budget_limit (project_id, dimension, "window", max_value, action)
SELECT DISTINCT ON (project_id, dimension, "window", action)
       project_id, dimension, "window", max_value, action
FROM (
    SELECT project_id,
           CASE dimension WHEN 'cost_usd' THEN 'cost' ELSE dimension END AS dimension,
           "window",
           GREATEST(
               0,
               CASE WHEN dimension = 'cost_usd' THEN max_value ELSE trunc(max_value) END
           ) AS max_value,
           action
    FROM legacy_budget_limit
    WHERE tag IS NULL
) sanitized
WHERE max_value < 'Infinity'::double precision
ORDER BY project_id, dimension, "window", action, max_value;

-- An unpriceable legacy rate is dropped rather than imported: the model then
-- records unknown_pricing, which blocks an active cost budget by design,
-- instead of the whole migration failing its CHECK constraints.
INSERT INTO model_price (
    provider, model_pattern, service_tier,
    input_per_million, output_per_million,
    cache_read_per_million, cache_creation_per_million,
    effective_from
)
SELECT '*', model, '*',
       input_usd_per_mtok, output_usd_per_mtok,
       NULL, NULL, '1970-01-01T00:00:00Z'::timestamptz
FROM legacy_model_price
WHERE input_usd_per_mtok >= 0
  AND input_usd_per_mtok < 'Infinity'::double precision
  AND output_usd_per_mtok >= 0
  AND output_usd_per_mtok < 'Infinity'::double precision;

CREATE INDEX idx_client_key_public_id ON client_key(public_id);
CREATE INDEX idx_budget_limit_project_id ON budget_limit(project_id);
CREATE INDEX idx_request_event_project_requested_at ON request_event(project_id, requested_at);
CREATE INDEX idx_request_event_accounting_completed_at ON request_event(accounting_state, completed_at);
CREATE INDEX idx_usage_attempt_request_id ON usage_attempt(request_id);
CREATE INDEX idx_usage_attempt_created_at ON usage_attempt(created_at);
CREATE INDEX idx_model_price_provider_tier_effective
    ON model_price(provider, service_tier, effective_from DESC);
