# LLMGW — OpenAI / ChatGPT-Codex provider (design)

Status: proposed · Date: 2026-06-19 · Builds on `2026-06-18-llmgw-design.md`.

## 1. Goal

Add a **second backend provider** to LLMGW that serves requests from the operator's
**ChatGPT subscription** (Plus/Pro) instead of the pay-per-token OpenAI platform API — the
exact mirror of what `claudemax` already does for a Claude Max subscription.

Clients reach it through a new **OpenAI-standard surface** (`POST /v1/chat/completions`) and
never see the Codex machinery behind it. The existing Anthropic surface (`/v1/messages` →
Claude Max) is unchanged. Both surfaces feed the **same** project/tag usage tracking and
budget enforcement.

This is the V2 "OpenRouter / non-Anthropic provider" slot from the original design, narrowed
to the subscription path (OAuth + Codex spoof) rather than an API key.

## 2. Why the subscription path looks like `claudemax`

The ChatGPT subscription is **not** reachable via the public OpenAI API. It is reached by
imitating the first-party **Codex** client over OAuth — structurally identical to the Claude
Code spoof:

| Concern | `claudemax` (Claude Max) | `codex` (ChatGPT subscription) |
| --- | --- | --- |
| Auth | OAuth access token + refresh | OAuth access token + refresh |
| First-party spoof | Claude Code system block + headers | Codex `instructions` + `originator`/UA headers |
| Backend wire | Anthropic Messages | OpenAI **Responses** API |
| Endpoint | `…/v1/messages` | `https://chatgpt.com/backend-api/codex/responses` |
| Multi-account | pool + reset-aware cooldown | pool + reset-aware cooldown (same logic) |

The **new** work versus `claudemax` is the wire mismatch: the client surface is
`chat/completions` but the backend speaks **Responses**, so this provider **translates** in
both directions (a pure passthrough is impossible here). `claudemax` is otherwise the
template.

## 3. Client-facing surface & behaviour

- **Endpoint:** `POST /v1/chat/completions`, standard OpenAI Chat Completions wire
  (request and response). Any OpenAI-compatible client/SDK (e.g. Hermes Agent's
  `provider: custom` + `base_url`) works with no changes.
- **Streaming and non-streaming** both supported (`stream: true` → SSE chunks).
- **Tools / function calling** supported (mapped to/from the Responses `tools` shape).
- **Clean output (hard requirement):** the Codex system prompt is **never** emitted to the
  client; reasoning items are dropped; only `content`, `tool_calls`, `finish_reason`, and
  `usage` reach the consumer. The response is indistinguishable from a normal OpenAI one.
- **Headers (unchanged from the Anthropic surface):** `X-Project` (project attribution,
  falls back to the configured default project), `X-Tags` (budget bucket). On this surface
  the **default tag is `agentic`** when `X-Tags` is absent (the Anthropic surface keeps
  `default`).
- **Routing note:** `/chat/completions` currently exists only as a hack-alias forwarding to
  the Anthropic handler (`server.go:43`, for clients that hardcode the path but send an
  Anthropic body). This design **repurposes the path** as the real OpenAI→`codex` endpoint;
  the alias is removed.

## 4. Architecture (fits the existing hexagon)

The current `messagesHandler` is **already wire-agnostic except two call sites**: it only
touches the wire through `req.Model()` and `req.Stream()`. Everything else (project/tag
resolution, atomic budget reservation, usage recording, the streaming sink, error→HTTP
mapping) is generic. We exploit that:

1. **Wire-agnostic port.** `domain.Provider.Send` stops taking the concrete Anthropic
   `llm.ChatRequest` and takes a minimal interface:

   ```go
   // Request is the wire-agnostic view the gateway needs to meter and route a call.
   type Request interface {
       Model() string  // Model is the requested model id, for usage rows and routing.
       Stream() bool   // Stream reports whether the consumer asked for SSE.
       Bytes() []byte  // Bytes is the raw client request body, parsed by the provider.
   }
   ```

   The HTTP wire does a **light** parse (just `model` + `stream`) to satisfy this interface
   and carries the raw body; it never builds a full provider request and never imports a
   provider package. Each provider does the **single full** parse of `Bytes()` in its own
   wire — `claudemax` via `llm.ParseRequest` (Anthropic: `Normalize`, `FirstUserText`, the
   Claude Code block), `codex` into Chat Completions for translation. So there is no full
   double-parse, no cross-wire type assertion, and the domain never learns the OpenAI wire
   (it stays inside `codex`).

2. **Generic handler.** `messagesHandler` becomes a small generic handler parameterised by
   `(wire, provider)`, where `wire` parses a body into a `Request` and tags the default
   bucket. Wired twice in the composition root:
   - `/v1/messages` → (Anthropic wire, `claudemax`, default tag `default`)
   - `/v1/chat/completions` → (OpenAI wire, `codex`, default tag `agentic`)

3. **Provider injected, not resolved.** Each handler holds its provider directly (passed to
   `httpserver.New`). The `Store.DefaultRoute` indirection (a single-provider singleton) is
   dropped — it doesn't generalise to two surfaces and isn't needed once providers are wired
   per route.

4. **Shared provider-error contract.** The handler must map either provider's failures to
   HTTP without knowing concrete types. Today `writeProviderError` type-asserts five
   **concrete** `claudemax` error types (`messages.go:234-267`) and `httpserver` imports
   `claudemax` — so a second provider's errors would all fall through to `500` (no
   `503`/`502`/`Retry-After`, clients never back off). We introduce a domain interface both
   providers implement:

   ```go
   // ProviderError is a provider failure the gateway maps to HTTP without knowing the
   // concrete provider. RetryAfter's bool is false when no Retry-After header applies.
   type ProviderError interface {
       error
       HTTPStatus() int                    // HTTPStatus is the status to send the client.
       ErrorType() string                  // ErrorType is the stable machine-readable type.
       RetryAfter() (time.Duration, bool)  // RetryAfter is the backoff when one is known.
   }
   ```

   `writeProviderError` collapses to a single `errors.As(err, &provErr)`; `httpserver` stops
   importing `claudemax`. This unblocks the second provider AND removes the duplicated
   five-way type switch.

5. **Per-provider store scoping, resolved once.** Account/token access
   (`LoadAccounts`/`SetCooldown`/`LoadToken`/`SaveToken`) is currently bound to the single
   default provider (`defaultProviderID`, **uncached**, queried on every call). Each method
   gains a `providerName`; each provider **resolves its provider id once in `New()`** and
   reuses it, so scoping adds no per-request DB round-trips. `DefaultRoute` /
   `SetDefaultProvider` are removed.

6. **New isolated package** `internal/adapter/provider/codex` — OAuth/refresh, the Codex
   header spoof, the translation layer, and its own account pool. `claudemax` changes are
   limited to the `Send` signature, the `providerName` plumbing, and implementing
   `ProviderError` on its error types.

Result: one gateway logic, two thin wire adapters, two providers, one error contract. A
future third provider (e.g. OpenRouter via API key) is a third `(wire, provider)` pair —
nothing else moves.

## 5. The `codex` provider

### 5.1 Backend call

- **Endpoint:** `POST https://chatgpt.com/backend-api/codex/responses` (base URL injectable
  for tests).
- **Auth:** `Authorization: Bearer <access_token>`, refreshed from a stored `refresh_token`
  (single-flight, like `claudemax`'s token manager).
- **Codex spoof headers:** `originator` (must be a whitelisted first-party value, e.g.
  `codex_cli_rs` — a non-Codex originator is rejected `403`), a Codex `User-Agent`,
  `ChatGPT-Account-ID`, `x-client-request-id`, and the rest of the Codex client header set.
  The exact set is captured from the real Codex CLI and pinned in config; verified against
  the live backend during implementation.
- **Body constraints:** `store: false` is forced (any other value is rejected); requests are
  always sent with `stream: true` upstream and re-buffered when the client asked for a
  non-streaming response (simpler than supporting both upstream modes).
- **`instructions`:** the Codex system prompt. We try the **smallest** value that passes the
  backend's validity check first; if the backend rejects it (ChatMock and other proxies ship
  a full `prompt_gpt5_codex.md`, a strong signal a too-thin value is refused), we **fall back
  to embedding the real Codex prompt**, pinned in the package, under the ~32 KiB cap. Which
  one is required is settled during the Phase-2 capture. Either way this is the unavoidable
  injection (the analogue of the Claude Code spoof) — but it lives in the Responses
  `instructions` field, **separate** from the conversation (the client's own system message
  goes into `input`, see 5.2), and it never reaches the client.

### 5.2 Request translation (Chat Completions → Responses)

| Chat Completions | Responses |
| --- | --- |
| `messages[]` system/developer | a `developer` message in `input[]` (NOT `instructions`) |
| `messages[]` user/assistant | corresponding `input[]` message items |
| `messages[]` tool result | `function_call_output` item |
| assistant `tool_calls` | `function_call` items |
| `tools[]` (functions) | Responses `tools[]` (function shape) |
| `tool_choice` | `tool_choice` |
| `max_tokens` | `max_output_tokens` |
| `model` | validated/mapped to a Codex-served model (see 7) |

### 5.3 Response translation (Responses → Chat Completions)

- **Non-streaming:** read the upstream SSE to completion, fold `output[]` items into one
  `choices[0].message` (`content` + `tool_calls`), **drop** `reasoning` items, map
  `finish_reason`, and translate `usage` (`input_tokens`/`output_tokens` →
  `prompt_tokens`/`completion_tokens`). Emit a single Chat Completions JSON object.
- **Streaming:** translate the Responses event stream into Chat Completions chunks on the
  fly —
  - `response.output_text.delta` → `choices[0].delta.content`
  - `response.function_call_arguments.delta` / item added → `choices[0].delta.tool_calls`
  - `response.completed` → final chunk: `finish_reason` + (when the client sent
    `stream_options.include_usage`) a usage chunk
  - `response.created` / `response.in_progress` (which carry the full Codex `instructions`)
    and all `reasoning` events → **dropped, never forwarded**
  - terminate with `data: [DONE]`
  This is the heaviest piece of the provider and the main test target.

### 5.4 Account pool

Transposed from `claudemax` (`selectOrder` / `cool` / `allCooling` / round-robin cursor):
multi-account from day one, reset-aware cooldown, `AllCoolingError` → `503 + Retry-After`.
Only the **error classification** differs (5.5); the rotation logic is the same shape.

### 5.5 Error handling & cooldown

Upstream status → typed error → failover decision (mirrors `cooldownFor`):

| Upstream | Typed error | Action |
| --- | --- | --- |
| `429` | `RateLimitError` (reset from header when present) | cool (reset-aware), fail over |
| `401` | dead/invalid token | refresh once; if still failing, cool, fail over |
| `403` (Cloudflare / originator) | `UpstreamError` | cool short, fail over (account-specific) |
| `5xx` | `UpstreamError` | cool short, fail over |
| other `4xx` | `UpstreamError` | surface to client unchanged (request-level) |
| all accounts cooling | `AllCoolingError` | `503` + `Retry-After` |

Each codex error type implements `ProviderError` (§4), so the shared `writeProviderError`
maps it with **no codex-specific code in the handler**. The JSON error envelope stays a
**single shared shape**, extended to `{"error":{"message","type","code","param"}}` (the
existing `type`/`message` plus nullable `code`/`param`) — parseable by both OpenAI and
Anthropic SDKs, so no per-route envelope is needed.

## 6. Budget & usage (reused as-is)

No new budget logic. The generic handler calls the same `EnsureProject` →
`ReserveIfAdmitted` (atomic pre-check + reservation) → `ReleaseReservation` → `RecordUsage`
path. Each `usage_event` is labelled with `provider = "chatgpt-codex"` and the requested
model, sharing projects, tags, hourly/daily windows, and limits with the Anthropic surface.

`cost_usd` stays **notional** (list price from `model_price`), consistent with the existing
`costFor` — the subscription is flat-rate, but notional cost keeps cross-provider budgets
comparable. Usage tokens come from the real Responses `usage` (5.3).

## 7. Storage (migrations)

Additive only; no existing rows change.

1. **Provider type.** Extend the `provider.type` CHECK to include `chatgpt_codex_oauth`, and
   seed one provider row (`name = 'chatgpt-codex'`, `type = 'chatgpt_codex_oauth'`).
2. **Account id.** Add `chatgpt_account_id TEXT` (nullable) to `oauth_token` — the
   `ChatGPT-Account-ID` is per account and rides alongside the existing
   `refresh_token`/`access_token`/`cooldown_until` columns. Claude Max rows leave it null.
3. **Model prices.** Seed `model_price` with notional list prices for the Codex-served models
   to be exposed (e.g. `gpt-5`, `gpt-5-codex`, `gpt-5.5`). The exact model list is confirmed
   at implementation time against what the subscription serves; unpriced models record zero
   cost (existing fail-open behaviour).

**Credential seeding (option A, chosen).** The operator obtains a Codex `refresh_token` +
`ChatGPT-Account-ID` once (out of band) and seeds them — via the same idempotent config-seed
pattern as `SessionKeys` today, into the `chatgpt-codex` provider's `oauth_token` rows. The
gateway only refreshes; there is **no** interactive login surface in the gateway.

## 8. Configuration

New config (env), mirroring `ClaudeCodeVersion`:

- `CODEX_VERSION` / Codex client identifiers used to build the spoof headers (`originator`,
  `User-Agent`, …).
- Codex account seeds (label + `refresh_token` + `chatgpt_account_id`), idempotently seeded
  at boot like session keys.

`DefaultProject` and listen address are shared with the existing config.

## 9. Scope

**In scope (V1 of this provider):**

- `/v1/chat/completions` surface, streaming + non-streaming.
- Chat Completions ⇄ Responses translation, with reasoning/Codex-prompt filtering.
- Tools / function calling.
- Multi-account pool with reset-aware cooldown and all-cooling `503`.
- Shared project/tag usage tracking and budget enforcement; default tag `agentic`.
- OAuth refresh from seeded credentials.

**Out of scope (deferred):**

- No interactive OAuth/PKCE login in the gateway (credentials are seeded).
- No reasoning exposure, no Responses-only features (built-in web/file-search tools,
  server-side state) — explicitly filtered for clean output.
- No image-generation tool.
- No `/v1/responses` passthrough surface (clients speak Chat Completions).

## 10. Testing (E2E-first, real backend)

Same philosophy as `claudemax`: every feature covered by E2E tests driving the real gateway,
hitting the **real Codex backend** with seeded test credentials (the spoof + OAuth is the
core risk and a mock would not exercise it). Assert on **shape and plausibility** (valid
Chat Completions structure, non-empty plausible content, correct `finish_reason`,
`tool_calls` when tools are used), never exact text. Retry transient upstream errors; never
retry the gateway's own `402`/`503`.

Because a ChatGPT subscription's rate limits are tighter than Claude Max, the real-backend
tests are kept **deliberately few** — a small smoke set (one non-streaming, one streaming,
one tools round-trip) to prove the live path — while the bulk of behaviour (translation
tables, prompt/reasoning filtering, pool/cooldown, budget) is asserted via **domain unit
tests and the local stub upstream**, which don't burn subscription quota.

- **Non-streaming:** valid Chat Completions object; usage recorded from the real response.
- **Streaming:** well-formed SSE chunks ending in `[DONE]`; **assert the Codex system prompt
  and reasoning never appear** in the stream (the clean-output guarantee).
- **Tools:** a forced function call round-trips through the translation.
- **Budget:** a `calls` cap blocks deterministically; tokens/cost by crossing — shared with
  Anthropic rows (same project, different provider).
- **Failure injection (local stub upstream only):** forced `429` → cooldown; all-cooling →
  `503 + Retry-After`; refresh failure → dead-token handling.

Domain unit tests cover the translation tables (5.2/5.3) deterministically without network.

## 11. Build order (risk-reducing slices)

1. **Port + handler generalisation** (interface `Request`; `ProviderError` contract +
   error-mapping refactor; provider-scoped store resolved once in `New()`; generic handler;
   provider injected per route; drop `DefaultRoute`) — `claudemax` keeps passing its existing
   E2E suite. No behaviour change.
2. **`codex` skeleton**: OAuth refresh + single-account spoofed call to the Responses
   backend, non-streaming, minimal `instructions`. Proves the spoof end to end.
3. **Translation**: request + non-streaming response, then the streaming translator
   (the hard part) with filtering. Tools.
4. **Metering + budget** wired through the shared path; `agentic` default tag; model prices.
5. **Multi-account pool** + reset-aware cooldown + all-cooling `503`.
