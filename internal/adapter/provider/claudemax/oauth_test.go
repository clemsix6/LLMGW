package claudemax

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

// LoadToken returns the stored token for the given provider name and account label, or
// domain.ErrTokenNotFound when absent.
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

// stubOAuth is a fake OAuth token endpoint that counts the refresh requests it serves.
type stubOAuth struct {
	server *httptest.Server // server is the running stub endpoint.

	calls atomic.Int64 // calls counts handled refresh requests.
}

// newStubOAuth starts a stub endpoint returning the given status/body after an optional delay.
func newStubOAuth(t *testing.T, status int, body string, delay time.Duration) *stubOAuth {
	t.Helper()

	stub := &stubOAuth{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stub.calls.Add(1)
		time.Sleep(delay)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

// successBody marshals a successful OAuth token-exchange response body.
func successBody(access, refresh string, expiresIn int) string {
	b, _ := json.Marshal(tokenResponse{AccessToken: access, RefreshToken: refresh, ExpiresIn: expiresIn})
	return string(b)
}

// managerFor builds a token manager pointed at the stub base URL.
func managerFor(store tokenStore, baseURL string) *tokenManager {
	m := newTokenManager(store, "2.1.214", postgres.DefaultProviderName)
	m.baseURL = baseURL
	return m
}

// expiredToken returns a token whose access token already expired.
func expiredToken(refresh string) domain.Token {
	return domain.Token{AccessToken: "old", RefreshToken: refresh, ExpiresAt: time.Now().Add(-time.Hour)}
}

func TestOAuthRefreshesExpiredToken(t *testing.T) {
	store := newFakeTokenStore()
	store.tokens[tokenKey{postgres.DefaultProviderName, "acct"}] = expiredToken("r1")
	stub := newStubOAuth(t, http.StatusOK, successBody("new-access", "r2", 28800), 0)
	m := managerFor(store, stub.server.URL)

	got, err := m.Valid(context.Background(), "acct")
	if err != nil {
		t.Fatalf("Valid: %v", err)
	}
	if got != "new-access" {
		t.Fatalf("access token = %q, want %q", got, "new-access")
	}
	if n := stub.calls.Load(); n != 1 {
		t.Fatalf("refresh calls = %d, want 1", n)
	}
}

func TestOAuthValidUsesCachedTokenWhenFresh(t *testing.T) {
	store := newFakeTokenStore()
	store.tokens[tokenKey{postgres.DefaultProviderName, "acct"}] = domain.Token{AccessToken: "live", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)}
	stub := newStubOAuth(t, http.StatusOK, successBody("unused", "r2", 28800), 0)
	m := managerFor(store, stub.server.URL)

	got, err := m.Valid(context.Background(), "acct")
	if err != nil {
		t.Fatalf("Valid: %v", err)
	}
	if got != "live" {
		t.Fatalf("access token = %q, want %q", got, "live")
	}
	if n := stub.calls.Load(); n != 0 {
		t.Fatalf("refresh calls = %d, want 0 (token still fresh)", n)
	}
}

func TestOAuthSingleFlightRefresh(t *testing.T) {
	store := newFakeTokenStore()
	store.tokens[tokenKey{postgres.DefaultProviderName, "acct"}] = expiredToken("r1")
	stub := newStubOAuth(t, http.StatusOK, successBody("new-access", "r2", 28800), 100*time.Millisecond)
	m := managerFor(store, stub.server.URL)

	const callers = 5
	start := make(chan struct{})
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	errs := make([]error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = m.Valid(context.Background(), "acct")
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("Valid[%d]: %v", i, errs[i])
		}
		if tokens[i] != "new-access" {
			t.Fatalf("Valid[%d] = %q, want %q", i, tokens[i], "new-access")
		}
	}
	if n := stub.calls.Load(); n != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1 (single-flight)", n)
	}
}

func TestOAuthPersistsRotatedToken(t *testing.T) {
	store := newFakeTokenStore()
	store.tokens[tokenKey{postgres.DefaultProviderName, "acct"}] = expiredToken("r1")
	stub := newStubOAuth(t, http.StatusOK, successBody("new-access", "rotated", 28800), 0)
	m := managerFor(store, stub.server.URL)

	if _, err := m.Valid(context.Background(), "acct"); err != nil {
		t.Fatalf("Valid: %v", err)
	}

	saved, err := store.LoadToken(context.Background(), postgres.DefaultProviderName, "acct")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if saved.RefreshToken != "rotated" {
		t.Fatalf("persisted refresh token = %q, want %q", saved.RefreshToken, "rotated")
	}
	if saved.AccessToken != "new-access" {
		t.Fatalf("persisted access token = %q, want %q", saved.AccessToken, "new-access")
	}
	if !saved.ExpiresAt.After(time.Now()) {
		t.Fatalf("persisted expiry = %v, want a future time", saved.ExpiresAt)
	}
}

func TestOAuthInvalidGrantReturnsDeadError(t *testing.T) {
	store := newFakeTokenStore()
	store.tokens[tokenKey{postgres.DefaultProviderName, "acct"}] = expiredToken("r1")
	stub := newStubOAuth(t, http.StatusBadRequest, `{"error":"invalid_grant"}`, 0)
	m := managerFor(store, stub.server.URL)

	_, err := m.Valid(context.Background(), "acct")

	var dead *DeadRefreshTokenError
	if !errors.As(err, &dead) {
		t.Fatalf("error = %v, want *DeadRefreshTokenError", err)
	}
	if dead.Account != "acct" {
		t.Fatalf("dead account = %q, want %q", dead.Account, "acct")
	}
}
