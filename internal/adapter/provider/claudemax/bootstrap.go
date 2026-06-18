package claudemax

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

const (
	// ccRedirectURI is the Claude Code OAuth redirect URI the authorization code is delivered to.
	ccRedirectURI = "https://console.anthropic.com/oauth/code/callback"

	// ccScopes are the OAuth scopes Claude Code requests, space-separated in a single scope param.
	ccScopes = "user:profile user:inference"
)

// Bootstrap exchanges a claude.ai session key for a fresh Claude Code OAuth token set against the
// real endpoint, without persistence. It runs the same bootstrap -> authorize -> token exchange
// clewdr does, so a stored session key can mint (and later re-mint) OAuth tokens. The E2E
// coordinator and the token manager's self-heal path both use it.
func Bootstrap(ctx context.Context, sessionKey, claudeCodeVersion string) (domain.Token, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return bootstrapFromSession(ctx, client, defaultAnthropicBaseURL, claudeCodeVersion, sessionKey)
}

// bootstrapFromSession runs the cookie->OAuth flow with an injectable client and baseURL (tests).
// It resolves the organization, authorizes a PKCE code, exchanges it for tokens, and stamps the
// session key onto the result so the caller can re-bootstrap when the refresh token later dies.
func bootstrapFromSession(ctx context.Context, client *http.Client, baseURL, version, sessionKey string) (domain.Token, error) {
	orgUUID, err := getOrganization(ctx, client, baseURL, version, sessionKey)
	if err != nil {
		return domain.Token{}, err
	}

	code, verifier, state, err := authorizeCode(ctx, client, baseURL, version, sessionKey, orgUUID)
	if err != nil {
		return domain.Token{}, err
	}

	token, err := exchangeAuthCode(ctx, client, baseURL, code, verifier, state)
	if err != nil {
		return domain.Token{}, err
	}

	token.SessionKey = sessionKey
	return token, nil
}

// getOrganization fetches the account bootstrap with the session cookie and returns the UUID of a
// paid, chat-capable organization. A 401/403 means the session key is dead and must be re-seeded.
func getOrganization(ctx context.Context, client *http.Client, baseURL, version, sessionKey string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/bootstrap", nil)
	if err != nil {
		return "", fmt.Errorf("build bootstrap request:\n%w", err)
	}
	setCookieHeaders(req, version, sessionKey)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("bootstrap request:\n%w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read bootstrap response:\n%w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", &DeadRefreshTokenError{Account: "session"}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bootstrap failed: status %d: %s", resp.StatusCode, body)
	}
	return pickOrgUUID(body)
}

// bootstrapResponse is the slice of the /api/bootstrap payload the org selection needs.
type bootstrapResponse struct {
	Account struct {
		Memberships []struct {
			Organization struct {
				UUID         string   `json:"uuid"`         // UUID identifies the organization.
				Capabilities []string `json:"capabilities"` // Capabilities lists the org's enabled features.
			} `json:"organization"`
		} `json:"memberships"`
	} `json:"account"`
}

// pickOrgUUID selects the first chat-capable organization and verifies it is a paid tier, mirroring
// clewdr's selection: a chat-capable but non-paid account cannot mint Claude Code tokens.
func pickOrgUUID(body []byte) (string, error) {
	var parsed bootstrapResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode bootstrap response:\n%w", err)
	}

	for _, m := range parsed.Account.Memberships {
		org := m.Organization
		if !hasCapability(org.Capabilities, "chat") {
			continue
		}
		if !hasPaidTier(org.Capabilities) {
			return "", fmt.Errorf("bootstrap: organization %q is not a paid (pro/max) account", org.UUID)
		}
		return org.UUID, nil
	}
	return "", fmt.Errorf("bootstrap: no chat-capable organization found for this session key")
}

// hasCapability reports whether caps contains an exact capability.
func hasCapability(caps []string, target string) bool {
	for _, c := range caps {
		if c == target {
			return true
		}
	}
	return false
}

// hasPaidTier reports whether any capability marks a paid plan (substring match, as clewdr does).
func hasPaidTier(caps []string) bool {
	for _, c := range caps {
		for _, tier := range []string{"pro", "max", "enterprise", "raven"} {
			if strings.Contains(c, tier) {
				return true
			}
		}
	}
	return false
}

// authorizeCode runs the PKCE authorization with the session cookie and returns the authorization
// code, the PKCE verifier (needed for the token exchange), and the state echoed by the redirect.
func authorizeCode(ctx context.Context, client *http.Client, baseURL, version, sessionKey, orgUUID string) (string, string, string, error) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		return "", "", "", err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return "", "", "", err
	}

	req, err := authorizeRequest(ctx, baseURL, version, sessionKey, orgUUID, authorizePayload(orgUUID, challenge, state))
	if err != nil {
		return "", "", "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("authorize request:\n%w", err)
	}
	defer resp.Body.Close()

	code, returnedState, err := parseAuthorizeResponse(resp)
	if err != nil {
		return "", "", "", err
	}
	if returnedState != "" {
		state = returnedState
	}
	return code, verifier, state, nil
}

// authorizePayload builds the JSON body of the authorize request: the standard OAuth PKCE fields
// plus the organization_uuid clewdr sends alongside them.
func authorizePayload(orgUUID, challenge, state string) map[string]string {
	return map[string]string{
		"client_id":             oauthClientID,
		"response_type":         "code",
		"redirect_uri":          ccRedirectURI,
		"scope":                 ccScopes,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"state":                 state,
		"organization_uuid":     orgUUID,
	}
}

// authorizeRequest builds the POST to the org-scoped authorize endpoint, carrying the session cookie.
func authorizeRequest(ctx context.Context, baseURL, version, sessionKey, orgUUID string, payload map[string]string) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode authorize payload:\n%w", err)
	}

	endpoint := fmt.Sprintf("%s/v1/oauth/%s/authorize", baseURL, orgUUID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build authorize request:\n%w", err)
	}

	setCookieHeaders(req, version, sessionKey)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// parseAuthorizeResponse extracts the authorization code and state from the authorize response,
// whose body carries a redirect_uri with the code in its query. A 401/403 means a dead session key.
func parseAuthorizeResponse(resp *http.Response) (string, string, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read authorize response:\n%w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", "", &DeadRefreshTokenError{Account: "session"}
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("authorize failed: status %d: %s", resp.StatusCode, body)
	}

	var parsed struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("decode authorize response:\n%w", err)
	}
	return codeFromRedirect(parsed.RedirectURI)
}

// codeFromRedirect parses the code and state out of the authorize redirect URI's query string.
func codeFromRedirect(redirectURI string) (string, string, error) {
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return "", "", fmt.Errorf("parse authorize redirect %q:\n%w", redirectURI, err)
	}

	query := parsed.Query()
	code := query.Get("code")
	if code == "" {
		return "", "", fmt.Errorf("authorize redirect carries no code: %q", redirectURI)
	}
	return code, query.Get("state"), nil
}

// exchangeAuthCode swaps the authorization code (plus PKCE verifier) for a token set at the OAuth
// token endpoint, reusing the shared response parsing so invalid_grant maps to a dead-credential.
func exchangeAuthCode(ctx context.Context, client *http.Client, baseURL, code, verifier, state string) (domain.Token, error) {
	form := authCodeForm(code, verifier, state)

	endpoint := baseURL + "/v1/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return domain.Token{}, fmt.Errorf("build token request:\n%w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("anthropic-beta", oauthBeta)

	resp, err := client.Do(req)
	if err != nil {
		return domain.Token{}, fmt.Errorf("token request:\n%w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return domain.Token{}, fmt.Errorf("read token response:\n%w", err)
	}
	return parseExchange("session", resp.StatusCode, body)
}

// authCodeForm builds the form body of the authorization_code grant.
func authCodeForm(code, verifier, state string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"client_id":     {oauthClientID},
		"redirect_uri":  {ccRedirectURI},
		"state":         {state},
	}
}

// setCookieHeaders applies the session cookie and the Claude Code client headers clewdr sends on
// the cookie-authenticated bootstrap and authorize requests.
func setCookieHeaders(req *http.Request, version, sessionKey string) {
	req.Header.Set("Cookie", "sessionKey="+sessionKey)
	req.Header.Set("User-Agent", "claude-code/"+version)
	req.Header.Set("Origin", defaultAnthropicBaseURL)
	req.Header.Set("Referer", defaultAnthropicBaseURL+"/new")
}

// newPKCE generates a PKCE verifier and its S256 challenge (base64url, no padding).
func newPKCE() (string, string, error) {
	verifier, err := randomURLSafe(32)
	if err != nil {
		return "", "", err
	}

	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// randomURLSafe returns n cryptographically random bytes encoded as base64url without padding.
func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random bytes:\n%w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
