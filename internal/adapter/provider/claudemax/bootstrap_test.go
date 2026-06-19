package claudemax

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/domain"
)

// bootstrapStub serves the full cookie->OAuth flow: /api/bootstrap returns a paid chat org, the
// org-scoped authorize returns a redirect carrying a code, and the token endpoint returns a token
// set for an authorization_code grant while rejecting any refresh_token grant with invalid_grant.
// It lets the manager's "no refresh token" and "self-heal on invalid_grant" paths run end-to-end.
func bootstrapStub(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/bootstrap":
			_, _ = w.Write([]byte(`{"account":{"memberships":[{"organization":{"uuid":"org-123","capabilities":["chat","claude_max"]}}]}}`))
		case strings.HasSuffix(r.URL.Path, "/authorize"):
			_, _ = w.Write([]byte(`{"redirect_uri":"https://console.anthropic.com/oauth/code/callback?code=the-code&state=st"}`))
		case r.URL.Path == "/v1/oauth/token":
			_ = r.ParseForm()
			if r.FormValue("grant_type") == "refresh_token" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_, _ = w.Write([]byte(successBody("bootstrapped-access", "boot-refresh", 28800)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// managerForBootstrap builds a token manager pointed at the bootstrap stub.
func managerForBootstrap(t *testing.T, store tokenStore) *tokenManager {
	m := newTokenManager(store, "2.1.181", postgres.DefaultProviderName)
	m.baseURL = bootstrapStub(t).URL
	return m
}

func TestOAuthBootstrapsWhenNoRefreshToken(t *testing.T) {
	store := newFakeTokenStore()
	store.tokens[tokenKey{postgres.DefaultProviderName, "acct"}] = domain.Token{SessionKey: "sess-abc"} // freshly seeded: session key only

	m := managerForBootstrap(t, store)
	got, err := m.Valid(context.Background(), "acct")
	if err != nil {
		t.Fatalf("Valid: %v", err)
	}
	if got != "bootstrapped-access" {
		t.Fatalf("access token = %q, want bootstrapped-access", got)
	}

	saved, _ := store.LoadToken(context.Background(), postgres.DefaultProviderName, "acct")
	if saved.RefreshToken != "boot-refresh" {
		t.Fatalf("persisted refresh = %q, want boot-refresh", saved.RefreshToken)
	}
}

func TestOAuthReBootstrapsOnInvalidGrant(t *testing.T) {
	store := newFakeTokenStore()
	store.tokens[tokenKey{postgres.DefaultProviderName, "acct"}] = domain.Token{
		AccessToken:  "old",
		RefreshToken: "dead-refresh",
		SessionKey:   "sess-abc",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}

	m := managerForBootstrap(t, store)
	got, err := m.Valid(context.Background(), "acct")
	if err != nil {
		t.Fatalf("Valid: %v", err)
	}
	if got != "bootstrapped-access" {
		t.Fatalf("access token = %q, want bootstrapped-access (should re-bootstrap from session key)", got)
	}
}

func TestOAuthDeadRefreshWithoutSessionKeyFails(t *testing.T) {
	store := newFakeTokenStore()
	store.tokens[tokenKey{postgres.DefaultProviderName, "acct"}] = domain.Token{
		AccessToken:  "old",
		RefreshToken: "dead-refresh", // no session key to recover from
		ExpiresAt:    time.Now().Add(-time.Hour),
	}

	m := managerForBootstrap(t, store)
	_, err := m.Valid(context.Background(), "acct")

	var dead *DeadRefreshTokenError
	if !errors.As(err, &dead) {
		t.Fatalf("error = %v, want *DeadRefreshTokenError (no session key to self-heal)", err)
	}
}
