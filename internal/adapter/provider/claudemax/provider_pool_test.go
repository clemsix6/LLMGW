package claudemax

// These tests drive the multi-account pool against a STUB Anthropic upstream (forced 429s the real
// API will not produce on demand) backed by a real testcontainers Postgres seeded with 2 accounts.
// They prove: a 429 cools the offending account (honoring the reset header, else a 60s default —
// never clewdr's 1h) and the request succeeds on the next account; an account stays skipped while
// cooling; and when every account is cooling the provider returns *AllCoolingError. Both the
// non-streaming and streaming paths flow through the same selection + cooldown loop.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// stub200JSON is a minimal non-streaming Messages 200 the stub returns for a non-rate-limited
// account; its usage proves the response was metered.
const stub200JSON = `{"id":"msg_stub","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`

// stubUsageExhaustedJSON is Anthropic's "out of extra usage" 400 — an account-specific budget
// exhaustion the provider must cool + fail over (not surface).
const stubUsageExhaustedJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"You're out of extra usage. Add more at claude.ai/settings/usage and keep going."}}`

// stubBadRequestJSON is a request-level 400 (not usage) the provider must surface unchanged,
// without cooling or failing over (it would fail on every account).
const stubBadRequestJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"messages: at least one message is required"}}`

// TestProviderPool runs the pool's cooldown/rotation scenarios against one Postgres container,
// clearing the seeded accounts between subtests so each starts from a clean pool.
func TestProviderPool(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	store, dsn := newPoolStore(t, ctx)

	t.Run("ResetHeaderCoolsAccountAndNextServes", func(t *testing.T) {
		clearAccounts(t, ctx, dsn)
		seedAccount(t, ctx, store, "acct-a", "tok-a")
		seedAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newStubUpstream(t)
		resetEpoch := time.Now().Add(5 * time.Minute).Unix()
		stub.rateLimit("tok-a", strconv.FormatInt(resetEpoch, 10))

		provider := poolProvider(store, stub.url())
		metered, err := provider.Send(ctx, poolRequest(t, false), &recordingSink{})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}

		if metered.InputTokens != 5 || metered.OutputTokens != 3 {
			t.Fatalf("usage = %+v, want {5, 3} (acct-b's 200)", metered)
		}
		if got := stub.seenBearers(); len(got) != 2 || got[0] != "tok-a" || got[1] != "tok-b" {
			t.Fatalf("upstream bearers = %v, want [tok-a tok-b] (a 429'd, rotated to b)", got)
		}

		accounts := loadAccounts(t, ctx, store)
		if cd := cooldownOf(accounts, "acct-a"); cd.Unix() != resetEpoch {
			t.Fatalf("acct-a cooldown unix = %d, want %d (from reset header)", cd.Unix(), resetEpoch)
		}
		if cd := cooldownOf(accounts, "acct-b"); !cd.IsZero() {
			t.Fatalf("acct-b cooldown = %v, want zero (it served the request)", cd)
		}
	})

	t.Run("DefaultCooldownIs60sNot1hWhenNoResetHeader", func(t *testing.T) {
		clearAccounts(t, ctx, dsn)
		seedAccount(t, ctx, store, "acct-a", "tok-a")
		seedAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newStubUpstream(t)
		stub.rateLimit("tok-a", "") // 429 with no reset hint

		provider := poolProvider(store, stub.url())
		before := time.Now()
		if _, err := provider.Send(ctx, poolRequest(t, false), &recordingSink{}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		after := time.Now()

		cd := cooldownOf(loadAccounts(t, ctx, store), "acct-a")
		lo, hi := before.Add(defaultCooldown-5*time.Second), after.Add(defaultCooldown+5*time.Second)
		if cd.Before(lo) || cd.After(hi) {
			t.Fatalf("acct-a cooldown = %v, want ~%s from now (default)", cd, defaultCooldown)
		}
		if cd.After(after.Add(30 * time.Minute)) {
			t.Fatalf("acct-a cooldown = %v is ~1h out — the clewdr 1h cooldown leaked in", cd)
		}
	})

	t.Run("AllCoolingReturnsAllCoolingErrorAndSkipsUpstream", func(t *testing.T) {
		clearAccounts(t, ctx, dsn)
		seedAccount(t, ctx, store, "acct-a", "tok-a")
		seedAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newStubUpstream(t)
		soonEpoch := time.Now().Add(2 * time.Minute).Unix()
		stub.rateLimit("tok-a", strconv.FormatInt(soonEpoch, 10))
		stub.rateLimit("tok-b", strconv.FormatInt(time.Now().Add(9*time.Minute).Unix(), 10))

		provider := poolProvider(store, stub.url())

		_, err := provider.Send(ctx, poolRequest(t, false), &recordingSink{})
		var allCooling *AllCoolingError
		if !errors.As(err, &allCooling) {
			t.Fatalf("Send error = %v, want *AllCoolingError", err)
		}
		if allCooling.After <= 0 || allCooling.After > 3*time.Minute {
			t.Fatalf("After = %v, want ~2m (soonest cooldown)", allCooling.After)
		}

		// A second Send must not touch the upstream: both accounts are cooling and get skipped.
		hitsAfterFirst := len(stub.seenBearers())
		if _, err := provider.Send(ctx, poolRequest(t, false), &recordingSink{}); !errors.As(err, &allCooling) {
			t.Fatalf("second Send error = %v, want *AllCoolingError", err)
		}
		if extra := len(stub.seenBearers()) - hitsAfterFirst; extra != 0 {
			t.Fatalf("second Send hit upstream %d times, want 0 (both cooling)", extra)
		}
	})

	t.Run("StreamingPathRotatesAndCools", func(t *testing.T) {
		clearAccounts(t, ctx, dsn)
		seedAccount(t, ctx, store, "acct-a", "tok-a")
		seedAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newStubUpstream(t)
		stub.rateLimit("tok-a", "") // a 429s; b streams the SSE

		provider := poolProvider(store, stub.url())
		sink := &recordingSink{}
		metered, err := provider.Send(ctx, poolRequest(t, true), sink)
		if err != nil {
			t.Fatalf("Send (stream): %v", err)
		}

		if metered.OutputTokens != 21 { // latest message_delta in the canned SSE
			t.Fatalf("stream usage output = %d, want 21", metered.OutputTokens)
		}
		if !strings.Contains(sink.buf.String(), "message_stop") {
			t.Fatalf("relayed stream missing terminal event: %q", sink.buf.String())
		}
		if got := stub.seenBearers(); len(got) != 2 || got[1] != "tok-b" {
			t.Fatalf("upstream bearers = %v, want acct-b to serve the stream after a 429", got)
		}
		if cooldownOf(loadAccounts(t, ctx, store), "acct-a").IsZero() {
			t.Fatal("acct-a should be cooling after its 429 on the streaming path")
		}
	})

	t.Run("UsageExhaustedCoolsAccountAndNextServes", func(t *testing.T) {
		clearAccounts(t, ctx, dsn)
		seedAccount(t, ctx, store, "acct-a", "tok-a")
		seedAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newStubUpstream(t)
		stub.usageExhausted("tok-a") // a is out of extra usage; b still serves

		provider := poolProvider(store, stub.url())
		metered, err := provider.Send(ctx, poolRequest(t, false), &recordingSink{})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if metered.InputTokens != 5 || metered.OutputTokens != 3 {
			t.Fatalf("usage = %+v, want {5, 3} (acct-b's 200)", metered)
		}
		if got := stub.seenBearers(); len(got) != 2 || got[0] != "tok-a" || got[1] != "tok-b" {
			t.Fatalf("bearers = %v, want [tok-a tok-b] (a exhausted, failed over to b)", got)
		}

		accounts := loadAccounts(t, ctx, store)
		if cooldownOf(accounts, "acct-a").IsZero() {
			t.Fatal("acct-a should be cooling after 'out of extra usage'")
		}
		if cd := cooldownOf(accounts, "acct-b"); !cd.IsZero() {
			t.Fatalf("acct-b cooldown = %v, want zero (it served)", cd)
		}
	})

	t.Run("AllExhaustedReturnsAllCoolingError", func(t *testing.T) {
		clearAccounts(t, ctx, dsn)
		seedAccount(t, ctx, store, "acct-a", "tok-a")
		seedAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newStubUpstream(t)
		stub.usageExhausted("tok-a")
		stub.usageExhausted("tok-b")

		provider := poolProvider(store, stub.url())
		_, err := provider.Send(ctx, poolRequest(t, false), &recordingSink{})
		var allCooling *AllCoolingError
		if !errors.As(err, &allCooling) {
			t.Fatalf("Send error = %v, want *AllCoolingError (both exhausted)", err)
		}
		if cooldownOf(loadAccounts(t, ctx, store), "acct-a").IsZero() {
			t.Fatal("acct-a should be cooling after exhaustion")
		}
	})

	t.Run("RequestLevel400IsSurfacedNotFailedOver", func(t *testing.T) {
		clearAccounts(t, ctx, dsn)
		seedAccount(t, ctx, store, "acct-a", "tok-a")
		seedAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newStubUpstream(t)
		stub.failRequest("tok-a") // a malformed-request 400 — not account-specific

		provider := poolProvider(store, stub.url())
		_, err := provider.Send(ctx, poolRequest(t, false), &recordingSink{})

		var upstream *UpstreamError
		if !errors.As(err, &upstream) || upstream.Status != http.StatusBadRequest {
			t.Fatalf("Send error = %v, want *UpstreamError status 400 (surfaced, not failed over)", err)
		}
		if got := stub.seenBearers(); len(got) != 1 || got[0] != "tok-a" {
			t.Fatalf("bearers = %v, want [tok-a] (no failover on a request-level 400)", got)
		}
		if cd := cooldownOf(loadAccounts(t, ctx, store), "acct-a"); !cd.IsZero() {
			t.Fatalf("acct-a cooldown = %v, want zero (request-level error must not cool)", cd)
		}
	})

	t.Run("RoundRobinDistributesAcrossAccounts", func(t *testing.T) {
		clearAccounts(t, ctx, dsn)
		seedAccount(t, ctx, store, "acct-a", "tok-a")
		seedAccount(t, ctx, store, "acct-b", "tok-b")
		seedAccount(t, ctx, store, "acct-c", "tok-c")

		stub := newStubUpstream(t) // all healthy → each Send served by its rotation start
		provider := poolProvider(store, stub.url())

		for i := 0; i < 6; i++ {
			if _, err := provider.Send(ctx, poolRequest(t, false), &recordingSink{}); err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
		}

		want := []string{"tok-a", "tok-b", "tok-c", "tok-a", "tok-b", "tok-c"}
		if got := stub.seenBearers(); !equalStrings(got, want) {
			t.Fatalf("bearers = %v, want %v (strict round-robin, one account per request)", got, want)
		}
	})

	t.Run("StreamingUsageExhaustedFailsOver", func(t *testing.T) {
		clearAccounts(t, ctx, dsn)
		seedAccount(t, ctx, store, "acct-a", "tok-a")
		seedAccount(t, ctx, store, "acct-b", "tok-b")

		stub := newStubUpstream(t)
		stub.usageExhausted("tok-a") // a exhausted on the streaming path; b streams

		provider := poolProvider(store, stub.url())
		sink := &recordingSink{}
		metered, err := provider.Send(ctx, poolRequest(t, true), sink)
		if err != nil {
			t.Fatalf("Send (stream): %v", err)
		}
		if metered.OutputTokens != 21 {
			t.Fatalf("stream usage output = %d, want 21 (acct-b's canned stream)", metered.OutputTokens)
		}
		if !strings.Contains(sink.buf.String(), "message_stop") {
			t.Fatalf("relayed stream missing terminal event: %q", sink.buf.String())
		}
		if got := stub.seenBearers(); len(got) != 2 || got[1] != "tok-b" {
			t.Fatalf("bearers = %v, want acct-b to serve the stream after a's exhaustion", got)
		}
		if cooldownOf(loadAccounts(t, ctx, store), "acct-a").IsZero() {
			t.Fatal("acct-a should be cooling after exhaustion on the streaming path")
		}
	})
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
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

// stubUpstream is a fake Anthropic Messages endpoint for forced-failure tests: it 429s the bearers
// registered as rate-limited (optionally with a reset header) and otherwise returns a minimal 200
// (JSON, or the canned SSE for a streaming request). It records every request's bearer.
type stubUpstream struct {
	server *httptest.Server // server is the running stub endpoint.

	mu sync.Mutex // mu guards the maps and the recorded bearers.

	rateLimited map[string]string // rateLimited maps a bearer → reset header value (""=none); presence ⇒ 429.

	exhausted map[string]bool // exhausted marks bearers that get a 400 "out of extra usage".

	badRequest map[string]bool // badRequest marks bearers that get a 400 request-level error (not usage).

	bearers []string // bearers records the access token of each handled request, in order.
}

// newStubUpstream starts a stub Anthropic endpoint and registers its shutdown.
func newStubUpstream(t *testing.T) *stubUpstream {
	t.Helper()

	stub := &stubUpstream{rateLimited: map[string]string{}, exhausted: map[string]bool{}, badRequest: map[string]bool{}}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(stub.server.Close)
	return stub
}

// url returns the stub's base URL.
func (s *stubUpstream) url() string { return s.server.URL }

// rateLimit marks a bearer as rate-limited, with an optional unified-reset epoch header value.
func (s *stubUpstream) rateLimit(bearer, resetHeader string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited[bearer] = resetHeader
}

// usageExhausted marks a bearer to receive a 400 "out of extra usage" (its budget is spent).
func (s *stubUpstream) usageExhausted(bearer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exhausted[bearer] = true
}

// failRequest marks a bearer to receive a 400 request-level error (not usage exhaustion).
func (s *stubUpstream) failRequest(bearer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badRequest[bearer] = true
}

// seenBearers returns a copy of the bearers handled so far, in order.
func (s *stubUpstream) seenBearers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bearers...)
}

// handle records the request's bearer and replies with a forced 429 or a minimal 200.
func (s *stubUpstream) handle(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	s.bearers = append(s.bearers, bearer)
	reset, limited := s.rateLimited[bearer]
	exhausted := s.exhausted[bearer]
	bad := s.badRequest[bearer]
	s.mu.Unlock()

	if limited {
		if reset != "" {
			w.Header().Set("anthropic-ratelimit-unified-reset", reset)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
		return
	}

	if exhausted {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(stubUsageExhaustedJSON))
		return
	}

	if bad {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(stubBadRequestJSON))
		return
	}

	if streamRequested(body) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(cannedStream))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(stub200JSON))
}

// streamRequested reports whether the forwarded body asked for a streamed response.
func streamRequested(body []byte) bool {
	var parsed struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &parsed)
	return parsed.Stream
}

// poolProvider builds a pool provider over store, pointed at the stub base URL.
func poolProvider(store accountStore, baseURL string) *Provider {
	p := New(store, "2.1.181")
	p.baseURL = baseURL
	return p
}

// poolRequest builds a tiny Messages request, streaming or not.
func poolRequest(t *testing.T, stream bool) llm.ChatRequest {
	t.Helper()

	body := `{"model":"claude-sonnet-4-6","max_tokens":16,"stream":` + strconv.FormatBool(stream) +
		`,"messages":[{"role":"user","content":"hi"}]}`
	req, err := llm.ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return req
}

// seedAccount persists a fresh (non-expired) access token for an account so the token manager
// serves it directly without an OAuth refresh.
func seedAccount(t *testing.T, ctx context.Context, store *postgres.Store, label, accessToken string) {
	t.Helper()

	token := domain.Token{AccessToken: accessToken, RefreshToken: "r-" + label, ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.SaveToken(ctx, label, token); err != nil {
		t.Fatalf("seed account %q: %v", label, err)
	}
}

// loadAccounts reads the pool's accounts (with cooldown state) for assertions.
func loadAccounts(t *testing.T, ctx context.Context, store *postgres.Store) []domain.Account {
	t.Helper()

	accounts, err := store.LoadAccounts(ctx)
	if err != nil {
		t.Fatalf("load accounts: %v", err)
	}
	return accounts
}

// cooldownOf returns the cooldown time of the named account, or the zero time when absent.
func cooldownOf(accounts []domain.Account, label string) time.Time {
	for _, account := range accounts {
		if account.Label == label {
			return account.CooldownUntil
		}
	}
	return time.Time{}
}

// clearAccounts removes every seeded account so the next subtest starts from an empty pool.
func clearAccounts(t *testing.T, ctx context.Context, dsn string) {
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

// newPoolStore starts an ephemeral Postgres, applies migrations, and returns the store and DSN.
func newPoolStore(t *testing.T, ctx context.Context) (*postgres.Store, string) {
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
