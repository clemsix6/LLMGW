package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestPriceRuleFor verifies deterministic provider, tier, pattern, and time precedence.
func TestPriceRuleFor(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	requestedAt := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)

	t.Run("exact provider beats wildcard", func(t *testing.T) {
		insertPriceRule(t, ctx, store, "*", "provider-exact-model", "tier", 1, requestedAt.Add(-time.Hour))
		insertPriceRule(t, ctx, store, "provider-exact", "*", "tier", 2, requestedAt.Add(-time.Hour))
		assertInputPrice(t, ctx, store, "provider-exact", "provider-exact-model", "tier", requestedAt, 2)
	})

	t.Run("exact tier beats wildcard", func(t *testing.T) {
		insertPriceRule(t, ctx, store, "tier-provider", "tier-model-specific", "*", 3, requestedAt.Add(-time.Hour))
		insertPriceRule(t, ctx, store, "tier-provider", "*", "priority", 4, requestedAt.Add(-time.Hour))
		assertInputPrice(t, ctx, store, "tier-provider", "tier-model-specific", "priority", requestedAt, 4)
	})

	t.Run("most literal model pattern wins", func(t *testing.T) {
		insertPriceRule(t, ctx, store, "pattern-provider", "family-*", "tier", 5, requestedAt.Add(-time.Hour))
		insertPriceRule(t, ctx, store, "pattern-provider", "family-special-*", "tier", 6, requestedAt.Add(-time.Hour))
		assertInputPrice(t, ctx, store, "pattern-provider", "family-special-v1", "tier", requestedAt, 6)
	})

	t.Run("latest effective rule wins", func(t *testing.T) {
		insertPriceRule(t, ctx, store, "effective-provider", "effective-*", "tier", 7, requestedAt.Add(-2*time.Hour))
		insertPriceRule(t, ctx, store, "effective-provider", "effective-*", "tier", 8, requestedAt.Add(-time.Hour))
		insertPriceRule(t, ctx, store, "effective-provider", "effective-*", "tier", 9, requestedAt.Add(time.Hour))
		assertInputPrice(t, ctx, store, "effective-provider", "effective-model", "tier", requestedAt, 8)
	})

	// Providers resolve an alias to a dated identifier: a request for
	// "claude-haiku-4-5" is served and reported as "claude-haiku-4-5-20251001".
	// Seeded rules must cover that real shape, otherwise every attempt is
	// recorded as unknown_pricing and cost budgets can never block.
	t.Run("seeded rules price dated provider models", func(t *testing.T) {
		for _, model := range []string{
			"claude-haiku-4-5-20251001",
			"claude-opus-4-8-20260115",
			"claude-sonnet-4-6-20260220",
			"gpt-5-codex-20251201",
		} {
			rule, found, err := store.PriceRuleFor(ctx, "claude", model, "standard", requestedAt)
			if err != nil {
				t.Fatalf("PriceRuleFor(%s): %v", model, err)
			}
			if !found {
				t.Fatalf("PriceRuleFor(%s) found no seeded rule", model)
			}
			if rule.InputPerMillion == nil || *rule.InputPerMillion <= 0 {
				t.Fatalf("PriceRuleFor(%s) input price = %v, want a positive rate",
					model, rule.InputPerMillion)
			}
		}
	})

	t.Run("no matching rule", func(t *testing.T) {
		rule, found, err := store.PriceRuleFor(
			ctx,
			"absent-provider",
			"absent-model",
			"absent-tier",
			requestedAt,
		)
		if err != nil {
			t.Fatalf("PriceRuleFor: %v", err)
		}
		if found || rule != (governance.PriceRule{}) {
			t.Fatalf("PriceRuleFor() = (%#v, %t), want zero, false", rule, found)
		}
	})
}

// TestRecordAttempt verifies atomic idempotent attempt and parent persistence.
func TestRecordAttempt(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Date(2030, 7, 27, 12, 0, 0, 0, time.UTC)

	t.Run("generation becomes observed and attempts are idempotent", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "attempt-generation")
		requestID := seedRequestEvent(
			t,
			ctx,
			store,
			project.ID,
			keyID,
			governance.OperationGeneration,
			now,
			governance.RequestCompleted,
			governance.AccountingPending,
		)
		keyPublicID := clientKeyPublicID(t, ctx, store, keyID)
		attempt := completeAttempt(requestID, keyPublicID, nextAdmissionUUID(), now.Add(time.Second))

		if err := store.RecordAttempt(ctx, attempt); err != nil {
			t.Fatalf("RecordAttempt: %v", err)
		}
		if err := store.RecordAttempt(ctx, attempt); err != nil {
			t.Fatalf("RecordAttempt repeat: %v", err)
		}
		assertAttemptCount(t, ctx, store, requestID, 1)
		assertAttemptFields(t, ctx, store, attempt)
		assertAttemptParent(t, ctx, store, requestID, governance.AccountingObserved, attempt.RequestedAlias, false)

		second := completeAttempt(requestID, keyPublicID, nextAdmissionUUID(), now.Add(2*time.Second))
		second.Failed = true
		second.CostUSD = nil
		second.PricingState = governance.PricingUnknown
		if err := store.RecordAttempt(ctx, second); err != nil {
			t.Fatalf("RecordAttempt second: %v", err)
		}
		assertAttemptCount(t, ctx, store, requestID, 2)
	})

	t.Run("metadata stays not applicable", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "attempt-metadata")
		requestID := seedRequestEvent(
			t,
			ctx,
			store,
			project.ID,
			keyID,
			governance.OperationMetadata,
			now,
			governance.RequestCompleted,
			governance.AccountingNotApplicable,
		)
		attempt := completeAttempt(
			requestID,
			clientKeyPublicID(t, ctx, store, keyID),
			nextAdmissionUUID(),
			now.Add(time.Second),
		)

		if err := store.RecordAttempt(ctx, attempt); err != nil {
			t.Fatalf("RecordAttempt metadata: %v", err)
		}
		assertAttemptParent(t, ctx, store, requestID, governance.AccountingNotApplicable, "test-model", false)
	})

	for _, accounting := range []governance.AccountingState{
		governance.AccountingUnknown,
		governance.AccountingResolvedZero,
	} {
		t.Run("late usage resolves "+string(accounting), func(t *testing.T) {
			project, keyID := createAdmissionProject(t, ctx, store, "attempt-late")
			requestID := seedRequestEvent(
				t,
				ctx,
				store,
				project.ID,
				keyID,
				governance.OperationGeneration,
				now,
				governance.RequestCompleted,
				accounting,
			)
			resolvedAt := now.Add(-time.Minute)
			if _, err := store.pool.Exec(
				ctx,
				`UPDATE request_event SET requested_model = NULL, accounting_resolved_at = $2 WHERE id = $1`,
				requestID,
				resolvedAt,
			); err != nil {
				t.Fatalf("prepare late usage parent: %v", err)
			}
			attempt := completeAttempt(
				requestID,
				clientKeyPublicID(t, ctx, store, keyID),
				nextAdmissionUUID(),
				now.Add(time.Second),
			)

			if err := store.RecordAttempt(ctx, attempt); err != nil {
				t.Fatalf("RecordAttempt late: %v", err)
			}
			assertAttemptParent(t, ctx, store, requestID, governance.AccountingObserved, attempt.RequestedAlias, false)
		})
	}

	t.Run("mismatched public key fails before parent mutation", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "attempt-correlation-owner")
		_, otherKeyID := createAdmissionProject(t, ctx, store, "attempt-correlation-other")
		requestID := seedRequestEvent(
			t,
			ctx,
			store,
			project.ID,
			keyID,
			governance.OperationGeneration,
			now,
			governance.RequestCompleted,
			governance.AccountingPending,
		)
		attempt := completeAttempt(
			requestID,
			clientKeyPublicID(t, ctx, store, otherKeyID),
			nextAdmissionUUID(),
			now.Add(time.Second),
		)

		err := store.RecordAttempt(ctx, attempt)
		if !errors.Is(err, governance.ErrUsageCorrelation) {
			t.Fatalf("RecordAttempt mismatch error = %v, want ErrUsageCorrelation", err)
		}
		assertAttemptCount(t, ctx, store, requestID, 0)
		assertAttemptParent(t, ctx, store, requestID, governance.AccountingPending, "test-model", false)
	})

	// This catches removing the repository boundary validation and allowing a
	// zero SDK attempt timestamp to become a misleading year-one audit row.
	t.Run("zero creation time fails before persistence", func(t *testing.T) {
		project, keyID := createAdmissionProject(t, ctx, store, "attempt-zero-created")
		requestID := seedRequestEvent(
			t,
			ctx,
			store,
			project.ID,
			keyID,
			governance.OperationGeneration,
			now,
			governance.RequestCompleted,
			governance.AccountingPending,
		)
		attempt := completeAttempt(
			requestID,
			clientKeyPublicID(t, ctx, store, keyID),
			nextAdmissionUUID(),
			now.Add(time.Second),
		)
		attempt.CreatedAt = time.Time{}

		if err := store.RecordAttempt(ctx, attempt); err == nil {
			t.Fatal("RecordAttempt accepted zero creation time")
		}
		assertAttemptCount(t, ctx, store, requestID, 0)
		assertAttemptParent(
			t,
			ctx,
			store,
			requestID,
			governance.AccountingPending,
			"test-model",
			false,
		)
	})
}

// insertPriceRule inserts one exact model-price fixture.
func insertPriceRule(
	t *testing.T,
	ctx context.Context,
	store *Store,
	provider string,
	pattern string,
	tier string,
	input float64,
	effectiveFrom time.Time,
) {
	t.Helper()
	const query = `
INSERT INTO model_price (
    provider, model_pattern, service_tier, input_per_million,
    output_per_million, cache_read_per_million,
    cache_creation_per_million, effective_from
) VALUES ($1, $2, $3, $4, 1, 1, 1, $5)`
	if _, err := store.pool.Exec(ctx, query, provider, pattern, tier, input, effectiveFrom); err != nil {
		t.Fatalf("insert price rule: %v", err)
	}
}

// assertInputPrice verifies one resolved rule's identifying input price.
func assertInputPrice(
	t *testing.T,
	ctx context.Context,
	store *Store,
	provider string,
	model string,
	tier string,
	requestedAt time.Time,
	want float64,
) {
	t.Helper()
	rule, found, err := store.PriceRuleFor(ctx, provider, model, tier, requestedAt)
	if err != nil {
		t.Fatalf("PriceRuleFor: %v", err)
	}
	if !found || rule.InputPerMillion == nil || *rule.InputPerMillion != want {
		t.Fatalf("PriceRuleFor() = (%#v, %t), want input %.2f", rule, found, want)
	}
}

// completeAttempt returns a fixture covering every persisted attempt field.
func completeAttempt(
	requestID string,
	keyPublicID string,
	attemptID string,
	createdAt time.Time,
) governance.UsageAttempt {
	status := 429
	cost := 0.00125
	return governance.UsageAttempt{
		ID:                  attemptID,
		RequestID:           requestID,
		ClientKeyPublicID:   keyPublicID,
		Provider:            "openai-compatibility",
		ExecutorType:        "OpenAICompatExecutor",
		ResolvedModel:       "upstream-model",
		RequestedAlias:      "test-model",
		UpstreamAuthID:      "account-a",
		UpstreamAuthType:    "api-key",
		Tokens:              governance.TokenBreakdown{UncachedInput: 8, CacheRead: 1, CacheCreation: 1, Output: 4, Reasoning: 2, Total: 14},
		ServiceTier:         "auto",
		ResponseServiceTier: "default",
		Failed:              false,
		UpstreamStatus:      &status,
		Latency:             1500 * time.Millisecond,
		TTFT:                125 * time.Millisecond,
		CostUSD:             &cost,
		PricingState:        governance.PricingPriced,
		CreatedAt:           createdAt,
	}
}

func clientKeyPublicID(t *testing.T, ctx context.Context, store *Store, keyID int64) string {
	t.Helper()
	var publicID string
	if err := store.pool.QueryRow(
		ctx,
		`SELECT public_id FROM client_key WHERE id = $1`,
		keyID,
	).Scan(&publicID); err != nil {
		t.Fatalf("load client key public ID: %v", err)
	}
	return publicID
}

// assertAttemptCount verifies idempotency and multi-attempt behavior.
func assertAttemptCount(t *testing.T, ctx context.Context, store *Store, requestID string, want int64) {
	t.Helper()
	var got int64
	if err := store.pool.QueryRow(
		ctx,
		`SELECT count(*) FROM usage_attempt WHERE request_id = $1`,
		requestID,
	).Scan(&got); err != nil {
		t.Fatalf("count usage attempts: %v", err)
	}
	if got != want {
		t.Fatalf("usage attempt count = %d, want %d", got, want)
	}
}

// assertAttemptFields verifies all approved normalized columns.
func assertAttemptFields(t *testing.T, ctx context.Context, store *Store, want governance.UsageAttempt) {
	t.Helper()
	var got governance.UsageAttempt
	var latencyMS, ttftMS int64
	const query = `
SELECT id, request_id, provider, executor_type, resolved_model, requested_alias,
       upstream_auth_id, upstream_auth_type, input_tokens, cache_read_tokens,
       cache_creation_tokens, output_tokens, reasoning_tokens, total_tokens,
       unclassified_tokens, service_tier, response_service_tier, failed,
       upstream_status, latency_ms, ttft_ms, cost_usd, pricing_state, created_at
FROM usage_attempt WHERE id = $1`
	err := store.pool.QueryRow(ctx, query, want.ID).Scan(
		&got.ID,
		&got.RequestID,
		&got.Provider,
		&got.ExecutorType,
		&got.ResolvedModel,
		&got.RequestedAlias,
		&got.UpstreamAuthID,
		&got.UpstreamAuthType,
		&got.Tokens.UncachedInput,
		&got.Tokens.CacheRead,
		&got.Tokens.CacheCreation,
		&got.Tokens.Output,
		&got.Tokens.Reasoning,
		&got.Tokens.Total,
		&got.Tokens.Unclassified,
		&got.ServiceTier,
		&got.ResponseServiceTier,
		&got.Failed,
		&got.UpstreamStatus,
		&latencyMS,
		&ttftMS,
		&got.CostUSD,
		&got.PricingState,
		&got.CreatedAt,
	)
	if err != nil {
		t.Fatalf("read usage attempt: %v", err)
	}
	got.Latency = time.Duration(latencyMS) * time.Millisecond
	got.TTFT = time.Duration(ttftMS) * time.Millisecond
	if got.ID != want.ID || got.RequestID != want.RequestID ||
		got.Provider != want.Provider || got.ExecutorType != want.ExecutorType ||
		got.ResolvedModel != want.ResolvedModel || got.RequestedAlias != want.RequestedAlias ||
		got.UpstreamAuthID != want.UpstreamAuthID || got.UpstreamAuthType != want.UpstreamAuthType ||
		got.Tokens != want.Tokens || got.ServiceTier != want.ServiceTier ||
		got.ResponseServiceTier != want.ResponseServiceTier || got.Failed != want.Failed ||
		got.UpstreamStatus == nil || *got.UpstreamStatus != *want.UpstreamStatus ||
		got.Latency != want.Latency || got.TTFT != want.TTFT ||
		got.CostUSD == nil || *got.CostUSD != *want.CostUSD ||
		got.PricingState != want.PricingState ||
		!got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("stored attempt = %#v, want %#v", got, want)
	}
}

// assertAttemptParent verifies the request transition and alias behavior.
func assertAttemptParent(
	t *testing.T,
	ctx context.Context,
	store *Store,
	requestID string,
	wantState governance.AccountingState,
	wantModel string,
	wantResolvedAt bool,
) {
	t.Helper()
	var state governance.AccountingState
	var model *string
	var resolvedAt *time.Time
	if err := store.pool.QueryRow(
		ctx,
		`SELECT accounting_state, requested_model, accounting_resolved_at FROM request_event WHERE id = $1`,
		requestID,
	).Scan(&state, &model, &resolvedAt); err != nil {
		t.Fatalf("read attempt parent: %v", err)
	}
	if state != wantState || model == nil || *model != wantModel || (resolvedAt != nil) != wantResolvedAt {
		t.Fatalf("attempt parent = (%s, %v, %v), want (%s, %q, resolved=%t)", state, model, resolvedAt, wantState, wantModel, wantResolvedAt)
	}
}
