package postgres

import (
	"context"
	"testing"
	"time"
)

// TestRecoverInterruptedClassifiesFromDurableAttempts verifies that startup
// recovery reads the durable attempt set instead of declaring every interrupted
// generation unknown. A generation whose usage already landed must recover as
// observed: accounting_unknown blocks token and cost budgets by design, so
// misclassifying observed usage locks those budgets for the rest of the window
// after every restart with traffic in flight.
func TestRecoverInterruptedClassifiesFromDurableAttempts(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	requestedAt := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)
	recoveredAt := requestedAt.Add(time.Minute)

	key, err := store.CreateKey(ctx, "recover-project", "recover-key", "pk-recover", make([]byte, 32), nil)
	if err != nil {
		t.Fatalf("create project key: %v", err)
	}

	const observedID = "00000000-0000-0000-0000-000000000001"
	const unknownID = "00000000-0000-0000-0000-000000000002"
	const metadataID = "00000000-0000-0000-0000-000000000003"
	insertInFlightRequest(t, ctx, store, observedID, key.ProjectID, key.ID, "generation", requestedAt)
	insertInFlightRequest(t, ctx, store, unknownID, key.ProjectID, key.ID, "generation", requestedAt)
	insertInFlightRequest(t, ctx, store, metadataID, key.ProjectID, key.ID, "metadata", requestedAt)
	insertDurableAttempt(t, ctx, store, "00000000-0000-0000-0000-00000000000a", observedID, requestedAt)

	recovered, err := store.RecoverInterrupted(ctx, recoveredAt)
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if recovered != 3 {
		t.Fatalf("RecoverInterrupted recovered %d requests, want 3", recovered)
	}

	assertAccountingState(t, ctx, store, observedID, "observed")
	assertAccountingState(t, ctx, store, unknownID, "accounting_unknown")
	assertAccountingState(t, ctx, store, metadataID, "not_applicable")
}

// insertInFlightRequest inserts one request_event fixture left in flight.
func insertInFlightRequest(
	t *testing.T,
	ctx context.Context,
	store *Store,
	requestID string,
	projectID int64,
	clientKeyID int64,
	operation string,
	requestedAt time.Time,
) {
	t.Helper()
	const query = `
INSERT INTO request_event (
    id, project_id, client_key_id, operation, requested_at,
    method, path, state, accounting_state
) VALUES ($1, $2, $3, $4, $5, 'POST', '/v1/messages', 'in_flight', 'pending')`
	if _, err := store.pool.Exec(ctx, query, requestID, projectID, clientKeyID, operation, requestedAt); err != nil {
		t.Fatalf("insert in-flight request %q: %v", requestID, err)
	}
}

// insertDurableAttempt inserts one persisted usage attempt for a request.
func insertDurableAttempt(
	t *testing.T,
	ctx context.Context,
	store *Store,
	attemptID string,
	requestID string,
	createdAt time.Time,
) {
	t.Helper()
	const query = `
INSERT INTO usage_attempt (
    id, request_id, provider, executor_type, resolved_model, requested_alias,
    upstream_auth_id, upstream_auth_type, input_tokens, output_tokens,
    reasoning_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
    unclassified_tokens, service_tier, response_service_tier, failed,
    latency_ms, ttft_ms, pricing_state, created_at
) VALUES ($1, $2, 'claude', 'executor', 'model', 'alias',
          'auth', 'oauth', 10, 5, 0, 0, 0, 15, 0, 'standard', 'standard', false,
          100, 50, 'priced', $3)`
	if _, err := store.pool.Exec(ctx, query, attemptID, requestID, createdAt); err != nil {
		t.Fatalf("insert usage attempt %q: %v", attemptID, err)
	}
}

// assertAccountingState fails unless the request completed with this state.
func assertAccountingState(
	t *testing.T,
	ctx context.Context,
	store *Store,
	requestID string,
	want string,
) {
	t.Helper()
	const query = `SELECT state, accounting_state FROM request_event WHERE id = $1`
	var state, accounting string
	if err := store.pool.QueryRow(ctx, query, requestID).Scan(&state, &accounting); err != nil {
		t.Fatalf("read request %q: %v", requestID, err)
	}
	if state != "completed" || accounting != want {
		t.Fatalf("request %q recovered as (%s, %s), want (completed, %s)", requestID, state, accounting, want)
	}
}
