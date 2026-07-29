package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestProjectAuth verifies project headers and the opaque SDK usage principal.
func TestProjectAuth(t *testing.T) {
	created := testHarness.createKey(t, "auth")
	assertMissingProjectAuth(t)
	assertAcceptedProjectHeaders(t, created.Plaintext)

	other := testHarness.createKey(t, "auth-other")
	assertStableProjectAuthFailures(t, created.Plaintext, other.Plaintext)
	testHarness.assertSecretsAbsent(t, created.Plaintext, other.Plaintext)
}

// assertMissingProjectAuth verifies generation rejects a missing credential.
func assertMissingProjectAuth(t *testing.T) {
	t.Helper()
	status, body := gatewayRequest(
		t,
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(chatFixture),
		requestHeaders{},
	)
	if status != http.StatusUnauthorized {
		t.Fatalf("missing-key status = %d, want 401", status)
	}
	if len(body) == 0 {
		t.Fatal("missing-key response body is empty")
	}
}

// assertAcceptedProjectHeaders verifies both supported credential headers.
func assertAcceptedProjectHeaders(t *testing.T, plaintext string) {
	t.Helper()
	for name, headers := range map[string]requestHeaders{
		"Bearer":    {authorization: "Bearer " + plaintext},
		"x-api-key": {apiKey: plaintext},
	} {
		status, _ := gatewayRequest(t, http.MethodGet, "/v1/models", nil, headers)
		if status != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", name, status)
		}
	}
}

// assertStableProjectAuthFailures verifies rejected credentials share one response.
func assertStableProjectAuthFailures(t *testing.T, plaintext string, otherPlaintext string) {
	t.Helper()
	_, missing := gatewayRequest(t, http.MethodGet, "/v1/models", nil, requestHeaders{})
	mismatchStatus, mismatch := gatewayRequest(t, http.MethodGet, "/v1/models", nil, requestHeaders{
		authorization: "Bearer " + plaintext,
		apiKey:        otherPlaintext,
	})
	unknownStatus, unknown := gatewayRequest(t, http.MethodGet, "/v1/models", nil, requestHeaders{
		authorization: "Bearer " + corruptKey(plaintext),
	})
	if mismatchStatus != http.StatusUnauthorized || unknownStatus != http.StatusUnauthorized {
		t.Fatal("mismatched or unknown project key did not return 401")
	}
	if !bytes.Equal(missing, mismatch) || !bytes.Equal(missing, unknown) {
		t.Fatal("authentication failures did not use one stable response")
	}
}

// TestRoutePolicy verifies metadata, denied, and unknown route accounting.
func TestRoutePolicy(t *testing.T) {
	created := testHarness.createKey(t, "routes")
	assertDeniedRoutes(t, created)
	assertUnknownRoute(t, created)
	testHarness.assertSecretsAbsent(t, created.Plaintext)
}

// assertDeniedRoutes verifies forbidden SDK surfaces remain invisible and unmetered.
func assertDeniedRoutes(t *testing.T, created governance.CreatedKey) {
	t.Helper()
	generationBefore := requestCount(t, created, governance.OperationGeneration, "")
	for _, path := range deniedRoutes {
		status, _ := gatewayRequest(t, http.MethodGet, path, nil, requestHeaders{
			authorization: "Bearer " + created.Plaintext,
		})
		if status != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, status)
		}
	}
	if got := requestCount(t, created, governance.OperationGeneration, ""); got != generationBefore {
		t.Fatalf("generation count after denied routes = %d, want %d", got, generationBefore)
	}
}

// assertUnknownRoute verifies fail-closed authentication and generation accounting.
func assertUnknownRoute(t *testing.T, created governance.CreatedKey) {
	t.Helper()
	status, _ := gatewayRequest(t, http.MethodGet, "/new-sdk-route", nil, requestHeaders{})
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated new route status = %d, want 401", status)
	}
	status, _ = gatewayRequest(t, http.MethodGet, "/new-sdk-route", nil, requestHeaders{
		authorization: "Bearer " + created.Plaintext,
	})
	if status != http.StatusNotFound {
		t.Fatalf("authenticated new route status = %d, want 404", status)
	}
	if got := requestCount(t, created, governance.OperationGeneration, "/new-sdk-route"); got != 1 {
		t.Fatalf("new route generation count = %d, want 1", got)
	}
}

// requestCount returns one project's matching request count.
func requestCount(
	t *testing.T,
	created governance.CreatedKey,
	operation governance.Operation,
	path string,
) int64 {
	t.Helper()
	return testHarness.countRequests(t, created.Key.ProjectID, operation, path)
}

// requestHeaders contains the two supported downstream credential fields.
type requestHeaders struct {
	authorization string // authorization is the optional Authorization value.
	apiKey        string // apiKey is the optional x-api-key value.
}

// gatewayRequest performs one bounded request without logging credential headers.
func gatewayRequest(
	t *testing.T,
	method string,
	path string,
	body io.Reader,
	headers requestHeaders,
) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(
		context.Background(),
		method,
		testHarness.BaseURL+path,
		body,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if headers.authorization != "" {
		request.Header.Set("Authorization", headers.authorization)
	}
	if headers.apiKey != "" {
		request.Header.Set("x-api-key", headers.apiKey)
	}

	response, err := testHarness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

// corruptKey returns an unknown but structurally valid project key.
func corruptKey(raw string) string {
	if raw[len(raw)-1] == 'A' {
		return raw[:len(raw)-1] + "B"
	}
	return raw[:len(raw)-1] + "A"
}

// chatFixture is a non-secret deterministic downstream prompt.
const chatFixture = `{"model":"test-model","messages":[{"role":"user","content":"fixture-prompt"}]}`

// deniedRoutes lists every SDK surface hard-denied by the current policy.
var deniedRoutes = []string{
	"/",
	"/anthropic/callback",
	"/codex/callback",
	"/antigravity/callback",
	"/management.html",
	"/v0/management/config",
	"/v0/resource/plugins/x",
}
