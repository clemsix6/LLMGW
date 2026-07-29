package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestRecoverInterrupted catches a recovery mutation that leaves pre-start requests in flight.
func TestRecoverInterrupted(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)
	project, keyID := createAdmissionProject(t, ctx, store, "recover-interrupted")

	generationOne := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		now.Add(-time.Minute), governance.RequestInFlight, governance.AccountingPending,
	)
	generationTwo := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		now.Add(-time.Minute), governance.RequestInFlight, governance.AccountingPending,
	)
	metadata := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationMetadata,
		now.Add(-time.Minute), governance.RequestInFlight, governance.AccountingNotApplicable,
	)

	updated, err := store.RecoverInterrupted(ctx, now)
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if updated != 3 {
		t.Fatalf("RecoverInterrupted updated = %d, want 3", updated)
	}
	assertRequestLifecycle(t, ctx, store, generationOne, governance.RequestCompleted, governance.AccountingUnknown, now)
	assertRequestLifecycle(t, ctx, store, generationTwo, governance.RequestCompleted, governance.AccountingUnknown, now)
	assertRequestLifecycle(t, ctx, store, metadata, governance.RequestCompleted, governance.AccountingNotApplicable, now)
}

// TestReconcileAccounting catches an accounting sweep that resolves the wrong pending requests.
func TestReconcileAccounting(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)
	project, keyID := createAdmissionProject(t, ctx, store, "reconcile-accounting")

	observed := completedPendingRequest(t, ctx, store, project.ID, keyID, now.Add(-31*time.Second))
	seedUsageAttempt(t, ctx, store, observed, 12, 0, floatPointer(1), governance.PricingPriced, now.Add(-30*time.Second))
	unknown := completedPendingRequest(t, ctx, store, project.ID, keyID, now.Add(-31*time.Second))
	recent := completedPendingRequest(t, ctx, store, project.ID, keyID, now.Add(-29*time.Second))
	stale := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		now.Add(-6*time.Hour-time.Second), governance.RequestInFlight, governance.AccountingPending,
	)

	result, err := store.ReconcileAccounting(ctx, now, 30*time.Second, 6*time.Hour)
	if err != nil {
		t.Fatalf("ReconcileAccounting: %v", err)
	}
	if result != (governance.ReconcileResult{Observed: 1, Unknown: 2}) {
		t.Fatalf("ReconcileAccounting result = %#v, want observed=1 unknown=2", result)
	}
	assertRequestLifecycle(t, ctx, store, observed, governance.RequestCompleted, governance.AccountingObserved, now.Add(-31*time.Second))
	assertRequestLifecycle(t, ctx, store, unknown, governance.RequestCompleted, governance.AccountingUnknown, now.Add(-31*time.Second))
	assertRequestLifecycle(t, ctx, store, stale, governance.RequestCompleted, governance.AccountingUnknown, now)
	assertRequestLifecycle(t, ctx, store, recent, governance.RequestCompleted, governance.AccountingPending, now.Add(-29*time.Second))

	for _, requestID := range []string{unknown, stale} {
		attempt := completeAttempt(requestID, clientKeyPublicID(t, ctx, store, keyID), nextAdmissionUUID(), now)
		if err := store.RecordAttempt(ctx, attempt); err != nil {
			t.Fatalf("RecordAttempt(%s): %v", requestID, err)
		}
		assertAttemptParent(t, ctx, store, requestID, governance.AccountingObserved, attempt.RequestedAlias, false)
	}
}

// TestReconcileAccountingCompletesStaleObserved catches a stale sweep that handles only pending rows without attempts.
func TestReconcileAccountingCompletesStaleObserved(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)
	project, keyID := createAdmissionProject(t, ctx, store, "reconcile-stale-observed")

	pendingWithAttempt := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		now.Add(-6*time.Hour-time.Second), governance.RequestInFlight, governance.AccountingPending,
	)
	seedUsageAttempt(
		t, ctx, store, pendingWithAttempt, 11, 0,
		floatPointer(1.25), governance.PricingPriced, now.Add(-time.Hour),
	)
	observedWithAttempt := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		now.Add(-6*time.Hour-time.Second), governance.RequestInFlight, governance.AccountingObserved,
	)
	seedUsageAttempt(
		t, ctx, store, observedWithAttempt, 13, 0,
		floatPointer(1.5), governance.PricingPriced, now.Add(-time.Hour),
	)

	result, err := store.ReconcileAccounting(ctx, now, 30*time.Second, 6*time.Hour)
	if err != nil {
		t.Fatalf("ReconcileAccounting: %v", err)
	}
	if result != (governance.ReconcileResult{Observed: 2}) {
		t.Fatalf("ReconcileAccounting result = %#v, want observed=2", result)
	}
	assertRequestLifecycle(
		t, ctx, store, pendingWithAttempt,
		governance.RequestCompleted, governance.AccountingObserved, now,
	)
	assertRequestLifecycle(
		t, ctx, store, observedWithAttempt,
		governance.RequestCompleted, governance.AccountingObserved, now,
	)
}

// TestReconcileAccountingRecordAttemptInterleaving catches a stale sweep that overwrites or strands a concurrent attempt.
func TestReconcileAccountingRecordAttemptInterleaving(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)
	project, keyID := createAdmissionProject(t, ctx, store, "reconcile-stale-race")
	requestID := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		now.Add(-6*time.Hour-time.Second), governance.RequestInFlight, governance.AccountingPending,
	)
	installObservedUpdateBarrier(t, ctx, store)
	releaseBarrier := holdObservedUpdateBarrier(t, ctx, store)

	attempt := completeAttempt(
		requestID,
		clientKeyPublicID(t, ctx, store, keyID),
		nextAdmissionUUID(),
		now,
	)
	attemptDone := make(chan error, 1)
	go func() {
		attemptDone <- store.RecordAttempt(ctx, attempt)
	}()
	waitForDatabaseWait(t, ctx, store, "advisory")

	reconcileDone := make(chan reconcileOutcome, 1)
	go func() {
		result, err := store.ReconcileAccounting(ctx, now, 30*time.Second, 6*time.Hour)
		reconcileDone <- reconcileOutcome{result: result, err: err}
	}()
	waitForDatabaseWait(t, ctx, store, "transactionid")

	releaseBarrier()
	if err := waitForAttempt(t, attemptDone); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	outcome := waitForReconcile(t, reconcileDone)
	if outcome.err != nil {
		t.Fatalf("ReconcileAccounting: %v", outcome.err)
	}
	if outcome.result != (governance.ReconcileResult{Observed: 1}) {
		t.Fatalf("ReconcileAccounting result = %#v, want observed=1", outcome.result)
	}
	assertRequestLifecycle(
		t, ctx, store, requestID,
		governance.RequestCompleted, governance.AccountingObserved, now,
	)
	assertAttemptCount(t, ctx, store, requestID, 1)
}

// TestResolveUnknownAsZero catches a resolution that changes a non-unknown request.
func TestResolveUnknownAsZero(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)
	project, keyID := createAdmissionProject(t, ctx, store, "resolve-unknown")

	unknown := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		now.Add(-time.Hour), governance.RequestCompleted, governance.AccountingUnknown,
	)
	if err := store.ResolveUnknownAsZero(ctx, unknown, now); err != nil {
		t.Fatalf("ResolveUnknownAsZero: %v", err)
	}
	assertResolvedZero(t, ctx, store, unknown, now)
	mustSetBudget(t, ctx, store, project.Name, governance.DimensionCalls, governance.WindowHour, 1, governance.ActionBlock)
	admission, err := store.AdmitGeneration(ctx, generationEvent(project.ID, keyID, now), now)
	if err != nil {
		t.Fatalf("AdmitGeneration after zero resolution: %v", err)
	}
	if admission.Allowed {
		t.Fatal("resolved-zero request was not counted as one call")
	}

	for _, rejected := range []struct {
		operation  governance.Operation
		accounting governance.AccountingState
	}{
		{operation: governance.OperationGeneration, accounting: governance.AccountingPending},
		{operation: governance.OperationGeneration, accounting: governance.AccountingObserved},
		{operation: governance.OperationMetadata, accounting: governance.AccountingNotApplicable},
	} {
		requestID := seedRequestEvent(
			t, ctx, store, project.ID, keyID, rejected.operation,
			now, governance.RequestCompleted, rejected.accounting,
		)
		if err := store.ResolveUnknownAsZero(ctx, requestID, now); err == nil {
			t.Fatalf(
				"ResolveUnknownAsZero(%s, %s) succeeded, want rejection",
				rejected.operation,
				rejected.accounting,
			)
		}
	}
	if err := store.ResolveUnknownAsZero(ctx, nextAdmissionUUID(), now); err == nil {
		t.Fatal("ResolveUnknownAsZero(unknown UUID) succeeded, want rejection")
	}
}

// TestResolveUnknownAsZeroPreservesAttempts catches a resolution that rejects or mutates durable attempt evidence.
func TestResolveUnknownAsZeroPreservesAttempts(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)
	project, keyID := createAdmissionProject(t, ctx, store, "resolve-unknown-attempt")
	requestID := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		now.Add(-time.Hour), governance.RequestCompleted, governance.AccountingUnknown,
	)
	seedUsageAttempt(
		t, ctx, store, requestID, 17, 3,
		floatPointer(2.5), governance.PricingPriced, now.Add(-time.Minute),
	)
	before := usageAttemptSnapshot(t, ctx, store, requestID)

	if err := store.ResolveUnknownAsZero(ctx, requestID, now); err != nil {
		t.Fatalf("ResolveUnknownAsZero with durable attempt: %v", err)
	}

	assertResolvedZero(t, ctx, store, requestID, now)
	after := usageAttemptSnapshot(t, ctx, store, requestID)
	if after != before {
		t.Fatalf("usage attempt changed during zero resolution:\nbefore=%s\nafter=%s", before, after)
	}
}

// completedPendingRequest creates a completed generation that is awaiting accounting evidence.
func completedPendingRequest(
	t *testing.T,
	ctx context.Context,
	store *Store,
	projectID int64,
	keyID int64,
	completedAt time.Time,
) string {
	t.Helper()
	event := generationEvent(projectID, keyID, completedAt.Add(-time.Second))
	event.State = governance.RequestCompleted
	event.CompletedAt = &completedAt
	seedRequest(t, ctx, store, event)
	return event.ID
}

// assertRequestLifecycle verifies a request lifecycle state without using repository helpers.
func assertRequestLifecycle(
	t *testing.T,
	ctx context.Context,
	store *Store,
	requestID string,
	wantState governance.RequestState,
	wantAccounting governance.AccountingState,
	wantCompleted time.Time,
) {
	t.Helper()
	var state governance.RequestState
	var accounting governance.AccountingState
	var completedAt time.Time
	if err := store.pool.QueryRow(
		ctx,
		`SELECT state, accounting_state, completed_at FROM request_event WHERE id = $1`,
		requestID,
	).Scan(&state, &accounting, &completedAt); err != nil {
		t.Fatalf("read request lifecycle: %v", err)
	}
	if state != wantState || accounting != wantAccounting || !completedAt.Equal(wantCompleted) {
		t.Fatalf(
			"request lifecycle = (%s, %s, %v), want (%s, %s, %v)",
			state,
			accounting,
			completedAt,
			wantState,
			wantAccounting,
			wantCompleted,
		)
	}
}

// assertResolvedZero verifies an explicit zero resolution and its audit timestamp.
func assertResolvedZero(t *testing.T, ctx context.Context, store *Store, requestID string, resolvedAt time.Time) {
	t.Helper()
	var accounting governance.AccountingState
	var storedResolvedAt time.Time
	if err := store.pool.QueryRow(
		ctx,
		`SELECT accounting_state, accounting_resolved_at FROM request_event WHERE id = $1`,
		requestID,
	).Scan(&accounting, &storedResolvedAt); err != nil {
		t.Fatalf("read zero resolution: %v", err)
	}
	if accounting != governance.AccountingResolvedZero || !storedResolvedAt.Equal(resolvedAt) {
		t.Fatalf("zero resolution = (%s, %v), want (%s, %v)", accounting, storedResolvedAt, governance.AccountingResolvedZero, resolvedAt)
	}
}
