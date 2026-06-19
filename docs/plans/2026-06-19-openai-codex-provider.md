# OpenAI / ChatGPT-Codex Provider — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a second backend provider that serves the operator's ChatGPT subscription via the Codex OAuth + Responses backend, exposed as a clean OpenAI `/v1/chat/completions` surface, sharing all project/tag usage tracking and budget with the existing Claude Max provider.

**Architecture:** Generalise the gateway's wire-coupled core into a wire-agnostic shape — a minimal `llm.Request` interface, a domain `ProviderError` contract so the handler maps any provider's errors without knowing concrete types, a generic per-route handler, and store account/token access scoped by provider (resolved once per provider). Then add an isolated `codex` provider that translates Chat Completions ⇄ Responses, spoofs the Codex client over OAuth, and runs a multi-account pool. Phase 1 is a pure refactor with zero behaviour change (Claude Max keeps passing its E2E suite); Phases 2–5 build the new provider.

**Tech Stack:** Go (stdlib `net/http`), PostgreSQL via `pgx/v5`, hexagonal layout (`internal/domain`, `internal/adapter`, `cmd`). Backend: `https://chatgpt.com/backend-api/codex/responses` (OpenAI Responses wire) over OAuth.

**Execution:** this plan is implemented in a **single run, end to end** — all 13 tasks, every phase. "Phases" are an internal ordering, NOT checkpoints to be relaunched; nothing here is optional or deferred and no task is skipped. The only things excluded are the spec's explicit non-goals (excluded by design, never built — not work to be skipped). The real-backend E2E smokes need the operator's test ChatGPT credentials to actually execute; without them the code is still fully written and the unit + stub-upstream suite passes (the real smokes stay credential-gated, exactly like the existing Claude Max E2E).

**Design spec:** `docs/specs/2026-06-19-openai-codex-provider-design.md`. Section references below (e.g. "spec §5.2") point at its mapping tables.

## Global Constraints

- **KISS / YAGNI:** simplest code that works; no premature abstraction; implement only what a task needs.
- **Minimal public API:** unexported by default; export only what another package strictly needs.
- **Docstrings:** every function, type, struct, interface, struct field has a Go-format docstring (starts with the symbol name). No doc comment above `package`.
- **Function size:** 15–25 lines, 30 max. One responsibility. Split into named helpers.
- **File size:** 200–300 lines, 400 max. Split by domain.
- **Error wrapping:** always `fmt.Errorf("operation description:\n%w", err)` — `\n` before `%w`.
- **Domain purity:** `internal/domain` imports no HTTP, SQL, or provider wire format. (`llm.ChatRequest` is a pre-existing exception; do NOT add the OpenAI wire to the domain — it lives in `codex`.)
- **Testing:** E2E-first against the real gateway. The Anthropic surface hits the real Anthropic API; the Codex surface hits the real Codex backend with seeded test credentials but is kept to a SMALL smoke set (subscription quota is tight); bulk behaviour is covered by domain unit tests + the local stub upstream. Assert on shape/plausibility, never exact text. Retry transient upstream (5xx/network/timeout); never retry the gateway's own 402/503.
- **go.mod hygiene:** never commit a `replace` directive.
- **Commit convention:** title line = a few words, NO prefix. Blank line. Then one change per line prefixed `[+]` add · `[-]` remove · `[&]` change · `[!]` fix. NO footers, NO "Generated with", NO "Co-Authored-By", NO emojis.
- **Branch:** all work on `feat/openai-codex-provider` (already created). Never commit to `main`.

---

## File Structure

**Phase 1 — foundation (refactor, no behaviour change):**
- Modify `internal/domain/llm/request.go` — add the `Request` interface.
- Create `internal/domain/errors.go` — the `ProviderError` interface.
- Modify `internal/domain/ports.go` — `Provider.Send` takes `llm.Request`; remove `DefaultRoute` from `Store`.
- Modify `internal/adapter/provider/claudemax/` — `Send` takes `llm.Request` + re-parses; error types implement `ProviderError`; provider id resolved once; `providerName` threaded through store calls.
- Modify `internal/adapter/postgres/accounts.go`, `store.go` — account/token queries take `providerName`; `providerIDByName`; drop `DefaultRoute`/`SetDefaultProvider`.
- Create `internal/adapter/httpserver/handler.go`, `wire.go` — generic per-route handler + light-parse wires.
- Modify `internal/adapter/httpserver/server.go`, `messages.go`, `streaming.go`, `errors.go` — route table; `writeProviderError` uses `ProviderError`; enriched error envelope.
- Modify `cmd/llmgw/main.go`, `test/e2e/harness.go` — route wiring; harness drops `SetDefaultProvider`.

**Phases 2–5 — the `codex` provider (new package `internal/adapter/provider/codex`):**
- `provider.go`, `oauth.go`, `spoof.go`, `errors.go` (errors implement `domain.ProviderError`).
- `translate_request.go`, `translate_response.go`, `translate_stream.go`.
- `pool.go` (transposed from claudemax).
- `instructions.go` (minimal + full Codex prompt, with the fallback).
- Migrations + `internal/config` additions + `postgres.CodexProviderName` constant.

---

## Phase 1 — Wire-agnostic foundation (no behaviour change)

### Task 1: `llm.Request` interface + retype the port

**Files:**
- Modify: `internal/domain/llm/request.go`
- Modify: `internal/domain/ports.go:124-129`
- Modify: `internal/adapter/provider/claudemax/provider.go` (`Send` signature, re-parse)
- Test: `internal/domain/llm/request_test.go`

**Interfaces:**
- Produces: `llm.Request` — `Model() string`, `Stream() bool`, `Bytes() []byte`.
- Produces: `domain.Provider.Send(ctx, req llm.Request, out StreamSink) (usage.Usage, error)`.

- [ ] **Step 1: Write the failing test** (`ChatRequest` is usable as an `llm.Request`)

```go
func TestChatRequestSatisfiesRequest(t *testing.T) {
	var req Request = ChatRequest{body: map[string]any{"model": "claude-x", "stream": true}}
	if req.Model() != "claude-x" || !req.Stream() || len(req.Bytes()) == 0 {
		t.Fatalf("ChatRequest does not satisfy Request: %q %v %d", req.Model(), req.Stream(), len(req.Bytes()))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/llm/ -run TestChatRequestSatisfiesRequest`
Expected: FAIL — `undefined: Request`.

- [ ] **Step 3: Add the interface** in `request.go` (above `ChatRequest`):

```go
// Request is the wire-agnostic view the gateway needs to meter and route a call, regardless
// of the provider's wire format. The HTTP wire resolves model/stream with a light parse and
// carries the raw body; the provider does the single full parse of Bytes() in its own wire.
type Request interface {
	// Model returns the requested model id, used for usage rows and routing.
	Model() string

	// Stream reports whether the consumer asked for a streamed (SSE) response.
	Stream() bool

	// Bytes returns the raw client request body, parsed by the provider's wire.
	Bytes() []byte
}
```

- [ ] **Step 4: Retype the port** in `ports.go`:

```go
type Provider interface {
	// Send forwards req upstream, writing the response to out and returning the Usage.
	Send(ctx context.Context, req llm.Request, out StreamSink) (usage.Usage, error)
}
```

- [ ] **Step 5: Adapt claudemax** — `Send` takes `llm.Request`, re-parses once:

```go
func (p *Provider) Send(ctx context.Context, req llm.Request, out domain.StreamSink) (usage.Usage, error) {
	chat, err := llm.ParseRequest(req.Bytes())
	if err != nil {
		return usage.Usage{}, fmt.Errorf("parse anthropic request:\n%w", err)
	}
	// existing body, calling p.sendVia(ctx, account, chat, out) with the concrete type
}
```

Keep `sendVia`/`buildRequest` on the concrete `llm.ChatRequest`.

- [ ] **Step 6: Run test + build**

Run: `go test ./internal/domain/llm/ -run TestChatRequestSatisfiesRequest && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/domain/llm/ internal/domain/ports.go internal/adapter/provider/claudemax/provider.go
git commit -m "Wire-agnostic provider request" -m $'[+] llm.Request interface (Model/Stream/Bytes)\n[&] Provider.Send takes llm.Request; claudemax re-parses internally'
```

### Task 2: `ProviderError` contract + error-mapping refactor

> **Why:** the audit found `writeProviderError` (`messages.go:234-267`) type-asserts five CONCRETE `claudemax` error types and `httpserver` imports `claudemax`. A second provider's errors would all fall through to `500` (no `503`/`502`/`Retry-After`). This task decouples the handler and removes the duplicated switch — the prerequisite for a generic handler.

**Files:**
- Create: `internal/domain/errors.go`
- Modify: `internal/adapter/provider/claudemax/errors.go` (the typed errors: `AllCoolingError`, `RateLimitError`, `DeadRefreshTokenError`, `UsageExhaustedError`, `UpstreamError`)
- Modify: `internal/adapter/httpserver/messages.go` (`writeProviderError`, `errorDetail`)
- Test: `internal/adapter/httpserver/cooldown_test.go` (already exercises the mapping) + a new domain test

**Interfaces:**
- Produces: `domain.ProviderError` — `error`, `HTTPStatus() int`, `ErrorType() string`, `RetryAfter() (time.Duration, bool)`.
- Produces: `writeProviderError` does one `errors.As(err, &provErr domain.ProviderError)`; `httpserver` no longer imports `claudemax`.
- Produces: `errorDetail` gains nullable `code`/`param` (OpenAI + Anthropic compatible).

- [ ] **Step 1: Write the failing test** (a fake `ProviderError` maps to its status + Retry-After)

In `internal/adapter/httpserver/handler_error_test.go`:

```go
type fakeProvErr struct{ status int; typ string; after time.Duration; has bool }

func (e fakeProvErr) Error() string                     { return e.typ }
func (e fakeProvErr) HTTPStatus() int                   { return e.status }
func (e fakeProvErr) ErrorType() string                 { return e.typ }
func (e fakeProvErr) RetryAfter() (time.Duration, bool) { return e.after, e.has }

func TestWriteProviderErrorUsesContract(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProviderError(rec, fakeProvErr{status: 503, typ: "all_cooling", after: 90 * time.Second, has: true})

	if rec.Code != 503 || rec.Header().Get("Retry-After") != "90" {
		t.Fatalf("status=%d retry=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/httpserver/ -run TestWriteProviderErrorUsesContract`
Expected: FAIL — `writeProviderError` still switches on concrete claudemax types.

- [ ] **Step 3: Add the domain interface** in `internal/domain/errors.go`:

```go
// ProviderError is a provider failure the gateway maps to HTTP without knowing the concrete
// provider. RetryAfter's bool is false when no Retry-After header applies.
type ProviderError interface {
	error
	HTTPStatus() int                   // HTTPStatus is the status to send the client.
	ErrorType() string                 // ErrorType is the stable machine-readable type.
	RetryAfter() (time.Duration, bool) // RetryAfter is the backoff when one is known.
}
```

- [ ] **Step 4: Implement it on claudemax's error types**

Add `HTTPStatus`/`ErrorType`/`RetryAfter` methods to each claudemax error so the existing mapping (cooling→503+RetryAfter, rate→503, dead→502, exhausted→503, upstream→echo via `upstreamStatus`) is preserved, now carried by the errors themselves.

- [ ] **Step 5: Rewrite `writeProviderError`** to the single contract + enrich the envelope:

```go
func writeProviderError(w http.ResponseWriter, err error) {
	var pe domain.ProviderError
	if errors.As(err, &pe) {
		if d, ok := pe.RetryAfter(); ok {
			w.Header().Set("Retry-After", retryAfterDuration(d))
		}
		writeError(w, pe.HTTPStatus(), pe.ErrorType(), pe.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}
```

Add nullable `Code`/`Param` (`json:"code"`/`json:"param"` with `omitempty` or `*string`) to `errorDetail`. Remove the `claudemax` import from `httpserver`.

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/adapter/httpserver/ ./internal/adapter/provider/claudemax/ && go build ./...`
Expected: PASS (the existing `cooldown_test.go` still passes through the new path).

- [ ] **Step 7: Commit**

```bash
git add internal/domain/errors.go internal/adapter/provider/claudemax/errors.go internal/adapter/httpserver/
git commit -m "Provider-error contract" -m $'[+] domain.ProviderError (HTTPStatus/ErrorType/RetryAfter)\n[&] claudemax error types implement ProviderError\n[&] writeProviderError maps via the contract; drop claudemax import\n[+] nullable code/param on the shared error envelope'
```

### Task 3: Scope store account/token access by provider, resolved once

> **Scope note:** the audit found ~7 product call sites + 13+ test sites + 2 interfaces + the E2E harness. Update test doubles (`fakeTokenStore`, `provider_pool_test` helpers, `harness.go`) FIRST so the suite stays green as production changes land. `defaultProviderID` is NOT cached — do not call it per request; resolve the id once at provider construction.

**Files:**
- Modify: `internal/adapter/postgres/accounts.go` (`LoadAccounts`, `SetCooldown`), `store.go` (`LoadToken`, `SaveToken`, `SeedSessionKey`; `defaultProviderID` → `providerIDByName`)
- Modify: `internal/adapter/provider/claudemax/oauth.go` (`tokenStore`), `pool.go` (`accountStore`), `provider.go` (`New` resolves+caches the provider id)
- Modify: `test/e2e/harness.go`; `internal/adapter/postgres/store_test.go`, `provider/claudemax/oauth_test.go`, `bootstrap_test.go`, `provider_pool_test.go`
- Test: `internal/adapter/postgres/store_test.go` (provider-scoped token round-trip)

**Interfaces:**
- Produces: `LoadAccounts(ctx, providerName)`, `SetCooldown(ctx, providerName, account, until)`, `LoadToken(ctx, providerName, account)`, `SaveToken(ctx, providerName, account, t)` — `tokenStore`/`accountStore` match.
- Produces: `claudemax.New(store, claudeCodeVersion)` resolves the provider id once (lazily on first use or eagerly), passing `postgres.DefaultProviderName` to every store call.

- [ ] **Step 1: Update test doubles first** — change `fakeTokenStore` / pool test helpers / `harness.go` method signatures to include `providerName`, keyed by `(providerName, account)`. Run the suite to confirm only the new signatures are missing on the production side.

- [ ] **Step 2: Write the failing test** (scoping)

```go
func TestTokenRoundTripScopedByProvider(t *testing.T) {
	store := newTestStore(t)
	want := domain.Token{RefreshToken: "rt", AccessToken: "at"}
	if err := store.SaveToken(context.Background(), "chatgpt-codex", "acct", want); err != nil {
		t.Fatal(err)
	}
	// the default provider must NOT see the codex token
	if _, err := store.LoadToken(context.Background(), postgres.DefaultProviderName, "acct"); err == nil {
		t.Fatal("token leaked across providers")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/adapter/postgres/ -run TestTokenRoundTripScopedByProvider`
Expected: FAIL — methods don't take a provider name yet.

- [ ] **Step 4: Thread `providerName` + add `providerIDByName`**

Rename `defaultProviderID` to `providerIDByName(ctx, name)`; have each account/token query call it with the passed name. In `claudemax`, store the provider name on `Provider` (default `postgres.DefaultProviderName`) and pass it to all store calls; resolve+cache the id once in `New` (or memoize on first call) so scoping adds no per-request round-trips.

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/adapter/postgres/ ./internal/adapter/provider/claudemax/ && go build ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/adapter/ test/e2e/harness.go
git commit -m "Provider-scoped account and token store" -m $'[&] account/token queries take a provider name\n[&] claudemax passes its provider name; provider id resolved once\n[&] test doubles and E2E harness updated for provider scope'
```

### Task 4: Generic per-route handler + light-parse wires

**Files:**
- Create: `internal/adapter/httpserver/handler.go`, `internal/adapter/httpserver/wire.go`
- Modify: `internal/adapter/httpserver/messages.go` (fold into handler), `streaming.go`
- Test: `internal/adapter/httpserver/handler_test.go`

**Interfaces:**
- Produces: `type wire interface { Parse(body []byte) (llm.Request, error); DefaultTag() string }`; `AnthropicWire` (`Parse` = `llm.ParseRequest`, `DefaultTag` = `"default"`). A `rawRequest{model, stream, body}` light impl of `llm.Request` for wires that don't need the full provider parse.
- Produces: `newHandler(store domain.Store, provider domain.Provider, w wire, providerName, defaultProject string) *handler` with `handle(w, r)`.
- Consumes: `llm.Request` (Task 1), provider-scoped store (Task 3), `writeProviderError` (Task 2).

- [ ] **Step 1: Write the failing test** (fake wire + fake provider; assert relay + usage recorded)

```go
func TestHandlerForwardsAndRecords(t *testing.T) {
	store := &fakeStore{projectID: 7}
	prov := &fakeProvider{body: []byte(`{"ok":true}`), usage: usage.Usage{InputTokens: 3, OutputTokens: 5}}
	h := newHandler(store, prov, AnthropicWire{}, "claude-max", "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("X-Project", "p")
	h.handle(rec, req)

	if rec.Code != http.StatusOK || store.recorded.OutputTokens != 5 {
		t.Fatalf("status=%d recorded=%+v", rec.Code, store.recorded)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/httpserver/ -run TestHandlerForwardsAndRecords`
Expected: FAIL — `newHandler`/`AnthropicWire` undefined.

- [ ] **Step 3: Implement** — rename `messagesHandler` to `handler` with injected `provider` + `w wire`; replace the `DefaultRoute` lookup with `h.provider`; `parseBody` calls `h.w.Parse(body)`; `tagOrDefault` falls back to `h.w.DefaultTag()`. Add `AnthropicWire` and the `rawRequest` light type in `wire.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/adapter/httpserver/ -run TestHandlerForwardsAndRecords`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/httpserver/
git commit -m "Generic per-route handler" -m $'[+] wire interface + AnthropicWire + rawRequest light parse\n[&] handler takes injected provider and wire; drop DefaultRoute lookup'
```

### Task 5: Route table + drop `DefaultRoute`

**Files:**
- Modify: `internal/adapter/httpserver/server.go` (`New` takes `[]Route`), `internal/domain/ports.go` (drop `DefaultRoute`), `internal/adapter/postgres/store.go` (drop `DefaultRoute`/`SetDefaultProvider`/`provider` field), `cmd/llmgw/main.go`, `test/e2e/harness.go`
- Test: full claudemax E2E suite

**Interfaces:**
- Produces: `type Route struct { Path string; Provider domain.Provider; Wire wire; ProviderName string }`; `New(store domain.Store, defaultProject string, routes []Route) *Server`.

- [ ] **Step 1: Replace `New` + register routes** (remove the `/chat/completions` Anthropic alias):

```go
func New(store domain.Store, defaultProject string, routes []Route) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	for _, rt := range routes {
		h := newHandler(store, rt.Provider, rt.Wire, rt.ProviderName, defaultProject)
		mux.HandleFunc("POST "+rt.Path, h.handle)
	}
	// server struct as before
}
```

- [ ] **Step 2: Delete `DefaultRoute`/`SetDefaultProvider`** from `ports.go` and `store.go` (and the `provider` field).

- [ ] **Step 3: Wire main.go + harness**

```go
claude := claudemax.New(store, cfg.ClaudeCodeVersion)
routes := []httpserver.Route{{Path: "/v1/messages", Provider: claude, Wire: httpserver.AnthropicWire{}, ProviderName: postgres.DefaultProviderName}}
server := httpserver.New(store, cfg.DefaultProject, routes)
```

Update `harness.go` `SeedClaudeMax` to build the route instead of calling `SetDefaultProvider`.

- [ ] **Step 4: Build + full E2E**

Run: `go build ./... && go test ./...` (test credentials present)
Expected: PASS — Claude Max behaviour unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/ test/
git commit -m "Route-based server wiring" -m $'[&] httpserver.New registers one handler per Route\n[-] remove DefaultRoute/SetDefaultProvider singleton\n[-] remove the /chat/completions Anthropic alias hack\n[&] main and E2E harness wire the Anthropic route explicitly'
```

---

## Phase 2 — `codex` skeleton: OAuth + spoofed Responses call

> **Wire sourcing (done INSIDE the run, first step of Phase 2 — not a manual prerequisite):** the Codex wire is undocumented but fully reverse-engineered in public reference implementations (`codex-proxy`, `ChatMock`, `opencode-openai-codex-auth`). Pull the concrete facts from them and pin them as constants + `testdata/`: the OAuth token endpoint + Codex `client_id`, the request header set (`originator`, `User-Agent`, `ChatGPT-Account-ID`, `x-client-request-id`, …), the request/response body shapes, a full streamed Responses event sequence (incl. the `response.completed` payload) as `testdata/responses_stream.sse` / `responses_completed.json`, and the Codex `instructions` prompt (minimal + full). These are interface facts (endpoints, header names, formats) — sourced the same way the project already spoofs Claude Code. A `mitmproxy` capture against the real Codex CLI is an optional cross-check when credentials are available, never a blocker.

### Task 6: Migrations + `CodexProviderName` constant

**Files:**
- Create: `internal/adapter/postgres/migrations/0006_chatgpt_codex_provider.sql`, `0007_oauth_chatgpt_account_id.sql`, `0008_seed_codex_model_prices.sql`
- Modify: `internal/adapter/postgres/store.go` (add `const CodexProviderName = "chatgpt-codex"` next to `DefaultProviderName`)

- [ ] **Step 1: Provider type + seed** (`0006`):

```sql
ALTER TABLE provider DROP CONSTRAINT provider_type_check;
ALTER TABLE provider ADD CONSTRAINT provider_type_check
    CHECK (type IN ('claude_max_oauth', 'anthropic_api', 'openrouter', 'chatgpt_codex_oauth'));
INSERT INTO provider (name, type) VALUES ('chatgpt-codex', 'chatgpt_codex_oauth')
ON CONFLICT (name) DO NOTHING;
```

- [ ] **Step 2: Account id column** (`0007`):

```sql
ALTER TABLE oauth_token ADD COLUMN chatgpt_account_id TEXT;
```

- [ ] **Step 3: Notional prices** (`0008`, numbers from the reference implementations / OpenAI public list prices):

```sql
INSERT INTO model_price (model, input_usd_per_mtok, output_usd_per_mtok) VALUES
    ('gpt-5', 1.25, 10.00), ('gpt-5-codex', 1.25, 10.00), ('gpt-5.5', 1.25, 10.00)
ON CONFLICT (model) DO NOTHING;
```

- [ ] **Step 4: Add the constant + apply migrations** against a scratch DB; confirm `provider` shows `chatgpt-codex`. Use `postgres.CodexProviderName` everywhere instead of the literal.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/postgres/
git commit -m "Codex provider migrations" -m $'[+] chatgpt_codex_oauth provider type + seed row\n[+] oauth_token.chatgpt_account_id column\n[+] notional Codex model prices\n[+] CodexProviderName constant'
```

### Task 7: Config + credential seeding

**Files:**
- Modify: `internal/config` (`CodexVersion`, `CodexAccounts []CodexAccount{Label, RefreshToken, AccountID}`)
- Modify: `internal/adapter/postgres/store.go` (`SeedCodexAccount` — idempotent, writes `refresh_token` + `chatgpt_account_id` under `CodexProviderName`)
- Modify: `cmd/llmgw/main.go` (`seedCodexAccounts`)
- Test: `internal/config` parse test

**Interfaces:**
- Produces: `config.Config.CodexAccounts`, `config.Config.CodexVersion`; `store.SeedCodexAccount(ctx, label, refreshToken, accountID)`.

- [ ] **Step 1: Failing config test**

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

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/config/ -run TestLoadParsesCodexAccounts` → FAIL.
- [ ] **Step 3: Implement** parsing (follow the `SessionKeys` shape), `SeedCodexAccount` (`INSERT ... ON CONFLICT (provider_id, account_label) DO NOTHING`), and `seedCodexAccounts` at boot.
- [ ] **Step 4: Run test + build** — `go test ./internal/config/ -run TestLoadParsesCodexAccounts && go build ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/config/ internal/adapter/postgres/store.go cmd/llmgw/main.go
git commit -m "Codex account config and seeding" -m $'[+] CodexAccounts/CodexVersion config\n[+] SeedCodexAccount idempotent insert with chatgpt_account_id\n[+] seed Codex accounts at boot'
```

### Task 8: OAuth + spoof + `instructions` (with fallback) + minimal call

**Files:**
- Create: `internal/adapter/provider/codex/oauth.go`, `spoof.go`, `instructions.go`, `provider.go`, `errors.go`
- Test: `internal/adapter/provider/codex/spoof_test.go`; E2E smoke

**Interfaces:**
- Produces: `codex.New(store accountStore, version string) *Provider` satisfying `domain.Provider`; `spoof.decorate(req, accessToken, accountID)`; `instructions()` returning the minimal value, with `fullInstructions()` as the fallback.
- Produces: codex error types implementing `domain.ProviderError` (Task 2).

- [ ] **Step 1: Failing spoof test** (required Codex headers present + account id propagated — values from capture)

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

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/adapter/provider/codex/ -run TestSpoofDecorateSetsCodexHeaders` → FAIL (package undefined).
- [ ] **Step 3: Implement** oauth.go (token manager mirroring claudemax: single-flight refresh; OpenAI token endpoint + client_id from capture; no session-key bootstrap), spoof.go (captured headers), instructions.go (minimal + `fullInstructions` fallback per the capture finding), a single-account provider.go forwarding a hardcoded Responses body (`store:false`, `instructions`) to prove the path, errors.go (typed errors implementing `domain.ProviderError`).
- [ ] **Step 4: Run unit test + E2E smoke** asserting a 200 with non-empty content from the real subscription.
- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/
git commit -m "Codex OAuth and spoof skeleton" -m $'[+] codex OAuth token manager (single-flight refresh, seeded token)\n[+] Codex client header spoof\n[+] minimal instructions with full-prompt fallback\n[+] minimal Responses call proving the subscription path\n[+] typed errors implementing domain.ProviderError'
```

---

## Phase 3 — Translation

### Task 9: Request translation (Chat Completions → Responses)

**Files:** Create `internal/adapter/provider/codex/translate_request.go`, `request.go` (`openaiRequest` satisfying `llm.Request`); Test `translate_request_test.go`.

**Interfaces:** Produces `translateRequest(body []byte, instructions string) ([]byte, error)` per spec §5.2; `openaiRequest` for the wire.

- [ ] **Step 1: Failing test** (system→developer input, tools, max_tokens→max_output_tokens, store:false, instructions):

```go
func TestTranslateRequestMapsCoreFields(t *testing.T) {
	in := []byte(`{"model":"gpt-5","max_tokens":256,"messages":[
		{"role":"system","content":"be terse"},{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`)
	out, err := translateRequest(in, "CODEX_MIN")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["store"] != false || got["max_output_tokens"].(float64) != 256 || got["instructions"] != "CODEX_MIN" {
		t.Fatalf("core fields wrong: %v", got)
	}
	assertHasDeveloperInput(t, got)
	assertHasFunctionTool(t, got)
}
```

- [ ] **Step 2: Run to verify it fails** — FAIL (`translateRequest` undefined).
- [ ] **Step 3: Implement** per spec §5.2 (messages→input items incl. system/developer, tool results→function_call_output, assistant tool_calls→function_call; tools→Responses function tools; tool_choice; max_tokens→max_output_tokens; force store:false, stream:true; set instructions; validate/map model). Small named helpers (function-size limit).
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/translate_request.go internal/adapter/provider/codex/request.go internal/adapter/provider/codex/translate_request_test.go
git commit -m "Chat Completions to Responses request translation" -m $'[+] openaiRequest wire type (Model/Stream/Bytes)\n[+] translateRequest per spec §5.2'
```

### Task 10: Non-streaming response translation

> **Note:** the backend forces `stream:true`, so even a non-streaming client response is built by reading the upstream SSE to completion and taking the `response` object from the **`response.completed`** event — that object is the input to `translateResponse`.

**Files:** Create `internal/adapter/provider/codex/translate_response.go`; Modify `provider.go`; Test `translate_response_test.go`.

**Interfaces:** Produces `translateResponse(completed []byte) (chatCompletionsJSON []byte, u usage.Usage, err error)` and `aggregateCompleted(upstream io.Reader) (completed []byte, err error)` extracting the `response.completed` payload from the SSE.

- [ ] **Step 1: Failing test** (captured `response.completed` → valid chat.completion; reasoning dropped; usage mapped):

```go
func TestTranslateResponseFoldsAndMapsUsage(t *testing.T) {
	body := readTestdata(t, "responses_completed.json")
	out, u, err := translateResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	var cc map[string]any
	_ = json.Unmarshal(out, &cc)
	if cc["object"] != "chat.completion" || u.InputTokens == 0 || u.OutputTokens == 0 {
		t.Fatalf("bad translation: %v usage=%+v", cc["object"], u)
	}
	assertNoReasoningLeaked(t, out)
}
```

- [ ] **Step 2: Run to verify it fails** — FAIL (`translateResponse` undefined; add `testdata/responses_completed.json` from the capture).
- [ ] **Step 3: Implement** per spec §5.3 (non-streaming): fold `output[]` messages into `choices[0].message.content`, `function_call` into `tool_calls`, drop `reasoning`, map `finish_reason`, map `usage.input_tokens/output_tokens` → `prompt_tokens/completion_tokens`. Wire the non-streaming `Send` path: `aggregateCompleted` → `translateResponse` → write to the buffered sink.
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/translate_response.go internal/adapter/provider/codex/translate_response_test.go internal/adapter/provider/codex/testdata/ internal/adapter/provider/codex/provider.go
git commit -m "Responses to Chat Completions response translation" -m $'[+] aggregateCompleted extracts the response.completed object from the SSE\n[+] translateResponse folds output[] into one choice, drops reasoning\n[+] usage mapping; non-streaming Send emits a clean chat.completion'
```

### Task 11: Streaming translation

**Files:** Create `internal/adapter/provider/codex/translate_stream.go`; Modify `provider.go`; Test `translate_stream_test.go`.

**Interfaces:** Produces `relayTranslatedStream(upstream io.Reader, out domain.StreamSink, includeUsage bool) (usage.Usage, error)` per spec §5.3.

- [ ] **Step 1: Failing test** (captured Responses SSE → clean chat.completion.chunk stream; no prompt/reasoning leak; usage accumulated):

```go
func TestRelayTranslatedStreamProducesCleanChunks(t *testing.T) {
	upstream := readTestdataReader(t, "responses_stream.sse")
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
		t.Fatal("Codex prompt or reasoning leaked")
	}
	if u.OutputTokens == 0 {
		t.Fatal("usage not accumulated")
	}
}
```

- [ ] **Step 2: Run to verify it fails** — FAIL (add `testdata/responses_stream.sse`).
- [ ] **Step 3: Implement** per spec §5.3 (reuse the `bufio`+`sseData` shape from `claudemax/stream.go`): `response.output_text.delta`→`delta.content`; function-call deltas→`delta.tool_calls`; `response.completed`→final `finish_reason` chunk + usage chunk when `includeUsage`; DROP `response.created`/`response.in_progress`/`reasoning`; emit `data: [DONE]`; flush per event; accumulate usage. Wire the streaming `Send` path.
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/translate_stream.go internal/adapter/provider/codex/translate_stream_test.go internal/adapter/provider/codex/testdata/ internal/adapter/provider/codex/provider.go
git commit -m "Responses to Chat Completions streaming translation" -m $'[+] relayTranslatedStream mapping Responses events to chat.completion.chunk\n[+] drop Codex-prompt and reasoning events (clean output)\n[+] accumulate usage; terminate with [DONE]'
```

---

## Phase 4 — Wire the route + metering

### Task 12: Register `/v1/chat/completions` with OpenAI wire + budget

**Files:** Modify `internal/adapter/httpserver/wire.go` (add `OpenAIWire` — light-parse `model`/`stream`, `DefaultTag` = `"agentic"`); Modify `cmd/llmgw/main.go`; Test gateway E2E (smoke).

**Interfaces:** Consumes `codex.New` (Task 8), the generic handler + routes (Tasks 4–5), the shared error envelope (Task 2). Produces a live `/v1/chat/completions` route metering into `usage_event` with `provider=CodexProviderName`, default tag `agentic`.

- [ ] **Step 1: Add `OpenAIWire`** — `Parse` returns a `rawRequest` (light `model`/`stream` parse, raw body); `DefaultTag()` returns `"agentic"`. No per-route error envelope (the shared one from Task 2 already serves OpenAI SDKs).

- [ ] **Step 2: Wire the route**

```go
codexProv := codex.New(store, cfg.CodexVersion)
routes = append(routes, httpserver.Route{
	Path: "/v1/chat/completions", Provider: codexProv, Wire: httpserver.OpenAIWire{}, ProviderName: postgres.CodexProviderName,
})
```

- [ ] **Step 3: E2E smoke (kept small — quota)** — one non-streaming chat (valid `chat.completion`, usage recorded, `provider=chatgpt-codex`, tag `agentic`); one streaming chat (clean chunks, `[DONE]`, no prompt/reasoning leak); one forced function call. Budget/cooldown assertions go through the stub upstream (Task 13), not the real backend.
- [ ] **Step 4: Run E2E** — `go test ./...` (credentials present) → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/adapter/httpserver/wire.go cmd/llmgw/main.go internal/...e2e...
git commit -m "Live OpenAI Codex route with metering" -m $'[+] OpenAIWire (light parse + agentic default tag)\n[+] /v1/chat/completions route -> codex provider\n[+] E2E smoke: non-streaming, streaming, tools'
```

---

## Phase 5 — Multi-account pool

### Task 13: Transpose the account pool + error classifier

**Files:** Create `internal/adapter/provider/codex/pool.go`; Modify `provider.go`, `errors.go` (`AllCoolingError`, `cooldownFor`, `classifyUpstream`; all implementing `domain.ProviderError`); Test `pool_test.go` + failure-injection E2E (stub upstream).

**Interfaces:** Consumes provider-scoped `LoadAccounts`/`SetCooldown` (Task 3). Produces round-robin `selectOrder`; `AllCoolingError` → 503 + Retry-After via the `ProviderError` contract.

- [ ] **Step 1: Failing pool test** (mirror `claudemax/pool_test.go`):

```go
func TestSelectOrderSkipsCoolingAccounts(t *testing.T) {
	p := &Provider{}
	now := time.Now()
	accounts := []domain.Account{{Label: "a", CooldownUntil: now.Add(time.Minute)}, {Label: "b"}}
	order := p.selectOrder(accounts, now)
	if len(order) != 1 || order[0] != "b" {
		t.Fatalf("selectOrder = %v, want [b]", order)
	}
}
```

- [ ] **Step 2: Run to verify it fails** — FAIL (`selectOrder` undefined).
- [ ] **Step 3: Transpose** `selectOrder`/`cooling`/`cool`/`allCooling`/`soonestCooldown`/`retryAfterUntil` + round-robin cursor from claudemax; adapt `cooldownFor`/`classifyUpstream` to Codex statuses (429 reset-aware, 401 dead-token, 403 Cloudflare/originator short-cool failover, 5xx failover, other 4xx surfaced). Make `Send` loop over `selectOrder` like claudemax. Confirm every codex error implements `domain.ProviderError`.
- [ ] **Step 4: Run test + failure-injection E2E** (stub upstream forcing 429→cooldown, all-cooling→503+Retry-After, refresh failure→dead-token) → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/adapter/provider/codex/
git commit -m "Codex multi-account pool and cooldown" -m $'[+] round-robin selectOrder with reset-aware cooldown\n[+] cooldownFor/classifyUpstream for 429/401/403/5xx as ProviderError\n[+] AllCoolingError -> 503 + Retry-After\n[+] failure-injection E2E via stub upstream'
```

---

## Self-Review

**Spec coverage:**
- §3 surface (chat/completions, stream/non-stream, tools, clean output, X-Project/X-Tags, agentic default, alias removal) → Tasks 5, 9–12.
- §4 architecture: `llm.Request` → Task 1; `ProviderError` contract + error refactor → Task 2; provider-scoped store resolved once → Task 3; generic handler + light wire → Task 4; provider injection + drop DefaultRoute → Task 5; isolated package → Tasks 8–13.
- §5.1 backend call (endpoint, auth, spoof headers, store:false, minimal instructions + full fallback) → Tasks 8, 9.
- §5.2 request translation → Task 9.
- §5.3 response (incl. `response.completed` aggregation) + streaming translation with filtering → Tasks 10, 11.
- §5.4 pool → Task 13. §5.5 errors/cooldown (via ProviderError) → Tasks 2, 8, 13.
- §6 budget reuse (notional cost, provider label) → Tasks 5, 12. §7 storage → Tasks 6, 7. §8 config → Task 7.
- §10 testing (small real smoke + stub/unit bulk) → Tasks 11–13.
- §11 build order → Phases 1–5 map to the slices (Phase 1 now 5 tasks: +ProviderError, +store scoping).

**Review-fix coverage (the 8 audit findings):**
1. Error coupling (🔴) → Task 2 (`ProviderError`, single `errors.As`, drop claudemax import).
2. `providerID` not cached (🟠) → Task 3 (resolved once in `New`).
3. Scoping refactor under-sized (🟠) → Task 3 (test doubles first, ~20 sites, harness).
4. Error envelope per-route (🟠) → Tasks 2, 12 (one shared enriched envelope, no per-route renderer).
5. Non-streaming SSE aggregation (🟡) → Task 10 (`aggregateCompleted` from `response.completed`).
6. Minimal `instructions` optimism (🟡) → Tasks 8 + Phase-2 capture (full-prompt fallback).
7. E2E quota burn (🟡) → Constraints + Tasks 12–13 (small real smoke; stub/unit for the rest).
8. Double full-parse / `Body` vs `Bytes` (🟢) → Task 1 (`Bytes`) + Task 4 (light-parse wire; single full parse in the provider).

**Wire sourcing:** Tasks 8–11 consume the wire facts pulled in Phase 2's first step from the public reference implementations (headers, OAuth endpoint + `client_id`, Responses event shapes, the Codex `instructions` prompt). Part of the single run — no manual capture prerequisite, no placeholders left in merged code.

**Verified against the real backend:** the small E2E smoke set is the final confirmation the sourced wire is correct; it runs when the operator's test credentials are present (the unit + stub suite is green regardless).

**Type consistency:** `llm.Request`/`Bytes()` (Task 1) used by wires (Task 4) and `openaiRequest` (Task 9); `domain.ProviderError` (Task 2) implemented by claudemax (Task 2) and codex (Tasks 8, 13); `translateRequest`/`translateResponse`/`aggregateCompleted`/`relayTranslatedStream` consistent across Tasks 9–12; `CodexProviderName` used in Tasks 6, 7, 12.
