package integration

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestUsageSuccess verifies one real SDK success becomes one priced attempt.
func TestUsageSuccess(t *testing.T) {
	created := testHarness.createKey(t, "usage-success")
	seedIntegrationPrice(t)
	testHarness.Upstream.Enqueue(jsonUsageResponse(10, 4))

	status, _ := gatewayRequest(
		t,
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(chatFixture),
		requestHeaders{authorization: "Bearer " + created.Plaintext},
	)
	if status != http.StatusOK {
		t.Fatalf("usage success status = %d, want 200", status)
	}

	rows := awaitUsageAttempts(t, created.Key.ProjectID, 1)
	if len(rows) != 1 || rows[0].TotalTokens != 14 ||
		rows[0].InputTokens != 10 || rows[0].OutputTokens != 4 {
		t.Fatalf("success attempts = %#v", rows)
	}
	if rows[0].CreatedAt.IsZero() {
		t.Fatal("success attempt has zero SDK request time")
	}
	wantCost := (10*2.0 + 4*4.0) / 1_000_000
	if rows[0].CostUSD == nil || math.Abs(*rows[0].CostUSD-wantCost) > 1e-12 ||
		rows[0].PricingState != governance.PricingPriced {
		t.Fatalf("success price = (%v, %s), want %.12f priced", rows[0].CostUSD, rows[0].PricingState, wantCost)
	}
	assertUsageRequest(t, created, 1)
	testHarness.assertSecretsAbsent(t, created.Plaintext)
}

// TestRetryAttempts verifies account failover persists each SDK attempt under one call.
func TestRetryAttempts(t *testing.T) {
	created := testHarness.createKey(t, "usage-retry")
	seedIntegrationPrice(t)
	testHarness.Upstream.Enqueue(
		StubResponse{
			Status:  http.StatusTooManyRequests,
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    `{"error":{"message":"retry fixture","type":"rate_limit_error"}}`,
		},
		jsonUsageResponse(7, 3),
	)

	status, _ := gatewayRequest(
		t,
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(chatFixture),
		requestHeaders{authorization: "Bearer " + created.Plaintext},
	)
	if status != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", status)
	}

	rows := awaitUsageAttempts(t, created.Key.ProjectID, 2)
	failed, total := 0, int64(0)
	for _, row := range rows {
		if row.Failed {
			failed++
			if row.UpstreamStatus == nil || *row.UpstreamStatus != http.StatusTooManyRequests {
				t.Fatalf("failed attempt status = %v, want 429", row.UpstreamStatus)
			}
		}
		total += row.TotalTokens
	}
	if failed != 1 || total != 10 {
		t.Fatalf("retry attempts = %#v, want one failed and 10 total tokens", rows)
	}
	assertUsageRequest(t, created, 2)
	testHarness.assertSecretsAbsent(t, created.Plaintext)
}

// TestStreamingUsage verifies final SSE usage is durably normalized.
func TestStreamingUsage(t *testing.T) {
	created := testHarness.createKey(t, "usage-stream")
	seedIntegrationPrice(t)
	testHarness.Upstream.Enqueue(streamingUsageResponse(6, 2))
	fixture := `{
		"model":"test-model",
		"messages":[{"role":"user","content":"fixture-prompt"}],
		"stream":true,
		"stream_options":{"include_usage":true}
	}`

	status, _ := gatewayRequest(
		t,
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(fixture),
		requestHeaders{authorization: "Bearer " + created.Plaintext},
	)
	if status != http.StatusOK {
		t.Fatalf("streaming status = %d, want 200", status)
	}

	rows := awaitUsageAttempts(t, created.Key.ProjectID, 1)
	if len(rows) != 1 || rows[0].InputTokens != 6 ||
		rows[0].OutputTokens != 2 || rows[0].TotalTokens != 8 {
		t.Fatalf("streaming attempts = %#v", rows)
	}
	assertUsageRequest(t, created, 1)
	testHarness.assertSecretsAbsent(t, created.Plaintext)
}

// TestUsageUnpriced verifies real SDK usage survives with a null notional cost.
func TestUsageUnpriced(t *testing.T) {
	created := testHarness.createKey(t, "usage-unpriced")
	testHarness.Upstream.Enqueue(jsonUsageResponseForModel("unpriced-upstream-model", 5, 2))
	fixture := `{
		"model":"unpriced-model",
		"messages":[{"role":"user","content":"fixture-prompt"}]
	}`

	status, _ := gatewayRequest(
		t,
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(fixture),
		requestHeaders{authorization: "Bearer " + created.Plaintext},
	)
	if status != http.StatusOK {
		t.Fatalf("unpriced status = %d, want 200", status)
	}

	rows := awaitUsageAttempts(t, created.Key.ProjectID, 1)
	if len(rows) != 1 || rows[0].TotalTokens != 7 ||
		rows[0].CostUSD != nil || rows[0].PricingState != governance.PricingUnknown {
		t.Fatalf("unpriced attempts = %#v", rows)
	}
	assertUsageRequest(t, created, 1)
	testHarness.assertSecretsAbsent(t, created.Plaintext)
}

// TestConcurrentProjectUsageCorrelation proves the real SDK cannot cross-attach
// a delayed request A usage record to concurrent project B.
func TestConcurrentProjectUsageCorrelation(t *testing.T) {
	projectA := testHarness.createKey(t, "usage-concurrent-a")
	projectB := testHarness.createKey(t, "usage-concurrent-b")
	startedA := make(chan struct{}, 1)
	releaseA := make(chan struct{})
	responseA := jsonUsageResponse(11, 1)
	responseA.Started = startedA
	responseA.Release = releaseA
	testHarness.Upstream.Enqueue(responseA, jsonUsageResponse(22, 2))

	start := func(plaintext string) <-chan gatewayOutcome {
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
		result := make(chan gatewayOutcome, 1)
		go func() {
			response, err := testHarness.client.Do(request)
			if err != nil {
				result <- gatewayOutcome{err: err}
				return
			}
			_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			closeErr := response.Body.Close()
			if readErr != nil {
				result <- gatewayOutcome{err: readErr}
				return
			}
			if closeErr != nil {
				result <- gatewayOutcome{err: closeErr}
				return
			}
			result <- gatewayOutcome{status: response.StatusCode}
		}()
		return result
	}

	resultA := start(projectA.Plaintext)
	select {
	case <-startedA:
	case <-time.After(5 * time.Second):
		t.Fatal("delayed project A request did not reach upstream")
	}
	resultB := start(projectB.Plaintext)
	requireGatewayOutcome(t, resultB)
	close(releaseA)
	requireGatewayOutcome(t, resultA)

	attemptsA := awaitUsageAttempts(t, projectA.Key.ProjectID, 1)
	attemptsB := awaitUsageAttempts(t, projectB.Key.ProjectID, 1)
	if attemptsA[0].TotalTokens != 12 || attemptsB[0].TotalTokens != 24 {
		t.Fatalf(
			"concurrent totals = project A:%d project B:%d, want 12 and 24",
			attemptsA[0].TotalTokens,
			attemptsB[0].TotalTokens,
		)
	}
	assertUsageRequest(t, projectA, 1)
	assertUsageRequest(t, projectB, 1)
	testHarness.assertSecretsAbsent(t, projectA.Plaintext, projectB.Plaintext)
}

// TestUsageBackpressureBoundsRealSDKGroups blocks the real plugin repository,
// fills the configured two-group bridge, and proves excess traffic never
// reaches SDK execution or generation admission.
func TestUsageBackpressureBoundsRealSDKGroups(t *testing.T) {
	const capacity = 2
	created := testHarness.createKey(t, "usage-backpressure")
	entered, unblock := testHarness.UsageRepo.block()
	t.Cleanup(unblock)
	testHarness.Upstream.Enqueue(jsonUsageResponse(3, 1), jsonUsageResponse(4, 1))
	upstreamBefore := len(testHarness.Upstream.Authorizations())

	for index := 0; index < capacity; index++ {
		status, _ := gatewayRequest(
			t,
			http.MethodPost,
			"/v1/chat/completions",
			bytes.NewBufferString(chatFixture),
			requestHeaders{authorization: "Bearer " + created.Plaintext},
		)
		if status != http.StatusOK {
			t.Fatalf("admitted request %d status = %d, want 200", index, status)
		}
		if index == 0 {
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("usage repository did not block")
			}
		}
	}

	status, _ := gatewayRequest(
		t,
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(chatFixture),
		requestHeaders{authorization: "Bearer " + created.Plaintext},
	)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("excess generation status = %d, want 503", status)
	}
	if got := len(testHarness.Upstream.Authorizations()) - upstreamBefore; got != capacity {
		t.Fatalf("SDK upstream groups = %d, want exactly %d", got, capacity)
	}
	if got := requestCount(
		t,
		created,
		governance.OperationGeneration,
		"/v1/chat/completions",
	); got != capacity {
		t.Fatalf("admitted generation records = %d, want exactly %d", got, capacity)
	}

	unblock()
	rows := awaitUsageAttempts(t, created.Key.ProjectID, capacity)
	if len(rows) != capacity {
		t.Fatalf("persisted admitted attempts = %d, want %d", len(rows), capacity)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		status, _ = gatewayRequest(
			t,
			http.MethodPost,
			"/v1/chat/completions",
			bytes.NewBufferString(chatFixture),
			requestHeaders{authorization: "Bearer " + created.Plaintext},
		)
		if status == http.StatusOK {
			break
		}
		if status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("capacity return status = %d", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	awaitUsageAttempts(t, created.Key.ProjectID, capacity+1)
	if got := len(testHarness.Upstream.Authorizations()) - upstreamBefore; got != capacity+1 {
		t.Fatalf("SDK upstream groups after release = %d, want %d", got, capacity+1)
	}
}

func TestCanceledSSEKeepsCapacityUntilLateUsageIsDurable(t *testing.T) {
	const capacity = 2
	created := testHarness.createKey(t, "usage-canceled-stream")
	entered, unblock := testHarness.UsageRepo.block()
	t.Cleanup(unblock)
	upstreamBefore := len(testHarness.Upstream.Authorizations())

	for index := 0; index < capacity; index++ {
		release := make(chan struct{})
		flushed := make(chan struct{}, 1)
		testHarness.Upstream.Enqueue(StubResponse{
			Status:  http.StatusOK,
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Chunks: []StubChunk{{
				Body:    `data: {"id":"chatcmpl-canceled","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}` + "\n\n",
				Flushed: flushed,
				Release: release,
			}},
		})
		cancelStreamingRequestAfterFirstChunk(t, created.Plaintext, flushed)
		close(release)
	}

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("late canceled-stream usage did not reach durable persistence")
	}
	status, _ := gatewayRequest(
		t,
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(chatFixture),
		requestHeaders{authorization: "Bearer " + created.Plaintext},
	)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("capacity while late usage blocked = %d, want 503", status)
	}
	if got := len(testHarness.Upstream.Authorizations()) - upstreamBefore; got != capacity {
		t.Fatalf("upstream groups while late usage blocked = %d, want %d", got, capacity)
	}

	unblock()
	rows := awaitUsageAttempts(t, created.Key.ProjectID, capacity)
	if len(rows) != capacity {
		t.Fatalf("late canceled-stream attempts = %d, want %d", len(rows), capacity)
	}
	testHarness.Upstream.Enqueue(jsonUsageResponse(3, 1))
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, _ = gatewayRequest(
			t,
			http.MethodPost,
			"/v1/chat/completions",
			bytes.NewBufferString(chatFixture),
			requestHeaders{authorization: "Bearer " + created.Plaintext},
		)
		if status == http.StatusOK {
			break
		}
		if status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("capacity recovery after late usage status = %d", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	awaitUsageAttempts(t, created.Key.ProjectID, capacity+1)
}

func cancelStreamingRequestAfterFirstChunk(
	t *testing.T,
	plaintext string,
	upstreamFlushed <-chan struct{},
) {
	t.Helper()
	fixture := `{
		"model":"test-model",
		"messages":[{"role":"user","content":"fixture-prompt"}],
		"stream":true,
		"stream_options":{"include_usage":true}
	}`
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		testHarness.BaseURL+"/v1/chat/completions",
		bytes.NewBufferString(fixture),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+plaintext)
	response, err := testHarness.client.Do(request)
	if err != nil {
		t.Fatal("start canceled streaming request failed")
	}
	select {
	case <-upstreamFlushed:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not flush first canceled-stream chunk")
	}
	if _, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil {
		t.Fatal("read first canceled-stream chunk failed")
	}
	cancel()
	_ = response.Body.Close()
}

type gatewayOutcome struct {
	status int
	err    error
}

func requireGatewayOutcome(t *testing.T, result <-chan gatewayOutcome) {
	t.Helper()
	select {
	case outcome := <-result:
		if outcome.err != nil {
			t.Fatal("concurrent gateway request failed")
		}
		if outcome.status != http.StatusOK {
			t.Fatalf("concurrent gateway status = %d, want 200", outcome.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent gateway request timed out")
	}
}

// integrationAttempt is the safe database projection used by usage assertions.
type integrationAttempt struct {
	InputTokens    int64                   // InputTokens is canonical uncached input.
	OutputTokens   int64                   // OutputTokens includes reasoning exactly once.
	TotalTokens    int64                   // TotalTokens is the provider-reported total.
	Failed         bool                    // Failed identifies an upstream failed attempt.
	UpstreamStatus *int                    // UpstreamStatus is the optional provider status.
	CostUSD        *float64                // CostUSD is null when pricing is unknown.
	PricingState   governance.PricingState // PricingState identifies known or unknown pricing.
	CreatedAt      time.Time               // CreatedAt is the SDK attempt start time.
}

// seedIntegrationPrice installs deterministic notional pricing for the priced fixture.
func seedIntegrationPrice(t *testing.T) {
	t.Helper()
	const query = `
INSERT INTO model_price (
    provider, model_pattern, service_tier, input_per_million,
    output_per_million, cache_read_per_million,
    cache_creation_per_million, effective_from
) VALUES ('openai-compatible-integration', 'upstream-model', '*', 2, 4, 1, 1, '1970-01-01')
ON CONFLICT DO NOTHING`
	if _, err := testHarness.db.Exec(context.Background(), query); err != nil {
		t.Fatal("seed integration price failed")
	}
}

// awaitUsageAttempts polls PostgreSQL until the exact attempt count is durable.
func awaitUsageAttempts(t *testing.T, projectID int64, want int) []integrationAttempt {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows := usageAttempts(t, projectID)
		if len(rows) == want {
			return rows
		}
		if len(rows) > want {
			t.Fatalf("usage attempts = %d, want %d", len(rows), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("usage attempts did not reach %d", want)
	return nil
}

// usageAttempts returns safe normalized fields for one project's generation calls.
func usageAttempts(t *testing.T, projectID int64) []integrationAttempt {
	t.Helper()
	const query = `
SELECT a.input_tokens, a.output_tokens, a.total_tokens, a.failed,
       a.upstream_status, a.cost_usd, a.pricing_state, a.created_at
FROM usage_attempt a
JOIN request_event r ON r.id = a.request_id
WHERE r.project_id = $1 AND r.operation = 'generation'
ORDER BY a.created_at, a.id`
	rows, err := testHarness.db.Query(context.Background(), query, projectID)
	if err != nil {
		t.Fatal("query integration usage attempts failed")
	}
	defer rows.Close()

	var attempts []integrationAttempt
	for rows.Next() {
		var attempt integrationAttempt
		if err := rows.Scan(
			&attempt.InputTokens,
			&attempt.OutputTokens,
			&attempt.TotalTokens,
			&attempt.Failed,
			&attempt.UpstreamStatus,
			&attempt.CostUSD,
			&attempt.PricingState,
			&attempt.CreatedAt,
		); err != nil {
			t.Fatal("scan integration usage attempt failed")
		}
		attempts = append(attempts, attempt)
	}
	if rows.Err() != nil {
		t.Fatal("iterate integration usage attempts failed")
	}
	return attempts
}

// assertUsageRequest verifies one call and project/key correlation only through the parent.
func assertUsageRequest(t *testing.T, created governance.CreatedKey, wantAttempts int64) {
	t.Helper()
	const query = `
SELECT count(DISTINCT r.id), count(a.id),
       count(*) FILTER (WHERE r.client_key_id = $2 AND ck.public_id = $3),
       count(*) FILTER (
           WHERE a.provider IN ($3, $4) OR a.executor_type IN ($3, $4) OR
                 a.resolved_model IN ($3, $4) OR a.requested_alias IN ($3, $4) OR
                 a.upstream_auth_id IN ($3, $4) OR a.upstream_auth_type IN ($3, $4) OR
                 a.service_tier IN ($3, $4) OR a.response_service_tier IN ($3, $4)
       )
FROM request_event r
JOIN usage_attempt a ON a.request_id = r.id
JOIN client_key ck ON ck.id = r.client_key_id
WHERE r.project_id = $1 AND r.operation = 'generation'`
	var calls, attempts, auditableKey, leakedKeyMaterial int64
	if err := testHarness.db.QueryRow(
		context.Background(),
		query,
		created.Key.ProjectID,
		created.Key.ID,
		created.Key.PublicID,
		created.Plaintext,
	).Scan(&calls, &attempts, &auditableKey, &leakedKeyMaterial); err != nil {
		t.Fatal("assert integration request correlation failed")
	}
	if calls != 1 || attempts != wantAttempts ||
		auditableKey != wantAttempts || leakedKeyMaterial != 0 {
		t.Fatalf(
			"usage correlation = calls:%d attempts:%d key-fk:%d leaked-key-fields:%d",
			calls,
			attempts,
			auditableKey,
			leakedKeyMaterial,
		)
	}
}
