# LLMGW — Design (V1)

**Status:** approved design, pre-implementation (revised after independent design review)
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

**Origin:** TrueWallet's Processor drove all LLM traffic through clewdr (Claude Max via
OAuth). Anthropic began returning `429 rate_limit_error` to clewdr's requests; a full
investigation (§9) showed the **account is healthy** and the request body is fine — the
trigger is clewdr's **browser-TLS impersonation fingerprint**, compounded by clewdr's
**1-hour cooldown on a no-reset 429** which turns each rejection into a long outage.
LLMGW fixes both: a normal Go HTTP client (no flagged fingerprint) and budget-based
pre-call rate control instead of a blunt cooldown.

## 2. Scope & non-goals

**In scope (V1):**

- Public API: Anthropic Messages surface (`POST /v1/messages`), full-control ("code")
  semantics — consumer-supplied system prompt + tools. **Both non-streaming and streaming
  (`stream:true`) are supported** — streaming is required for long generations.
- One backend provider: **Claude Max via OAuth** (the clewdr `/code` path, reimplemented).
- Per-`(project, tag)` usage tracking and budget limits (calls / tokens / cost),
  hourly + daily windows, hard-block enforcement, **concurrency-safe**.
- Postgres-backed state (separate DB in the existing instance).

**Non-goals (V1) — deliberately deferred:**

- No auth / API keys / multi-tenant security. Binds `127.0.0.1`, trusted local traffic only.
- No settings endpoints. Limits and routing are **rows in the DB, edited by hand**.
- No OpenAI-compatible surface and no request/response format translation (added with
  OpenRouter, V2+).

The body is forwarded **unchanged except for a prepended Claude Code system block** (and a
minimal required normalization set — dropping empty system blocks and stripping ephemeral cache
`scope`; see §9). It is otherwise not transformed: tools, function-calling, and content blocks
pass through.

## 3. Architecture (hexagonal)

- **Domain** (`internal/domain/...`): pure logic, no infra imports.
  - `project`: identity, auto-creation on first use.
  - `budget`: limit definitions, window evaluation, reservation + enforcement decisions.
  - `usage`: metering (tokens, cost, calls) from a buffered response OR an SSE stream.
  - `routing`: choose a provider for a request.
  - Ports (interfaces) for: store, provider, clock.
- **Adapters** (`internal/adapter/...`):
  - `postgres`: store implementation (projects, limits, counters, audit, oauth tokens, prices, routes).
  - `provider/claudemax`: Claude Max OAuth provider — token refresh, Claude Code spoof,
    forward (buffered + SSE relay) via stdlib `net/http`.
  - `httpserver`: the `/v1/messages` surface, header parsing, SSE relay, error mapping.
- **Composition root** (`cmd/llmgw`): config from env, wiring, server start.

## 4. Public API

`POST /v1/messages` — drop-in Anthropic Messages. A consumer points any Anthropic SDK at
LLMGW by changing `base_url`. The request **body is standard Anthropic**. Governance
travels in **headers** (so the body stays compatible with any SDK/agent):

- `X-Project: <name>` — the project. **Auto-created** on first sighting; usage is tracked
  immediately (limits apply only once configured).
- `X-Tags: <tag>` — which part of the project (e.g. `news`). The budget bucket. Optional;
  defaults to a `default` tag.

`stream:true` is honored: LLMGW relays the provider's SSE to the consumer as it arrives
(no buffering — streaming latency preserved) while tapping the stream to accumulate usage.
Non-streaming returns the provider's JSON verbatim.

On a budget block, LLMGW returns **HTTP 402** with a typed body **before** forwarding
(402, not 429, so agent SDKs do not treat it as a retryable provider rate-limit). The body
states which `(project, tag, dimension, window)` limit was hit.

## 5. Request lifecycle

1. **Parse** `X-Project` (auto-create if new) and `X-Tags`.
2. **Budget pre-check + reservation** (atomic): evaluate configured limits for
   `(project, tag)` against current windowed counters **plus in-flight reservations**. If
   any `block` limit is met/exceeded → return 402, do not forward. Otherwise record a
   reservation (in-flight) so concurrent requests cannot collectively overshoot a `calls`
   (or input-token) limit. Pre-call dimensions (`calls`, input tokens) are enforced exactly;
   `cost`/output-token limits are enforced at crossing (the call that crosses completes,
   subsequent calls block).
3. **Route**: resolve the provider (V1: the single Claude Max provider).
4. **Inject credentials**: provider applies the Claude Code spoof + a valid OAuth access
   token (single-flight refresh if expired — see §9).
5. **Forward** over stdlib `net/http`: buffered for non-streaming, SSE relay for streaming.
6. **Meter**: non-streaming → read `usage` from the JSON; streaming → accumulate from
   `message_start` (input) and `message_delta.usage` (output) events. Compute notional cost
   via `model_price`.
7. **Record** a `usage_event`, release the reservation, update counters.

## 6. Budget model

- **Granularity:** limits per `(project, tag)`. `tag = NULL` applies to the whole project
  (evaluated as a tag-agnostic aggregate).
- **Dimensions:** `calls`, `tokens` (total = input + output unless a row specifies a side),
  `cost_usd` — any combination, multiple simultaneous. Any single `block` limit hit → block.
- **Windows:** **sliding** — `hour` = `ts >= now() - interval '1 hour'`, `day` =
  `now() - interval '24 hours'`. (Chosen over calendar buckets: accurate for rate-limiting
  and matches "max N per hour" intent.)
- **Pricing (notional):** a `model_price` table (input/output USD per million tokens)
  converts tokens → cost, applied uniformly (Claude Max valued at the equivalent API rate).
  **Unknown model** (no `model_price` row): cost-based limits cannot compute → the request
  is **blocked** with a typed 402 (fail-closed; add the price row to unblock). `calls`/`tokens`
  limits are unaffected.
- **Counters:** windowed `SUM` over `usage_event` + in-flight reservations. Indexed on
  `(project_id, tag, ts)`. `usage_event` has a **retention policy** (prune rows older than
  the longest window + a margin, e.g. 35 days) so the table stays bounded; the hot-path
  aggregate stays cheap.
- **Concurrency:** the pre-check + reservation is atomic per `(project, tag, window)`
  (a short transaction inserting a reservation, or a per-key advisory lock + in-process
  semaphore), so simultaneous requests cannot collectively breach a limit.

## 7. Providers & routing

- `provider` rows describe a backend (`type` = `claude_max_oauth` | `anthropic_api` |
  `openrouter`, plus `config_json`).
- `route` rows map a request to a provider (by `model_pattern`, optionally per project).
  **V1: a single default route to the Claude Max provider.** The consumer sends a plain
  Anthropic `model` id; routing lives in the DB (editable like budgets).
- **Multi-account & exhaustion (V1):** the Claude Max provider may hold several accounts
  (round-robin). On a provider `429`, the account is put on cooldown **honoring Anthropic's
  reset timestamp** when present (`anthropic-ratelimit-unified-reset` / `resetsAt`), else a
  short backoff (seconds–minutes, **never the 1h clewdr default**). When **all** accounts are
  cooling, LLMGW returns **HTTP 503 + `Retry-After`** so the consumer backs off cleanly.
  Pre-call budget limits are the primary mechanism that keeps us under the 429 threshold in
  the first place.
- Migration path: adding the **Anthropic API-key** provider is transparent (same Messages
  format, only credentials change). **OpenRouter** (V2) adds any LLM and motivates the
  optional OpenAI-compatible surface + translation.

## 8. Storage (Postgres)

A dedicated database (e.g. `llmgw`) inside the existing Postgres instance. Configuration is
done by editing rows directly (no settings API); agents can investigate via SQL. Schema is
managed by versioned SQL migrations (a `migrations/` dir, applied on deploy).

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
oauth_token(provider_id, account_label, session_key, access_token, refresh_token,
            expires_at, cooldown_until, updated_at)   -- session_key = durable seed (owned by the
                                                      -- seed path); access/refresh are derived & rotated
usage_event(id, ts, project_id, tag, model, provider,
            input_tokens, output_tokens, cost_usd, status, latency_ms, error)
reservation(id, project_id, tag, created_at, expires_at)  -- in-flight, counted in pre-check; TTL-cleaned
```

## 9. Claude Max OAuth backend — verified facts

The V1 provider reimplements clewdr's working `/code` path in Go. Below, **(source)** =
confirmed in the clewdr source `/tmp/clewdr-src`; **(probe)** = verified by live request
during the investigation (not derivable from source).

**OAuth (refresh):**

- Endpoint `POST https://api.anthropic.com/v1/oauth/token`, **form-encoded** (source:
  `constants.rs:19`, `exchange.rs` `AuthType::RequestBody`).
- `grant_type=refresh_token`, `client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e` (source:
  `constants.rs:18`); headers `anthropic-version: 2023-06-01`, `anthropic-beta: oauth-2025-04-20`
  (source: `exchange.rs:56-63`).
- Response includes `access_token`, a **rotated `refresh_token`**, `expires_in` (probe: 28800 = 8h).
- **Persistence + self-heal:** the **durable seed is a claude.ai session key**
  (`oauth_token.session_key`, seeded from `LLMGW_SESSION_KEYS`), owned by the seed path — `SaveToken`
  writes only the derived access/refresh tokens and never overwrites it. The gateway **bootstraps** an
  OAuth token set from the session key on first use (see below), refreshes it **single-flight per
  account**, and on a refresh `invalid_grant` **re-bootstraps from the session key** (clewdr's
  self-heal, `exchange.rs` invalid_grant branch). Unlike a refresh token, a session key does not
  rotate on use, so it is a stable credential; only a revoked/expired session key needs a **manual
  re-seed** (documented runbook).

**Bootstrap (session key → OAuth) — clewdr's cookie flow (source: `claude_code_state/`):**

- `GET https://api.anthropic.com/api/bootstrap` with `Cookie: sessionKey=<sid>` → pick the
  `account.memberships[].organization` with a `chat` capability and a paid tier
  (`pro|max|enterprise|raven`) → `organization_uuid` (source: `organization.rs`).
- `POST https://api.anthropic.com/v1/oauth/{org_uuid}/authorize` with the cookie + JSON
  `{client_id, response_type:code, redirect_uri:https://console.anthropic.com/oauth/code/callback,
  scope:"user:profile user:inference", code_challenge (PKCE S256), code_challenge_method:S256, state,
  organization_uuid}` → response `{redirect_uri:"…/callback?code=…&state=…"}`; auto-approved because
  the session is authenticated and the Claude Code `client_id` is pre-trusted (source:
  `exchange.rs::exchange_code`).
- `POST .../v1/oauth/token` form-encoded `grant_type=authorization_code` (+ code, code_verifier,
  client_id, redirect_uri, state) → `{access_token, refresh_token, expires_in}` (source:
  `exchange.rs::exchange_token`). (probe) validated end-to-end: a session key mints a token that
  returns 200 on `/v1/messages`.

**Spoof (Claude Code surface):**

- `POST https://api.anthropic.com/v1/messages`, `Authorization: Bearer <access_token>`,
  `User-Agent: claude-code/<version>`, `anthropic-beta: oauth-2025-04-20`,
  `anthropic-version: 2023-06-01` (source: `chat.rs:139-142`, `constants.rs:22`). `<version>`
  is the operator's **current** Claude Code version, set via `LLMGW_CLAUDE_CODE_VERSION`
  (**default `2.1.181`**; clewdr hardcodes the now-stale `2.1.76`, which still passes today but
  tracking the live version avoids being flagged as an outdated client). The same `<version>`
  feeds the billing-header `cc_version`.
- clewdr prepends a **billing-header system block** on the `/code` path
  (`x-anthropic-billing-header: cc_version=2.1.76.<hash>; cc_entrypoint=cli; cch=00000;`)
  (source: `request.rs:119-136`, injected `:362-373`). The hash is a SHA-256 of sampled
  first-user-message bytes + salt + version; **replicate clewdr's exact computation** (do not
  send a placeholder — Anthropic may tighten validation; clewdr's author treats it as load-bearing).
- (probe) A request to the OAuth surface **without** a Claude Code marker (billing block or the
  identity system `"You are Claude Code, Anthropic's official CLI for Claude."`) returns
  `429 rate_limit_error`; **with** a marker it returns 200 (incl. custom system + tools + large
  context + opus/sonnet, single & concurrent). The account's usage is low and healthy. Note:
  clewdr itself injects only the billing block (the identity string is the real CC CLI's prompt);
  LLMGW will use the billing block.

**Streaming:**

- clewdr handles both modes (source: `chat.rs:320-328`); for `stream:true` it relays the SSE
  while accumulating output tokens from `message_delta` events (source: `forward_stream_with_usage`,
  `chat.rs:346-401`). LLMGW must do the same: **relay SSE unbuffered + accumulate usage** so cost
  / output-token budgets stay correct for streaming consumers.

**Why clewdr fails (and what we drop):** by elimination the only difference between a passing
stdlib request and clewdr's failing one is clewdr's **`wreq` browser-TLS impersonation**
(source: `Cargo.toml` `wreq`, `utils/mod.rs` `Emulation::Chrome145`), which Anthropic's
anti-abuse flags → `429`. A normal Go `net/http` client passes (probe). We **drop**: the TLS
impersonation, the web/chat surface, the dead `skip_rate_limit` flag, and the **1-hour
cooldown on a no-reset 429** (source: `error.rs:400` `now + 3600`; applied only when no reset
time is present, `:380-402`) — replaced by §6/§7 budget + per-account reset-aware cooldown.

**Caveat:** this remains a Claude-Code spoof on Max accounts — clean and unflagged today, but
out-of-band for Anthropic; the API-key / OpenRouter providers are the durable exit.

## 10. Phasing & build order

- **V1** (resolves TrueWallet's outage, removes clewdr), built in risk-reducing slices:
  1. **Passthrough proxy**: `/v1/messages` (non-streaming + streaming) → Claude Max OAuth,
     single account, spoof + single-flight refresh. Kills clewdr / ends the outage.
  2. **Metering**: `usage_event` recording + `model_price`.
  3. **Budget**: limit evaluation + atomic reservation + 402 blocking (calls/tokens/cost, hourly/daily).
  4. **Multi-account** rotation + reset-aware cooldown + all-cooling 503.
- **V2+:** Anthropic API-key provider (transparent migration); then OpenRouter for any LLM,
  with an optional OpenAI-compatible surface + format translation when an OpenAI-native
  consumer must reach an Anthropic backend.

## 11. Testing (E2E-first, against the real provider)

**Principle: every feature is covered by end-to-end tests that drive the real gateway AND hit
the real Anthropic API.** A mock upstream would not exercise the OAuth + Claude Code spoof —
the very core that broke with clewdr and the project's main risk. We accept non-determinism:
tests assert on response *shape and plausibility*, not exact content, and retry transient API
errors.

Harness:

- **Real gateway** — booted in-process on a random local port.
- **Real Postgres** — ephemeral DB (testcontainers-go or a test DSN), migrations applied, so
  budget-aggregation SQL is exercised for real.
- **Real Anthropic backend** — the gateway routes to actual Claude Max via OAuth. Requires a
  dedicated test credential (a real claude.ai **session key**, `LLMGW_TEST_SESSION_KEY`); the suite
  bootstraps an OAuth token set from it once and shares it. The session key does not rotate on use,
  so the same secret stays valid across runs. This is the point: a green suite proves the bootstrap
  + spoof + OAuth still work against Anthropic today.
- **Resilience** — the test client retries transient API errors (5xx, network, timeouts) with
  bounded backoff; it never retries the gateway's own `402`/`503` (those are assertions). The
  retry budget keeps real-API flakiness from failing the suite spuriously.
- **Local stub upstream — failure injection only** — behaviours the real API will not produce on
  demand (a forced provider `429` → cooldown, all-accounts-cooling → `503`, a refresh failure)
  point the gateway at a local stub for those specific tests. This is the only non-real-provider
  part, scoped to failure paths.

Assertions are tolerant: HTTP 200, valid Anthropic response structure, **non-empty content of a
plausible length** (≥ a chars/tokens threshold), expected `stop_reason`, and a `tool_use` block
when tools are exercised — never exact text.

Coverage matrix (each = an E2E test asserting response **and** DB state):

- Non-streaming happy path: 200, plausible-length content, `usage_event` recorded with real
  tokens > 0 and notional cost > 0, counters updated.
- Streaming happy path: `stream:true` → real SSE deltas received, usage accumulated from
  `message_start`/`message_delta`, `usage_event` recorded.
- Project auto-creation: a new `X-Project` creates a `project` row and is tracked.
- Tag bucketing: usage attributed to the correct `(project, tag)`.
- Budget `calls` limit (deterministic): a low cap + N+1 real calls → the (N+1)th returns 402.
- Budget `tokens` / `cost_usd` limit: real calls until the running total crosses the limit → the
  next call returns 402 (the crossing call completes). Repeated over `hour` and `day` windows.
- Concurrency safety: N concurrent real calls against a near-limit `calls` cap → exactly the cap
  succeed, the rest 402 (no overshoot).
- Unknown model: fail-closed 402 for cost limits; `calls` / `tokens` limits unaffected.
- Spoof validation: a real 200 is itself proof the spoof is accepted (the canary).
- OAuth refresh: seed a near-expired token → next call triggers a real single-flight refresh
  against Anthropic → request proceeds; rotated `refresh_token` persisted; concurrent expired
  requests trigger exactly one refresh.
- (stub) Provider `429` → account cooldown honoring the reset header; all accounts cooling → 503
  + `Retry-After`. (stub) `warn`-action limit: not blocked, recorded.

Practicalities: the real-API suite needs test Max credentials + network and consumes quota, so it
is gated by their presence (runs locally and in CI where the secret is set). Domain unit tests
cover pure budget arithmetic (window boundaries, mixed dimensions) without network.

## 12. Open questions

- Listen port + exact env var names (`ANTHROPIC_OAUTH_REFRESH_TOKENS` seed, Postgres DSN).
- Deploy mechanics alongside TrueWallet (compose entry, migration application on deploy).
