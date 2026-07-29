package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

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
