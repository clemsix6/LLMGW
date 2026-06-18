# LLMGW V1 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: use superpowers:subagent-driven-development —
> **one implementation subagent per BATCH** (not per task), with a review subagent between
> batches that holds BOTH the spec and this plan in context (TrueWallet convention). Steps use
> checkbox (`- [ ]`) syntax.

**Goal:** Build LLMGW V1 — a local LLM gateway that proxies the Claude Max OAuth backend, tracks
per-`(project, tag)` usage, and enforces budget limits — fully implemented and prod-ready.

**Architecture:** Go, hexagonal (domain / adapters / cmd). One HTTP surface (`POST /v1/messages`,
Anthropic Messages, streaming + non-streaming). One provider (Claude Max via OAuth, reimplementing
clewdr's `/code` path with a normal `net/http` client). Postgres for state, counters (windowed
`SUM` over `usage_event` + in-flight reservations), and hand-edited config (limits, routes, prices).

**Tech Stack:** Go 1.26, `pgx` (Postgres), stdlib `net/http`, `testcontainers-go` (E2E Postgres),
golang-migrate-style SQL migrations.

**Source of truth:** `docs/specs/2026-06-18-llmgw-design.md`. Read it before starting any batch.
Clewdr reference source (read-only, for the OAuth/spoof port): `/tmp/clewdr-src`.

**Scope:** Everything in the spec EXCEPT OpenRouter and the OpenAI-compatible surface (the
`anthropic_api` provider type column may exist but only `claude_max_oauth` is implemented). All
other features are implemented and prod-ready at the end of this plan.

---

## Batch cadence (TrueWallet convention)

- Each batch is **1-3 coherent tasks**. **Wiring (cmd/llmgw) goes IN each batch** so every batch
  leaves the build green and its own tests passing.
- End every batch with: `go build ./...` green, `go vet ./...` clean, the batch's tests passing,
  then **commit (one per batch, commit convention from CLAUDE.md) and `git push`**.
- Between batches, a **review subagent** (spec + plan in context) checks the batch didn't drift;
  apply fixes before the next batch.
- Commit message format (CLAUDE.md): title line (no prefix) + `[+]/[-]/[&]/[!]` lines. No footers.

## Testing model (from spec §11)

E2E tests **hit the real Anthropic API** through the gateway. Assertions are tolerant
(status 200, valid Anthropic structure, non-empty content of plausible length, expected
`stop_reason`, `tool_use` when tools used) and the test client **retries transient API errors**
(5xx/network/timeout) with bounded backoff — never retrying the gateway's own `402`/`503`.
Real-API tests are **gated**: skip when `LLMGW_TEST_REFRESH_TOKEN` (+ Postgres DSN) is absent.
A **local stub upstream** is used ONLY for failure injection (forced 429/503, refresh failure).
Domain unit tests cover pure budget arithmetic without network.

---

## File structure

```
cmd/llmgw/main.go                              composition root (grows each batch)
internal/config/config.go                      env config (DSN, listen addr, seed refresh tokens)
internal/domain/ports.go                       Store, Provider, Clock interfaces
internal/domain/llm/request.go                 ChatRequest value type (parse / inject system / bytes)
internal/domain/usage/usage.go                 Usage, notional cost computation
internal/domain/budget/budget.go               limit evaluation over windowed usage + reservations
internal/domain/project/project.go             project model
internal/adapter/postgres/store.go             Store impl (pgx)
internal/adapter/postgres/migrations/*.sql     schema
internal/adapter/provider/claudemax/oauth.go   token manager (single-flight refresh, persist)
internal/adapter/provider/claudemax/spoof.go   Claude Code headers + billing-header system block
internal/adapter/provider/claudemax/provider.go forward (buffered + SSE relay), usage extraction, 429 mapping, account pool
internal/adapter/httpserver/server.go          router, middleware
internal/adapter/httpserver/messages.go        POST /v1/messages handler
test/e2e/harness.go                            boot gateway + ephemeral Postgres + real/stub upstream + retrying client
test/e2e/*_test.go                             E2E suites
```

### Shared contracts (define in Batch 0/1, keep stable across batches)

```go
// internal/domain/usage/usage.go
type Usage struct {
    InputTokens  int
    OutputTokens int
}

// internal/domain/ports.go
type Clock interface{ Now() time.Time }

type Store interface {
    // config / projects
    EnsureProject(ctx context.Context, name string) (projectID int64, err error) // auto-create, idempotent
    LimitsFor(ctx context.Context, projectID int64, tag string) ([]BudgetLimit, error)
    PriceFor(ctx context.Context, model string) (in, out float64, ok bool, err error)
    DefaultRoute(ctx context.Context) (Provider, error) // V1: single route
    // usage + counters
    RecordUsage(ctx context.Context, e UsageEvent) error
    WindowedTotals(ctx context.Context, projectID int64, tag string, since time.Time) (Totals, error)
    // reservations (concurrency-safe pre-check)
    Reserve(ctx context.Context, projectID int64, tag string, ttl time.Duration) (reservationID int64, err error)
    ReleaseReservation(ctx context.Context, reservationID int64) error
    InflightTotals(ctx context.Context, projectID int64, tag string) (Totals, error)
    // oauth tokens
    LoadToken(ctx context.Context, account string) (Token, error)
    SaveToken(ctx context.Context, account string, t Token) error
}

type Provider interface {
    // Send forwards req upstream. For non-streaming it writes the JSON body to out and returns
    // Usage; for streaming it relays SSE to out while accumulating Usage. A provider rate-limit
    // surfaces as *RateLimitError (carries reset time when present).
    Send(ctx context.Context, req llm.ChatRequest, out http.ResponseWriter) (usage.Usage, error)
}
```

---

## Batch 0 — Scaffolding, config, DB schema + store skeleton

**Files:** Create `internal/config/config.go`, `internal/adapter/postgres/store.go`,
`internal/adapter/postgres/migrations/0001_init.sql`, `cmd/llmgw/main.go`, `test/e2e/harness.go`.

- [ ] **Task 0.1 — Config + migrations.** `config.Load()` reads env: `LLMGW_LISTEN` (default
  `127.0.0.1:8088`), `LLMGW_POSTGRES_DSN`, `LLMGW_OAUTH_REFRESH_TOKENS` (comma-separated seed,
  `account_label=token` pairs). Wrap missing-required with `fmt.Errorf`. Write `0001_init.sql`
  with the full spec §8 schema: `project, budget_limit, provider, route, model_price,
  oauth_token, usage_event, reservation` (+ index `usage_event(project_id, tag, ts)`,
  `reservation(project_id, tag)`). `dimension`/`window`/`action` as text with CHECK constraints.
- [ ] **Task 0.2 — Postgres store skeleton + migration runner.** `postgres.New(ctx, dsn)` opens a
  `pgxpool`, runs migrations on boot (embed the `migrations/` dir). Implement `EnsureProject`
  (INSERT … ON CONFLICT (name) DO UPDATE … RETURNING id) and a `Ping`. Stub the rest of `Store`
  returning `errors.New("not implemented")` so it compiles.
- [ ] **Task 0.3 — E2E harness + smoke test.** `test/e2e/harness.go`: start an ephemeral Postgres
  via `testcontainers-go`, build config, boot the gateway on a random port, expose a retrying
  HTTP client. `cmd/llmgw/main.go`: load config → open store (migrate) → start a server with a
  `GET /health` route returning 200. E2E `TestHealth`: harness boots, `GET /health` → 200, and the
  migrated DB has the expected tables. Skips if Docker unavailable.

**Verify:** `go build ./... && go vet ./...` clean; `go test ./test/e2e -run TestHealth` PASS.
**Wiring:** `cmd/llmgw` boots server + DB. **Commit + push.**

---

## Batch 1 — Claude Max OAuth token manager (single-flight refresh)

**Files:** Create `internal/adapter/provider/claudemax/oauth.go`; implement `LoadToken`/`SaveToken`
in the store; modify `cmd/llmgw/main.go` (seed tokens from config into `oauth_token` on boot if absent).

**Reference (verified):** `POST https://api.anthropic.com/v1/oauth/token`, form-encoded,
`grant_type=refresh_token`, `client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e`, headers
`anthropic-version: 2023-06-01` + `anthropic-beta: oauth-2025-04-20`. Response: `access_token`,
**rotated** `refresh_token`, `expires_in` (8h). Clewdr source: `claude_code_state/exchange.rs`.

- [ ] **Task 1.1 — Token type + store persistence.** `Token{AccessToken, RefreshToken string;
  ExpiresAt time.Time}`. Implement `LoadToken`/`SaveToken` (per `account` label) against
  `oauth_token`. Boot seeds from `LLMGW_OAUTH_REFRESH_TOKENS` only when the row is absent (never
  overwrite a persisted, rotated token). Unit test: save → load round-trips.
- [ ] **Task 1.2 — tokenManager with single-flight refresh.** `tokenManager.Valid(ctx, account)
  (string, error)` returns a non-expired access token, refreshing within a margin (60s). Refresh
  is **single-flight per account** (`golang.org/x/sync/singleflight` keyed by account) so
  concurrent callers trigger one HTTP refresh. On success, **persist the rotated token in the same
  call before returning** (crash-safety: commit before use). On `invalid_grant`, return a typed
  `*DeadRefreshTokenError` (no interactive re-auth — operator re-seeds; document in README runbook).
  Unit test with a stub token endpoint: expired token → one refresh; 5 concurrent `Valid` calls →
  exactly one HTTP call (assert via the stub's request counter); rotated token persisted.

**Verify:** `go test ./internal/adapter/provider/claudemax -run OAuth` PASS; build+vet green.
**Wiring:** main seeds tokens. **Commit + push.**

---

## Batch 2 — Claude Code spoof + non-streaming forward

**Files:** Create `internal/adapter/provider/claudemax/spoof.go`,
`internal/adapter/provider/claudemax/provider.go`, `internal/domain/llm/request.go`.

- [ ] **Task 2.1 — ChatRequest + system injection.** `llm.ParseRequest([]byte) (ChatRequest,
  error)` parses the Anthropic body (keep the full object). Methods: `Model() string`,
  `Stream() bool`, `FirstUserText() string`, `WithClaudeCodeSystem() ChatRequest` (prepend the
  billing-header block to `system`, handling string vs array vs absent — mirror clewdr
  `prepend_system_blocks`), `Bytes() []byte`. Unit tests for each system shape.
- [ ] **Task 2.2 — Spoof (billing header).** `spoof.go`: replicate clewdr's
  `claude_code_billing_header` (source `middleware/claude/request.rs:119-136` + its
  `sample_js_code_unit` helper): `x-anthropic-billing-header: cc_version=2.1.76.<hash3>;
  cc_entrypoint=cli; cch=00000;` where `<hash3>` = first 3 hex of
  `sha256("59cf53e54c78" + sampled[4,7,20] + "2.1.76")`. **Note:** a live probe showed Anthropic
  does not currently validate the hash (a placeholder passed), so presence + format is what matters
  today — but replicate the exact algorithm for future-proofing; read the clewdr helper for the
  exact code-unit sampling. Constants: UA `claude-code/2.1.76`, beta `oauth-2025-04-20`, version
  `2023-06-01`. Unit test: header format matches the regex `cc_version=2\.1\.76\.[0-9a-f]{3}; cc_entrypoint=cli; cch=00000;`.
- [ ] **Task 2.3 — Non-streaming Send + real-API E2E.** `provider.Send` (non-streaming path):
  build request → `WithClaudeCodeSystem` → POST `https://api.anthropic.com/v1/messages` with Bearer
  (from tokenManager) + spoof headers via stdlib `net/http` → on 200 write body to `out`, parse
  `usage.{input_tokens,output_tokens}`, return `Usage`; on 429 return `*RateLimitError{ResetAt}`
  (parse `anthropic-ratelimit-unified-reset`/`retry-after`); wrap other non-2xx as errors. E2E
  (real, gated): a tiny request through a test provider instance → 200, content length ≥ threshold,
  `Usage.OutputTokens > 0`. Tolerant assertions + retry on transient errors.

**Verify:** unit tests PASS; gated E2E `TestProviderRealNonStreaming` PASS when creds present
(else SKIP); build+vet green. **Commit + push.**

---

## Batch 3 — `/v1/messages` passthrough handler + project auto-create + usage recording (KILLS CLEWDR)

**Files:** Create `internal/adapter/httpserver/server.go`,
`internal/adapter/httpserver/messages.go`, `internal/domain/project/project.go`; implement
`RecordUsage`, `WindowedTotals` in the store; modify `cmd/llmgw/main.go` (wire handler → provider → store).

- [ ] **Task 3.1 — Handler (no budget yet).** `POST /v1/messages`: read `X-Project` (required;
  `EnsureProject`), `X-Tags` (default `default`), parse body, call `provider.Send(ctx, req, w)`,
  then `RecordUsage(UsageEvent{ts, projectID, tag, model, provider, usage, cost=0 for now, status,
  latency})`. Map `*RateLimitError`/`*DeadRefreshTokenError` to clean HTTP errors (503 + Retry-After
  / 502). Streaming requests (`stream:true`) are handled in Batch 4 — for now return 501 for them
  so the contract is explicit (replaced next batch).
- [ ] **Task 3.2 — Store usage + totals.** Implement `RecordUsage` (INSERT into `usage_event`) and
  `WindowedTotals` (sliding `SUM(calls=1, tokens, cost)` over `usage_event` since `ts >= $since`,
  for `(project, tag)`; tag-agnostic when tag is the whole-project sentinel). Unit test against the
  ephemeral DB: insert events → totals match for 1h and 24h windows.
- [ ] **Task 3.3 — Real-API E2E passthrough.** E2E (gated): `POST /v1/messages` with
  `X-Project: e2e`, a real Anthropic body → 200, plausible content; assert a `project` row `e2e`
  exists and one `usage_event` row recorded with `output_tokens > 0`. **This batch makes LLMGW a
  working drop-in replacement for clewdr's `/code` path.**

**Verify:** gated E2E `TestPassthroughRealNonStreaming` PASS; unit tests PASS; build+vet green.
**Wiring:** full request path live (minus streaming/budget). **Commit + push.**

---

## Batch 4 — Streaming (SSE relay + usage accumulation)

**Files:** Modify `internal/adapter/provider/claudemax/provider.go` (streaming path),
`internal/adapter/httpserver/messages.go` (remove the 501).

**Reference:** clewdr `forward_stream_with_usage` (`claude_code_state/chat.rs:346-401`): relay SSE
unbuffered; accumulate `input_tokens` from `message_start`, `output_tokens` from the latest
`message_delta.usage`.

- [ ] **Task 4.1 — SSE relay + accumulate.** When `req.Stream()`: set SSE headers, stream the
  upstream response body to `out` line-by-line with `http.Flusher.Flush()` after each event (no
  buffering), while parsing `event:`/`data:` lines to accumulate `Usage` (input from
  `message_start.message.usage`, output from each `message_delta.usage.output_tokens`). Return the
  accumulated `Usage` when the stream ends. Handle client disconnect (ctx cancel) cleanly.
- [ ] **Task 4.2 — Real-API streaming E2E.** E2E (gated): `stream:true` request → receive ≥2 SSE
  events, a terminal `message_stop`, assert accumulated/recorded `usage_event.output_tokens > 0`.
  Assert latency-to-first-byte is small (streaming not buffered) — first event arrives well before
  the full generation completes.

**Verify:** gated E2E `TestPassthroughRealStreaming` PASS; build+vet green. **Commit + push.**

---

## Batch 5 — Pricing (notional cost)

**Files:** Implement `PriceFor` in the store; modify the handler to compute + store `cost_usd`;
seed `model_price` via migration `0002_seed_prices.sql`.

- [ ] **Task 5.1 — Cost computation + unknown-model policy.** `usage.Cost(u Usage, inUSDPerMTok,
  outUSDPerMTok float64) float64`. Handler looks up `PriceFor(model)`; if found, compute and store
  `cost_usd`. Seed `model_price` for the models in use (opus-4-8, sonnet-4-6, haiku-4-5) with
  current API list prices in `0002_seed_prices.sql`. Unknown model → cost stays 0 / unpriced flag
  (enforcement consequence handled in Batch 7). Unit test for `Cost`; E2E asserts a real call
  records `cost_usd > 0` for a priced model.

**Verify:** unit + gated E2E PASS; build+vet green. **Commit + push.**

---

## Batch 6 — Budget evaluation (read side, pure domain)

**Files:** Create `internal/domain/budget/budget.go`; implement `LimitsFor`, `Reserve`,
`ReleaseReservation`, `InflightTotals` in the store.

- [ ] **Task 6.1 — Limit evaluation.** `budget.Decision` type and
  `budget.Evaluate(limits []BudgetLimit, current Totals, inflight Totals, reqInputTokens int)
  Decision` — for each limit, compute the window total (current + inflight + the pre-call known
  cost of this request for `calls`/input-`tokens`) and decide `Allow` / `Block(limit)`. Pre-call
  dimensions (`calls`, input tokens) block exactly; `cost_usd`/output-`tokens` block at crossing
  (current ≥ max). `warn` action never blocks (returns a Warn flag). Pure function — exhaustive
  unit tests: each dimension × window, under/at/over, multiple simultaneous limits, `tag=NULL`
  whole-project, `warn` vs `block`.
- [ ] **Task 6.2 — Store: limits + reservations.** Implement `LimitsFor` (SELECT for `(project,
  tag)` + whole-project `tag IS NULL`). Implement `Reserve` (INSERT a `reservation` row with
  `expires_at = now()+ttl`), `ReleaseReservation` (DELETE), `InflightTotals` (count non-expired
  reservations for `(project, tag)`; prune expired). Unit tests against the ephemeral DB.

**Verify:** unit tests PASS (no network); build+vet green. **Commit + push.**

---

## Batch 7 — Budget enforcement in the request path (atomic reservation + 402)

**Files:** Modify `internal/adapter/httpserver/messages.go` (pre-check + reserve + release);
create `internal/adapter/httpserver/errors.go` (typed 402 body).

- [ ] **Task 7.1 — Pre-check + reserve + enforce.** In the handler, before forwarding: load
  `LimitsFor`, `WindowedTotals` (1h + 24h), `InflightTotals`; call `budget.Evaluate`. On `Block`,
  return **402** with a typed JSON body `{type:"budget_exceeded", project, tag, dimension, window,
  limit, current}` — do NOT forward. Otherwise `Reserve` (TTL e.g. 2m), forward, and
  `ReleaseReservation` + `RecordUsage` in a deferred step (release even on error). Make the
  pre-check + reserve atomic per `(project, tag)` (short serializable txn or `pg_advisory_xact_lock`
  on `hash(project,tag)`) so concurrent requests can't both pass a near-limit `calls` cap.
- [ ] **Task 7.2 — Real-API budget E2E.** Gated E2E with config rows inserted by the test:
  (a) **calls limit deterministic** — `budget_limit(project, tag, calls, hour, 3, block)`; fire 4
  real calls → first 3 → 200, 4th → 402 with the typed body. (b) **concurrency** — limit 5 calls,
  fire 10 concurrent → exactly 5 succeed, 5 → 402 (no overshoot). (c) **cost crossing** — a tiny
  `cost_usd` hourly limit; fire real calls until the recorded total crosses it → the next → 402.
  (d) **unknown model** → 402 fail-closed for a cost limit. Tolerant content assertions + retry.

**Verify:** gated E2E `TestBudget*` PASS; unit tests PASS; build+vet green. **Commit + push.**

---

## Batch 8 — Multi-account rotation + reset-aware cooldown + all-cooling 503

**Files:** Modify `internal/adapter/provider/claudemax/provider.go` (account pool); add
`cooldown_until` handling via `oauth_token`; modify the handler's 503 mapping.

- [ ] **Task 8.1 — Account pool + cooldown.** The provider holds N accounts (from config /
  `oauth_token` rows). `Send` picks the next non-cooling account (round-robin). On a provider 429,
  set that account's `cooldown_until` (honor the upstream reset header when present; else a short
  default e.g. 60s — **never 1h**), retry the next account; if all are cooling, return
  `*AllCoolingError{RetryAfter}`. Unit test with a **stub upstream** (forced 429 + reset header):
  account goes on cooldown for the right duration, rotation moves to the next, all-cooling →
  `AllCoolingError`.
- [ ] **Task 8.2 — Handler 503 + stub E2E.** Map `*AllCoolingError` → **503 + Retry-After**. E2E
  using the local stub upstream (failure injection): all accounts forced 429 → gateway returns 503
  + Retry-After. Plus a real-API multi-account happy test (2 seeded accounts) → requests succeed,
  rotation observed in `usage_event.provider`/account column.

**Verify:** unit + stub E2E `TestCooldown*` PASS; gated real multi-account test PASS; build+vet
green. **Commit + push.**

---

## Batch 9 — Deployment (Dockerfile, compose, migrations on deploy, runbook)

**Files:** Create `Dockerfile`, `docker-compose.yml` (or a service snippet), `.env.example`;
extend `README.md` with run + token re-seed runbook.

- [ ] **Task 9.1 — Container + compose.** Multi-stage `Dockerfile` (build `linux/amd64` — the
  server is x86_64), distroless/alpine runtime, non-root, binary `llmgw`. `docker-compose.yml`
  service binding **`127.0.0.1:<port>`**, `LLMGW_*` env, depends on the existing Postgres (separate
  `llmgw` database — document `CREATE DATABASE llmgw`). Migrations run on container boot (already
  wired in Batch 0). `.env.example` lists every env var.
- [ ] **Task 9.2 — Runbook + final verification.** README: how to create the `llmgw` DB, set
  budgets/routes/prices by editing rows (example SQL), and the **dead-refresh-token re-seed**
  procedure. Final check: `go build ./... && go vet ./... && go test ./...` (real-API suites run
  where creds present), and a documented manual smoke against the deployed container.

**Verify:** image builds; compose renders; full `go test ./...` green (gated suites SKIP without
creds). **Commit + push.**

---

## Self-review (against spec)

- **§2 API / §4** → Batch 3 (handler, headers, auto-create), Batch 4 (streaming). ✓
- **§5 lifecycle** → Batch 3 (parse/forward/record) + Batch 7 (pre-check/reserve/release). ✓
- **§6 budget** (dimensions, sliding windows, notional pricing, unknown-model fail-closed,
  concurrency) → Batches 5, 6, 7. ✓
- **§7 providers/routing + multi-account + 503** → Batch 2 (provider), Batch 3 (default route),
  Batch 8 (pool/cooldown/503). ✓
- **§8 storage** → Batch 0 (schema/migrations), implemented across batches as needed. ✓
- **§9 OAuth + spoof + streaming + TLS-normal client** → Batches 1, 2, 4. ✓
- **§11 testing** (real-API E2E, tolerant + retry, stub for failure injection, calls-limit
  deterministic, concurrency, unit for arithmetic) → harness Batch 0, suites across batches. ✓
- **Excluded:** OpenRouter / OpenAI surface / `anthropic_api` provider impl — out of scope per
  instruction (column allowed, not implemented).

No placeholders: each batch lists exact files, contracts, test cases, verification commands, and
ends with commit + push. Type consistency: `Store`/`Provider`/`Usage`/`ChatRequest` defined in
Batches 0-2 and reused verbatim downstream.
