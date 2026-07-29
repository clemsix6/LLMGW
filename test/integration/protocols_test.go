package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestProtocolParity catches routing or translation changes that break a supported downstream API.
func TestProtocolParity(t *testing.T) {
	created := testHarness.createKey(t, "protocol")
	tests := []struct {
		name string
		path string
		body string
		want func(*testing.T, map[string]any)
	}{
		{
			name: "anthropic messages",
			path: "/v1/messages",
			body: `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"fixture-prompt"}]}`,
			want: assertAnthropicMessageShape,
		},
		{
			name: "openai chat completions",
			path: "/v1/chat/completions",
			body: chatFixture,
			want: assertOpenAIChatShape,
		},
		{
			name: "openai responses",
			path: "/v1/responses",
			body: `{"model":"test-model","input":"fixture-prompt"}`,
			want: assertOpenAIResponseShape,
		},
		{
			name: "gemini generate content",
			path: "/v1beta/models/test-model:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"fixture-prompt"}]}]}`,
			want: assertGeminiResponseShape,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testHarness.Upstream.Enqueue(defaultStubResponse())
			status, body := gatewayRequest(
				t,
				http.MethodPost,
				test.path,
				bytes.NewBufferString(test.body),
				requestHeaders{authorization: "Bearer " + created.Plaintext},
			)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
			}
			test.want(t, decodeJSON(t, body))
		})
	}
	testHarness.assertSecretsAbsent(t, created.Plaintext, "fixture-prompt")
}

// TestStreamingProtocolParity catches translation changes that emit an invalid stream envelope.
func TestStreamingProtocolParity(t *testing.T) {
	created := testHarness.createKey(t, "protocol-stream")
	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "anthropic messages",
			path: "/v1/messages",
			body: `{"model":"test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"fixture-prompt"}]}`,
		},
		{
			name: "openai chat completions",
			path: "/v1/chat/completions",
			body: `{"model":"test-model","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"fixture-prompt"}]}`,
		},
		{
			name: "openai responses",
			path: "/v1/responses",
			body: `{"model":"test-model","stream":true,"input":"fixture-prompt"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testHarness.Upstream.Enqueue(streamingUsageResponse(3, 2))
			status, body := gatewayRequest(
				t,
				http.MethodPost,
				test.path,
				bytes.NewBufferString(test.body),
				requestHeaders{authorization: "Bearer " + created.Plaintext},
			)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
			}
			if err := validateProtocolStream(test.name, body); err != nil {
				t.Fatalf("invalid %s stream: %v", test.name, err)
			}
		})
	}
	testHarness.assertSecretsAbsent(t, created.Plaintext, "fixture-prompt")
}

// TestMetadataProtocolsAreAuthenticatedAndUnmetered catches accidental generation accounting.
func TestMetadataProtocolsAreAuthenticatedAndUnmetered(t *testing.T) {
	created := testHarness.createKey(t, "protocol-metadata")
	tests := []struct {
		method string
		path   string
		body   string
		check  func(*testing.T, map[string]any)
	}{
		{
			method: http.MethodPost,
			path:   "/v1/messages/count_tokens",
			body:   `{"model":"test-model","messages":[{"role":"user","content":"fixture-prompt"}]}`,
			check: func(t *testing.T, object map[string]any) {
				requireNumber(t, object, "input_tokens")
			},
		},
		{
			method: http.MethodGet,
			path:   "/v1/models",
			check: func(t *testing.T, object map[string]any) {
				requireArray(t, object, "data")
			},
		},
		{
			method: http.MethodGet,
			path:   "/v1beta/models",
			check: func(t *testing.T, object map[string]any) {
				requireArray(t, object, "models")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			before := requestCount(t, created, governance.OperationMetadata, test.path)
			var body io.Reader
			if test.body != "" {
				body = strings.NewReader(test.body)
			}
			status, responseBody := gatewayRequest(
				t,
				test.method,
				test.path,
				body,
				requestHeaders{authorization: "Bearer " + created.Plaintext},
			)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(responseBody))
			}
			test.check(t, decodeJSON(t, responseBody))
			if got := requestCount(t, created, governance.OperationMetadata, test.path); got != before+1 {
				t.Fatalf("metadata rows = %d, want %d", got, before+1)
			}
		})
	}
	if got := requestCount(t, created, governance.OperationGeneration, ""); got != 0 {
		t.Fatalf("metadata protocols created %d generation rows", got)
	}
}

// TestInactiveAndConflictingKeysShareGenericFailure catches credential-state disclosure.
func TestInactiveAndConflictingKeysShareGenericFailure(t *testing.T) {
	active := testHarness.createKey(t, "protocol-active")
	expired := testHarness.createExpiredKey(t, "protocol-expired")
	revoked := testHarness.createKey(t, "protocol-revoked")
	if err := testHarness.Keys.Revoke(context.Background(), revoked.Key.ID); err != nil {
		t.Fatal("revoke integration key failed")
	}
	_, generic := gatewayRequest(t, http.MethodGet, "/v1/models", nil, requestHeaders{
		authorization: "Bearer " + corruptKey(active.Plaintext),
	})
	tests := []requestHeaders{
		{authorization: "Bearer " + expired.Plaintext},
		{authorization: "Bearer " + revoked.Plaintext},
		{authorization: "Bearer " + active.Plaintext, apiKey: revoked.Plaintext},
	}
	for _, headers := range tests {
		status, body := gatewayRequest(t, http.MethodGet, "/v1/models", nil, headers)
		if status != http.StatusUnauthorized || !bytes.Equal(body, generic) {
			t.Fatal("inactive or conflicting credential disclosed a distinct failure")
		}
	}
	testHarness.assertSecretsAbsent(t, active.Plaintext, expired.Plaintext, revoked.Plaintext)
}

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode protocol JSON: %v; body=%s", err, safeBodySummary(body))
	}
	return object
}

func assertAnthropicMessageShape(t *testing.T, object map[string]any) {
	t.Helper()
	requireString(t, object, "id")
	requireString(t, object, "type")
	requireArray(t, object, "content")
	requireObject(t, object, "usage")
}

func assertOpenAIChatShape(t *testing.T, object map[string]any) {
	t.Helper()
	requireString(t, object, "id")
	requireString(t, object, "object")
	requireArray(t, object, "choices")
	requireObject(t, object, "usage")
}

func assertOpenAIResponseShape(t *testing.T, object map[string]any) {
	t.Helper()
	requireString(t, object, "id")
	requireString(t, object, "object")
	requireArray(t, object, "output")
	requireObject(t, object, "usage")
}

func assertGeminiResponseShape(t *testing.T, object map[string]any) {
	t.Helper()
	requireArray(t, object, "candidates")
	requireObject(t, object, "usageMetadata")
}

func requireString(t *testing.T, object map[string]any, key string) {
	t.Helper()
	if value, ok := object[key].(string); !ok || value == "" {
		t.Fatalf("%s = %#v, want non-empty string", key, object[key])
	}
}

func requireNumber(t *testing.T, object map[string]any, key string) {
	t.Helper()
	if _, ok := object[key].(float64); !ok {
		t.Fatalf("%s = %#v, want number", key, object[key])
	}
}

func requireArray(t *testing.T, object map[string]any, key string) {
	t.Helper()
	if _, ok := object[key].([]any); !ok {
		t.Fatalf("%s = %#v, want array", key, object[key])
	}
}

func requireObject(t *testing.T, object map[string]any, key string) {
	t.Helper()
	if _, ok := object[key].(map[string]any); !ok {
		t.Fatalf("%s = %#v, want object", key, object[key])
	}
}
