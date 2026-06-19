package codex

// These tests drive the multi-account pool against a STUB Codex Responses upstream (forced 429s,
// dead tokens, and request-level errors the real API will not produce on demand) backed by a real
// testcontainers Postgres seeded with 2-3 accounts. They prove: a 429 cools the offending account
// (honoring the Retry-After header, else a 60s default) and the request succeeds on the next
// account; an account stays skipped while cooling; when every account is cooling the provider
// returns *AllCoolingError; a dead refresh token cools the account and the next account serves;
// and a request-level 4xx is surfaced without cooling or failing over.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
)

// cannedResponsesStream is a minimal Responses API SSE stream the stub returns for healthy
// accounts. It contains a response.completed event with input_tokens=5, output_tokens=3 and a
// single text output item so both the non-streaming (aggregation) and streaming paths can use it.
const cannedResponsesStream = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_stub","object":"response","created_at":1700000000,"status":"in_progress","model":"gpt-5"}}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_001","role":"assistant"}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","item_id":"msg_001","output_index":0,"content_index":0,"delta":"ok"}` + "\n\n" +
	"event: response.output_text.done\n" +
	`data: {"type":"response.output_text.done","item_id":"msg_001","output_index":0,"content_index":0,"text":"ok"}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_001","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_stub","object":"response","created_at":1700000000,"status":"completed","model":"gpt-5","output":[{"type":"message","id":"msg_001","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestCodexProviderPool runs the pool's cooldown/rotation scenarios against one Postgres container,
// clearing the seeded accounts between subtests so each starts from a clean pool.
func TestCodexProviderPool(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	store, dsn := newCodexPoolStore(t, ctx)

	t.Run("RateLimitCoolsAccountAndNextServes", func(t *testing.T) {
		clearCodexAccounts(t, ctx, dsn)
		seedCodexAccount(t, ctx, store, "acct-a", "tok-a")
		seedCodexAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newCodexStub(t)
		retryAfterSecs := 120
		stub.rateLimit("tok-a", strconv.Itoa(retryAfterSecs))

		provider := codexPoolProvider(store, stub.url())
		metered, err := provider.Send(ctx, codexPoolRequest(t, false), &captureSink{})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}

		if metered.InputTokens != 5 || metered.OutputTokens != 3 {
			t.Fatalf("usage = %+v, want {5, 3} (acct-b's 200)", metered)
		}
		if got := stub.seenBearers(); len(got) != 2 || got[0] != "tok-a" || got[1] != "tok-b" {
			t.Fatalf("upstream bearers = %v, want [tok-a tok-b] (a 429'd, rotated to b)", got)
		}

		accounts := loadCodexAccounts(t, ctx, store)
		cdA := codexCooldownOf(accounts, "acct-a")
		lo := time.Now().Add(time.Duration(retryAfterSecs-5) * time.Second)
		hi := time.Now().Add(time.Duration(retryAfterSecs+5) * time.Second)
		if cdA.Before(lo) || cdA.After(hi) {
			t.Fatalf("acct-a cooldown = %v, want ~%ds from now (Retry-After header)", cdA, retryAfterSecs)
		}
		if cd := codexCooldownOf(accounts, "acct-b"); !cd.IsZero() {
			t.Fatalf("acct-b cooldown = %v, want zero (it served)", cd)
		}
	})

	t.Run("DefaultCooldownWhenNoRetryAfterHeader", func(t *testing.T) {
		clearCodexAccounts(t, ctx, dsn)
		seedCodexAccount(t, ctx, store, "acct-a", "tok-a")
		seedCodexAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newCodexStub(t)
		stub.rateLimit("tok-a", "") // 429 with no reset hint

		provider := codexPoolProvider(store, stub.url())
		before := time.Now()
		if _, err := provider.Send(ctx, codexPoolRequest(t, false), &captureSink{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		after := time.Now()

		cd := codexCooldownOf(loadCodexAccounts(t, ctx, store), "acct-a")
		lo, hi := before.Add(defaultCooldown-5*time.Second), after.Add(defaultCooldown+5*time.Second)
		if cd.Before(lo) || cd.After(hi) {
			t.Fatalf("acct-a cooldown = %v, want ~%s from now (default)", cd, defaultCooldown)
		}
		if cd.After(after.Add(30 * time.Minute)) {
			t.Fatalf("acct-a cooldown = %v is unexpectedly long (> 30m)", cd)
		}
	})

	t.Run("AllCoolingReturnsAllCoolingError", func(t *testing.T) {
		clearCodexAccounts(t, ctx, dsn)
		seedCodexAccount(t, ctx, store, "acct-a", "tok-a")
		seedCodexAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newCodexStub(t)
		stub.rateLimit("tok-a", strconv.Itoa(int(2*time.Minute/time.Second)))
		stub.rateLimit("tok-b", strconv.Itoa(int(9*time.Minute/time.Second)))

		provider := codexPoolProvider(store, stub.url())
		_, err := provider.Send(ctx, codexPoolRequest(t, false), &captureSink{})

		var allCooling *AllCoolingError
		if !errors.As(err, &allCooling) {
			t.Fatalf("Send error = %v, want *AllCoolingError", err)
		}
		if allCooling.After <= 0 || allCooling.After > 3*time.Minute {
			t.Fatalf("After = %v, want ~2m (soonest cooldown)", allCooling.After)
		}

		// A second Send must not touch the upstream — both accounts are cooling.
		hitsAfterFirst := len(stub.seenBearers())
		if _, err := provider.Send(ctx, codexPoolRequest(t, false), &captureSink{}); !errors.As(err, &allCooling) {
			t.Fatalf("second Send error = %v, want *AllCoolingError", err)
		}
		if extra := len(stub.seenBearers()) - hitsAfterFirst; extra != 0 {
			t.Fatalf("second Send hit upstream %d times, want 0 (both cooling)", extra)
		}
	})

	t.Run("DeadTokenCoolsAndNextServes", func(t *testing.T) {
		clearCodexAccounts(t, ctx, dsn)
		seedDeadAccount(t, ctx, store, "acct-a") // expired + no refresh token → DeadRefreshTokenError
		seedCodexAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newCodexStub(t) // only tok-b is expected to hit the stub
		provider := codexPoolProvider(store, stub.url())

		metered, err := provider.Send(ctx, codexPoolRequest(t, false), &captureSink{})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if metered.InputTokens != 5 || metered.OutputTokens != 3 {
			t.Fatalf("usage = %+v, want {5, 3} (acct-b's 200)", metered)
		}

		accounts := loadCodexAccounts(t, ctx, store)
		if cdA := codexCooldownOf(accounts, "acct-a"); cdA.IsZero() {
			t.Fatal("acct-a should be cooling after dead refresh token")
		}
		if cd := codexCooldownOf(accounts, "acct-b"); !cd.IsZero() {
			t.Fatalf("acct-b cooldown = %v, want zero (it served)", cd)
		}
		if got := stub.seenBearers(); len(got) != 1 || got[0] != "tok-b" {
			t.Fatalf("stub bearers = %v, want [tok-b] only (acct-a failed before HTTP)", got)
		}
	})

	t.Run("AllDeadTokensReturnAllCoolingError", func(t *testing.T) {
		clearCodexAccounts(t, ctx, dsn)
		seedDeadAccount(t, ctx, store, "acct-a")
		seedDeadAccount(t, ctx, store, "acct-b")

		stub := newCodexStub(t) // no HTTP calls expected
		provider := codexPoolProvider(store, stub.url())

		_, err := provider.Send(ctx, codexPoolRequest(t, false), &captureSink{})
		var allCooling *AllCoolingError
		if !errors.As(err, &allCooling) {
			t.Fatalf("Send error = %v, want *AllCoolingError (both dead)", err)
		}
		if allCooling.After <= 0 {
			t.Fatalf("After = %v, want positive duration", allCooling.After)
		}
		if got := stub.seenBearers(); len(got) != 0 {
			t.Fatalf("stub bearers = %v, want empty (no HTTP calls for dead tokens)", got)
		}
	})

	t.Run("RequestLevel400IsSurfacedNotFailedOver", func(t *testing.T) {
		clearCodexAccounts(t, ctx, dsn)
		seedCodexAccount(t, ctx, store, "acct-a", "tok-a")
		seedCodexAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newCodexStub(t)
		stub.badRequest("tok-a") // 400 not from rate limiting — a request-level error

		provider := codexPoolProvider(store, stub.url())
		_, err := provider.Send(ctx, codexPoolRequest(t, false), &captureSink{})

		var upstream *UpstreamError
		if !errors.As(err, &upstream) || upstream.Status != http.StatusBadRequest {
			t.Fatalf("Send error = %v, want *UpstreamError status 400 (surfaced, not failed over)", err)
		}
		if got := stub.seenBearers(); len(got) != 1 || got[0] != "tok-a" {
			t.Fatalf("bearers = %v, want [tok-a] (no failover on a request-level 400)", got)
		}
		if cd := codexCooldownOf(loadCodexAccounts(t, ctx, store), "acct-a"); !cd.IsZero() {
			t.Fatalf("acct-a cooldown = %v, want zero (request-level error must not cool)", cd)
		}
	})

	t.Run("RoundRobinDistributesAcrossAccounts", func(t *testing.T) {
		clearCodexAccounts(t, ctx, dsn)
		seedCodexAccount(t, ctx, store, "acct-a", "tok-a")
		seedCodexAccount(t, ctx, store, "acct-b", "tok-b")
		seedCodexAccount(t, ctx, store, "acct-c", "tok-c")

		stub := newCodexStub(t) // all healthy
		provider := codexPoolProvider(store, stub.url())

		for i := 0; i < 6; i++ {
			if _, err := provider.Send(ctx, codexPoolRequest(t, false), &captureSink{}); err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
		}

		want := []string{"tok-a", "tok-b", "tok-c", "tok-a", "tok-b", "tok-c"}
		if got := stub.seenBearers(); !equalStringSlices(got, want) {
			t.Fatalf("bearers = %v, want %v (strict round-robin)", got, want)
		}
	})

	t.Run("StreamingPathRotatesAndCools", func(t *testing.T) {
		clearCodexAccounts(t, ctx, dsn)
		seedCodexAccount(t, ctx, store, "acct-a", "tok-a")
		seedCodexAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newCodexStub(t)
		stub.rateLimit("tok-a", "") // a 429s; b streams

		provider := codexPoolProvider(store, stub.url())
		sink := &captureSink{}
		metered, err := provider.Send(ctx, codexPoolRequest(t, true), sink)
		if err != nil {
			t.Fatalf("Send (stream): %v", err)
		}

		if metered.OutputTokens != 3 {
			t.Fatalf("stream usage output = %d, want 3 (acct-b's canned stream)", metered.OutputTokens)
		}
		if !strings.Contains(sink.buf.String(), "[DONE]") {
			t.Fatalf("relayed stream missing [DONE]: %q", sink.buf.String())
		}
		if got := stub.seenBearers(); len(got) != 2 || got[1] != "tok-b" {
			t.Fatalf("bearers = %v, want acct-b to serve after a's 429", got)
		}
		if codexCooldownOf(loadCodexAccounts(t, ctx, store), "acct-a").IsZero() {
			t.Fatal("acct-a should be cooling after its 429 on the streaming path")
		}
	})
}

// equalStringSlices reports whether two string slices are element-wise equal.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// codexStub is a fake Codex Responses endpoint for forced-failure tests: it 429s the bearers
// registered as rate-limited (optionally with a Retry-After header) and otherwise returns a
// minimal 200 with the canned Responses SSE stream. It records every request's bearer.
type codexStub struct {
	server *httptest.Server // server is the running stub endpoint.

	mu sync.Mutex // mu guards the maps and the recorded bearers.

	rateLimited map[string]string // rateLimited maps bearer → Retry-After value (""=none); presence ⇒ 429.

	badRequests map[string]bool // badRequests marks bearers that receive a 400 response.

	bearers []string // bearers records the access token of each handled request, in order.
}

// newCodexStub starts a stub Codex Responses endpoint and registers its cleanup.
func newCodexStub(t *testing.T) *codexStub {
	t.Helper()

	stub := &codexStub{rateLimited: map[string]string{}, badRequests: map[string]bool{}}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(stub.server.Close)
	return stub
}

// url returns the stub's base URL.
func (s *codexStub) url() string { return s.server.URL }

// rateLimit marks a bearer as rate-limited, with an optional Retry-After delta in seconds.
func (s *codexStub) rateLimit(bearer, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited[bearer] = retryAfter
}

// badRequest marks a bearer to receive a 400 response (request-level, not a rate limit).
func (s *codexStub) badRequest(bearer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badRequests[bearer] = true
}

// seenBearers returns a copy of the bearers handled so far, in order.
func (s *codexStub) seenBearers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bearers...)
}

// handle records the request's bearer and replies with a forced 429, 400, or a canned 200 SSE.
func (s *codexStub) handle(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	s.mu.Lock()
	s.bearers = append(s.bearers, bearer)
	retryAfter, limited := s.rateLimited[bearer]
	bad := s.badRequests[bearer]
	s.mu.Unlock()

	if limited {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate_limit_exceeded"}`))
		return
	}

	if bad {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request","message":"bad request"}`))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(cannedResponsesStream))
}

// codexPoolProvider builds a pool provider over store, pointed at the stub base URL.
func codexPoolProvider(store accountStore, baseURL string) *Provider {
	p := New(store, "0.40.0")
	p.baseURL = baseURL
	return p
}

// codexPoolRequest builds a tiny Chat Completions request, streaming or not.
func codexPoolRequest(t *testing.T, stream bool) llm.Request {
	t.Helper()

	body := `{"model":"gpt-5","max_tokens":16,"stream":` + strconv.FormatBool(stream) +
		`,"messages":[{"role":"user","content":"hi"}]}`
	req, err := llm.ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return req
}

// seedCodexAccount persists a fresh (non-expired) access token for an account so the token
// manager serves it directly without an OAuth refresh.
func seedCodexAccount(t *testing.T, ctx context.Context, store *postgres.Store, label, accessToken string) {
	t.Helper()

	token := domain.Token{
		AccessToken:  accessToken,
		RefreshToken: "r-" + label,
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := store.SaveToken(ctx, postgres.CodexProviderName, label, token); err != nil {
		t.Fatalf("seed account %q: %v", label, err)
	}
}

// seedDeadAccount persists a token with no refresh credential and an expired access token,
// so the token manager returns DeadRefreshTokenError when Send selects that account.
func seedDeadAccount(t *testing.T, ctx context.Context, store *postgres.Store, label string) {
	t.Helper()

	token := domain.Token{
		AccessToken:  "",
		RefreshToken: "", // empty → doRefresh returns DeadRefreshTokenError immediately
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	if err := store.SaveToken(ctx, postgres.CodexProviderName, label, token); err != nil {
		t.Fatalf("seed dead account %q: %v", label, err)
	}
}

// loadCodexAccounts reads the pool's accounts (with cooldown state) for assertions.
func loadCodexAccounts(t *testing.T, ctx context.Context, store *postgres.Store) []domain.Account {
	t.Helper()

	accounts, err := store.LoadAccounts(ctx, postgres.CodexProviderName)
	if err != nil {
		t.Fatalf("load codex accounts: %v", err)
	}
	return accounts
}

// codexCooldownOf returns the cooldown time of the named account, or the zero time when absent.
func codexCooldownOf(accounts []domain.Account, label string) time.Time {
	for _, account := range accounts {
		if account.Label == label {
			return account.CooldownUntil
		}
	}
	return time.Time{}
}

// clearCodexAccounts removes every seeded account so the next subtest starts from an empty pool.
func clearCodexAccounts(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to db: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "DELETE FROM oauth_token"); err != nil {
		t.Fatalf("clear accounts: %v", err)
	}
}

// newCodexPoolStore starts an ephemeral Postgres, applies migrations, and returns the store and DSN.
func newCodexPoolStore(t *testing.T, ctx context.Context) (*postgres.Store, string) {
	t.Helper()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("llmgw"),
		tcpostgres.WithUsername("llmgw"),
		tcpostgres.WithPassword("llmgw"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}

	store, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(store.Close)
	return store, dsn
}
