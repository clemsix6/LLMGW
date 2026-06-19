package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/clemsix6/LLMGW/internal/domain"
)

const (
	// oauthClientID is the public Codex OAuth client identifier the refresh grant authenticates as.
	// Sourced from ChatMock and corroborated by opencode-openai-codex-auth.
	oauthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// defaultIssuerBaseURL is the base of the OpenAI OAuth issuer hosting the token endpoint.
	defaultIssuerBaseURL = "https://auth.openai.com"

	// tokenPath is the path of the OAuth token endpoint under the issuer.
	tokenPath = "/oauth/token"

	// refreshMargin refreshes the access token this long before it actually expires.
	refreshMargin = 60 * time.Second

	// defaultTokenTTL is the assumed access-token lifetime when neither the response nor the token
	// itself carries an expiry, kept short so an unknown-expiry token is refreshed promptly.
	defaultTokenTTL = time.Hour
)

// tokenStore is the subset of the persistence port the token manager needs.
type tokenStore interface {
	// LoadToken returns the persisted token for the given provider name and account label.
	LoadToken(ctx context.Context, providerName, account string) (domain.Token, error)

	// SaveToken persists the token for the given provider name and account label.
	SaveToken(ctx context.Context, providerName, account string, t domain.Token) error
}

// tokenManager hands out valid OAuth access tokens (and the per-account ChatGPT account id),
// refreshing expired ones. Refreshes are single-flight per account and the rotated token is
// persisted before it is returned. Unlike claudemax there is no session-key bootstrap: Codex
// accounts are seeded directly with a refresh token and account id.
type tokenManager struct {
	store tokenStore // store persists tokens so rotation survives restarts.

	providerName string // providerName scopes every store call to the Codex provider.

	httpClient *http.Client // httpClient performs the refresh request.

	baseURL string // baseURL is the OAuth issuer base; injectable for tests.

	group singleflight.Group // group coalesces concurrent refreshes per account.
}

// newTokenManager builds a token manager backed by store, scoped to providerName, pointing at the
// real OpenAI OAuth issuer.
func newTokenManager(store tokenStore, providerName string) *tokenManager {
	return &tokenManager{
		store:        store,
		providerName: providerName,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		baseURL:      defaultIssuerBaseURL,
	}
}

// Valid returns a non-expired access token and the account's durable ChatGPT account id,
// refreshing the access token when needed. The account id is seeded once and never rotates, so it
// is always read from the stored token rather than the refresh response.
func (m *tokenManager) Valid(ctx context.Context, account string) (accessToken, accountID string, err error) {
	token, err := m.store.LoadToken(ctx, m.providerName, account)
	if err != nil {
		return "", "", fmt.Errorf("load token for %q:\n%w", account, err)
	}

	if fresh(token) {
		return token.AccessToken, token.ChatGPTAccountID, nil
	}

	refreshed, err := m.refresh(ctx, account)
	if err != nil {
		return "", "", err
	}
	return refreshed, token.ChatGPTAccountID, nil
}

// fresh reports whether the access token is present and outside the refresh margin of expiry.
func fresh(t domain.Token) bool {
	if t.AccessToken == "" {
		return false
	}
	return time.Now().Add(refreshMargin).Before(t.ExpiresAt)
}

// refresh performs a single-flight refresh per account and returns the new access token.
func (m *tokenManager) refresh(ctx context.Context, account string) (string, error) {
	result, err, _ := m.group.Do(account, func() (any, error) {
		return m.doRefresh(ctx, account)
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

// doRefresh exchanges the stored refresh token, persists the rotated token (preserving the seeded
// account id), then returns the fresh access token. The rotated token is committed before use for
// crash-safety. A missing refresh token means the account was never seeded.
func (m *tokenManager) doRefresh(ctx context.Context, account string) (string, error) {
	current, err := m.store.LoadToken(ctx, m.providerName, account)
	if err != nil {
		return "", fmt.Errorf("load token for %q:\n%w", account, err)
	}

	// A prior single-flight leader may have already refreshed; skip the redundant exchange.
	if fresh(current) {
		return current.AccessToken, nil
	}
	if current.RefreshToken == "" {
		return "", &DeadRefreshTokenError{Account: account}
	}

	refreshed, err := m.exchange(ctx, account, current.RefreshToken)
	if err != nil {
		return "", err
	}

	refreshed.ChatGPTAccountID = current.ChatGPTAccountID
	if err := m.store.SaveToken(ctx, m.providerName, account, refreshed); err != nil {
		return "", fmt.Errorf("persist rotated token for %q:\n%w", account, err)
	}
	return refreshed.AccessToken, nil
}

// exchange calls the OAuth token endpoint to swap a refresh token for a fresh token set.
func (m *tokenManager) exchange(ctx context.Context, account, refreshToken string) (domain.Token, error) {
	req, err := m.buildRequest(ctx, refreshToken)
	if err != nil {
		return domain.Token{}, err
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return domain.Token{}, fmt.Errorf("oauth refresh request:\n%w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return domain.Token{}, fmt.Errorf("read oauth response:\n%w", err)
	}

	return parseExchange(account, resp.StatusCode, body)
}

// buildRequest builds the form-encoded refresh_token grant request.
func (m *tokenManager) buildRequest(ctx context.Context, refreshToken string) (*http.Request, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {oauthClientID},
	}

	endpoint := m.baseURL + tokenPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build oauth request:\n%w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

// tokenResponse is the JSON body of a successful OAuth token exchange. The Codex refresh response
// rotates the refresh token and returns a JWT access token whose exp claim drives expiry.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`  // AccessToken is the new bearer token (a JWT).
	RefreshToken string `json:"refresh_token"` // RefreshToken is the rotated refresh credential.
	ExpiresIn    int    `json:"expires_in"`    // ExpiresIn is the access token lifetime in seconds, when provided.
}

// parseExchange turns the OAuth endpoint response into a Token, mapping invalid_grant to a
// DeadRefreshTokenError and other non-2xx responses to a generic error. A rotated refresh token
// may be absent, in which case the prior one is kept by the caller's stored value semantics.
func parseExchange(account string, status int, body []byte) (domain.Token, error) {
	if status != http.StatusOK {
		if isInvalidGrant(body) {
			return domain.Token{}, &DeadRefreshTokenError{Account: account}
		}
		return domain.Token{}, fmt.Errorf("oauth refresh failed: status %d: %s", status, body)
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return domain.Token{}, fmt.Errorf("decode oauth response:\n%w", err)
	}

	return domain.Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    expiresAtFor(parsed.AccessToken, parsed.ExpiresIn),
	}, nil
}

// isInvalidGrant reports whether the OAuth error body indicates a dead refresh token.
func isInvalidGrant(body []byte) bool {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	return parsed.Error == "invalid_grant"
}

// expiresAtFor derives the access token's expiry, preferring the JWT exp claim (authoritative for
// the Codex access token), then the OAuth expires_in, then a conservative default.
func expiresAtFor(accessToken string, expiresIn int) time.Time {
	if exp, ok := jwtExpiry(accessToken); ok {
		return exp
	}
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	return time.Now().Add(defaultTokenTTL)
}

// jwtExpiry decodes the exp claim from a JWT access token, reporting false when the token is not a
// decodable JWT or carries no exp.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
