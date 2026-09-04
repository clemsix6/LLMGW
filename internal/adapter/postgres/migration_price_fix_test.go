package postgres

import (
	"context"
	"testing"
)

// haikuPriceFixMigration names the migration that repairs the Haiku 4.5 seed.
const haikuPriceFixMigration = "migrations/0019_fix_haiku_4_5_price.sql"

// TestHaikuPriceFixSkipsPricesItDidNotSeed replays the price fix on a database
// whose Haiku rows already read right, next to a price an operator set
// themselves at the very rates the fix hunts for. Both must cross the
// migration untouched: it is guarded on the identity of the rows the seed
// produced, not on the model name, and a gateway that re-prices a rate its
// operator chose bills real traffic at a number nobody agreed to.
func TestHaikuPriceFixSkipsPricesItDidNotSeed(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)

	seedOperatorHaikuPrice(t, ctx, store)

	statements, err := migrationsFS.ReadFile(haikuPriceFixMigration)
	if err != nil {
		t.Fatalf("read %s: %v", haikuPriceFixMigration, err)
	}

	tag, err := store.pool.Exec(ctx, string(statements))
	if err != nil {
		t.Fatalf("replay %s: %v", haikuPriceFixMigration, err)
	}

	if tag.RowsAffected() != 0 {
		t.Fatalf("replayed price fix updated %d rows, want 0", tag.RowsAffected())
	}
}

// seedOperatorHaikuPrice prices claude-haiku-4-5 the way an operator on an
// inherited contract would: the same model pattern, their own provider, and
// the exact rates the fix treats as faulty everywhere else.
func seedOperatorHaikuPrice(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()

	const query = `
INSERT INTO model_price (
    provider, model_pattern, service_tier,
    input_per_million, output_per_million,
    cache_read_per_million, cache_creation_per_million,
    effective_from
) VALUES ('operator-contract', 'claude-haiku-4-5', '*', 0.80, 4, 0.08, 1, '1970-01-01T00:00:00Z')`
	if _, err := store.pool.Exec(ctx, query); err != nil {
		t.Fatalf("seed operator haiku price: %v", err)
	}
}
