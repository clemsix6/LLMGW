package postgres

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestAdmissionRollingTotals verifies persisted generation usage and rolling reset boundaries.
func TestAdmissionRollingTotals(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)

	t.Run("generation lifecycle counts calls", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "call-lifecycle")
		first := now.Add(-50 * time.Minute)
		seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, first, governance.RequestInFlight, governance.AccountingPending)
		seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now.Add(-40*time.Minute), governance.RequestCompleted, governance.AccountingObserved)
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionCalls, governance.WindowHour, 2, governance.ActionBlock)

		admission := admitForTest(t, ctx, store, project.ID, keyID, now)

		assertSingleBreach(t, admission.Blocks, governance.DimensionCalls, governance.WindowHour, now.Add(10*time.Minute))
		assertRequestCount(t, ctx, store, project.ID, 2)
	})

	t.Run("metadata is unmetered", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "metadata-unmetered")
		seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationMetadata, now.Add(-10*time.Minute), governance.RequestInFlight, governance.AccountingNotApplicable)
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionCalls, governance.WindowHour, 1, governance.ActionBlock)

		admission := admitForTest(t, ctx, store, project.ID, keyID, now)

		if !admission.Allowed {
			t.Fatalf("admission = %#v, want metadata excluded from call total", admission)
		}
		assertRequestCount(t, ctx, store, project.ID, 2)

		t.Run("priced tokens", func(t *testing.T) {
			project, keyID := createAdmissionProject(t, ctx, store, "metadata-tokens")
			requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationMetadata, now.Add(-10*time.Minute), governance.RequestCompleted, governance.AccountingNotApplicable)
			seedUsageAttempt(t, ctx, store, requestID, 100, 0, floatPointer(0), governance.PricingPriced, now.Add(-5*time.Minute))
			mustSetBudget(t, ctx, store, project.Name, governance.DimensionTokens, governance.WindowHour, 100, governance.ActionBlock)

			admission := admitForTest(t, ctx, store, project.ID, keyID, now)
			if !admission.Allowed {
				t.Fatalf("admission = %#v, want metadata tokens excluded", admission)
			}
		})

		t.Run("priced cost", func(t *testing.T) {
			project, keyID := createAdmissionProject(t, ctx, store, "metadata-cost")
			requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationMetadata, now.Add(-10*time.Minute), governance.RequestCompleted, governance.AccountingNotApplicable)
			seedUsageAttempt(t, ctx, store, requestID, 0, 0, floatPointer(2), governance.PricingPriced, now.Add(-5*time.Minute))
			mustSetBudget(t, ctx, store, project.Name, governance.DimensionCost, governance.WindowHour, 2, governance.ActionBlock)

			admission := admitForTest(t, ctx, store, project.ID, keyID, now)
			if !admission.Allowed {
				t.Fatalf("admission = %#v, want metadata cost excluded", admission)
			}
		})

		t.Run("unknown pricing", func(t *testing.T) {
			project, keyID := createAdmissionProject(t, ctx, store, "metadata-unknown-pricing")
			requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationMetadata, now.Add(-10*time.Minute), governance.RequestCompleted, governance.AccountingNotApplicable)
			seedUsageAttempt(t, ctx, store, requestID, 0, 0, nil, governance.PricingUnknown, now.Add(-5*time.Minute))
			mustSetBudget(t, ctx, store, project.Name, governance.DimensionCost, governance.WindowHour, 100, governance.ActionBlock)

			admission := admitForTest(t, ctx, store, project.ID, keyID, now)
			if !admission.Allowed {
				t.Fatalf("admission = %#v, want metadata unknown pricing excluded", admission)
			}
		})
	})

	t.Run("token attempts sum", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "token-sum")
		requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now.Add(-30*time.Minute), governance.RequestCompleted, governance.AccountingObserved)
		first := now.Add(-20 * time.Minute)
		seedUsageAttempt(t, ctx, store, requestID, 40, 0, floatPointer(0.4), governance.PricingPriced, first)
		seedUsageAttempt(t, ctx, store, requestID, 60, 0, floatPointer(0.6), governance.PricingPriced, now.Add(-10*time.Minute))
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionTokens, governance.WindowHour, 100, governance.ActionBlock)

		admission := admitForTest(t, ctx, store, project.ID, keyID, now)

		assertSingleBreach(t, admission.Blocks, governance.DimensionTokens, governance.WindowHour, now.Add(40*time.Minute))
	})

	t.Run("reasoning is already included in total tokens", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "reasoning-in-total")
		requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now.Add(-30*time.Minute), governance.RequestCompleted, governance.AccountingObserved)
		seedUsageAttempt(t, ctx, store, requestID, 100, 40, floatPointer(1), governance.PricingPriced, now.Add(-10*time.Minute))
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionTokens, governance.WindowHour, 120, governance.ActionBlock)

		admission := admitForTest(t, ctx, store, project.ID, keyID, now)
		if !admission.Allowed {
			t.Fatalf("admission = %#v, want reasoning excluded from total-token addition", admission)
		}
	})

	t.Run("cost sums priced attempts", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "priced-cost")
		requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now.Add(-40*time.Minute), governance.RequestCompleted, governance.AccountingObserved)
		first := now.Add(-25 * time.Minute)
		seedUsageAttempt(t, ctx, store, requestID, 10, 0, floatPointer(1), governance.PricingPriced, first)
		seedUsageAttempt(t, ctx, store, requestID, 20, 0, floatPointer(1.5), governance.PricingPriced, now.Add(-15*time.Minute))
		seedUsageAttempt(t, ctx, store, requestID, 999, 0, nil, governance.PricingUnknown, now.Add(-61*time.Minute))
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionCost, governance.WindowHour, 2.5, governance.ActionBlock)

		admission := admitForTest(t, ctx, store, project.ID, keyID, now)

		assertSingleBreach(t, admission.Blocks, governance.DimensionCost, governance.WindowHour, now.Add(35*time.Minute))
	})

	t.Run("unknown accounting is window scoped", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "unknown-accounting")
		unknownAt := now.Add(-23 * time.Hour)
		seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, unknownAt, governance.RequestCompleted, governance.AccountingUnknown)
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionTokens, governance.WindowHour, 100, governance.ActionBlock)
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionTokens, governance.WindowDay, 100, governance.ActionBlock)

		admission := admitForTest(t, ctx, store, project.ID, keyID, now)

		assertSingleBreach(t, admission.Blocks, governance.DimensionTokens, governance.WindowDay, now.Add(time.Hour))
	})

	t.Run("unknown pricing is window scoped", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "unknown-pricing")
		requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now.Add(-23*time.Hour), governance.RequestCompleted, governance.AccountingObserved)
		unknownAt := now.Add(-23 * time.Hour)
		seedUsageAttempt(t, ctx, store, requestID, 50, 0, nil, governance.PricingUnknown, unknownAt)
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionCost, governance.WindowHour, 100, governance.ActionBlock)
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionCost, governance.WindowDay, 100, governance.ActionBlock)

		admission := admitForTest(t, ctx, store, project.ID, keyID, now)

		assertSingleBreach(t, admission.Blocks, governance.DimensionCost, governance.WindowDay, now.Add(time.Hour))
	})

	t.Run("rolling boundary is inclusive", func(t *testing.T) {
		tests := []struct {
			name   string
			window governance.Window
			span   time.Duration
		}{
			{"hour", governance.WindowHour, time.Hour},
			{"day", governance.WindowDay, 24 * time.Hour},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				project, keyID := createAdmissionProject(t, ctx, store, "boundary-"+test.name)
				seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now.Add(-test.span-time.Nanosecond), governance.RequestCompleted, governance.AccountingObserved)
				seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now.Add(-test.span), governance.RequestCompleted, governance.AccountingObserved)
				mustSetBudget(t, ctx, store, project.Name, governance.DimensionCalls, test.window, 1, governance.ActionBlock)

				admission := admitForTest(t, ctx, store, project.ID, keyID, now)

				assertSingleBreach(t, admission.Blocks, governance.DimensionCalls, test.window, now)

				t.Run("token attempt", func(t *testing.T) {
					project, keyID := createAdmissionProject(t, ctx, store, "token-boundary-"+test.name)
					requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now, governance.RequestCompleted, governance.AccountingObserved)
					seedUsageAttempt(t, ctx, store, requestID, 100, 0, floatPointer(0), governance.PricingPriced, now.Add(-test.span))
					mustSetBudget(t, ctx, store, project.Name, governance.DimensionTokens, test.window, 100, governance.ActionBlock)

					admission := admitForTest(t, ctx, store, project.ID, keyID, now)
					assertSingleBreach(t, admission.Blocks, governance.DimensionTokens, test.window, now)
				})

				t.Run("priced cost attempt", func(t *testing.T) {
					project, keyID := createAdmissionProject(t, ctx, store, "cost-boundary-"+test.name)
					requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now, governance.RequestCompleted, governance.AccountingObserved)
					seedUsageAttempt(t, ctx, store, requestID, 0, 0, floatPointer(2), governance.PricingPriced, now.Add(-test.span))
					mustSetBudget(t, ctx, store, project.Name, governance.DimensionCost, test.window, 2, governance.ActionBlock)

					admission := admitForTest(t, ctx, store, project.ID, keyID, now)
					assertSingleBreach(t, admission.Blocks, governance.DimensionCost, test.window, now)
				})

				t.Run("unknown pricing attempt", func(t *testing.T) {
					project, keyID := createAdmissionProject(t, ctx, store, "unknown-boundary-"+test.name)
					requestID := seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration, now, governance.RequestCompleted, governance.AccountingObserved)
					seedUsageAttempt(t, ctx, store, requestID, 0, 0, nil, governance.PricingUnknown, now.Add(-test.span))
					mustSetBudget(t, ctx, store, project.Name, governance.DimensionCost, test.window, 100, governance.ActionBlock)

					admission := admitForTest(t, ctx, store, project.ID, keyID, now)
					assertSingleBreach(t, admission.Blocks, governance.DimensionCost, test.window, now)
				})
			})
		}
	})

	// The plugin stores the SDK RequestedAt value in usage_attempt.created_at.
	// These cases catch treating delayed callback delivery as the accounting
	// time: a just-expired attempt stays outside, while a just-inside attempt
	// contributes a reset derived from its persisted SDK request time.
	t.Run("attempt windows use persisted SDK request time", func(t *testing.T) {
		tests := []struct {
			name   string
			window governance.Window
			span   time.Duration
		}{
			{"hour", governance.WindowHour, time.Hour},
			{"day", governance.WindowDay, 24 * time.Hour},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				outside, outsideKeyID := createAdmissionProject(t, ctx, store, "delayed-outside-"+test.name)
				outsideRequestedAt := now.Add(-test.span - time.Nanosecond)
				outsideRequest := seedRequestEvent(
					t,
					ctx,
					store,
					outside.ID,
					outsideKeyID,
					governance.OperationGeneration,
					outsideRequestedAt,
					governance.RequestCompleted,
					governance.AccountingObserved,
				)
				seedUsageAttempt(
					t,
					ctx,
					store,
					outsideRequest,
					1,
					0,
					floatPointer(0),
					governance.PricingPriced,
					outsideRequestedAt,
				)
				mustSetBudget(
					t,
					ctx,
					store,
					outside.Name,
					governance.DimensionTokens,
					test.window,
					1,
					governance.ActionBlock,
				)
				if admission := admitForTest(
					t, ctx, store, outside.ID, outsideKeyID, now,
				); !admission.Allowed {
					t.Fatalf("expired SDK request time counted after delayed callback: %#v", admission)
				}

				inside, insideKeyID := createAdmissionProject(t, ctx, store, "delayed-inside-"+test.name)
				insideRequestedAt := now.Add(-test.span + 7*time.Minute)
				insideRequest := seedRequestEvent(
					t,
					ctx,
					store,
					inside.ID,
					insideKeyID,
					governance.OperationGeneration,
					insideRequestedAt,
					governance.RequestCompleted,
					governance.AccountingObserved,
				)
				seedUsageAttempt(
					t,
					ctx,
					store,
					insideRequest,
					1,
					0,
					floatPointer(0),
					governance.PricingPriced,
					insideRequestedAt,
				)
				mustSetBudget(
					t,
					ctx,
					store,
					inside.Name,
					governance.DimensionTokens,
					test.window,
					1,
					governance.ActionBlock,
				)
				admission := admitForTest(t, ctx, store, inside.ID, insideKeyID, now)
				assertSingleBreach(
					t,
					admission.Blocks,
					governance.DimensionTokens,
					test.window,
					insideRequestedAt.Add(test.span),
				)
			})
		}
	})

	t.Run("zero limit resets now", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "zero-limit")
		mustSetBudget(t, ctx, store, project.Name, governance.DimensionCost, governance.WindowHour, 0, governance.ActionBlock)

		admission := admitForTest(t, ctx, store, project.ID, keyID, now)

		assertSingleBreach(t, admission.Blocks, governance.DimensionCost, governance.WindowHour, now)
	})
}

// TestAdmissionWarningAndLifecycle verifies warning persistence and request lifecycle writes.
func TestAdmissionWarningAndLifecycle(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)

	project, keyID := createAdmissionProject(t, ctx, store, "warning")
	mustSetBudget(t, ctx, store, project.Name, governance.DimensionCalls, governance.WindowHour, 0, governance.ActionWarn)
	generation := generationEvent(project.ID, keyID, now)

	admission, err := store.AdmitGeneration(ctx, generation, now)
	if err != nil {
		t.Fatalf("AdmitGeneration: %v", err)
	}
	if !admission.Allowed || admission.Request.ID != generation.ID {
		t.Fatalf("admission = %#v, want allowed persisted request", admission)
	}
	assertSingleBreach(t, admission.Warnings, governance.DimensionCalls, governance.WindowHour, now)
	assertRequestCount(t, ctx, store, project.ID, 1)

	metadata := generationEvent(project.ID, keyID, now.Add(time.Minute))
	metadata.Operation = governance.OperationGeneration
	metadata.State = governance.RequestCompleted
	metadata.AccountingState = governance.AccountingUnknown
	if err := store.RecordMetadata(ctx, metadata); err != nil {
		t.Fatalf("RecordMetadata: %v", err)
	}

	other := generationEvent(project.ID, keyID, now.Add(2*time.Minute))
	seedRequest(t, ctx, store, other)
	completedAt := now.Add(3 * time.Minute)
	if err := store.CompleteRequest(ctx, metadata.ID, 204, completedAt); err != nil {
		t.Fatalf("CompleteRequest: %v", err)
	}
	if err := store.CompleteRequest(ctx, metadata.ID, 204, completedAt); err != nil {
		t.Fatalf("CompleteRequest idempotent repeat: %v", err)
	}

	assertMetadataCompleted(t, ctx, store, metadata.ID, completedAt)
	assertRequestUnchanged(t, ctx, store, other.ID)
}

// TestAdmissionConcurrency verifies the real transaction lock admits exactly the call cap.
func TestAdmissionConcurrency(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)
	project, keyID := createAdmissionProject(t, ctx, store, "concurrent")
	mustSetBudget(t, ctx, store, project.Name, governance.DimensionCalls, governance.WindowHour, 5, governance.ActionBlock)

	const attempts = 50
	var admitted atomic.Int64
	var wait sync.WaitGroup
	errors := make(chan error, attempts)

	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()

			result, err := store.AdmitGeneration(ctx, generationEvent(project.ID, keyID, now), now)
			if err != nil {
				errors <- err
				return
			}
			if result.Allowed {
				admitted.Add(1)
			}
		}()
	}
	wait.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent AdmitGeneration: %v", err)
	}
	if got := admitted.Load(); got != 5 {
		t.Fatalf("admitted = %d, want exactly 5", got)
	}
	assertRequestCount(t, ctx, store, project.ID, 5)
}

// TestAdmissionLocksProjectsIndependently verifies advisory locks are keyed by project ID.
func TestAdmissionLocksProjectsIndependently(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)
	first, firstKeyID := createAdmissionProject(t, ctx, store, "lock-first")
	second, secondKeyID := createAdmissionProject(t, ctx, store, "lock-second")

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin held transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, first.ID); err != nil {
		t.Fatalf("hold first project lock: %v", err)
	}

	firstFinished := make(chan error, 1)
	go func() {
		_, err := store.AdmitGeneration(ctx, generationEvent(first.ID, firstKeyID, now), now)
		firstFinished <- err
	}()
	assertAdvisoryWait(t, ctx, store, firstFinished)

	secondFinished := make(chan error, 1)
	go func() {
		_, err := store.AdmitGeneration(ctx, generationEvent(second.ID, secondKeyID, now), now)
		secondFinished <- err
	}()

	select {
	case err := <-secondFinished:
		if err != nil {
			t.Fatalf("second project admission: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second project admission waited on first project advisory lock")
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release first project lock: %v", err)
	}
	select {
	case err := <-firstFinished:
		if err != nil {
			t.Fatalf("first project admission after lock release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first project admission remained blocked after advisory lock release")
	}
}
