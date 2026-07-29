package postgres

import (
	"context"
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
