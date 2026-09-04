package postgres

import (
	"context"
	"testing"
)

// haikuPriceFixMigration names the migration that repairs the Haiku 4.5 seed.
const haikuPriceFixMigration = "migrations/0019_fix_haiku_4_5_price.sql"

// TestHaikuPriceFixSkipsPricesItDidNotSeed replays the price fix on a database
// that already carries correct rates, next to a price an operator set
// themselves. Migrations run at startup and the fix reached running databases
// by hand before it shipped, so it has to pass over rows that already read
// right and leave a deliberate rate alone: the guard matches the faulty rates,
// never the model name. Rewriting either would silently re-price live traffic.
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

// seedOperatorHaikuPrice prices claude-haiku-4-5 the way an operator who
// negotiated their own rate would: same pattern, own provider, own numbers.
func seedOperatorHaikuPrice(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()

	const query = `
INSERT INTO model_price (
    provider, model_pattern, service_tier,
    input_per_million, output_per_million,
    cache_read_per_million, cache_creation_per_million,
    effective_from
) VALUES ('operator-negotiated', 'claude-haiku-4-5', '*', 0.5, 2.5, 0.05, 0.625, '1970-01-01')`
	if _, err := store.pool.Exec(ctx, query); err != nil {
		t.Fatalf("seed operator haiku price: %v", err)
	}
}
