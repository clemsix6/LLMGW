package claudemax

import (
	"context"
	"encoding/json"
	"errors"
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
	// oauthClientID is the Claude Code OAuth client identifier.
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// anthropicVersion is the required anthropic-version header value.
	anthropicVersion = "2023-06-01"

	// oauthBeta is the required anthropic-beta header value for the OAuth surface.
	oauthBeta = "oauth-2025-04-20"

	// defaultAnthropicBaseURL is the base URL of the OAuth token endpoint.
	defaultAnthropicBaseURL = "https://api.anthropic.com"

	// refreshMargin refreshes the access token this long before it actually expires.
	refreshMargin = 60 * time.Second
)

// DeadRefreshTokenError signals that an account's refresh token is no longer accepted
// (the OAuth endpoint returned invalid_grant). Recovery requires a manual re-seed.
type DeadRefreshTokenError struct {
	Account string // Account is the label of the account whose refresh token is dead.
}

// Error implements the error interface.
func (e *DeadRefreshTokenError) Error() string {
	return fmt.Sprintf("refresh token for account %q is dead (invalid_grant); re-seed required", e.Account)
}

// HTTPStatus returns 502 Bad Gateway; the upstream credential is dead.
func (e *DeadRefreshTokenError) HTTPStatus() int { return 502 }

// ErrorType returns the stable classifier "dead_refresh_token".
func (e *DeadRefreshTokenError) ErrorType() string { return "dead_refresh_token" }

// RetryAfter returns (0, false); no retry hint applies for a dead token.
func (e *DeadRefreshTokenError) RetryAfter() (time.Duration, bool) { return 0, false }

// tokenStore is the subset of the persistence port the token manager needs.
type tokenStore interface {
	// LoadToken returns the persisted token for the given provider name and account label.
	LoadToken(ctx context.Context, providerName, account string) (domain.Token, error)

	// SaveToken persists the token for the given provider name and account label.
	SaveToken(ctx context.Context, providerName, account string, t domain.Token) error
}

// tokenManager hands out valid OAuth access tokens, refreshing expired ones. Refreshes are
// single-flight per account and the rotated token is persisted before it is returned.
type tokenManager struct {
	store tokenStore // store persists tokens so rotation survives restarts.

	providerName string // providerName is passed to every store call to scope it to the correct provider.

	httpClient *http.Client // httpClient performs the refresh request.

	baseURL string // baseURL is the OAuth endpoint base; injectable for tests.

	claudeCodeVersion string // claudeCodeVersion is the spoofed client version the session-key bootstrap sends.

	group singleflight.Group // group coalesces concurrent refreshes per account.
}

// newTokenManager builds a token manager backed by store, scoped to providerName, and pointing
// at the real OAuth endpoint.
func newTokenManager(store tokenStore, claudeCodeVersion, providerName string) *tokenManager {
	return &tokenManager{
		store:             store,
		providerName:      providerName,
		httpClient:        &http.Client{Timeout: 30 * time.Second},
		baseURL:           defaultAnthropicBaseURL,
		claudeCodeVersion: claudeCodeVersion,
	}
}

// Valid returns a non-expired access token for the account, refreshing it if needed.
func (m *tokenManager) Valid(ctx context.Context, account string) (string, error) {
	token, err := m.store.LoadToken(ctx, m.providerName, account)
	if err != nil {
		return "", fmt.Errorf("load token for %q:\n%w", account, err)
	}

	if fresh(token) {
		return token.AccessToken, nil
	}

	return m.refresh(ctx, account)
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

// doRefresh exchanges the stored refresh token, persists the rotated token, then returns the
// fresh access token. The rotated token is committed before use for crash-safety.
func (m *tokenManager) doRefresh(ctx context.Context, account string) (string, error) {
	current, err := m.store.LoadToken(ctx, m.providerName, account)
	if err != nil {
		return "", fmt.Errorf("load token for %q:\n%w", account, err)
	}

	// A prior single-flight leader may have already refreshed; skip the redundant exchange.
	if fresh(current) {
		return current.AccessToken, nil
	}

	// No refresh token yet (freshly seeded account): bootstrap from the session key.
	if current.RefreshToken == "" {
		return m.bootstrapAndSave(ctx, account, current.SessionKey)
	}

	refreshed, err := m.exchange(ctx, account, current.RefreshToken)
	if err != nil {
		var dead *DeadRefreshTokenError
		if errors.As(err, &dead) && current.SessionKey != "" {
			return m.bootstrapAndSave(ctx, account, current.SessionKey) // self-heal from the session key
		}
		return "", err
	}

	if err := m.store.SaveToken(ctx, m.providerName, account, refreshed); err != nil {
		return "", fmt.Errorf("persist rotated token for %q:\n%w", account, err)
	}
	return refreshed.AccessToken, nil
}

// bootstrapAndSave mints a fresh OAuth token set from the account's session key and persists it.
// An empty session key means the account has no recoverable credential, surfaced as a dead-token
// error so the operator re-seeds it.
func (m *tokenManager) bootstrapAndSave(ctx context.Context, account, sessionKey string) (string, error) {
	if sessionKey == "" {
		return "", &DeadRefreshTokenError{Account: account}
	}

	token, err := bootstrapFromSession(ctx, m.httpClient, m.baseURL, m.claudeCodeVersion, sessionKey)
	if err != nil {
		return "", err
	}

	if err := m.store.SaveToken(ctx, m.providerName, account, token); err != nil {
		return "", fmt.Errorf("persist bootstrapped token for %q:\n%w", account, err)
	}
	return token.AccessToken, nil
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

	endpoint := m.baseURL + "/v1/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build oauth request:\n%w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("anthropic-beta", oauthBeta)
	return req, nil
}

// tokenResponse is the JSON body of a successful OAuth token exchange.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`  // AccessToken is the new bearer token.
	RefreshToken string `json:"refresh_token"` // RefreshToken is the rotated refresh credential.
	ExpiresIn    int    `json:"expires_in"`    // ExpiresIn is the access token lifetime in seconds.
}

// parseExchange turns the OAuth endpoint response into a Token, mapping invalid_grant to a
// DeadRefreshTokenError and other non-2xx responses to a generic error.
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
		ExpiresAt:    time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second),
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
