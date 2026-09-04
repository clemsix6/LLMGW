package postgres

import (
	"context"
	"testing"
)

// priceFixCase pairs a seed-repair migration with the operator price it must
// leave alone: the same model pattern, the operator's own provider, and the
// exact rates the migration treats as faulty everywhere else.
type priceFixCase struct {
	migration     string  // migration is the embedded path of the repair statement.
	pattern       string  // pattern is the model the repair targets.
	input         float64 // input is the faulty input rate the repair hunts for.
	output        float64 // output is the faulty output rate the repair hunts for.
	cacheRead     float64 // cacheRead is the cache-read rate derived from the faulty input.
	cacheCreation float64 // cacheCreation is the cache-creation rate derived from the faulty input.
}

// TestPriceFixesSkipPricesTheyDidNotSeed replays each price fix on a database
// whose seeded rows already read right, next to a price an operator set
// themselves at the very rates the fix hunts for. Both must cross the
// migration untouched: it is guarded on the identity of the rows the seed
// produced, not on the model name, and a gateway that re-prices a rate its
// operator chose bills real traffic at a number nobody agreed to.
func TestPriceFixesSkipPricesTheyDidNotSeed(t *testing.T) {
	cases := []priceFixCase{
		{
			migration: "migrations/0019_fix_haiku_4_5_price.sql", pattern: "claude-haiku-4-5",
			input: 0.80, output: 4, cacheRead: 0.08, cacheCreation: 1,
		},
		{
			migration: "migrations/0020_fix_opus_4_8_price.sql", pattern: "claude-opus-4-8",
			input: 15, output: 75, cacheRead: 1.5, cacheCreation: 18.75,
		},
	}

	for _, c := range cases {
		t.Run(c.pattern, func(t *testing.T) {
			ctx := context.Background()
			store := newGovernanceStore(t)

			seedOperatorPrice(t, ctx, store, c)

			statements, err := migrationsFS.ReadFile(c.migration)
			if err != nil {
				t.Fatalf("read %s: %v", c.migration, err)
			}

			tag, err := store.pool.Exec(ctx, string(statements))
			if err != nil {
				t.Fatalf("replay %s: %v", c.migration, err)
			}

			if tag.RowsAffected() != 0 {
				t.Fatalf("replayed price fix updated %d rows, want 0", tag.RowsAffected())
			}
		})
	}
}

// seedOperatorPrice prices one model the way an operator on an inherited
// contract would: the case's pattern, their own provider, and the faulty rates.
func seedOperatorPrice(t *testing.T, ctx context.Context, store *Store, c priceFixCase) {
	t.Helper()

	const query = `
INSERT INTO model_price (
    provider, model_pattern, service_tier,
    input_per_million, output_per_million,
    cache_read_per_million, cache_creation_per_million,
    effective_from
) VALUES ('operator-contract', $1, '*', $2, $3, $4, $5, '1970-01-01T00:00:00Z')`
	if _, err := store.pool.Exec(ctx, query, c.pattern, c.input, c.output, c.cacheRead, c.cacheCreation); err != nil {
		t.Fatalf("seed operator price of %q: %v", c.pattern, err)
	}
}
