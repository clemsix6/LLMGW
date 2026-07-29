package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestCallsBudgetUnderLogicalConcurrency catches non-atomic admission at the capacity gate.
func TestCallsBudgetUnderLogicalConcurrency(t *testing.T) {
	created := testHarness.createKey(t, "budget-calls-concurrent")
	testHarness.setBudget(t, created, governance.DimensionCalls, 5, governance.ActionBlock)

	const clients = 50
	successes, blocked, capacityRetries := exerciseCapacityBudgetClients(t, created, clients)
	if successes != 5 || blocked != clients-5 {
		t.Fatalf("logical results = success:%d blocked:%d, want 5/%d", successes, blocked, clients-5)
	}
	if capacityRetries != clients-2 {
		t.Fatal("concurrent logical clients never exercised the capacity 503")
	}
	awaitUsageAttempts(t, created.Key.ProjectID, 5)
	if got := requestCount(t, created, governance.OperationGeneration, "/v1/chat/completions"); got != 5 {
		t.Fatalf("persisted admitted calls = %d, want 5", got)
	}
}

// TestOneFailoverConsumesOneCall catches attempt-count based call accounting.
func TestOneFailoverConsumesOneCall(t *testing.T) {
	created := testHarness.createKey(t, "budget-call-failover")
	testHarness.setBudget(t, created, governance.DimensionCalls, 1, governance.ActionBlock)
	testHarness.Upstream.Enqueue(
		StubResponse{
			Status:  http.StatusServiceUnavailable,
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    `{"error":{"message":"transient-failover-fixture","type":"server_error"}}`,
		},
		jsonUsageResponse(4, 2),
	)
	status, _ := authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusOK {
		t.Fatalf("failover status = %d, want 200", status)
	}
	awaitUsageAttempts(t, created.Key.ProjectID, 2)
	status, _ = authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusPaymentRequired {
		t.Fatalf("next call status = %d, want 402", status)
	}
	if got := requestCount(t, created, governance.OperationGeneration, ""); got != 1 {
		t.Fatalf("failover request rows = %d, want one call", got)
	}
}

// TestObservedTokensAndCostBlockOnlyTheNextRequest catches preemptive crossing blocks.
func TestObservedTokensAndCostBlockOnlyTheNextRequest(t *testing.T) {
	t.Run("tokens sum attempts", func(t *testing.T) {
		created := testHarness.createKey(t, "budget-tokens")
		assertMultiAttemptCrossing(t, created, governance.DimensionTokens, 8)
	})

	t.Run("cost sums priced attempts", func(t *testing.T) {
		created := testHarness.createKey(t, "budget-cost")
		assertMultiAttemptCrossing(t, created, governance.DimensionCost, 0.00002)
	})
}

// TestConcurrentAcceptedRequestsMayOvershoot catches serialized post-usage admission.
func TestConcurrentAcceptedRequestsMayOvershoot(t *testing.T) {
	tests := []struct {
		name      string
		dimension governance.Dimension
		maximum   float64
		price     bool
	}{
		{name: "tokens", dimension: governance.DimensionTokens, maximum: 1},
		{name: "cost", dimension: governance.DimensionCost, maximum: 0.000001, price: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			created := testHarness.createKey(t, "budget-overshoot-"+test.name)
			if test.price {
				seedIntegrationPrice(t)
			}
			testHarness.setBudget(t, created, test.dimension, test.maximum, governance.ActionBlock)
			started := []chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)}
			release := make(chan struct{})
			for index := range 2 {
				response := jsonUsageResponse(3, 2)
				response.Started = started[index]
				response.Release = release
				testHarness.Upstream.Enqueue(response)
			}
			results := make(chan budgetHTTPResult, 2)
			for range 2 {
				startGenerationWorker(results, created.Plaintext, "test-model")
			}
			awaitSignal(t, started[0], "first overshoot request did not reach upstream")
			awaitSignal(t, started[1], "second overshoot request did not reach upstream")
			close(release)
			for range 2 {
				result := awaitBudgetHTTPResult(t, results)
				if result.err != nil || result.status != http.StatusOK {
					t.Fatalf("concurrent crossing result = %d/%v, want 200", result.status, result.err)
				}
			}
			awaitUsageAttempts(t, created.Key.ProjectID, 2)
			status, _ := authenticatedGeneration(t, created.Plaintext, "test-model")
			if status != http.StatusPaymentRequired {
				t.Fatalf("post-overshoot status = %d, want 402", status)
			}
		})
	}
}

// TestUnknownPricingBlocksOnlyAnActiveCostLimit catches global unpriced fail-closed behavior.
func TestUnknownPricingBlocksOnlyAnActiveCostLimit(t *testing.T) {
	unlimited := testHarness.createKey(t, "budget-unpriced-unlimited")
	testHarness.Upstream.Enqueue(jsonUsageResponseForModel("unpriced-upstream-model", 2, 1))
	status, _ := authenticatedGeneration(t, unlimited.Plaintext, "unpriced-model")
	if status != http.StatusOK {
		t.Fatalf("unpriced unlimited status = %d, want 200", status)
	}
	awaitUsageAttempts(t, unlimited.Key.ProjectID, 1)
	status, _ = authenticatedGeneration(t, unlimited.Plaintext, "unpriced-model")
	if status != http.StatusOK {
		t.Fatalf("unpriced unlimited next status = %d, want 200", status)
	}
	awaitUsageAttempts(t, unlimited.Key.ProjectID, 2)

	limited := testHarness.createKey(t, "budget-unpriced-limited")
	testHarness.setBudget(t, limited, governance.DimensionCost, 10, governance.ActionBlock)
	testHarness.Upstream.Enqueue(jsonUsageResponseForModel("unpriced-upstream-model", 2, 1))
	status, _ = authenticatedGeneration(t, limited.Plaintext, "unpriced-model")
	if status != http.StatusOK {
		t.Fatalf("unpriced crossing status = %d, want 200", status)
	}
	awaitUsageAttempts(t, limited.Key.ProjectID, 1)
	status, _ = authenticatedGeneration(t, limited.Plaintext, "unpriced-model")
	if status != http.StatusPaymentRequired {
		t.Fatalf("unpriced active-cost status = %d, want 402", status)
	}
}

// TestAccountingUnknownBlocksTokenCostButNotCalls catches dimension-independent uncertainty.
func TestAccountingUnknownBlocksTokenCostButNotCalls(t *testing.T) {
	for _, dimension := range []governance.Dimension{
		governance.DimensionTokens,
		governance.DimensionCost,
		governance.DimensionCalls,
	} {
		t.Run(string(dimension), func(t *testing.T) {
			created := testHarness.createKey(t, "budget-unknown-"+string(dimension))
			testHarness.insertUnknownAccounting(t, created)
			maximum := 100.0
			if dimension == governance.DimensionCalls {
				maximum = 2
			}
			testHarness.setBudget(t, created, dimension, maximum, governance.ActionBlock)
			status, _ := authenticatedGeneration(t, created.Plaintext, "test-model")
			if dimension == governance.DimensionCalls {
				if status != http.StatusOK {
					t.Fatalf("calls-only unknown status = %d, want 200", status)
				}
				awaitUsageAttempts(t, created.Key.ProjectID, 1)
				return
			}
			if status != http.StatusPaymentRequired {
				t.Fatalf("%s unknown status = %d, want 402", dimension, status)
			}
		})
	}
}

// TestWarnBudgetNeverChangesSDKResponse catches warn-as-block mutations.
func TestWarnBudgetNeverChangesSDKResponse(t *testing.T) {
	created := testHarness.createKey(t, "budget-warn")
	testHarness.setBudget(t, created, governance.DimensionCalls, 0, governance.ActionWarn)
	testHarness.Upstream.Enqueue(jsonUsageResponse(2, 1))
	status, body := authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusOK {
		t.Fatalf("warn status = %d, want SDK 200", status)
	}
	assertOpenAIChatShape(t, decodeJSON(t, body))
	awaitUsageAttempts(t, created.Key.ProjectID, 1)
}

type capacityEvent struct {
	id     int              // id identifies one of the fifty logical clients.
	result budgetHTTPResult // result is one bounded transport attempt.
}

// exerciseCapacityBudgetClients retries only the proven capacity-rejected first wave.
func exerciseCapacityBudgetClients(
	t *testing.T,
	created governance.CreatedKey,
	clients int,
) (int, int, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	statusBefore := len(testHarness.Upstream.ResponseStatuses())
	release, releaseOnce := make(chan struct{}), &sync.Once{}
	events, initial, done := make(chan capacityEvent, clients), make(chan capacityEvent, clients), make(chan struct{}, clients)
	defer finishCapacityWorkers(t, cancel, releaseOnce, release, done, clients)

	started := []chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)}
	for id := range 2 {
		response := jsonUsageResponse(1, 1)
		response.Started, response.Release = started[id], release
		testHarness.Upstream.Enqueue(response)
		startBudgetClient(ctx, id, created.Plaintext, nil, nil, events, nil, done)
	}
	awaitSignal(t, started[0], "first capacity holder did not reach upstream")
	awaitSignal(t, started[1], "second capacity holder did not reach upstream")
	rowsBefore := requestCount(t, created, governance.OperationGeneration, "")
	upstreamBefore := testHarness.Upstream.RequestCount()

	retry := make([]chan struct{}, clients)
	start := make(chan struct{})
	for id := 2; id < clients; id++ {
		retry[id] = make(chan struct{}, 1)
		startBudgetClient(ctx, id, created.Plaintext, start, retry[id], initial, events, done)
	}
	close(start)
	for range clients - 2 {
		assertCapacityRejection(t, awaitCapacityEvent(t, initial).result)
	}
	if err := testHarness.db.Ping(ctx); err != nil {
		t.Fatal("database unhealthy during capacity rejection")
	}
	if testHarness.Upstream.RequestCount() != upstreamBefore ||
		requestCount(t, created, governance.OperationGeneration, "") != rowsBefore {
		t.Fatal("capacity-rejected transport attempt reached persistence or upstream")
	}

	releaseOnce.Do(func() { close(release) })
	successes, blocked := consumeCapacityEvents(t, events, 2)
	awaitUsageAttempts(t, created.Key.ProjectID, successes)
	for id := 2; id < clients; id += 2 {
		retry[id] <- struct{}{}
		retry[id+1] <- struct{}{}
		ok, denied := consumeCapacityEvents(t, events, 2)
		successes, blocked = successes+ok, blocked+denied
		if ok > 0 {
			awaitUsageAttempts(t, created.Key.ProjectID, successes)
		}
	}
	assertNoUpstream503(t, statusBefore)
	return successes, blocked, clients - 2
}

// startBudgetClient runs either one holder request or one proven-capacity retry.
func startBudgetClient(
	ctx context.Context,
	id int,
	plaintext string,
	start <-chan struct{},
	retry <-chan struct{},
	initial chan<- capacityEvent,
	terminal chan<- capacityEvent,
	done chan<- struct{},
) {
	go func() {
		defer func() { done <- struct{}{} }()
		if start != nil {
			select {
			case <-start:
			case <-ctx.Done():
				return
			}
		}
		initial <- capacityEvent{id: id, result: generationHTTPResult(ctx, plaintext, "test-model")}
		if retry == nil {
			return
		}
		select {
		case <-retry:
			terminal <- capacityEvent{id: id, result: generationHTTPResult(ctx, plaintext, "test-model")}
		case <-ctx.Done():
		}
	}()
}

func awaitCapacityEvent(t *testing.T, events <-chan capacityEvent) capacityEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("capacity client did not return")
		return capacityEvent{}
	}
}

func assertCapacityRejection(t *testing.T, result budgetHTTPResult) {
	t.Helper()
	var envelope map[string]map[string]string
	if result.err != nil || result.status != http.StatusServiceUnavailable ||
		json.Unmarshal(result.body, &envelope) != nil ||
		envelope["error"]["type"] != "service_unavailable" {
		t.Fatalf("saturated capacity result = %d/%v, want controlled 503", result.status, result.err)
	}
}

func consumeCapacityEvents(t *testing.T, events <-chan capacityEvent, count int) (int, int) {
	t.Helper()
	successes, blocked := 0, 0
	for range count {
		result := awaitCapacityEvent(t, events).result
		if result.err != nil {
			t.Fatalf("capacity client transport failed: %v", result.err)
		}
		switch result.status {
		case http.StatusOK:
			successes++
		case http.StatusPaymentRequired:
			blocked++
		default:
			t.Fatalf("capacity client terminal status = %d", result.status)
		}
	}
	return successes, blocked
}

func finishCapacityWorkers(
	t *testing.T,
	cancel context.CancelFunc,
	releaseOnce *sync.Once,
	release chan struct{},
	done <-chan struct{},
	count int,
) {
	t.Helper()
	releaseOnce.Do(func() { close(release) })
	cancel()
	for range count {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("capacity client worker did not terminate")
			return
		}
	}
}

func assertNoUpstream503(t *testing.T, start int) {
	t.Helper()
	for _, status := range testHarness.Upstream.ResponseStatuses()[start:] {
		if status == http.StatusServiceUnavailable {
			t.Fatal("capacity test upstream emitted 503")
		}
	}
}

func awaitBudgetHTTPResult(t *testing.T, results <-chan budgetHTTPResult) budgetHTTPResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("generation worker did not return")
		return budgetHTTPResult{}
	}
}

// assertMultiAttemptCrossing proves two priced attempts combine before the next admission.
func assertMultiAttemptCrossing(
	t *testing.T,
	created governance.CreatedKey,
	dimension governance.Dimension,
	maximum float64,
) {
	t.Helper()
	seedMultiAttemptPrices(t)
	testHarness.setBudget(t, created, dimension, maximum, governance.ActionBlock)
	testHarness.Upstream.Enqueue(multiAttemptUsageResponse())
	status, _ := authenticatedMultiAttemptGeneration(t, created.Plaintext)
	if status != http.StatusOK {
		t.Fatalf("multi-attempt crossing status = %d, want 200", status)
	}
	rows := awaitUsageAttempts(t, created.Key.ProjectID, 2)
	assertMultiAttemptTotals(t, rows)
	status, _ = authenticatedMultiAttemptGeneration(t, created.Plaintext)
	if status != http.StatusPaymentRequired {
		t.Fatalf("post-crossing status = %d, want 402", status)
	}
	if got := requestCount(t, created, governance.OperationGeneration, "/v1/responses"); got != 1 {
		t.Fatalf("multi-attempt request rows = %d, want 1", got)
	}
}

func assertMultiAttemptTotals(t *testing.T, rows []integrationAttempt) {
	t.Helper()
	var tokens int64
	var cost float64
	for _, row := range rows {
		if row.TotalTokens <= 0 || row.CostUSD == nil || *row.CostUSD <= 0 {
			t.Fatal("multi-attempt row omitted non-zero priced usage")
		}
		tokens, cost = tokens+row.TotalTokens, cost+*row.CostUSD
	}
	const wantCost = 31.0 / 1_000_000
	if tokens != 10 || cost < wantCost-1e-12 || cost > wantCost+1e-12 {
		t.Fatalf("multi-attempt totals = %d/%v, want 10/%v", tokens, cost, wantCost)
	}
}

func authenticatedMultiAttemptGeneration(t *testing.T, plaintext string) (int, []byte) {
	t.Helper()
	body := `{"model":"multi-attempt-model","input":"fixture-prompt","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`
	return gatewayRequest(t, http.MethodPost, "/v1/responses", bytes.NewBufferString(body),
		requestHeaders{authorization: "Bearer " + plaintext})
}

func multiAttemptUsageResponse() StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:    `data: {"type":"response.completed","response":{"id":"resp-multi","object":"response","created_at":1,"status":"completed","model":"codex-upstream-model","output":[],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7},"tool_usage":{"image_gen":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}}` + "\n\n",
	}
}

func seedMultiAttemptPrices(t *testing.T) {
	t.Helper()
	const query = `
INSERT INTO model_price (
    provider, model_pattern, service_tier, input_per_million,
    output_per_million, effective_from
) VALUES
    ('codex', 'codex-upstream-model', '*', 2, 4, '1970-01-01'),
    ('codex', 'gpt-image-2', '*', 3, 5, '1970-01-01')
ON CONFLICT DO NOTHING`
	if _, err := testHarness.db.Exec(context.Background(), query); err != nil {
		t.Fatal("seed multi-attempt integration prices failed")
	}
}

func authenticatedGeneration(t *testing.T, plaintext string, model string) (int, []byte) {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"fixture-prompt"}]}`
	return gatewayRequest(
		t,
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(body),
		requestHeaders{authorization: "Bearer " + plaintext},
	)
}

// setBudget installs one rolling hourly project limit.
func (h *Harness) setBudget(
	t *testing.T,
	created governance.CreatedKey,
	dimension governance.Dimension,
	maximum float64,
	action governance.Action,
) {
	t.Helper()
	if _, err := h.Store.SetBudget(
		context.Background(),
		created.Key.ProjectName,
		dimension,
		governance.WindowHour,
		maximum,
		action,
	); err != nil {
		t.Fatal("set integration budget failed")
	}
}

// insertUnknownAccounting seeds one completed generation with missing usage.
func (h *Harness) insertUnknownAccounting(t *testing.T, created governance.CreatedKey) {
	t.Helper()
	const query = `
INSERT INTO request_event (
    id, project_id, client_key_id, operation, requested_at, completed_at,
    method, path, state, accounting_state, downstream_status
) VALUES (
    gen_random_uuid(), $1, $2, 'generation', now(), now(),
    'POST', '/v1/chat/completions', 'completed', 'accounting_unknown', 200
)`
	if _, err := h.db.Exec(
		context.Background(),
		query,
		created.Key.ProjectID,
		created.Key.ID,
	); err != nil {
		t.Fatal("insert unknown integration accounting failed")
	}
}

// createKey creates one isolated project key without logging its plaintext.
func (h *Harness) createKey(t *testing.T, label string) governance.CreatedKey {
	t.Helper()
	project := fmt.Sprintf("%s-%d", label, h.keyID.Add(1))
	created, err := h.Keys.Create(context.Background(), project, "client", nil)
	if err != nil {
		t.Fatal("create integration project key failed")
	}
	h.registerSecrets(created.Plaintext)
	return created
}

// createExpiredKey creates one key whose authentication deadline has passed.
func (h *Harness) createExpiredKey(t *testing.T, label string) governance.CreatedKey {
	t.Helper()
	project := fmt.Sprintf("%s-%d", label, h.keyID.Add(1))
	expiredAt := time.Now().Add(-time.Minute)
	created, err := h.Keys.Create(context.Background(), project, "client", &expiredAt)
	if err != nil {
		t.Fatal("create expired integration project key failed")
	}
	h.registerSecrets(created.Plaintext)
	return created
}
