# LLMGW — Design (V1)

**Status:** approved design, pre-implementation
**Date:** 2026-06-18

## 1. Purpose & context

LLMGW is a **local LLM gateway**: a single self-hosted service that sits between LLM
providers and any consuming program.

```
[ providers: Claude Max OAuth (V1) · OpenRouter / API keys (later) ]
                              ⇅
                     [ LLMGW : this service ]
                              ⇅
        [ any consumer: TrueWallet Processor, agents, other projects ]
```

It exists to:

- **Decouple consumers from the provider.** Consumers send a clean, standard request;
  LLMGW handles credentials, provider quirks, and routing. Migrating a backend
  (e.g. Claude Max → real Anthropic API → OpenRouter) requires no consumer change.
- **Track usage per project and per tag**, and **enforce budget limits** — natively,
  inside the gateway, instead of scattered in each consumer.
- **Replace clewdr** for the Anthropic/Claude-Max path with a maintained Go
  implementation that is not blocked by Anthropic's anti-abuse (see §9).

**Origin:** TrueWallet's Processor drove all LLM traffic through clewdr (Claude Max
via OAuth). Anthropic began returning `429 rate_limit_error` to clewdr's requests; a
full investigation (see §9) showed the **account is healthy** and the request body is
fine — the trigger is clewdr's **browser-TLS impersonation fingerprint**, and clewdr's
**1-hour cooldown on any 429** turns each rejection into a total outage. LLMGW fixes
both: a normal Go HTTP client (no flagged fingerprint) and budget-based pre-call rate
control instead of a blunt cooldown.

## 2. Scope & non-goals

**In scope (V1):**

- Public API: Anthropic Messages surface (`POST /v1/messages`), full-control ("code")
  semantics — consumer-supplied system prompt + tools.
- One backend provider: **Claude Max via OAuth** (the clewdr `/code` path, reimplemented).
- Per-`(project, tag)` usage tracking and budget limits (calls / tokens / cost),
  hourly + daily windows, hard-block enforcement.
- Postgres-backed state (separate DB in the existing instance).

**Non-goals (V1) — deliberately deferred:**

- No auth / API keys / multi-tenant security. Binds `127.0.0.1`, trusted local traffic only.
- No settings endpoints. Limits and routing are **rows in the DB, edited by hand**.
- No OpenAI-compatible surface and no format translation (added with OpenRouter, V2+).
- No request-body translation: the body is forwarded **verbatim** to the routed provider.

## 3. Architecture (hexagonal)

- **Domain** (`internal/domain/...`): pure logic, no infra imports.
  - `project`: identity, auto-creation on first use.
  - `budget`: limit definitions, window evaluation, enforcement decisions.
  - `usage`: metering (tokens, cost, calls) from a provider response.
  - `routing`: choose a provider for a request.
  - Ports (interfaces) for: store, provider, clock.
- **Adapters** (`internal/adapter/...`):
  - `postgres`: store implementation (projects, limits, counters, audit, oauth tokens, prices, routes).
  - `provider/claudemax`: Claude Max OAuth provider — token refresh, Claude Code spoof,
    verbatim forward via stdlib `net/http`.
  - `httpserver`: the `/v1/messages` surface, header parsing, error mapping.
- **Composition root** (`cmd/llmgw`): config from env, wiring, server start.

## 4. Public API

`POST /v1/messages` — drop-in Anthropic Messages. A consumer points any Anthropic SDK
at LLMGW by changing `base_url`. The request **body is standard Anthropic** and is
forwarded verbatim. Governance travels in **headers** (so the body stays compatible
with any SDK/agent):

- `X-Project: <name>` — the project. **Auto-created** on first sighting; usage is tracked
  against it immediately (limits apply only once configured).
- `X-Tags: <tag>` — which part of the project (e.g. `news`). The budget bucket. Optional;
  defaults to an empty/`default` tag.

Example:

```http
POST http://127.0.0.1:<port>/v1/messages
X-Project: truewallet
X-Tags: news
anthropic-version: 2023-06-01
content-type: application/json

{
  "model": "claude-sonnet-4-6",
  "max_tokens": 4096,
  "system": "<consumer's own system prompt>",
  "tools": [ { "name": "submit_topic", "input_schema": { ... } } ],
  "messages": [ { "role": "user", "content": "..." } ]
}
```

Response: the provider's response, verbatim. On a budget block, LLMGW returns a clear
error **before** forwarding (HTTP 402 or 429 with a typed body) so the consumer can
distinguish a budget stop from a provider error.

## 5. Request lifecycle

1. **Parse** `X-Project` (auto-create if new) and `X-Tags`.
2. **Budget pre-check**: evaluate configured limits for `(project, tag)` against current
   windowed counters. If any `block` limit is already met/exceeded → return 402/429,
   do not forward.
3. **Route**: resolve the provider for this request (V1: the single Claude Max provider).
4. **Inject credentials**: provider adapter applies the Claude Code spoof and a valid
   OAuth access token (refreshing if needed).
5. **Forward verbatim** over stdlib `net/http` (normal TLS).
6. **Meter**: read `usage` from the response; compute notional cost via `model_price`.
7. **Record** a `usage_event` row (project, tag, model, provider, tokens, cost, status, latency).

Enforcement nuance (physics, not a choice): **calls** and **input tokens** are known
pre-call → blocked exactly. **Output tokens** and **cost** are known only post-response →
enforced at crossing: the call that crosses completes, subsequent calls are blocked.
For TrueWallet a `calls/hour` limit blocks exactly pre-call — the lever that prevents the
429 storm that motivated this project.

## 6. Budget model

- **Granularity:** limits are set per `(project, tag)`. `tag = NULL` applies to the whole project.
- **Dimensions:** `calls`, `tokens`, `cost_usd` — any combination, multiple simultaneous.
  Any single limit hit → block.
- **Windows:** `hour` and `day` (both supported, per limit).
- **Pricing (notional):** a `model_price` table (input/output USD per million tokens)
  converts tokens → cost, applied uniformly. Claude Max usage is valued at the
  equivalent API rate ("as if"), so a `cost_usd` budget meaningfully caps free-tier volume
  and lets us compare against real spend. TrueWallet caps in `calls`; paid providers later
  cap in `cost_usd`.
- **Counters:** derived as windowed `SUM` over `usage_event` (no separate counter table to
  keep in sync). Indexed on `(project_id, tag, ts)`.

## 7. Providers & routing

- `provider` rows describe a backend (`type` = `claude_max_oauth` | `anthropic_api` |
  `openrouter`, plus `config_json`).
- `route` rows map a request to a provider (by `model_pattern`, optionally per project).
  **V1: a single default route to the Claude Max provider.** The consumer sends a plain
  Anthropic `model` id; routing lives in the DB (editable like budgets).
- Migration path: adding the **Anthropic API key** provider is transparent (same Messages
  format, only credentials change). **OpenRouter** (V2) adds any LLM and motivates the
  optional OpenAI-compatible surface + translation — isolated, added when needed.

## 8. Storage (Postgres)

A dedicated database (e.g. `llmgw`) inside the existing Postgres instance. Configuration
is done by editing rows directly (no settings API); agents can investigate via SQL.

```sql
-- config (edited by hand)
project(id, name UNIQUE, created_at)                     -- auto-created on first request
budget_limit(id, project_id, tag NULL,                   -- tag NULL = whole project
             dimension,   -- calls | tokens | cost_usd
             window,      -- hour | day
             max_value, action)  -- action: block | warn
provider(id, name, type, config_json, enabled)
route(id, project_id NULL, model_pattern, provider_id)
model_price(model, input_usd_per_mtok, output_usd_per_mtok)

-- runtime state (written by the gateway)
oauth_token(provider_id, access_token, refresh_token, expires_at, updated_at)  -- ROTATES; persisted
usage_event(id, ts, project_id, tag, model, provider,
            input_tokens, output_tokens, cost_usd, status, latency_ms, error)
```

## 9. Claude Max OAuth backend — verified facts

The V1 provider reimplements clewdr's working `/code` path in Go. All of the following
was verified live against the production credentials during the investigation.

**OAuth (refresh):**

- Token endpoint: `POST https://api.anthropic.com/v1/oauth/token`, **form-encoded**.
- Claude Code OAuth `client_id`: `9d1c250a-e61b-44d9-88ed-5944d1962f5e`.
- Body: `grant_type=refresh_token`, `refresh_token=<…>`, `client_id=<…>`; headers
  `anthropic-version: 2023-06-01`, `anthropic-beta: oauth-2025-04-20`.
- Response includes `access_token`, **a rotated `refresh_token`**, `expires_in` (28800 = 8h).
- ⇒ The rotated `refresh_token` **must be persisted** (Postgres `oauth_token`), seeded
  from env on first run, updated on every refresh. Env alone breaks on the 2nd refresh.

**Spoof (Claude Code surface), proven to return 200:**

- `POST https://api.anthropic.com/v1/messages`, `Authorization: Bearer <access_token>`.
- Headers: `User-Agent: claude-code/2.1.76`, `anthropic-beta: oauth-2025-04-20`,
  `anthropic-version: 2023-06-01`.
- System: the consumer's own system blocks, prefixed with EITHER the Claude Code identity
  (`"You are Claude Code, Anthropic's official CLI for Claude."`) OR a billing-header
  system block (`x-anthropic-billing-header: cc_version=2.1.76.<hash>; cc_entrypoint=cli; cch=00000;`).
  A request to this OAuth surface **without** any such marker returns `429 rate_limit_error`
  (deterministic, not rate). Anthropic does not validate the billing hash (any value passes).
- Verified: with the spoof, requests succeed (200) with custom system + tools + large
  context + opus or sonnet, single or concurrent. The account's usage is low and healthy.

**Why clewdr fails (and what we drop):** by elimination, the only difference between a
passing stdlib request and clewdr's failing one is clewdr's **`wreq` browser-TLS
impersonation fingerprint**, which Anthropic's anti-abuse flags → `429`. A normal Go
`net/http` client (plain TLS, like `curl`) passes. We therefore **drop**: the TLS
impersonation, the web/chat surface, the dead `skip_rate_limit` flag, and especially the
**hardcoded 1-hour cooldown on a no-reset 429** (`error.rs`: `now + 3600`) — replaced by
LLMGW's pre-call budget control, which prevents the 429 rather than over-punishing it.

**Caveat:** this remains a Claude-Code spoof on Max accounts — clean and unflagged today,
but out-of-band for Anthropic; the API-key / OpenRouter providers are the durable exit.

## 10. Phasing

- **V1:** Anthropic Messages surface + Claude Max OAuth provider (clewdr-in-Go, fixed) +
  per-`(project, tag)` budget (calls/tokens/cost, hourly/daily, hard-block) + Postgres.
  Resolves TrueWallet's outage and removes clewdr.
- **V2+:** Anthropic API-key provider (transparent migration); then OpenRouter for any LLM,
  with an optional OpenAI-compatible surface + format translation when an OpenAI-native
  consumer must reach an Anthropic backend.

## 11. Open questions

- Port number and exact env var names (`ANTHROPIC_OAUTH_REFRESH_TOKENS` seed, Postgres DSN).
- Multi-account rotation policy detail (round-robin; behaviour when all accounts 429).
- Whether `warn`-action limits emit a log only or also a metric (observability target TBD).
