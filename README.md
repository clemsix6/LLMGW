# LLMGW

A **local LLM gateway**: one self-hosted Go service that fronts LLM providers behind a
stable API, with native per-project / per-tag usage tracking and budget limits.

- Drop-in **Anthropic Messages** API (`POST /v1/messages`) — point any Anthropic SDK at it.
- Governance via headers: `X-Project`, `X-Tags`. Projects auto-create on first use.
- Per-`(project, tag)` budgets in **calls / tokens / cost**, hourly + daily, hard-block.
- **V1 backend:** Claude Max via OAuth (a maintained Go reimplementation of clewdr's
  `/code` path, without the flagged TLS fingerprint). **Later:** Anthropic API keys,
  OpenRouter (any LLM).
- Local only (binds `127.0.0.1`), no auth, Postgres-backed state.

Design: [`docs/specs/2026-06-18-llmgw-design.md`](docs/specs/2026-06-18-llmgw-design.md).

## Deployment

The gateway is a single static binary; configuration lives in environment variables and in
hand-edited Postgres rows. It runs migrations on boot, so a fresh `llmgw` database is brought
up to schema automatically.

### 1. Create the database

LLMGW uses a **separate database** named `llmgw` inside the **existing** Postgres instance — it
does not run its own. Create it once:

```sql
CREATE DATABASE llmgw;
```

(Optionally a dedicated role: `CREATE ROLE llmgw LOGIN PASSWORD '...'; GRANT ALL ON DATABASE llmgw TO llmgw;`.)
The schema is created and migrated by the gateway on its next start — no manual DDL.

### 2. Configure the environment

```sh
cp .env.example .env
# edit .env: set LLMGW_POSTGRES_DSN, LLMGW_OAUTH_REFRESH_TOKENS, LLMGW_CLAUDE_CODE_VERSION
```

Every variable is documented in [`.env.example`](.env.example).

### 3. Run

```sh
docker compose up -d --build
docker compose logs -f llmgw    # watch for "llmgw listening on ..." and the migration log
curl -s http://127.0.0.1:8088/health   # -> ok
```

The compose service publishes the gateway on **`127.0.0.1:8088` only** (host loopback). Inside the
container the app binds `0.0.0.0` (`LLMGW_LISTEN`); the `127.0.0.1:` publish prefix is what keeps it
off the network. It connects to the existing Postgres via `LLMGW_POSTGRES_DSN` (use
`host.docker.internal` as the host when Postgres runs on the docker host; attach to the Postgres
network and use its service name otherwise).

To run without Docker: `set -a && source .env && set +a && go run ./cmd/llmgw` (set
`LLMGW_LISTEN=127.0.0.1:8088`).

## Configuration (edit rows directly)

There is no settings API. Limits, prices, and routes are **rows in the `llmgw` database**, edited
by hand with `psql`. Projects auto-create on first request, so you can set their limits afterward.
Note `"window"` must be quoted — it is a reserved word in PostgreSQL.

**Budgets** — e.g. cap the `news` tag of the `truewallet` project at 50 calls per hour, hard-block:

```sql
INSERT INTO budget_limit (project_id, tag, dimension, "window", max_value, action)
VALUES ((SELECT id FROM project WHERE name = 'truewallet'), 'news', 'calls', 'hour', 50, 'block');
```

Dimensions: `calls | tokens | cost_usd`. Windows: `hour | day` (sliding). Actions: `block | warn`.
A `tag` of `NULL` applies the limit to the whole project (aggregated across every tag).

**Prices** — notional cost is computed from `model_price` (USD per million tokens). A request for a
model with **no price row is blocked (402)** when a cost limit applies (fail-closed). Add or update
a price:

```sql
INSERT INTO model_price (model, input_usd_per_mtok, output_usd_per_mtok)
VALUES ('claude-opus-4-8', 15, 75)
ON CONFLICT (model) DO UPDATE
  SET input_usd_per_mtok = EXCLUDED.input_usd_per_mtok,
      output_usd_per_mtok = EXCLUDED.output_usd_per_mtok;
```

**Routes / providers** — V1 serves every request through the single seeded Claude Max provider
(`provider` + `route` rows are migration-seeded). Multi-provider routing arrives in V2.

Inspect current usage at any time, e.g. spend per tag over the last 24h:

```sql
SELECT tag, COUNT(*) AS calls, SUM(cost_usd) AS cost
FROM usage_event
WHERE project_id = (SELECT id FROM project WHERE name = 'truewallet') AND ts >= now() - interval '24 hours'
GROUP BY tag ORDER BY cost DESC;
```

## Pointing a consumer at the gateway

The gateway exposes the Anthropic **Messages** API at `POST /v1/messages`. Point any Anthropic SDK
or HTTP client at the gateway's base URL and add the governance headers:

- `X-Project: <name>` — **required**; the project is auto-created on first use.
- `X-Tags: <tag>` — optional budget bucket (defaults to `default`).

There is no auth (local, trusted traffic); upstream credentials are the gateway's OAuth tokens, so
no Anthropic API key is needed. SDKs that require an `api_key` field can pass any placeholder.

```sh
curl -s http://127.0.0.1:8088/v1/messages \
  -H 'content-type: application/json' \
  -H 'X-Project: truewallet' -H 'X-Tags: news' \
  -d '{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
```

Anthropic SDK: set `base_url` to `http://127.0.0.1:8088` and pass the headers as default headers.
Set `stream: true` in the body for streaming (SSE) responses.

## Operations

### Retention

A background sweep runs hourly and deletes `usage_event` rows older than 35 days plus any expired
`reservation` rows, keeping the windowed-aggregate hot path bounded. It needs no configuration; the
counts removed are logged at info. Graceful shutdown (SIGINT/SIGTERM) drains in-flight requests and
stops the sweep before exit.

### Recovering a dead refresh token

The gateway refreshes Claude Max OAuth tokens automatically. It does **not** perform
interactive OAuth re-authentication. When a refresh is rejected with `invalid_grant`
(the stored `refresh_token` is revoked or expired), the refresh fails with a
`DeadRefreshTokenError` and that account stops serving traffic until an operator
re-seeds it.

To recover:

1. Obtain a fresh Claude Code OAuth `refresh_token` for the account.
2. Update the credential the gateway reads on the next refresh:
   - **Existing DB:** update the matching `oauth_token` row's `refresh_token`
     (`UPDATE oauth_token SET refresh_token = '<new>' WHERE account_label = '<label>';`).
   - **Fresh DB:** set `LLMGW_OAUTH_REFRESH_TOKENS` (e.g. `label=<new-token>`); the
     seed runs on startup.
3. The gateway resumes on the next request — no restart is required for the DB update.
