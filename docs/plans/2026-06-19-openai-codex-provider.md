# OpenAI / ChatGPT-Codex Provider — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a second backend provider that serves the operator's ChatGPT subscription via the Codex OAuth + Responses backend, exposed as a clean OpenAI `/v1/chat/completions` surface, sharing all project/tag usage tracking and budget with the existing Claude Max provider.

**Architecture:** Generalise the gateway's wire-coupled core into a wire-agnostic shape (a minimal `llm.Request` interface, a generic handler parameterised per route, store account/token access scoped by provider), then add an isolated `codex` provider that translates Chat Completions ⇄ Responses, spoofs the Codex client over OAuth, and runs a multi-account pool. Phase 1 is a pure refactor with zero behaviour change (Claude Max keeps passing its E2E suite); Phases 2–5 build the new provider.

**Tech Stack:** Go (stdlib `net/http`), PostgreSQL via `pgx/v5`, hexagonal layout (`internal/domain`, `internal/adapter`, `cmd`). Backend: `https://chatgpt.com/backend-api/codex/responses` (OpenAI Responses wire) over OAuth.

**Design spec:** `docs/specs/2026-06-19-openai-codex-provider-design.md`. Section references below (e.g. "spec §5.2") point at its mapping tables.

## Global Constraints

- **KISS / YAGNI:** simplest code that works; no premature abstraction; implement only what a task needs.
- **Minimal public API:** unexported by default; export only what another package strictly needs.
- **Docstrings:** every function, type, struct, interface, struct field has a Go-format docstring (starts with the symbol name). No doc comment above `package`.
- **Function size:** 15–25 lines, 30 max. One responsibility. Split into named helpers.
- **File size:** 200–300 lines, 400 max. Split by domain.
- **Error wrapping:** always `fmt.Errorf("operation description:\n%w", err)` — `\n` before `%w`.
- **Domain purity:** `internal/domain` imports no HTTP, SQL, or provider wire format.
- **Testing:** E2E-first against the real gateway. The Anthropic surface hits the real Anthropic API; the Codex surface hits the real Codex backend with seeded test credentials. Assert on shape/plausibility, never exact text. Retry transient upstream (5xx/network/timeout); never retry the gateway's own 402/503. Local stub upstream ONLY for failure injection. Pure-logic edge cases (translation tables, billing arithmetic) covered by domain unit tests without network.
- **go.mod hygiene:** never commit a `replace` directive.
- **Commit convention:** title line = a few words, NO prefix. Blank line. Then one change per line prefixed `[+]` add · `[-]` remove · `[&]` change · `[!]` fix. NO footers, NO "Generated with", NO "Co-Authored-By", NO emojis.
- **Branch:** all work on `feat/openai-codex-provider` (already created). Never commit to `main`.

---

## File Structure

**Phase 1 — foundation (refactor, no behaviour change):**
- Modify `internal/domain/llm/request.go` — add the `Request` interface; `ChatRequest` keeps satisfying it.
- Modify `internal/domain/ports.go` — `Provider.Send` takes `llm.Request`; remove `DefaultRoute` from the `Store` port.
- Modify `internal/adapter/provider/claudemax/provider.go` — `Send` accepts `llm.Request`, re-parses to `ChatRequest` internally; account/token store calls become provider-scoped.
- Create `internal/adapter/httpserver/handler.go` — the generic per-route handler (extracted from `messages.go`).
- Modify `internal/adapter/httpserver/server.go` / `messages.go` / `streaming.go` — register routes from a `[]route`; handler no longer calls `DefaultRoute`.
- Modify `internal/adapter/postgres/accounts.go` / `store.go` — `LoadAccounts`/`SetCooldown`/token reads take a provider name; drop `DefaultRoute`/`SetDefaultProvider`.
- Modify `cmd/llmgw/main.go` — build providers and pass routes to `httpserver.New`.

**Phases 2–5 — the `codex` provider (new package `internal/adapter/provider/codex`):**
- `provider.go` — `Provider`, `New`, `Send` (pool loop, mirrors claudemax).
- `oauth.go` — token manager (refresh from seeded `refresh_token`, single-flight).
- `spoof.go` — Codex client headers (`originator`, User-Agent, `ChatGPT-Account-ID`, …).
- `translate_request.go` — Chat Completions → Responses (spec §5.2).
- `translate_response.go` — Responses → Chat Completions, buffered (spec §5.3).
- `translate_stream.go` — Responses SSE → Chat Completions SSE, with filtering (spec §5.3).
- `pool.go` — round-robin + cooldown (transposed from claudemax `pool.go`).
- `errors.go` — typed errors + classifier.
- Migrations under `internal/adapter/postgres/migrations/`.
- Config additions in `internal/config`.

---

## Phase 1 — Wire-agnostic foundation (no behaviour change)

### Task 1: Introduce the `llm.Request` interface and retype the port

**Files:**
- Modify: `internal/domain/llm/request.go`
- Modify: `internal/domain/ports.go:124-129`
- Modify: `internal/adapter/provider/claudemax/provider.go` (`Send`, `sendVia`, `buildRequest` signatures)
- Test: `internal/domain/llm/request_test.go`

**Interfaces:**
- Produces: `llm.Request` interface — `Model() string`, `Stream() bool`, `Bytes() []byte`. `llm.ChatRequest` satisfies it (all three methods already exist).
- Produces: `domain.Provider.Send(ctx context.Context, req llm.Request, out StreamSink) (usage.Usage, error)`.

- [ ] **Step 1: Write the failing test** (compile-time + behavioural assertion that `ChatRequest` is an `llm.Request`)

In `internal/domain/llm/request_test.go` add:

```go
func TestChatRequestSatisfiesRequest(t *testing.T) {
	var req Request = ChatRequest{body: map[string]any{"model": "claude-x", "stream": true}}

	if req.Model() != "claude-x" {
		t.Fatalf("Model() = %q, want claude-x", req.Model())
	}
	if !req.Stream() {
		t.Fatal("Stream() = false, want true")
	}
	if len(req.Bytes()) == 0 {
		t.Fatal("Bytes() returned empty")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/llm/ -run TestChatRequestSatisfiesRequest`
Expected: FAIL — `undefined: Request`.

- [ ] **Step 3: Add the interface**

In `internal/domain/llm/request.go`, above `ChatRequest`:

```go
// Request is the wire-agnostic view the gateway needs to meter and route a call, regardless
// of the provider's wire format. The HTTP handler resolves it from the request body; the
// provider re-parses Bytes() in its own wire to do the actual forwarding/translation.
type Request interface {
	// Model returns the requested model id, used for usage rows and routing.
	Model() string

	// Stream reports whether the consumer asked for a streamed (SSE) response.
	Stream() bool

	// Bytes returns the raw client request body, parsed by the provider's wire.
	Bytes() []byte
}
```

- [ ] **Step 4: Retype the port**

In `internal/domain/ports.go`, change `Send` to take `llm.Request`:

```go
// Provider forwards a request to an upstream LLM backend.
type Provider interface {
	// Send forwards req upstream. For non-streaming it writes the JSON body to out and
	// returns the Usage; for streaming it relays SSE to out while accumulating Usage.
	Send(ctx context.Context, req llm.Request, out StreamSink) (usage.Usage, error)
}
```

- [ ] **Step 5: Adapt claudemax to accept the interface, re-parse internally**

In `internal/adapter/provider/claudemax/provider.go`, change `Send` and `sendVia` to take `llm.Request`, and re-parse to the concrete `ChatRequest` once at the top of `Send`:

```go
func (p *Provider) Send(ctx context.Context, req llm.Request, out domain.StreamSink) (usage.Usage, error) {
	chat, err := llm.ParseRequest(req.Bytes())
	if err != nil {
		return usage.Usage{}, fmt.Errorf("parse anthropic request:\n%w", err)
	}
	// ... existing body, but call p.sendVia(ctx, account, chat, out) with the concrete type
}
```

Keep `sendVia`/`buildRequest` taking the concrete `llm.ChatRequest` (they use `Normalize`/`FirstUserText`/`WithClaudeCodeSystem`). Only `Send`'s public signature changes.

- [ ] **Step 6: Run the test and the package build**

Run: `go test ./internal/domain/llm/ -run TestChatRequestSatisfiesRequest && go build ./...`
Expected: PASS and a clean build (handler still compiles — it passes `llm.ChatRequest`, which satisfies `llm.Request`).

- [ ] **Step 7: Commit**

```bash
git add internal/domain/llm/request.go internal/domain/llm/request_test.go internal/domain/ports.go internal/adapter/provider/claudemax/provider.go
git commit -m "Wire-agnostic provider request" -m $'[&] add llm.Request interface (Model/Stream/Bytes)\n[&] Provider.Send takes llm.Request; claudemax re-parses internally'
```

### Task 2: Scope store account/token access by provider

**Files:**
- Modify: `internal/adapter/postgres/accounts.go` (`LoadAccounts`, `SetCooldown`)
- Modify: `internal/adapter/postgres/store.go` (token read/write helpers; `defaultProviderID` → `providerIDByName`)
- Modify: `internal/adapter/provider/claudemax/pool.go` (`accountStore` interface), `oauth.go` (`tokenStore` interface)
- Test: `internal/adapter/postgres/accounts_test.go` (if a DB test harness exists) or rely on existing E2E

**Interfaces:**
- Produces: store methods gain a `providerName string` first argument, e.g. `LoadAccounts(ctx, providerName) ([]domain.Account, error)`, `SetCooldown(ctx, providerName, account, until)`. The provider passes its own name (`postgres.DefaultProviderName` for claudemax, the codex provider name later).

- [ ] **Step 1: Write the failing test**

If a Postgres test harness exists (check `internal/adapter/postgres/*_test.go`), add a test seeding two providers each with one account and asserting `LoadAccounts` returns only the named provider's account:

```go
func TestLoadAccountsScopedByProvider(t *testing.T) {
	store := newTestStore(t) // existing harness helper
	seedProviderAccount(t, store, "prov-a", "acct-a")
	seedProviderAccount(t, store, "prov-b", "acct-b")

	got, err := store.LoadAccounts(context.Background(), "prov-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Label != "acct-a" {
		t.Fatalf("LoadAccounts(prov-a) = %+v, want [acct-a]", got)
	}
}
```

If no such harness exists, skip the unit test and rely on the existing claudemax E2E suite as the regression gate (note this in the commit).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/postgres/ -run TestLoadAccountsScopedByProvider`
Expected: FAIL — `LoadAccounts` takes no provider arg yet (compile error).

- [ ] **Step 3: Add a provider-id-by-name helper and thread the name through**

In `store.go`, add (or rename `defaultProviderID` to) `providerIDByName(ctx, name)`. Change `LoadAccounts`/`SetCooldown` in `accounts.go` to accept `providerName string` and call `providerIDByName(ctx, providerName)`. Do the same for the token read/update queries in `store.go`.

- [ ] **Step 4: Update claudemax interfaces and call sites**

In `pool.go` and `oauth.go`, change the `accountStore`/`tokenStore` method signatures to include `providerName`, and have the provider pass its configured name (store it on the `Provider` struct, default `postgres.DefaultProviderName`).

- [ ] **Step 5: Run tests + build**

Run: `go build ./... && go test ./internal/adapter/postgres/ -run TestLoadAccountsScopedByProvider`
Expected: PASS (or build-only PASS if no harness).

- [ ] **Step 6: Commit**

```bash
git add internal/adapter/postgres/ internal/adapter/provider/claudemax/
git commit -m "Scope account and token access by provider" -m $'[&] LoadAccounts/SetCooldown/token queries take a provider name\n[&] claudemax passes its provider name to the store'
```

### Task 3: Extract a generic per-route handler

**Files:**
- Create: `internal/adapter/httpserver/handler.go`
- Modify: `internal/adapter/httpserver/messages.go` (drop `DefaultRoute` lookup; provider + wire injected)
- Modify: `internal/adapter/httpserver/streaming.go` (uses `req.Model()`/`req.Stream()` via `llm.Request`)
- Test: existing httpserver tests + claudemax E2E

**Interfaces:**
- Produces: `type wire interface { Parse(body []byte) (llm.Request, error); DefaultTag() string }`.
- Produces: `newHandler(store domain.Store, provider domain.Provider, w wire, providerName, defaultProject string) *handler` with `handle(w, r)`.
- Consumes: `llm.Request` (Task 1), provider-scoped store (Task 2).

- [ ] **Step 1: Write the failing test** (a fake wire + fake provider drive the handler end to end, asserting usage recorded and body relayed)

In `internal/adapter/httpserver/handler_test.go`:

```go
func TestHandlerForwardsAndRecords(t *testing.T) {
	store := &fakeStore{projectID: 7}
	prov := &fakeProvider{body: []byte(`{"ok":true}`), usage: usage.Usage{InputTokens: 3, OutputTokens: 5}}
	h := newHandler(store, prov, anthropicWire{}, "claude-max", "")

	rec := httptest.NewRecorder()
	body := `{"model":"m","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Project", "p")
	h.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if store.recorded.OutputTokens != 5 {
		t.Fatalf("recorded output tokens = %d, want 5", store.recorded.OutputTokens)
	}
}
```

(Reuse or add `fakeStore`/`fakeProvider` test doubles; `anthropicWire` is the production wire from Step 3.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/httpserver/ -run TestHandlerForwardsAndRecords`
Expected: FAIL — `newHandler`/`anthropicWire` undefined.

- [ ] **Step 3: Move the handler logic into `handler.go`, parameterised**

Rename `messagesHandler` to `handler`; add `provider domain.Provider` and `w wire` fields. Replace the `h.store.DefaultRoute(...)` call in `forward` with the injected `h.provider`. Replace `parseBody` to call `h.w.Parse(body)`. Replace the hard-coded `defaultTag` with `h.w.DefaultTag()`. Define `anthropicWire{}` whose `Parse` calls `llm.ParseRequest` and `DefaultTag` returns `"default"`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/adapter/httpserver/ -run TestHandlerForwardsAndRecords`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/httpserver/
git commit -m "Generic per-route handler" -m $'[+] wire interface + anthropicWire (Parse + DefaultTag)\n[&] handler takes injected provider and wire; drop DefaultRoute lookup\n[+] handler_test driving forward + usage recording with test doubles'
```

### Task 4: Register routes and wire providers in the composition root

**Files:**
- Modify: `internal/adapter/httpserver/server.go` (`New` takes `[]route`; register each)
- Modify: `internal/domain/ports.go` (remove `DefaultRoute` from `Store`)
- Modify: `internal/adapter/postgres/store.go` (remove `DefaultRoute`/`SetDefaultProvider`/`provider` field)
- Modify: `cmd/llmgw/main.go`
- Test: claudemax E2E suite (full regression)

**Interfaces:**
- Produces: `type route struct { Path string; Provider domain.Provider; Wire wire; ProviderName string }` and `New(store domain.Store, defaultProject string, routes []route) *Server`.

- [ ] **Step 1: Replace `New` signature and route registration**

In `server.go`:

```go
// New constructs a Server registering one handler per route. Each route binds a path to its
// provider and wire (Anthropic Messages on /v1/messages, OpenAI Chat Completions on
// /v1/chat/completions). defaultProject is attributed to requests with no X-Project header.
func New(store domain.Store, defaultProject string, routes []route) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	for _, rt := range routes {
		h := newHandler(store, rt.Provider, rt.Wire, rt.ProviderName, defaultProject)
		mux.HandleFunc("POST "+rt.Path, h.handle)
	}
	// ... server struct as before
}
```

Remove the `/chat/completions` → Anthropic alias (it becomes the codex route in Phase 4).

- [ ] **Step 2: Remove `DefaultRoute` from the port and store**

Delete `DefaultRoute` from the `Store` interface in `ports.go`, and delete `DefaultRoute`/`SetDefaultProvider`/the `provider` field from `store.go`.

- [ ] **Step 3: Wire in main.go**

```go
claude := claudemax.New(store, cfg.ClaudeCodeVersion) // already provider-scoped (Task 2)
routes := []httpserver.Route{
	{Path: "/v1/messages", Provider: claude, Wire: httpserver.AnthropicWire{}, ProviderName: postgres.DefaultProviderName},
}
server := httpserver.New(store, cfg.DefaultProject, routes)
```

(Export `Route`, `AnthropicWire`, and the wire interface as needed for the composition root — keep everything else unexported.)

- [ ] **Step 4: Build + run the full claudemax E2E suite**

Run: `go build ./... && go test ./... ` (with test credentials present for the real-API suite, per the project's E2E gating)
Expected: PASS — Claude Max behaviour is unchanged; only wiring moved.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "Route-based server wiring" -m $'[&] httpserver.New registers one handler per route\n[-] remove DefaultRoute/SetDefaultProvider singleton\n[-] remove the /chat/completions Anthropic alias hack\n[&] main wires the Anthropic route explicitly'
```

---

## Phase 2 — `codex` skeleton: OAuth + spoofed Responses call

> **Capture dependency:** Phases 2–3 imitate the real Codex client. Before coding the spoof and translation, capture one real round-trip with a debugging proxy (e.g. `mitmproxy`) in front of the official Codex CLI authenticated with the test ChatGPT account: record the exact request headers (`originator`, `User-Agent`, `ChatGPT-Account-ID`, `x-client-request-id`, …), the `instructions` value, the request body shape, the OAuth token endpoint + client_id, and a full streamed Responses event sequence. These captures are the source of truth for Tasks 5–10. Store a redacted sample under `internal/adapter/provider/codex/testdata/`.

### Task 5: Migrations — provider type, account id, model prices

**Files:**
- Create: `internal/adapter/postgres/migrations/0006_chatgpt_codex_provider.sql`
- Create: `internal/adapter/postgres/migrations/0007_oauth_chatgpt_account_id.sql`
- Create: `internal/adapter/postgres/migrations/0008_seed_codex_model_prices.sql`

**Interfaces:**
- Produces: a `provider` row `name='chatgpt-codex'`, `type='chatgpt_codex_oauth'`; an `oauth_token.chatgpt_account_id` column; `model_price` rows for the Codex models.

- [ ] **Step 1: Provider type + seed row**

`0006_chatgpt_codex_provider.sql`:

```sql
-- Allow the ChatGPT-subscription (Codex OAuth) provider type and seed its single row.
ALTER TABLE provider DROP CONSTRAINT provider_type_check;
ALTER TABLE provider ADD CONSTRAINT provider_type_check
    CHECK (type IN ('claude_max_oauth', 'anthropic_api', 'openrouter', 'chatgpt_codex_oauth'));

INSERT INTO provider (name, type) VALUES ('chatgpt-codex', 'chatgpt_codex_oauth')
ON CONFLICT (name) DO NOTHING;
```

- [ ] **Step 2: Per-account ChatGPT-Account-ID column**

`0007_oauth_chatgpt_account_id.sql`:

```sql
-- The ChatGPT-Account-ID header is per Codex account; Claude Max rows leave it NULL.
ALTER TABLE oauth_token ADD COLUMN chatgpt_account_id TEXT;
```

- [ ] **Step 3: Seed notional model prices** (values per the real subscription model list confirmed during capture; placeholders below are list-price USD per million tokens — replace with verified numbers)

`0008_seed_codex_model_prices.sql`:

```sql
-- Notional list prices (USD per MILLION tokens) for the Codex-served models. Cost is notional
-- only (the subscription is flat-rate) but keeps cross-provider budgets comparable.
INSERT INTO model_price (model, input_usd_per_mtok, output_usd_per_mtok) VALUES
    ('gpt-5',       1.25, 10.00),
    ('gpt-5-codex', 1.25, 10.00),
    ('gpt-5.5',     1.25, 10.00)
ON CONFLICT (model) DO NOTHING;
```

- [ ] **Step 4: Apply migrations against a scratch DB**

Run: start the gateway (or the migration runner) against an ephemeral Postgres and confirm migrations apply cleanly and `SELECT * FROM provider` shows `chatgpt-codex`.
Expected: no migration error; the row exists.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/postgres/migrations/
git commit -m "Codex provider migrations" -m $'[+] chatgpt_codex_oauth provider type + seed row\n[+] oauth_token.chatgpt_account_id column\n[+] notional model_price rows for Codex models'
```

### Task 6: Config + credential seeding for Codex accounts

**Files:**
- Modify: `internal/config` (add `CodexVersion`/client identifiers and `CodexAccounts []CodexAccount{Label, RefreshToken, AccountID}`)
- Modify: `internal/adapter/postgres/store.go` (add `SeedCodexAccount(ctx, label, refreshToken, accountID)` — idempotent insert, mirroring `SeedSessionKey`)
- Modify: `cmd/llmgw/main.go` (`seedCodexAccounts`, mirroring `seedSessionKeys`)
- Test: `internal/config` parse test

**Interfaces:**
- Produces: `config.Config.CodexAccounts`, `config.Config.CodexVersion`; `store.SeedCodexAccount` writing an `oauth_token` row under the `chatgpt-codex` provider with `chatgpt_account_id` set.

- [ ] **Step 1: Write the failing config test**

```go
func TestLoadParsesCodexAccounts(t *testing.T) {
	t.Setenv("LLMGW_CODEX_ACCOUNTS", "main:rt_abc:acct_123")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CodexAccounts) != 1 || cfg.CodexAccounts[0].AccountID != "acct_123" {
		t.Fatalf("CodexAccounts = %+v", cfg.CodexAccounts)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadParsesCodexAccounts`
Expected: FAIL — field/parsing absent.

- [ ] **Step 3: Implement parsing + seeding**

Add the env parsing (follow the existing `SessionKeys` parsing shape), the `CodexAccount` struct, `store.SeedCodexAccount` (idempotent `INSERT ... ON CONFLICT (provider_id, account_label) DO NOTHING` writing `refresh_token` + `chatgpt_account_id`), and the `seedCodexAccounts` boot step in `main.go`.

- [ ] **Step 4: Run test + build**

Run: `go test ./internal/config/ -run TestLoadParsesCodexAccounts && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/ internal/adapter/postgres/store.go cmd/llmgw/main.go
git commit -m "Codex account config and seeding" -m $'[+] CodexAccounts/CodexVersion config\n[+] SeedCodexAccount idempotent insert with chatgpt_account_id\n[+] seed Codex accounts at boot'
```

### Task 7: OAuth token manager + Codex spoof + minimal Responses call

**Files:**
- Create: `internal/adapter/provider/codex/oauth.go`, `spoof.go`, `provider.go`, `errors.go`
- Test: `internal/adapter/provider/codex/spoof_test.go`; an E2E smoke in the gateway E2E suite

**Interfaces:**
- Produces: `codex.New(store accountStore, version string) *Provider` satisfying `domain.Provider`.
- Produces: `spoof.decorate(req *http.Request, accessToken, accountID string)` setting the captured Codex headers.
- Consumes: provider-scoped store (Task 2), captured headers/endpoint (Phase 2 capture).

- [ ] **Step 1: Write the failing spoof test** (asserts the required Codex headers are set — values from the capture)

```go
func TestSpoofDecorateSetsCodexHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example/responses", nil)
	spoof{version: "0.40.0"}.decorate(req, "tok", "acct_123")

	for _, h := range []string{"Authorization", "User-Agent", "originator", "ChatGPT-Account-ID"} {
		if req.Header.Get(h) == "" {
			t.Fatalf("missing header %q", h)
		}
	}
	if req.Header.Get("ChatGPT-Account-ID") != "acct_123" {
		t.Fatal("account id not propagated")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/provider/codex/ -run TestSpoofDecorateSetsCodexHeaders`
Expected: FAIL — package/symbol undefined.

- [ ] **Step 3: Implement oauth.go, spoof.go, a single-account provider.go (non-streaming, raw passthrough first)**

Token manager mirrors `claudemax/oauth.go` (single-flight refresh from `refresh_token`; the token endpoint + client_id from the capture). `spoof.decorate` sets the captured headers. `provider.go` `Send` forwards to the Responses endpoint with `store:false`, the minimal `instructions`, and the request body **untranslated for now** (a raw Responses body built from the capture) — the goal of this task is to prove the spoof + OAuth reach a 200, not translation. Add `errors.go` with `RateLimitError`/`UpstreamError`/`DeadRefreshTokenError` (same shapes as claudemax).

- [ ] **Step 4: Run unit test + E2E smoke**

Run: `go test ./internal/adapter/provider/codex/ -run TestSpoofDecorateSetsCodexHeaders` then the gateway E2E smoke that POSTs a trivial hardcoded Responses body and asserts a 200 with non-empty content.
Expected: PASS; the smoke proves the subscription answers.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/
git commit -m "Codex OAuth and spoof skeleton" -m $'[+] codex OAuth token manager (single-flight refresh)\n[+] Codex client header spoof\n[+] minimal non-streaming Responses call proving the subscription path\n[+] typed upstream errors'
```

---

## Phase 3 — Translation

### Task 8: Request translation (Chat Completions → Responses)

**Files:**
- Create: `internal/adapter/provider/codex/translate_request.go`
- Create: `internal/adapter/provider/codex/request.go` (an `openaiRequest` type satisfying `llm.Request`)
- Test: `internal/adapter/provider/codex/translate_request_test.go`

**Interfaces:**
- Produces: `openaiRequest` with `Model()`/`Stream()`/`Bytes()` (the OpenAI wire for `httpserver`), and `translateRequest(body []byte, instructions string) ([]byte, error)` producing the Responses body per spec §5.2.

- [ ] **Step 1: Write the failing test** (covers the §5.2 table: system→developer input, tools, max_tokens→max_output_tokens, store:false)

```go
func TestTranslateRequestMapsCoreFields(t *testing.T) {
	in := []byte(`{"model":"gpt-5","max_tokens":256,"messages":[
		{"role":"system","content":"be terse"},
		{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`)

	out, err := translateRequest(in, "CODEX_MIN")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)

	if got["store"] != false {
		t.Fatal("store must be false")
	}
	if got["max_output_tokens"].(float64) != 256 {
		t.Fatal("max_tokens not mapped to max_output_tokens")
	}
	if got["instructions"] != "CODEX_MIN" {
		t.Fatal("instructions not set to the minimal Codex prompt")
	}
	// developer message present in input, tools mapped to Responses function shape
	assertHasDeveloperInput(t, got)
	assertHasFunctionTool(t, got)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/provider/codex/ -run TestTranslateRequestMapsCoreFields`
Expected: FAIL — `translateRequest` undefined.

- [ ] **Step 3: Implement `translateRequest` per spec §5.2**

Map messages → `input[]` items (system/developer → a `developer` item, NOT `instructions`; user/assistant → message items; tool results → `function_call_output`; assistant `tool_calls` → `function_call`), `tools[]` → Responses function tools, `tool_choice`, `max_tokens` → `max_output_tokens`, force `store:false` and `stream:true`, set `instructions` to the passed minimal Codex prompt, validate/map `model`. Keep each mapping in a small named helper (function-size limit).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/adapter/provider/codex/ -run TestTranslateRequestMapsCoreFields`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/translate_request.go internal/adapter/provider/codex/request.go internal/adapter/provider/codex/translate_request_test.go
git commit -m "Chat Completions to Responses request translation" -m $'[+] openaiRequest wire type (Model/Stream/Bytes)\n[+] translateRequest per spec §5.2 (messages, tools, max_output_tokens, store:false, instructions)'
```

### Task 9: Non-streaming response translation (Responses → Chat Completions)

**Files:**
- Create: `internal/adapter/provider/codex/translate_response.go`
- Modify: `internal/adapter/provider/codex/provider.go` (non-streaming path uses it)
- Test: `internal/adapter/provider/codex/translate_response_test.go`

**Interfaces:**
- Produces: `translateResponse(responsesBody []byte) (chatCompletionsJSON []byte, u usage.Usage, err error)` folding `output[]` into one choice, dropping reasoning, mapping usage per spec §5.3.

- [ ] **Step 1: Write the failing test** (a captured Responses completion → a valid Chat Completions object; reasoning dropped; usage mapped)

```go
func TestTranslateResponseFoldsAndMapsUsage(t *testing.T) {
	body := readTestdata(t, "responses_completion.json") // from the Phase 2 capture
	out, u, err := translateResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	var cc map[string]any
	_ = json.Unmarshal(out, &cc)

	if cc["object"] != "chat.completion" {
		t.Fatal("not a chat.completion object")
	}
	if u.InputTokens == 0 || u.OutputTokens == 0 {
		t.Fatal("usage not mapped from Responses usage")
	}
	assertNoReasoningLeaked(t, out)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/provider/codex/ -run TestTranslateResponseFoldsAndMapsUsage`
Expected: FAIL — `translateResponse` undefined (add the captured `testdata/responses_completion.json`).

- [ ] **Step 3: Implement `translateResponse` per spec §5.3 (non-streaming)**

Parse the Responses object, fold `output[]` message items into `choices[0].message.content`, fold `function_call` items into `choices[0].message.tool_calls`, drop `reasoning` items, map `finish_reason`, translate `usage.input_tokens/output_tokens` → `prompt_tokens/completion_tokens`. Wire the non-streaming `Send` path to read the upstream SSE to completion then call this.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/adapter/provider/codex/ -run TestTranslateResponseFoldsAndMapsUsage`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/translate_response.go internal/adapter/provider/codex/translate_response_test.go internal/adapter/provider/codex/testdata/ internal/adapter/provider/codex/provider.go
git commit -m "Responses to Chat Completions response translation" -m $'[+] translateResponse folding output[] into one choice, dropping reasoning\n[+] usage mapping input/output -> prompt/completion tokens\n[&] non-streaming Send path emits a clean chat.completion'
```

### Task 10: Streaming translation (Responses SSE → Chat Completions SSE)

**Files:**
- Create: `internal/adapter/provider/codex/translate_stream.go`
- Modify: `internal/adapter/provider/codex/provider.go` (streaming path)
- Test: `internal/adapter/provider/codex/translate_stream_test.go`

**Interfaces:**
- Produces: `relayTranslatedStream(upstream io.Reader, out domain.StreamSink, includeUsage bool) (usage.Usage, error)` translating events on the fly per spec §5.3 and filtering Codex-prompt/reasoning events.

- [ ] **Step 1: Write the failing test** (feed a captured Responses SSE sequence; assert Chat Completions chunks, `[DONE]`, no prompt/reasoning leak, usage accumulated)

```go
func TestRelayTranslatedStreamProducesCleanChunks(t *testing.T) {
	upstream := readTestdataReader(t, "responses_stream.sse") // from the Phase 2 capture
	sink := &captureSink{}

	u, err := relayTranslatedStream(upstream, sink, true)
	if err != nil {
		t.Fatal(err)
	}
	s := sink.String()

	if !strings.Contains(s, `"delta"`) || !strings.HasSuffix(strings.TrimSpace(s), "data: [DONE]") {
		t.Fatal("not a well-formed Chat Completions SSE stream")
	}
	if strings.Contains(s, "instructions") || strings.Contains(s, "reasoning") {
		t.Fatal("Codex prompt or reasoning leaked into the client stream")
	}
	if u.OutputTokens == 0 {
		t.Fatal("usage not accumulated from response.completed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/provider/codex/ -run TestRelayTranslatedStreamProducesCleanChunks`
Expected: FAIL — `relayTranslatedStream` undefined (add `testdata/responses_stream.sse`).

- [ ] **Step 3: Implement the streaming translator per spec §5.3**

Read upstream SSE line by line (reuse the `bufio` + `sseData` shape from `claudemax/stream.go`). Map `response.output_text.delta` → a `choices[0].delta.content` chunk; `response.function_call_arguments.delta`/item-added → `delta.tool_calls`; `response.completed` → a final `finish_reason` chunk plus a usage chunk when `includeUsage`; **drop** `response.created`/`response.in_progress`/all `reasoning` events; emit `data: [DONE]` at the end. Flush after each emitted event. Accumulate usage from `response.completed`. Wire the streaming `Send` path to use it.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/adapter/provider/codex/ -run TestRelayTranslatedStreamProducesCleanChunks`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/translate_stream.go internal/adapter/provider/codex/translate_stream_test.go internal/adapter/provider/codex/testdata/ internal/adapter/provider/codex/provider.go
git commit -m "Responses to Chat Completions streaming translation" -m $'[+] relayTranslatedStream mapping Responses events to chat.completion.chunk\n[+] drop Codex-prompt and reasoning events (clean output guarantee)\n[+] accumulate usage from response.completed; terminate with [DONE]'
```

---

## Phase 4 — Wire the route + metering

### Task 11: Register the `/v1/chat/completions` route with OpenAI wire and budget

**Files:**
- Modify: `internal/adapter/httpserver/` (add `OpenAIWire` — `Parse` builds an `openaiRequest`, `DefaultTag` returns `"agentic"`; OpenAI-shaped error envelope)
- Modify: `cmd/llmgw/main.go` (build the codex provider, add the route)
- Test: gateway E2E (non-streaming, streaming, tools, budget)

**Interfaces:**
- Consumes: `openaiRequest` (Task 8), `codex.New` (Task 7), generic handler + routes (Tasks 3–4).
- Produces: a live `/v1/chat/completions` route metering into shared `usage_event` with `provider="chatgpt-codex"`, default tag `agentic`.

- [ ] **Step 1: Add `OpenAIWire` + OpenAI error envelope**

`OpenAIWire.Parse(body)` returns `openaiRequest{body: body}`; `DefaultTag()` returns `"agentic"`. Add an OpenAI-shaped error writer (`{"error":{"message":...,"type":...}}`) and select it for this route so OpenAI SDKs parse gateway errors. (Keep the handler's error mapping generic; inject the envelope renderer per route.)

- [ ] **Step 2: Wire the route in main.go**

```go
codexProv := codex.New(store, cfg.CodexVersion)
routes = append(routes, httpserver.Route{
	Path: "/v1/chat/completions", Provider: codexProv, Wire: httpserver.OpenAIWire{}, ProviderName: "chatgpt-codex",
})
```

- [ ] **Step 3: E2E — non-streaming, streaming, tools, budget**

Add E2E tests (real Codex backend, seeded test creds): a non-streaming chat returns a valid `chat.completion` and records usage; a streaming chat returns clean chunks ending `[DONE]` with no prompt/reasoning leak; a forced function call round-trips; a `calls` budget cap on a project blocks the 2nd call with 402; usage rows carry `provider="chatgpt-codex"` and tag `agentic` by default.

- [ ] **Step 4: Run the E2E suite**

Run: `go test ./...` (test credentials present)
Expected: PASS; the new route is live and metered.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/httpserver/ cmd/llmgw/main.go internal/...e2e...
git commit -m "Live OpenAI Codex route with metering" -m $'[+] OpenAIWire (Parse + agentic default tag) and OpenAI error envelope\n[+] /v1/chat/completions route -> codex provider\n[+] E2E: non-streaming, streaming, tools, shared budget'
```

---

## Phase 5 — Multi-account pool

### Task 12: Transpose the account pool + reset-aware cooldown

**Files:**
- Create: `internal/adapter/provider/codex/pool.go`
- Modify: `internal/adapter/provider/codex/provider.go` (`Send` loops over `selectOrder`, cools on failover)
- Modify: `internal/adapter/provider/codex/errors.go` (`AllCoolingError`, `cooldownFor`, `classifyUpstream` for 429/401/403/5xx)
- Test: `internal/adapter/provider/codex/pool_test.go`; failure-injection E2E with the local stub upstream

**Interfaces:**
- Consumes: provider-scoped `LoadAccounts`/`SetCooldown` (Task 2).
- Produces: round-robin `selectOrder`, `AllCoolingError` → handler 503 + Retry-After (already mapped in `writeProviderError`; add the codex error to its `errors.As` chain or reuse a shared shape).

- [ ] **Step 1: Write the failing pool test** (mirror `claudemax/pool_test.go`: a cooling account is skipped; all-cooling yields `AllCoolingError` with the soonest retry)

```go
func TestSelectOrderSkipsCoolingAccounts(t *testing.T) {
	p := &Provider{}
	now := time.Now()
	accounts := []domain.Account{
		{Label: "a", CooldownUntil: now.Add(time.Minute)},
		{Label: "b"},
	}
	order := p.selectOrder(accounts, now)
	if len(order) != 1 || order[0] != "b" {
		t.Fatalf("selectOrder = %v, want [b]", order)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/provider/codex/ -run TestSelectOrderSkipsCoolingAccounts`
Expected: FAIL — `selectOrder` undefined.

- [ ] **Step 3: Transpose `pool.go` from claudemax**

Copy `selectOrder`/`cooling`/`cool`/`allCooling`/`soonestCooldown`/`retryAfterUntil` and the round-robin cursor; adapt the cooldown durations and `cooldownFor`/`classifyUpstream` to the Codex statuses (429 reset-aware, 401 dead-token, 403 Cloudflare/originator short-cool failover, 5xx failover, other 4xx surfaced). Make `Send` loop over `selectOrder` exactly like claudemax.

- [ ] **Step 4: Run test + failure-injection E2E**

Run: `go test ./internal/adapter/provider/codex/ -run TestSelectOrderSkipsCoolingAccounts` then the E2E with the local stub upstream forcing 429 → cooldown, all-cooling → 503 + Retry-After, refresh failure → dead-token handling.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/
git commit -m "Codex multi-account pool and cooldown" -m $'[+] round-robin selectOrder with reset-aware cooldown\n[+] cooldownFor/classifyUpstream for 429/401/403/5xx\n[+] AllCoolingError -> 503 + Retry-After\n[+] failure-injection E2E via stub upstream'
```

---

## Self-Review

**Spec coverage:**
- §3 surface (chat/completions, stream/non-stream, tools, clean output, X-Project/X-Tags, agentic default, alias removal) → Tasks 4, 8–11.
- §4 architecture (llm.Request, generic handler, provider injection, drop DefaultRoute, isolated package) → Tasks 1, 3, 4; codex package Tasks 7–12.
- §5.1 backend call (endpoint, auth, spoof headers, store:false, minimal instructions) → Tasks 7, 8.
- §5.2 request translation → Task 8.
- §5.3 response + streaming translation (with filtering) → Tasks 9, 10.
- §5.4 pool → Task 12.
- §5.5 errors/cooldown → Tasks 7 (typed errors), 12 (classifier/failover).
- §6 budget reuse (notional cost, provider label) → Tasks 4, 11.
- §7 storage (provider type, account_id, prices, seeding) → Tasks 5, 6.
- §8 config → Task 6.
- §10 testing (E2E non-stream/stream/tools/budget/failure-injection; unit translation) → Tasks 8–12.
- §11 build order → Phases 1–5 map 1:1 to the slices.

**Capture dependency:** Tasks 7–10 depend on the Phase 2 capture (headers, OAuth endpoint, Responses event shapes). This is flagged at Phase 2 and is concrete work, not a placeholder.

**Open value to verify at capture time (replace before merge):** exact Codex header set and `User-Agent`/`originator` values; OAuth token endpoint + client_id; minimal valid `instructions` content; the real Codex model list and verified `model_price` numbers (Task 5 uses placeholders).

**Type consistency:** `llm.Request`/`Bytes()` (Task 1) used by `openaiRequest` (Task 8) and the handler (Task 3); `translateRequest`/`translateResponse`/`relayTranslatedStream` names consistent across Tasks 8–11; pool symbols match the claudemax originals (Task 12).
