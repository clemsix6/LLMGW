package e2e

// This smoke drives the FULL gateway (POST /v1/chat/completions) against the REAL ChatGPT
// Codex backend. It boots the gateway + an ephemeral Postgres, seeds the Codex account,
// forwards a tiny real request, and asserts both the HTTP response shape and the recorded
// usage_event (provider = CodexProviderName, default tag = "agentic"). Gated on real
// test credentials; SKIPS cleanly when they are absent so the suite still compiles and runs.
//
// Provide credentials to exercise it (the refresh token rotates on use — re-seed after a run):
//
//	export LLMGW_CODEX_TEST_REFRESH_TOKEN=...   # the account's durable OAuth refresh token
//	export LLMGW_CODEX_TEST_ACCOUNT_ID=...      # the ChatGPT-Account-ID (acct_...)
//	go test ./test/e2e -run CodexGateway -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
)

const (
	// codexGatewayProject is the X-Project the codex gateway smoke attributes usage to.
	codexGatewayProject = "e2e-codex"
)

// TestCodexGatewayRealNonStreaming forwards a tiny real request through the full gateway,
// asserts a valid chat.completion response, and verifies a usage_event is recorded with
// provider=CodexProviderName and the default tag "agentic" (no X-Tags header sent).
func TestCodexGatewayRealNonStreaming(t *testing.T) {
	refreshToken, accountID := codexCreds(t)
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startCodexGatewayHarness(t, ctx, refreshToken, accountID)

	body := postChatCompletionsWithRetry(t, ctx, harness)
	assertPlausibleChatCompletion(t, body)

	assertCodexUsageRecorded(t, ctx, harness.DSN, codexGatewayProject, "agentic")
}

// startCodexGatewayHarness boots the gateway, seeds the Codex provider with the given
// refresh token and account ID, and returns the running harness.
func startCodexGatewayHarness(t *testing.T, ctx context.Context, refreshToken, accountID string) *Harness {
	t.Helper()

	harness, err := Start(ctx)
	if err != nil {
		t.Fatalf("start harness: %v", err)
	}
	t.Cleanup(func() { harness.Stop(context.Background()) })

	if err := harness.SeedCodex(ctx, codexTestAccount, refreshToken, accountID, testCodexVersion); err != nil {
		t.Fatalf("seed codex: %v", err)
	}
	return harness
}

// postChatCompletionsWithRetry POSTs a tiny request to /v1/chat/completions, retrying transient
// gateway 5xx with bounded backoff. It fails fast on non-retryable errors.
func postChatCompletionsWithRetry(t *testing.T, ctx context.Context, h *Harness) []byte {
	t.Helper()

	const body = `{"model":"gpt-5","max_tokens":16,"messages":[{"role":"user","content":"Reply with a single word."}]}`
	headers := map[string]string{
		"Content-Type": "application/json",
		"X-Project":    codexGatewayProject,
	}

	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if respBody, done := attemptChatPost(t, ctx, h, []byte(body), headers, attempt, &backoff); done {
			return respBody
		}
	}

	t.Fatalf("exhausted %d attempts against the real Codex API", maxAttempts)
	return nil
}

// attemptChatPost performs one POST to /v1/chat/completions and decides the outcome:
// returns the body with done=true on a 200, fails on a fatal status, or retries a transient one.
func attemptChatPost(t *testing.T, ctx context.Context, h *Harness, body []byte, headers map[string]string, attempt int, backoff *time.Duration) ([]byte, bool) {
	t.Helper()

	attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := h.Post(attemptCtx, "/v1/chat/completions", body, headers)
	if err != nil {
		retryTransient(t, attempt, backoff, "POST /v1/chat/completions: "+err.Error())
		return nil, false
	}

	respBody := readBody(t, resp)
	if resp.StatusCode == http.StatusOK {
		return respBody, true
	}

	classifyFailure(t, resp.StatusCode, respBody, attempt, backoff)
	return nil, false
}

// chatCompletion is the slice of the Chat Completions response the assertions inspect.
type chatCompletion struct {
	Object  string `json:"object"` // Object must be "chat.completion".
	Choices []struct {
		Message struct {
			Content string `json:"content"` // Content is the generated reply.
		} `json:"message"`
		FinishReason string `json:"finish_reason"` // FinishReason is why generation ended.
	} `json:"choices"`
}

// assertPlausibleChatCompletion checks that the response parses as a valid chat.completion with
// at least one choice carrying non-empty content of a plausible length.
func assertPlausibleChatCompletion(t *testing.T, body []byte) {
	t.Helper()

	var parsed chatCompletion
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, body)
	}
	if parsed.Object != "chat.completion" {
		t.Errorf("object = %q, want %q", parsed.Object, "chat.completion")
	}
	if len(parsed.Choices) == 0 {
		t.Fatalf("no choices in response: %s", body)
	}

	content := parsed.Choices[0].Message.Content
	if len(content) < minContentLength {
		t.Errorf("reply content too short (%d chars): %q", len(content), content)
	}

	finishReason := parsed.Choices[0].FinishReason
	if finishReason != "stop" && finishReason != "length" && finishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want stop, length, or tool_calls", finishReason)
	}
	t.Logf("codex gateway 200: finish_reason=%q content_len=%d reply=%q",
		finishReason, len(content), content)
}

// assertCodexUsageRecorded asserts a usage_event was recorded for (project, tag) with
// provider=CodexProviderName and a successful status.
func assertCodexUsageRecorded(t *testing.T, ctx context.Context, dsn, project, tag string) {
	t.Helper()

	conn := connectDB(t, ctx, dsn)
	defer conn.Close(ctx)

	const query = `
SELECT ue.provider, ue.tag, ue.status
FROM usage_event ue
JOIN project p ON p.id = ue.project_id
WHERE p.name = $1 AND ue.tag = $2 AND ue.status = 'ok'
ORDER BY ue.ts DESC
LIMIT 1`

	var provider, recordedTag, status string
	if err := conn.QueryRow(ctx, query, project, tag).Scan(&provider, &recordedTag, &status); err != nil {
		t.Fatalf("query usage_event for (%s, %s): %v", project, tag, err)
	}
	if provider != postgres.CodexProviderName {
		t.Errorf("usage_event provider = %q, want %q", provider, postgres.CodexProviderName)
	}
	t.Logf("usage_event recorded for (%s, %s): provider=%q status=%q", project, tag, provider, status)
}

// TestCodexGatewayRealStreaming forwards a tiny streaming request through the full gateway and
// asserts a valid chat.completion.chunk SSE stream: at least one chunk with choices[0].delta,
// a terminal data: [DONE], and no Codex instructions or reasoning leaked into the output.
func TestCodexGatewayRealStreaming(t *testing.T) {
	refreshToken, accountID := codexCreds(t)
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startCodexGatewayHarness(t, ctx, refreshToken, accountID)

	raw := postCodexStreamWithRetry(t, ctx, harness)
	assertPlausibleCodexStream(t, raw)
}

// TestCodexGatewayRealToolCall forwards a tool-calling request through the full gateway and
// asserts the response contains choices[0].message.tool_calls with the forced function name.
func TestCodexGatewayRealToolCall(t *testing.T) {
	refreshToken, accountID := codexCreds(t)
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startCodexGatewayHarness(t, ctx, refreshToken, accountID)

	body := postCodexToolCallWithRetry(t, ctx, harness)
	assertToolCallResponse(t, body)
}

// postCodexStreamWithRetry POSTs a streaming request to /v1/chat/completions and returns
// the full raw SSE text, retrying transient errors with bounded backoff.
func postCodexStreamWithRetry(t *testing.T, ctx context.Context, h *Harness) string {
	t.Helper()

	const body = `{"model":"gpt-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Reply with a single word."}]}`
	headers := map[string]string{
		"Content-Type": "application/json",
		"X-Project":    codexGatewayProject,
	}

	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if raw, done := attemptStreamPost(t, ctx, h, []byte(body), headers, attempt, &backoff); done {
			return raw
		}
	}

	t.Fatalf("exhausted %d attempts for streaming request", maxAttempts)
	return ""
}

// attemptStreamPost performs one streaming POST to /v1/chat/completions. It returns the raw
// SSE text with done=true on a 200, fails on a fatal status, or retries a transient one.
func attemptStreamPost(t *testing.T, ctx context.Context, h *Harness, body []byte, headers map[string]string, attempt int, backoff *time.Duration) (string, bool) {
	t.Helper()

	attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := h.Post(attemptCtx, "/v1/chat/completions", body, headers)
	if err != nil {
		retryTransient(t, attempt, backoff, "POST /v1/chat/completions (stream): "+err.Error())
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		classifyFailure(t, resp.StatusCode, raw, attempt, backoff)
		return "", false
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		retryTransient(t, attempt, backoff, "read stream body: "+err.Error())
		return "", false
	}
	return string(raw), true
}

// chatCompletionChunk is the slice of a Chat Completions SSE chunk the streaming assertions inspect.
type chatCompletionChunk struct {
	Object  string `json:"object"` // Object must be "chat.completion.chunk".
	Choices []struct {
		Delta        struct{} `json:"delta"`         // Delta holds incremental content or tool-call fragments.
		FinishReason *string  `json:"finish_reason"` // FinishReason is non-nil on the final chunk.
	} `json:"choices"`
}

// assertPlausibleCodexStream checks that the raw SSE output forms a valid chat.completion.chunk
// stream: at least one chunk with choices, a terminal data: [DONE], and no Codex
// instructions or reasoning leaked.
func assertPlausibleCodexStream(t *testing.T, raw string) {
	t.Helper()

	if strings.Contains(raw, "instructions") || strings.Contains(raw, "reasoning") {
		t.Error("Codex instructions or reasoning leaked into stream output")
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Errorf("stream does not contain data: [DONE]: %s", raw)
	}

	chunkCount := 0
	for _, line := range strings.Split(raw, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if !strings.HasPrefix(line, "data:") || payload == "[DONE]" {
			continue
		}
		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err == nil && len(chunk.Choices) > 0 {
			chunkCount++
		}
	}
	if chunkCount == 0 {
		t.Errorf("no valid chat.completion.chunk lines in stream: %s", raw)
	}
	t.Logf("codex stream: %d chunks with choices, raw_len=%d", chunkCount, len(raw))
}

// postCodexToolCallWithRetry POSTs a forced tool-call request to /v1/chat/completions, retrying
// transient errors with bounded backoff.
func postCodexToolCallWithRetry(t *testing.T, ctx context.Context, h *Harness) []byte {
	t.Helper()

	const body = `{"model":"gpt-5","max_tokens":64,"messages":[{"role":"user","content":"What is the weather in Paris?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the current weather","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"}},"required":["location"]}}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`
	headers := map[string]string{
		"Content-Type": "application/json",
		"X-Project":    codexGatewayProject,
	}

	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if respBody, done := attemptChatPost(t, ctx, h, []byte(body), headers, attempt, &backoff); done {
			return respBody
		}
	}

	t.Fatalf("exhausted %d attempts for tool-call request", maxAttempts)
	return nil
}

// toolCallCompletion is the slice of the Chat Completions response used for tool-call assertions.
type toolCallCompletion struct {
	Object  string `json:"object"` // Object must be "chat.completion".
	Choices []struct {
		Message struct {
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"` // Name is the function being called.
				} `json:"function"`
			} `json:"tool_calls"` // ToolCalls holds the function calls requested by the model.
		} `json:"message"`
		FinishReason string `json:"finish_reason"` // FinishReason should be "tool_calls".
	} `json:"choices"`
}

// assertToolCallResponse checks that the response is a valid chat.completion with at least one
// tool call for the expected function name and finish_reason "tool_calls".
func assertToolCallResponse(t *testing.T, body []byte) {
	t.Helper()

	var parsed toolCallCompletion
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, body)
	}
	if parsed.Object != "chat.completion" {
		t.Errorf("object = %q, want %q", parsed.Object, "chat.completion")
	}
	if len(parsed.Choices) == 0 {
		t.Fatalf("no choices in response: %s", body)
	}

	choice := parsed.Choices[0]
	if len(choice.Message.ToolCalls) == 0 {
		t.Fatalf("no tool_calls in choices[0].message: %s", body)
	}
	if name := choice.Message.ToolCalls[0].Function.Name; name != "get_weather" {
		t.Errorf("tool_calls[0].function.name = %q, want %q", name, "get_weather")
	}
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want %q", choice.FinishReason, "tool_calls")
	}
	t.Logf("codex tool-call 200: function=%q finish_reason=%q",
		choice.Message.ToolCalls[0].Function.Name, choice.FinishReason)
}
