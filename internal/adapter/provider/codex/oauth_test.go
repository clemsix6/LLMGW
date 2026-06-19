package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/domain"
)

// tokenKey is the composite key used by fakeTokenStore to scope tokens by provider name.
type tokenKey struct {
	provider string // provider is the provider name.
	account  string // account is the account label.
}

// fakeTokenStore is an in-memory, concurrency-safe tokenStore for OAuth manager tests.
type fakeTokenStore struct {
	mu     sync.Mutex
	tokens map[tokenKey]domain.Token // tokens is keyed by (provider, account) for isolation.
}

// newFakeTokenStore returns an empty in-memory token store.
func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{tokens: map[tokenKey]domain.Token{}}
}

// LoadToken returns the stored token, or domain.ErrTokenNotFound when absent.
func (f *fakeTokenStore) LoadToken(_ context.Context, providerName, account string) (domain.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	token, ok := f.tokens[tokenKey{providerName, account}]
	if !ok {
		return domain.Token{}, domain.ErrTokenNotFound
	}
	return token, nil
}

// SaveToken stores the token for the given provider name and account label.
func (f *fakeTokenStore) SaveToken(_ context.Context, providerName, account string, t domain.Token) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.tokens[tokenKey{providerName, account}] = t
	return nil
}

// newStubOAuth starts a stub OAuth endpoint returning the given status/body and counting calls.
func newStubOAuth(t *testing.T, status int, body string, calls *int) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// successBody marshals a successful refresh response with the given tokens and lifetime.
func successBody(access, refresh string, expiresIn int) string {
	b, _ := json.Marshal(tokenResponse{AccessToken: access, RefreshToken: refresh, ExpiresIn: expiresIn})
	return string(b)
}

// managerFor builds a token manager pointed at the stub base URL.
func managerFor(store tokenStore, baseURL string) *tokenManager {
	m := newTokenManager(store, postgres.CodexProviderName)
	m.baseURL = baseURL
	return m
}

// seed stores a token under the Codex provider for the test account.
func seed(store *fakeTokenStore, token domain.Token) {
	store.tokens[tokenKey{postgres.CodexProviderName, "acct"}] = token
}

func TestValidUsesFreshTokenAndReturnsAccountID(t *testing.T) {
	store := newFakeTokenStore()
	seed(store, domain.Token{AccessToken: "live", RefreshToken: "r1", ChatGPTAccountID: "acct_x", ExpiresAt: time.Now().Add(time.Hour)})
	calls := 0
	stub := newStubOAuth(t, http.StatusOK, successBody("unused", "r2", 3600), &calls)
	m := managerFor(store, stub.URL)

	access, accountID, err := m.Valid(context.Background(), "acct")
	if err != nil {
		t.Fatalf("Valid: %v", err)
	}
	if access != "live" {
		t.Fatalf("access = %q, want %q", access, "live")
	}
	if accountID != "acct_x" {
		t.Fatalf("accountID = %q, want %q", accountID, "acct_x")
	}
	if calls != 0 {
		t.Fatalf("refresh calls = %d, want 0 (token fresh)", calls)
	}
}

func TestValidRefreshesExpiredTokenAndKeepsAccountID(t *testing.T) {
	store := newFakeTokenStore()
	seed(store, domain.Token{AccessToken: "old", RefreshToken: "r1", ChatGPTAccountID: "acct_x", ExpiresAt: time.Now().Add(-time.Hour)})
	calls := 0
	stub := newStubOAuth(t, http.StatusOK, successBody("new-access", "rotated", 3600), &calls)
	m := managerFor(store, stub.URL)

	access, accountID, err := m.Valid(context.Background(), "acct")
	if err != nil {
		t.Fatalf("Valid: %v", err)
	}
	if access != "new-access" {
		t.Fatalf("access = %q, want %q", access, "new-access")
	}
	if accountID != "acct_x" {
		t.Fatalf("accountID = %q, want %q (seeded id preserved)", accountID, "acct_x")
	}
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}

	saved, _ := store.LoadToken(context.Background(), postgres.CodexProviderName, "acct")
	if saved.RefreshToken != "rotated" {
		t.Fatalf("persisted refresh = %q, want %q", saved.RefreshToken, "rotated")
	}
	if saved.ChatGPTAccountID != "acct_x" {
		t.Fatalf("persisted account id = %q, want %q", saved.ChatGPTAccountID, "acct_x")
	}
}

func TestValidInvalidGrantReturnsDeadError(t *testing.T) {
	store := newFakeTokenStore()
	seed(store, domain.Token{AccessToken: "old", RefreshToken: "r1", ChatGPTAccountID: "acct_x", ExpiresAt: time.Now().Add(-time.Hour)})
	calls := 0
	stub := newStubOAuth(t, http.StatusBadRequest, `{"error":"invalid_grant"}`, &calls)
	m := managerFor(store, stub.URL)

	_, _, err := m.Valid(context.Background(), "acct")

	var dead *DeadRefreshTokenError
	if !errors.As(err, &dead) {
		t.Fatalf("error = %v, want *DeadRefreshTokenError", err)
	}
	if dead.Account != "acct" {
		t.Fatalf("dead account = %q, want %q", dead.Account, "acct")
	}
}

func TestValidUnseededRefreshTokenIsDead(t *testing.T) {
	store := newFakeTokenStore()
	seed(store, domain.Token{ChatGPTAccountID: "acct_x"}) // no refresh token, no access token
	calls := 0
	stub := newStubOAuth(t, http.StatusOK, successBody("x", "y", 3600), &calls)
	m := managerFor(store, stub.URL)

	_, _, err := m.Valid(context.Background(), "acct")

	var dead *DeadRefreshTokenError
	if !errors.As(err, &dead) {
		t.Fatalf("error = %v, want *DeadRefreshTokenError", err)
	}
	if calls != 0 {
		t.Fatalf("refresh calls = %d, want 0 (nothing to exchange)", calls)
	}
}
