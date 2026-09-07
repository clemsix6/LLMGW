package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

const (
	// settledAccountingDelay separates the fixtures these tests settle from the
	// live traffic every other test in the package leaves behind: reconciliation
	// is global, so the delay keeps everything younger than it untouched.
	settledAccountingDelay = 15 * time.Minute
	// settledFixtureAge dates a fixture past that delay while keeping it inside
	// the rolling hour a project budget is evaluated over.
	settledFixtureAge = 20 * time.Minute
)

// probeRequests are the discovery probes model-listing clients send at a
// gateway. None of them is a route LLMGW serves.
var probeRequests = []struct {
	method string
	path   string
}{
	{http.MethodPost, "/api/show"},
	{http.MethodGet, "/api/tags"},
	{http.MethodGet, "/api/v1/models"},
	{http.MethodGet, "/models"},
	{http.MethodGet, "/props"},
	{http.MethodGet, "/v1/props"},
	{http.MethodGet, "/v1/models/claude-opus-5"},
	{http.MethodGet, "/version"},
}

// TestRejectedProbeIsNotUnresolvedAccounting proves a request the gateway
// answers from its own router leaves no accounting behind at all, and so
// cannot block a token budget. It reached no provider, so it owes nothing, and
// counting it as unresolved used to block every token and cost budget of the
// project that merely asked which models exist.
func TestRejectedProbeIsNotUnresolvedAccounting(t *testing.T) {
	created := testHarness.createKey(t, "probe-rejected")
	for _, probe := range probeRequests {
		status, _ := gatewayRequest(t, probe.method, probe.path, nil, requestHeaders{
			authorization: "Bearer " + created.Plaintext,
		})
		if status != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404", probe.method, probe.path, status)
		}
	}

	if got := requestCount(t, created, governance.OperationGeneration, ""); got != 0 {
		t.Fatalf("probe generation rows = %d, want none", got)
	}
	if got := requestCount(t, created, governance.OperationMetadata, ""); got != 0 {
		t.Fatalf("probe metadata rows = %d, want none", got)
	}

	testHarness.settleAccounting(t)
	if got := testHarness.unknownAccounting(t, created.Key.ProjectID); got != 0 {
		t.Fatalf("probe unresolved accounting = %d, want none", got)
	}

	testHarness.setBudget(t, created, governance.DimensionTokens, 1000, governance.ActionBlock)
	testHarness.Upstream.Enqueue(jsonUsageResponse(4, 2))
	status, _ := authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusOK {
		t.Fatalf("generation after probes status = %d, want 200", status)
	}
	awaitUsageAttempts(t, created.Key.ProjectID, 1)
}

// TestRecordlessGenerationStaysUnresolvedAccounting is the other half of the
// pair: a generation that did reach the SDK and produced no usage record is
// still unresolved accounting, and still blocks a token budget. That is what
// accounting_unknown is for, and a fix that quieted it here would be trading
// one wrong answer for a worse one.
func TestRecordlessGenerationStaysUnresolvedAccounting(t *testing.T) {
	created := testHarness.createKey(t, "probe-recordless")
	testHarness.insertRecordlessGeneration(t, created)

	testHarness.settleAccounting(t)
	if got := testHarness.unknownAccounting(t, created.Key.ProjectID); got != 1 {
		t.Fatalf("recordless unresolved accounting = %d, want 1", got)
	}

	testHarness.setBudget(t, created, governance.DimensionTokens, 1000, governance.ActionBlock)
	status, _ := authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusPaymentRequired {
		t.Fatalf("generation after recordless usage status = %d, want 402", status)
	}
}

// settleAccounting runs the production reconciliation over fixtures old enough
// to have settled, leaving every live request the package produced alone.
func (h *Harness) settleAccounting(t *testing.T) {
	t.Helper()
	if _, err := h.Store.ReconcileAccounting(
		context.Background(),
		time.Now().UTC(),
		settledAccountingDelay,
		24*time.Hour,
	); err != nil {
		t.Fatal("reconcile integration accounting failed")
	}
}

// insertRecordlessGeneration seeds one settled generation the SDK answered
// without ever reporting usage for it.
func (h *Harness) insertRecordlessGeneration(t *testing.T, created governance.CreatedKey) {
	t.Helper()
	const query = `
INSERT INTO request_event (
    id, project_id, client_key_id, operation, requested_at, completed_at,
    method, path, state, accounting_state, downstream_status
) VALUES (
    gen_random_uuid(), $1, $2, 'generation', $3, $3,
    'POST', '/v1/messages', 'completed', 'pending', 200
)`
	settled := time.Now().UTC().Add(-settledFixtureAge)
	if _, err := h.db.Exec(context.Background(), query, created.Key.ProjectID, created.Key.ID, settled); err != nil {
		t.Fatal("insert recordless integration generation failed")
	}
}

// unknownAccounting counts one project's unresolved generation requests.
func (h *Harness) unknownAccounting(t *testing.T, projectID int64) int64 {
	t.Helper()
	const query = `
SELECT count(*)
FROM request_event
WHERE project_id = $1 AND accounting_state = 'accounting_unknown'`
	var count int64
	if err := h.db.QueryRow(context.Background(), query, projectID).Scan(&count); err != nil {
		t.Fatal("count integration unresolved accounting failed")
	}
	return count
}
