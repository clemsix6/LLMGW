package postgres

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var admissionTestID atomic.Uint64

// newGovernanceStore starts PostgreSQL 16, applies every migration, and returns a ready Store.
func newGovernanceStore(t *testing.T) *Store {
	t.Helper()

	ctx := context.Background()
	dsn := startGovernancePostgres(t, ctx)

	store, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("open governance store: %v", err)
	}
	t.Cleanup(store.Close)

	return store
}

// startGovernancePostgres starts an ephemeral PostgreSQL 16 instance and returns its DSN.
func startGovernancePostgres(t *testing.T, ctx context.Context) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("llmgw"),
		tcpostgres.WithUsername("llmgw"),
		tcpostgres.WithPassword("llmgw"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start governance postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate governance postgres: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("governance postgres connection string: %v", err)
	}

	return dsn
}

// createAdmissionProject creates one persisted project and client key for request tests.
func createAdmissionProject(
	t *testing.T,
	ctx context.Context,
	store *Store,
	label string,
) (governance.Project, int64) {
	t.Helper()

	suffix := admissionTestID.Add(1)
	name := fmt.Sprintf("%s-%d", label, suffix)
	key, err := store.CreateKey(
		ctx,
		name,
		"test",
		fmt.Sprintf("llmgw_admission_%d", suffix),
		make([]byte, 32),
		nil,
	)
	if err != nil {
		t.Fatalf("CreateKey(%q): %v", name, err)
	}
	return governance.Project{ID: key.ProjectID, Name: name}, key.ID
}

// generationEvent returns one complete in-memory generation request.
func generationEvent(projectID, keyID int64, requestedAt time.Time) governance.RequestEvent {
	requestedModel := "test-model"
	return governance.RequestEvent{
		ID:              nextAdmissionUUID(),
		ProjectID:       projectID,
		ClientKeyID:     keyID,
		Operation:       governance.OperationGeneration,
		RequestedAt:     requestedAt,
		Method:          "POST",
		Path:            "/v1/messages",
		RequestedModel:  &requestedModel,
		State:           governance.RequestInFlight,
		AccountingState: governance.AccountingPending,
	}
}

// nextAdmissionUUID returns a unique valid UUID string for concurrent test requests.
func nextAdmissionUUID() string {
	value := admissionTestID.Add(1)
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", value)
}

// admitForTest admits one generated request and fails the current test on repository errors.
func admitForTest(
	t *testing.T,
	ctx context.Context,
	store *Store,
	projectID int64,
	keyID int64,
	now time.Time,
) governance.Admission {
	t.Helper()

	admission, err := store.AdmitGeneration(ctx, generationEvent(projectID, keyID, now), now)
	if err != nil {
		t.Fatalf("AdmitGeneration: %v", err)
	}
	return admission
}

// seedRequestEvent inserts a request fixture and returns its UUID.
func seedRequestEvent(
	t *testing.T,
	ctx context.Context,
	store *Store,
	projectID int64,
	keyID int64,
	operation governance.Operation,
	requestedAt time.Time,
	state governance.RequestState,
	accounting governance.AccountingState,
) string {
	t.Helper()

	event := generationEvent(projectID, keyID, requestedAt)
	event.Operation = operation
	event.State = state
	event.AccountingState = accounting
	seedRequest(t, ctx, store, event)
	return event.ID
}

// seedRequest inserts an exact request fixture through PostgreSQL.
func seedRequest(t *testing.T, ctx context.Context, store *Store, event governance.RequestEvent) {
	t.Helper()

	const query = `
INSERT INTO request_event (
    id, project_id, client_key_id, operation, requested_at, completed_at,
    method, path, requested_model, state, accounting_state, downstream_status,
    accounting_resolved_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	if _, err := store.pool.Exec(
		ctx,
		query,
		event.ID,
		event.ProjectID,
		event.ClientKeyID,
		event.Operation,
		event.RequestedAt,
		event.CompletedAt,
		event.Method,
		event.Path,
		event.RequestedModel,
		event.State,
		event.AccountingState,
		event.DownstreamStatus,
		event.AccountingResolvedAt,
	); err != nil {
		t.Fatalf("seed request: %v", err)
	}
}

// seedUsageAttempt inserts an exact usage fixture through PostgreSQL.
func seedUsageAttempt(
	t *testing.T,
	ctx context.Context,
	store *Store,
	requestID string,
	totalTokens int64,
	reasoningTokens int64,
	cost *float64,
	pricing governance.PricingState,
	createdAt time.Time,
) {
	t.Helper()

	const query = `
INSERT INTO usage_attempt (
    id, request_id, provider, executor_type, resolved_model, requested_alias,
    upstream_auth_id, upstream_auth_type, input_tokens, output_tokens,
    reasoning_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
    unclassified_tokens, service_tier, response_service_tier, failed,
    latency_ms, ttft_ms, cost_usd, pricing_state, created_at
) VALUES (
    $1, $2, 'test', 'test', 'test-model', 'test-alias', 'test-auth',
    'test-auth-type', 0, 0, $3, 0, 0, $4, 0, 'standard', 'standard',
    false, 0, 0, $5, $6, $7
)`
	if _, err := store.pool.Exec(
		ctx,
		query,
		nextAdmissionUUID(),
		requestID,
		reasoningTokens,
		totalTokens,
		cost,
		pricing,
		createdAt,
	); err != nil {
		t.Fatalf("seed usage attempt: %v", err)
	}
}

// floatPointer returns a pointer to a literal fixture cost.
func floatPointer(value float64) *float64 {
	return &value
}

// mustSetBudget persists one limit and fails the current test on error.
func mustSetBudget(
	t *testing.T,
	ctx context.Context,
	store *Store,
	project string,
	dimension governance.Dimension,
	window governance.Window,
	maximum float64,
	action governance.Action,
) governance.BudgetLimit {
	t.Helper()

	limit, err := store.SetBudget(ctx, project, dimension, window, maximum, action)
	if err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	return limit
}

// assertSingleBreach verifies one breach's dimension, window, and reset time.
func assertSingleBreach(
	t *testing.T,
	breaches []governance.BudgetBreach,
	dimension governance.Dimension,
	window governance.Window,
	resetAt time.Time,
) {
	t.Helper()

	if len(breaches) != 1 {
		t.Fatalf("breaches = %#v, want one", breaches)
	}
	got := breaches[0]
	if got.Limit.Dimension != dimension || got.Limit.Window != window || !got.ResetAt.Equal(resetAt) {
		t.Fatalf("breach = %#v, want dimension=%s window=%s reset=%v", got, dimension, window, resetAt)
	}
}

// assertRequestCount verifies the exact number of project request rows.
func assertRequestCount(t *testing.T, ctx context.Context, store *Store, projectID int64, want int64) {
	t.Helper()

	var got int64
	if err := store.pool.QueryRow(
		ctx,
		`SELECT count(*) FROM request_event WHERE project_id = $1`,
		projectID,
	).Scan(&got); err != nil {
		t.Fatalf("count request events: %v", err)
	}
	if got != want {
		t.Fatalf("request event count = %d, want %d", got, want)
	}
}

// assertAdvisoryWait verifies a concurrent admission is waiting in PostgreSQL on an advisory lock.
func assertAdvisoryWait(t *testing.T, ctx context.Context, store *Store, finished <-chan error) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-finished:
			t.Fatalf("same-project admission escaped held advisory lock: %v", err)
		default:
		}

		var waiting bool
		const query = `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity
    WHERE wait_event_type = 'Lock'
      AND wait_event = 'advisory'
      AND query LIKE 'SELECT pg_advisory_xact_lock%'
)`
		if err := store.pool.QueryRow(ctx, query).Scan(&waiting); err != nil {
			t.Fatalf("inspect advisory lock wait: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("same-project admission did not enter a PostgreSQL advisory lock wait")
}

// assertMetadataCompleted verifies metadata normalization and completion values.
func assertMetadataCompleted(
	t *testing.T,
	ctx context.Context,
	store *Store,
	requestID string,
	completedAt time.Time,
) {
	t.Helper()

	var operation governance.Operation
	var state governance.RequestState
	var accounting governance.AccountingState
	var storedCompleted time.Time
	var status int
	const query = `
SELECT operation, state, accounting_state, completed_at, downstream_status
FROM request_event WHERE id = $1`
	if err := store.pool.QueryRow(
		ctx,
		query,
		requestID,
	).Scan(&operation, &state, &accounting, &storedCompleted, &status); err != nil {
		t.Fatalf("read completed metadata: %v", err)
	}
	if operation != governance.OperationMetadata ||
		state != governance.RequestCompleted ||
		accounting != governance.AccountingNotApplicable ||
		!storedCompleted.Equal(completedAt) ||
		status != 204 {
		t.Fatalf(
			"completed metadata = (%s, %s, %s, %v, %d)",
			operation,
			state,
			accounting,
			storedCompleted,
			status,
		)
	}
}

// assertRequestUnchanged verifies completing another UUID did not update this row.
func assertRequestUnchanged(t *testing.T, ctx context.Context, store *Store, requestID string) {
	t.Helper()

	var state governance.RequestState
	var completedAt *time.Time
	var status *int
	if err := store.pool.QueryRow(
		ctx,
		`SELECT state, completed_at, downstream_status FROM request_event WHERE id = $1`,
		requestID,
	).Scan(&state, &completedAt, &status); err != nil {
		t.Fatalf("read unchanged request: %v", err)
	}
	if state != governance.RequestInFlight || completedAt != nil || status != nil {
		t.Fatalf("unrelated request changed: state=%s completed=%v status=%v", state, completedAt, status)
	}
}
