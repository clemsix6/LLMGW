package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	upstreamFailureSecret = "upstream-failure-body-secret"
	upstreamHeaderSecret  = "upstream-response-header-secret"
	fixtureToolSecret     = "fixture-tool-payload-secret"
	runtimeAccountSecret  = "runtime-added-account-secret"
)

// TestAccountModelCooldownScope catches global account or cross-model cooldown mutations.
func TestAccountModelCooldownScope(t *testing.T) {
	created := testHarness.createKey(t, "pool-cooldown-scope")
	assertNoGovernanceCooldownState(t)
	assertResetCooldownScope(t, created.Plaintext)
	assertNoResetCooldownBehavior(t, created.Plaintext)
	assertNoGovernanceCooldownState(t)
}

// TestAccountPoolFailover catches scheduler changes that retry one failed credential.
func TestAccountPoolFailover(t *testing.T) {
	created := testHarness.createKey(t, "pool-failover")
	before := len(testHarness.Upstream.Authorizations())
	testHarness.Upstream.Enqueue(StubResponse{
		Status:  http.StatusServiceUnavailable,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    `{"error":{"message":"` + upstreamFailureSecret + `","type":"server_error"}}`,
	}, jsonUsageResponse(2, 1), jsonUsageResponse(1, 1))
	status, _ := generationWithToolFixture(t, created.Plaintext, false)
	if status != http.StatusOK {
		t.Fatalf("failover status = %d, want 200", status)
	}
	authorizations := testHarness.Upstream.Authorizations()[before:]
	if len(authorizations) != 2 || authorizations[0] == authorizations[1] {
		t.Fatal("503 did not fail over to a distinct account")
	}
	status, _ = generationWithToolFixture(t, created.Plaintext, false)
	if status != http.StatusOK {
		t.Fatalf("post-503 eligibility status = %d, want 200", status)
	}
	next := testHarness.Upstream.Authorizations()
	if next[len(next)-1] != authorizations[0] {
		t.Fatal("503 account remained quarantined despite disabled transient cooldown")
	}
	awaitUsageAttempts(t, created.Key.ProjectID, 3)
	testHarness.assertSecretsAbsent(
		t,
		created.Plaintext,
		upstreamFailureSecret,
		upstreamHeaderSecret,
		fixtureToolSecret,
		testHarness.Upstream.URL(),
	)
}

// TestAccountPoolNetworkCloseFailsOver catches loss of transport-error retry.
func TestAccountPoolNetworkCloseFailsOver(t *testing.T) {
	created := testHarness.createKey(t, "pool-network")
	before := len(testHarness.Upstream.Authorizations())
	testHarness.Upstream.Enqueue(
		StubResponse{CloseConnection: true},
		jsonUsageResponse(2, 1),
	)
	status, _ := authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusOK {
		t.Fatalf("network-close failover status = %d, want 200", status)
	}
	authorizations := testHarness.Upstream.Authorizations()[before:]
	if len(authorizations) != 2 || authorizations[0] == authorizations[1] {
		t.Fatal("network close did not fail over to a distinct account")
	}
	awaitUsageAttempts(t, created.Key.ProjectID, 2)
}

// TestRuntimeAuthMutationIsIgnored catches re-enabling unsafe auth-directory reload.
func TestRuntimeAuthMutationIsIgnored(t *testing.T) {
	created := testHarness.createKey(t, "pool-auth-reload")
	path := filepath.Join(testHarness.AuthDir, "runtime-added.json")
	payload := validRuntimeAuthFixture(testHarness.Upstream.URL())
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal("write ignored runtime auth fixture failed")
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	assertRuntimeAuthNotSelected(t, created.Plaintext)
	testHarness.assertSecretsAbsent(t, runtimeAccountSecret)
}

// TestStreamFailureBoundaries catches replay after payload and lost pre-payload failover.
func TestStreamFailureBoundaries(t *testing.T) {
	created := testHarness.createKey(t, "pool-stream")
	t.Run("before first payload may fail over", func(t *testing.T) {
		before := testHarness.Upstream.RequestCount()
		testHarness.Upstream.Enqueue(
			StubResponse{
				Status:  http.StatusServiceUnavailable,
				Headers: http.Header{"Content-Type": []string{"application/json"}},
				Body:    `{"error":{"message":"` + upstreamFailureSecret + `","type":"server_error"}}`,
			},
			streamingUsageResponse(2, 1),
		)
		status, body := generationWithToolFixture(t, created.Plaintext, true)
		if status != http.StatusOK || !bytes.Contains(body, []byte("data: [DONE]")) {
			t.Fatalf("pre-payload failover status/body = %d/%s", status, safeBodySummary(body))
		}
		if got := testHarness.Upstream.RequestCount() - before; got != 2 {
			t.Fatalf("pre-payload upstream attempts = %d, want 2", got)
		}
	})

	t.Run("after first payload never replays", func(t *testing.T) {
		before := testHarness.Upstream.RequestCount()
		testHarness.Upstream.Enqueue(StubResponse{
			Status:  http.StatusOK,
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Chunks: []StubChunk{{
				Body: `data: {"id":"chatcmpl-partial","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n",
			}},
			CloseAfterChunks: true,
		})
		status, body := generationWithToolFixture(t, created.Plaintext, true)
		if status != http.StatusOK || len(body) == 0 {
			t.Fatalf("post-payload stream status/body = %d/%s", status, safeBodySummary(body))
		}
		if got := testHarness.Upstream.RequestCount() - before; got != 1 {
			t.Fatalf("post-payload upstream attempts = %d, want no replay", got)
		}
	})
	testHarness.assertSecretsAbsent(t, created.Plaintext, fixtureToolSecret, upstreamFailureSecret)
}

// TestAllAccountsCoolingReturnsPinnedReset catches loss of scheduler cooldown signaling.
func TestAllAccountsCoolingReturnsPinnedReset(t *testing.T) {
	created := testHarness.createKey(t, "pool-all-cooling")
	cooling := StubResponse{
		Status: http.StatusTooManyRequests,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"1"},
		},
		Body: `{"error":{"message":"cooling-fixture","type":"rate_limit_error"}}`,
	}
	testHarness.Upstream.Enqueue(cooling, cooling)
	response := rawGenerationResponse(t, created.Plaintext)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("all-cooling status = %d, want 429", response.StatusCode)
	}
	before := testHarness.Upstream.RequestCount()
	response = rawGenerationResponse(t, created.Plaintext)
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf(
			"all-cooling response = status:%d body:%s",
			response.StatusCode,
			safeBodySummary(body),
		)
	}
	object := decodeJSON(t, body)
	errorObject, ok := object["error"].(map[string]any)
	if !ok || errorObject["code"] != "model_cooldown" {
		t.Fatalf("all-cooling error = %#v, want model_cooldown", object["error"])
	}
	resetSeconds, ok := errorObject["reset_seconds"].(float64)
	if !ok || resetSeconds <= 0 {
		t.Fatalf("all-cooling reset_seconds = %#v, want positive", errorObject["reset_seconds"])
	}
	if got := testHarness.Upstream.RequestCount(); got != before {
		t.Fatalf("all-cooling selection contacted upstream %d times", got-before)
	}
	time.Sleep(1100 * time.Millisecond)
}

func assertResetCooldownScope(t *testing.T, plaintext string) {
	t.Helper()
	before := len(testHarness.Upstream.Authorizations())
	testHarness.Upstream.Enqueue(
		codexRateLimit(`,"resets_in_seconds":5`),
		codexUsageResponse("codex-cooldown-model"),
		codexUsageResponse("codex-cooldown-model"),
		codexUsageResponse("codex-other-model"),
	)
	for _, model := range []string{"cooldown-model", "cooldown-model", "cooldown-other-model"} {
		if status, _ := codexGeneration(t, plaintext, model); status != http.StatusOK {
			t.Fatalf("%s reset-scope status = %d, want 200", model, status)
		}
	}
	auths := testHarness.Upstream.Authorizations()[before:]
	if len(auths) != 4 || auths[0] == auths[1] {
		t.Fatalf("reset-scope attempts = %d distinct=%t, want four with failover", len(auths), len(auths) > 1 && auths[0] != auths[1])
	}
	if auths[2] != auths[1] {
		t.Fatal("reset-bearing 429 account was immediately reused for the same model")
	}
	if auths[3] != auths[0] {
		t.Fatal("model cooldown incorrectly quarantined the account for another model")
	}
}

func assertNoResetCooldownBehavior(t *testing.T, plaintext string) {
	t.Helper()
	before := len(testHarness.Upstream.Authorizations())
	testHarness.Upstream.Enqueue(
		codexRateLimit(""),
		codexUsageResponse("codex-no-reset-model"),
		codexUsageResponse("codex-no-reset-model"),
	)
	if status, _ := codexGeneration(t, plaintext, "cooldown-no-reset-model"); status != http.StatusOK {
		t.Fatalf("no-reset failover status = %d, want 200", status)
	}
	auths := testHarness.Upstream.Authorizations()[before:]
	if len(auths) != 2 || auths[0] == auths[1] {
		t.Fatal("no-reset 429 did not fail over to a distinct account")
	}
	failed, survivor := auths[0], auths[1]
	if status, _ := codexGeneration(t, plaintext, "cooldown-no-reset-model"); status != http.StatusOK {
		t.Fatalf("no-reset immediate retry status = %d, want 200", status)
	}
	if got := lastAuthorization(t); got != survivor {
		t.Fatal("no-reset 429 account was immediately reused")
	}

	deadline := time.Now().Add(3 * time.Second)
	for lastAuthorization(t) != failed {
		if !time.Now().Before(deadline) {
			t.Fatal("no-reset 429 account did not become eligible within bounded fallback")
		}
		testHarness.Upstream.Enqueue(codexUsageResponse("codex-no-reset-model"))
		status, _ := codexGenerationAwaitingCapacity(t, plaintext, "cooldown-no-reset-model", deadline)
		if status != http.StatusOK {
			t.Fatalf("no-reset eligibility probe status = %d, want 200", status)
		}
		select {
		case <-time.After(25 * time.Millisecond):
		case <-time.After(time.Until(deadline)):
			t.Fatal("no-reset cooldown eligibility timed out")
		}
	}
}

func assertNoGovernanceCooldownState(t *testing.T) {
	t.Helper()
	const columnsQuery = `
SELECT table_name, column_name
FROM information_schema.columns
WHERE table_schema = 'public'
  AND (table_name ILIKE '%cooldown%' OR column_name ILIKE '%cooldown%'
       OR column_name ILIKE '%retry_after%')
ORDER BY table_name, column_name`
	rows, err := testHarness.db.Query(context.Background(), columnsQuery)
	if err != nil {
		t.Fatal("audit governance cooldown schema failed")
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal("scan governance cooldown schema failed")
		}
		found = append(found, table+"."+column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal("read governance cooldown schema failed")
	}
	if strings.Join(found, ",") != "oauth_token.cooldown_until" {
		t.Fatalf("governance cooldown schema = %v, want only legacy column", found)
	}
	var legacyRows int
	if err := testHarness.db.QueryRow(
		context.Background(),
		`SELECT count(*) FROM oauth_token WHERE cooldown_until IS NOT NULL`,
	).Scan(&legacyRows); err != nil {
		t.Fatal("audit legacy cooldown rows failed")
	}
	if legacyRows != 0 {
		t.Fatalf("legacy governance cooldown rows changed: %d non-null", legacyRows)
	}
}

func codexRateLimit(extra string) StubResponse {
	return StubResponse{
		Status: http.StatusTooManyRequests,
		Headers: http.Header{
			"Content-Type":     []string{"application/json"},
			"X-Secret-Fixture": []string{upstreamHeaderSecret},
		},
		Body: `{"error":{"type":"usage_limit_reached","message":"` +
			upstreamFailureSecret + `"` + extra + `}}`,
	}
}

func codexUsageResponse(model string) StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: `data: {"type":"response.completed","response":{"id":"resp-cooldown",` +
			`"object":"response","created_at":1,"status":"completed","model":"` + model +
			`","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
	}
}

func codexGeneration(t *testing.T, plaintext, model string) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": model, "input": "fixture-prompt"})
	if err != nil {
		t.Fatal("marshal Codex request failed")
	}
	return gatewayRequest(t, http.MethodPost, "/v1/responses", bytes.NewReader(body),
		requestHeaders{authorization: "Bearer " + plaintext})
}

// codexGenerationAwaitingCapacity retries a generation that the gateway refused
// for usage backpressure. The harness deliberately runs a capacity of two, and a
// group stays admitted until its usage callback lands, so a probe issued right
// after a previous response can legitimately find no free permit. Such a refusal
// never reaches the upstream, so the queued stub response stays untouched.
func codexGenerationAwaitingCapacity(
	t *testing.T,
	plaintext, model string,
	deadline time.Time,
) (int, []byte) {
	t.Helper()
	for {
		status, body := codexGeneration(t, plaintext, model)
		if status != http.StatusServiceUnavailable || !time.Now().Before(deadline) {
			return status, body
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lastAuthorization(t *testing.T) string {
	t.Helper()
	auths := testHarness.Upstream.Authorizations()
	if len(auths) == 0 {
		t.Fatal("stub captured no authorization")
	}
	return auths[len(auths)-1]
}

func validRuntimeAuthFixture(baseURL string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type":         "xai",
		"access_token": runtimeAccountSecret,
		"base_url":     baseURL,
		"model_aliases": []map[string]any{{
			"name": "grok-4", "alias": "runtime-added-model", "force-mapping": true,
		}},
	})
	return payload
}

func assertRuntimeAuthNotSelected(t *testing.T, plaintext string) {
	t.Helper()
	before := testHarness.Upstream.RequestCount()
	for range 3 {
		status, _ := codexGeneration(t, plaintext, "runtime-added-model")
		if status == http.StatusOK {
			t.Fatal("fully valid runtime-added auth became selectable without restart")
		}
	}
	if got := testHarness.Upstream.RequestCount(); got != before {
		t.Fatalf("ignored runtime auth contacted its hermetic upstream %d times", got-before)
	}
	for _, authorization := range testHarness.Upstream.Authorizations() {
		if strings.Contains(authorization, runtimeAccountSecret) {
			t.Fatal("ignored runtime credential was selected")
		}
	}
}

func rawGenerationResponse(t *testing.T, plaintext string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		testHarness.BaseURL+"/v1/chat/completions",
		bytes.NewBufferString(chatFixture),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+plaintext)
	response, err := testHarness.client.Do(request)
	if err != nil {
		t.Fatal("send raw generation request failed")
	}
	return response
}

func generationWithToolFixture(t *testing.T, plaintext string, stream bool) (int, []byte) {
	t.Helper()
	body := `{
		"model":"test-model",
		"stream":` + boolString(stream) + `,
		"messages":[{"role":"user","content":"fixture-prompt"}],
		"tools":[{"type":"function","function":{"name":"lookup","description":"` + fixtureToolSecret + `","parameters":{"type":"object"}}}]
	}`
	return gatewayRequest(
		t,
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(body),
		requestHeaders{authorization: "Bearer " + plaintext},
	)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func safeBodySummary(body []byte) string {
	const maximum = 160
	body = bytes.ReplaceAll(body, []byte("fixture-prompt"), []byte("[redacted]"))
	if len(body) > maximum {
		body = body[:maximum]
	}
	return string(body)
}
