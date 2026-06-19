# Task 13 Report: Codex Multi-Account Pool and Cooldown

## Status
COMPLETE — all tests pass, go vet clean.

## What Was Implemented

### `internal/adapter/provider/codex/pool.go` (new)
- `defaultCooldown = 60s`, `deadTokenCooldown = 15min` constants.
- `accountStore` interface updated with `SetCooldown` (moved from provider.go, extended).
- `selectOrder(accounts, now)` — round-robin cursor via `next atomic.Uint64`, skips cooling accounts.
- `cooling(account, now)` — pure predicate.
- `cool(ctx, account, until)` — persists cooldown via store, logs but does not fail on persistence errors.
- `allCooling(ctx, now)` — re-reads pool, returns `AllCoolingError` with `After = retryAfterUntil(soonestCooldown(accounts), now)`.
- `soonestCooldown(accounts)` — earliest non-zero cooldown across pool.
- `retryAfterUntil(until, now)` — clamps to ≥ 1s, falls back to `defaultCooldown` for zero until.

### `internal/adapter/provider/codex/errors.go` (modified)
Added:
- `AllCoolingError` implementing `domain.ProviderError` (HTTPStatus 503, ErrorType `"all_cooling"`, RetryAfter = `e.After`).
- `cooldownFor(err, now)` — matches `*RateLimitError` (reset-aware), `*DeadRefreshTokenError` (`deadTokenCooldown`), `*UpstreamError` with `shouldFailoverStatus` (`defaultCooldown`); all others return `(zero, false)`.
- `shouldFailoverStatus(status)` — true for 401, 403, ≥ 500.
- `classifyUpstream(status, header, body)` — 429 → `RateLimitError` with `parseResetAt`; others → `UpstreamError`.
- `parseResetAt(header, now)` — moved from provider.go; reads `Retry-After` as delta-seconds or HTTP date.

### `internal/adapter/provider/codex/provider.go` (modified)
- Removed `accountStore` interface (moved to pool.go), `pickAccount` function, `classifyUpstream`, `parseResetAt`.
- Added `next atomic.Uint64` field to `Provider`.
- Rewrote `Send` to loop over `selectOrder(accounts, now)`, calling `sendVia` per account; on retryable error cools + continues; when no account served returns `allCooling(ctx, now)`.
- Added `sendVia(ctx, account, req, out)` — extracts per-account logic (token refresh → request translation → HTTP → handleResp); non-2xx is classified before any byte reaches `out`, preserving the failover contract.

### `internal/adapter/provider/codex/pool_test.go` (new)
12 unit tests covering: `selectOrder` (skips cooling, expired cooldowns rejoin, round-robin rotation, all-cooling), `cooling` predicate, `soonestCooldown`, `retryAfterUntil`, and `cooldownFor` (rate limit with/without reset, dead token, upstream failover statuses, upstream non-failover statuses).

### `internal/adapter/provider/codex/provider_pool_test.go` (new)
Failure-injection integration tests via `testcontainers` Postgres + `httptest.Server` stub Responses endpoint. 8 subtests:
1. `RateLimitCoolsAccountAndNextServes` — 429 with Retry-After: account-a cooled to reset time, account-b serves.
2. `DefaultCooldownWhenNoRetryAfterHeader` — 429 without header: account-a cooled ~60s.
3. `AllCoolingReturnsAllCoolingError` — both accounts 429'd: `AllCoolingError.After ≈ soonest cooldown (~2m)`.
4. `DeadTokenCoolsAndNextServes` — account-a has no refresh token: `DeadRefreshTokenError` → cooled, account-b serves; stub not hit for account-a.
5. `AllDeadTokensReturnAllCoolingError` — both accounts dead: `AllCoolingError` returned, stub not hit.
6. `RequestLevel400IsSurfacedNotFailedOver` — stub 400 → `UpstreamError` surfaced, no cooldown set.
7. `RoundRobinDistributesAcrossAccounts` — 6 sends on 3 accounts: strict a→b→c→a→b→c rotation.
8. `StreamingPathRotatesAndCools` — streaming request: account-a 429'd, account-b streams; output contains `[DONE]`.

## TDD RED/GREEN
- Step 1: Added `pool_test.go` with `TestSelectOrderSkipsCoolingAccounts` → FAIL (`selectOrder` undefined).
- Step 2: Implemented `pool.go` → all 12 unit tests GREEN.
- Step 3: Implemented `provider_pool_test.go` (containerized) → all 8 subtests GREEN.

## Failover/Cooldown Rules Implemented
| Upstream event | Action |
|---|---|
| 429 (with Retry-After) | Cool until reset time, fail over |
| 429 (no header) | Cool 60s, fail over |
| 401 / 403 | Cool 60s, fail over (account-specific) |
| 5xx | Cool 60s, fail over |
| Other 4xx | Surface to caller unchanged, no cooldown |
| DeadRefreshTokenError | Cool 15min, fail over |
| All accounts cooling | Return AllCoolingError (503 + Retry-After = soonest) |

## Stub-Upstream E2E
The `provider_pool_test.go` tests exercise all failure modes WITHOUT real Codex credentials:
- `newCodexStub(t)` starts an `httptest.Server` that records every bearer token, returning 429/400/200+SSE per configuration.
- Dead-token tests need no HTTP stub (the `tokenManager` returns `DeadRefreshTokenError` before making any HTTP call when `RefreshToken == ""`).
- Tests PASS when Docker is available (no creds required); the real-backend E2E in `test/e2e/` still skips without `LLMGW_CODEX_TEST_*` env vars.

## Files Changed
- `internal/adapter/provider/codex/pool.go` — created (118 lines)
- `internal/adapter/provider/codex/errors.go` — rewritten (186 lines)
- `internal/adapter/provider/codex/provider.go` — rewritten (190 lines)
- `internal/adapter/provider/codex/pool_test.go` — created (145 lines)
- `internal/adapter/provider/codex/provider_pool_test.go` — created (305 lines)

## Self-Review
- All functions are 15–30 lines; no file exceeds 310 lines.
- Every exported and unexported symbol has a docstring.
- Error wrapping uses `fmt.Errorf("desc:\n%w", err)` throughout.
- `go vet ./...` is clean.
- The `accountStore` interface now strictly requires `SetCooldown` — `postgres.Store` already implements it (confirmed by the integration tests passing).
- `AllCoolingError` is in `errors.go` per the task brief; pool functions are in `pool.go`.
- No `replace` directives in `go.mod`.

## Concerns
None material. One minor note: the dead-token `allCooling.After` is ~15min (from `deadTokenCooldown`) when all accounts are dead — which is accurate but possibly long. This is the same trade-off as claudemax and is acceptable per spec.

## Final-review fix wave

### Fixes applied

**Fix 1 — gofmt**
Formatted 5 files with divergent whitespace/alignment:
- `internal/adapter/provider/codex/provider.go`
- `internal/adapter/provider/codex/provider_pool_test.go`
- `internal/adapter/provider/codex/translate_request.go`
- `internal/adapter/provider/codex/translate_response.go`
- `internal/adapter/provider/codex/translate_stream.go`
- `test/e2e/codex_gateway_real_test.go`

**Fix 2 — InvalidModelError (400 instead of 500 for invalid model)**
Added `InvalidModelError` type in `internal/adapter/provider/codex/errors.go`:
- Implements `domain.ProviderError`: `HTTPStatus()` → 400, `ErrorType()` → `"invalid_request"`, `RetryAfter()` → `(0, false)`.
- Compile-time assertion added to the `var _ domain.ProviderError = (...)` block.
- `validateModel` in `translate_request.go` now returns `&InvalidModelError{Model: m}` instead of a bare `fmt.Errorf`.
- `TestTranslateRequestInvalidModel` extended to assert `*InvalidModelError` with `HTTPStatus == 400`.
- `fakeTokenStore.SaveToken` in `oauth_test.go` updated to preserve `ChatGPTAccountID` (mirrors the real Postgres UPSERT which does not write that column), fixing `TestValidRefreshesExpiredTokenAndKeepsAccountID`.

**Fix 3 — SeedCodexAccount error wrap**
In `internal/adapter/postgres/store.go`, `SeedCodexAccount`'s `providerIDByName` error is now wrapped:
`return fmt.Errorf("resolve codex provider id:\n%w", err)`.

**Fix 4 — Silent drops now log**
Two diagnostic `log.Printf` calls added in `internal/adapter/provider/codex/translate_request.go`:
- `translateToolResult`: logs when non-string tool-result content is dropped.
- `parseTextContent`: logs when a non-`"text"` content part (e.g. image) is dropped.

**Fix 5 — parseCodexTriplet robustness**
`parseCodexTriplet` in `internal/config/config.go` now uses `strings.Split(triplet, ":")` and checks `len == 3`, returning an error on any other count. A 4-field entry (extra colon) now errors cleanly. New test `TestParseCodexTripletRejectsFourFields` added.

**Fix 6 — No-op assignment removed in oauth.go**
Removed `refreshed.ChatGPTAccountID = current.ChatGPTAccountID` from `doRefresh`; added explanatory comment noting the account id is seed-owned and preserved by the DB's targeted UPDATE.

**Fix 7 — Minor carry-overs**
- `cmd/llmgw/main.go`: `serve` docstring now mentions "registers the supplied routes".
- `test/e2e/harness.go`: both `SeedClaudeMax` and `SeedCodex` now wrap `h.server.Shutdown` errors instead of discarding them.

### Final verification outputs

```
gofmt -l: (empty — clean)
go vet ./...: (empty — clean)
go build ./...: (empty — clean)
go test ./...:
  ok  github.com/clemsix6/LLMGW/internal/adapter/httpserver
  ok  github.com/clemsix6/LLMGW/internal/adapter/postgres
  ok  github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax
  ok  github.com/clemsix6/LLMGW/internal/adapter/provider/codex
  ok  github.com/clemsix6/LLMGW/internal/config
  ok  github.com/clemsix6/LLMGW/internal/domain/budget
  ok  github.com/clemsix6/LLMGW/internal/domain/llm
  ok  github.com/clemsix6/LLMGW/internal/domain/usage
  ok  github.com/clemsix6/LLMGW/test/e2e
```
