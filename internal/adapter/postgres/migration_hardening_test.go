package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestGovernancePivotMigrationSurvivesHostileLegacyData verifies that the 0010
// import tolerates every shape the unconstrained legacy schema allowed:
// duplicates, fractional integer caps, negative caps, NaN, Infinity, and
// unpriceable rates. A failing import means the gateway never starts again on
// that database, so hostile rows must sanitize fail-closed instead of aborting.
func TestGovernancePivotMigrationSurvivesHostileLegacyData(t *testing.T) {
	ctx := context.Background()
	dsn := startGovernancePostgres(t, ctx)
	projectID := seedHostileLegacySchema(t, ctx, dsn)

	store, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("migrate over hostile legacy data: %v", err)
	}
	t.Cleanup(store.Close)

	assertImportedBudgets(t, ctx, store, projectID)
	assertImportedPrices(t, ctx, store)
}

// seedHostileLegacySchema migrates up to the pre-pivot schema and fills it with
// rows the legacy constraints permitted but the pivot constraints reject.
func seedHostileLegacySchema(t *testing.T, ctx context.Context, dsn string) int64 {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open seeding pool: %v", err)
	}
	defer pool.Close()

	applyPrePivotMigrations(t, ctx, pool)

	var projectID int64
	const insertProject = `INSERT INTO project (name) VALUES ('hostile-project') RETURNING id`
	if err := pool.QueryRow(ctx, insertProject).Scan(&projectID); err != nil {
		t.Fatalf("insert legacy project: %v", err)
	}

	const insertBudget = `
INSERT INTO budget_limit (project_id, tag, dimension, "window", max_value, action)
VALUES ($1, $2, $3, $4, $5, $6)`
	for _, row := range []struct {
		tag       *string
		dimension string
		window    string
		max       string
		action    string
	}{
		{nil, "calls", "hour", "100", "block"},               // duplicate pair, higher cap
		{nil, "calls", "hour", "50", "block"},                // duplicate pair, lower cap wins
		{nil, "tokens", "day", "1000.5", "block"},            // fractional integer cap
		{nil, "cost_usd", "day", "-5", "block"},              // negative cap
		{nil, "calls", "day", "NaN", "warn"},                 // unusable, dropped
		{nil, "tokens", "hour", "Infinity", "warn"},          // unusable, dropped
		{ptrString("tagged"), "calls", "day", "42", "block"}, // tagged, out of scope
	} {
		args := []any{projectID, row.tag, row.dimension, row.window, row.max, row.action}
		if _, err := pool.Exec(ctx, insertBudget, args...); err != nil {
			t.Fatalf("insert legacy budget %v: %v", row, err)
		}
	}

	const insertPrice = `
INSERT INTO model_price (model, input_usd_per_mtok, output_usd_per_mtok)
VALUES ($1, $2::double precision, $3::double precision)`
	for _, row := range [][3]string{
		{"good-model", "1.5", "2.5"},
		{"bad-negative-model", "-1", "2"},
		{"bad-infinite-model", "1", "Infinity"},
	} {
		if _, err := pool.Exec(ctx, insertPrice, row[0], row[1], row[2]); err != nil {
			t.Fatalf("insert legacy price %v: %v", row, err)
		}
	}
	return projectID
}

// applyPrePivotMigrations applies every migration before the 0010 pivot.
func applyPrePivotMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		t.Fatalf("ensure migrations table: %v", err)
	}
	names, err := migrationNames()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	for _, name := range names {
		if name >= "0010" {
			break
		}
		if err := applyIfNeeded(ctx, pool, name); err != nil {
			t.Fatalf("apply pre-pivot migration %q: %v", name, err)
		}
	}
}

// assertImportedBudgets fails unless exactly the sanitized budgets survive.
func assertImportedBudgets(t *testing.T, ctx context.Context, store *Store, projectID int64) {
	t.Helper()
	const query = `
SELECT dimension, "window", max_value, action
FROM budget_limit
WHERE project_id = $1
ORDER BY dimension, "window"`
	rows, err := store.pool.Query(ctx, query, projectID)
	if err != nil {
		t.Fatalf("read imported budgets: %v", err)
	}
	defer rows.Close()

	type budget struct {
		dimension string
		window    string
		max       float64
		action    string
	}
	var imported []budget
	for rows.Next() {
		var b budget
		if err := rows.Scan(&b.dimension, &b.window, &b.max, &b.action); err != nil {
			t.Fatalf("scan imported budget: %v", err)
		}
		imported = append(imported, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate imported budgets: %v", err)
	}

	want := []budget{
		{"calls", "hour", 50, "block"},   // most restrictive duplicate kept
		{"cost", "day", 0, "block"},      // negative clamped to a full block
		{"tokens", "day", 1000, "block"}, // fractional cap truncated
	}
	if len(imported) != len(want) {
		t.Fatalf("imported budgets = %+v, want %+v", imported, want)
	}
	for i, b := range want {
		if imported[i] != b {
			t.Fatalf("imported budget %d = %+v, want %+v", i, imported[i], b)
		}
	}
}

// assertImportedPrices fails unless only priceable legacy rates were imported.
func assertImportedPrices(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	const query = `SELECT count(*) FROM model_price WHERE model_pattern = $1`
	for pattern, want := range map[string]int{
		"good-model":         1,
		"bad-negative-model": 0,
		"bad-infinite-model": 0,
	} {
		var got int
		if err := store.pool.QueryRow(ctx, query, pattern).Scan(&got); err != nil {
			t.Fatalf("count imported price %q: %v", pattern, err)
		}
		if got != want {
			t.Fatalf("imported price rows for %q = %d, want %d", pattern, got, want)
		}
	}
}

// ptrString returns a pointer to its argument.
func ptrString(value string) *string {
	return &value
}
