package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCLIProxyGovernanceMigration verifies the stopped-traffic upgrade from migration 0009.
func TestCLIProxyGovernanceMigration(t *testing.T) {
	ctx := context.Background()
	pool := openGovernanceMigrationPool(t, ctx)

	applyMigrationsThrough0009(t, ctx, pool)
	seedLegacyGovernanceState(t, ctx, pool)
	applyMigrationForTest(t, ctx, pool, "0010_cliproxy_governance.sql")

	assertArchivedGovernanceState(t, ctx, pool)
	assertPreservedLegacyObjects(t, ctx, pool)
	assertMappedLegacyPrice(t, ctx, pool)
	assertGovernanceEnumsAccepted(t, ctx, pool)
}

// openGovernanceMigrationPool starts PostgreSQL without applying migrations automatically.
func openGovernanceMigrationPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(ctx, startGovernancePostgres(t, ctx))
	if err != nil {
		t.Fatalf("open raw governance migration pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := ensureMigrationsTable(ctx, pool); err != nil {
		t.Fatalf("create migration table: %v", err)
	}

	return pool
}

// applyMigrationsThrough0009 applies the historical migrations at the exact upgrade boundary.
func applyMigrationsThrough0009(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	for _, name := range []string{
		"0001_init.sql",
		"0002_seed_default_provider.sql",
		"0003_seed_model_prices.sql",
		"0004_reservation_expiry_index.sql",
		"0005_session_key.sql",
		"0006_chatgpt_codex_provider.sql",
		"0007_oauth_chatgpt_account_id.sql",
		"0008_seed_codex_model_prices.sql",
		"0009_seed_gpt56_model_prices.sql",
	} {
		applyMigrationForTest(t, ctx, pool, name)
	}
}

// applyMigrationForTest applies one named migration and fails with its exact filename.
func applyMigrationForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()

	if err := applyMigration(ctx, pool, name); err != nil {
		t.Fatalf("apply migration %s: %v", name, err)
	}
}

// seedLegacyGovernanceState inserts state after migration 0009 and before migration 0010.
func seedLegacyGovernanceState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	const query = `
INSERT INTO project (name) VALUES ('truewallet');
INSERT INTO budget_limit (project_id, tag, dimension, "window", max_value, action)
VALUES
  ((SELECT id FROM project WHERE name='truewallet'), NULL, 'calls', 'hour', 50, 'block'),
  ((SELECT id FROM project WHERE name='truewallet'), 'worker-a', 'calls', 'hour', 5, 'block');
INSERT INTO usage_event (project_id, tag, model, provider, status)
VALUES ((SELECT id FROM project WHERE name='truewallet'), 'worker-a', 'legacy-model', 'claude_max', 'ok');
UPDATE budget_limit SET dimension = 'cost_usd' WHERE tag IS NULL;`

	if _, err := pool.Exec(ctx, query); err != nil {
		t.Fatalf("seed legacy governance state: %v", err)
	}
}

// assertArchivedGovernanceState verifies archival and project-wide budget conversion.
func assertArchivedGovernanceState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	assertRowCount(t, ctx, pool, "legacy_usage_event", 1)
	assertRowCount(t, ctx, pool, "legacy_budget_limit", 2)
	assertRowCount(t, ctx, pool, "budget_limit", 1)

	var projectName, dimension string
	var maximum float64
	const query = `
SELECT p.name, b.dimension, b.max_value
FROM budget_limit b
JOIN project p ON p.id = b.project_id`
	if err := pool.QueryRow(ctx, query).Scan(&projectName, &dimension, &maximum); err != nil {
		t.Fatalf("read migrated budget: %v", err)
	}
	if projectName != "truewallet" || dimension != "cost" || maximum != 50 {
		t.Fatalf("migrated budget = (%q, %q, %v), want (truewallet, cost, 50)", projectName, dimension, maximum)
	}
}

// assertPreservedLegacyObjects verifies required legacy tables survive while reservation is removed.
func assertPreservedLegacyObjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	for _, name := range []string{
		"project", "provider", "route", "oauth_token",
		"legacy_usage_event", "legacy_budget_limit", "legacy_model_price",
		"client_key", "budget_limit", "request_event", "usage_attempt", "model_price",
	} {
		assertRelationExists(t, ctx, pool, name, true)
	}
	assertRelationExists(t, ctx, pool, "reservation", false)

	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM project WHERE name = 'truewallet')`).Scan(&exists); err != nil {
		t.Fatalf("check preserved project: %v", err)
	}
	if !exists {
		t.Fatal("project truewallet was not preserved")
	}
}

// assertMappedLegacyPrice verifies legacy prices move without inferred cache discounts.
func assertMappedLegacyPrice(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	const query = `
SELECT provider, service_tier, input_per_million, output_per_million,
       cache_read_per_million IS NULL, cache_creation_per_million IS NULL, effective_from
FROM model_price
WHERE model_pattern = 'claude-sonnet-4-6'`

	var provider, tier string
	var input, output float64
	var cacheReadNull, cacheCreationNull bool
	var effectiveFrom time.Time
	if err := pool.QueryRow(ctx, query).Scan(
		&provider, &tier, &input, &output, &cacheReadNull, &cacheCreationNull, &effectiveFrom,
	); err != nil {
		t.Fatalf("read mapped legacy price: %v", err)
	}
	if provider != "*" || tier != "*" || input != 3 || output != 15 {
		t.Fatalf("mapped legacy price = (%q, %q, %v, %v), want (*, *, 3, 15)", provider, tier, input, output)
	}
	if !cacheReadNull || !cacheCreationNull || !effectiveFrom.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("mapped cache/effective values = (%v, %v, %v), want (true, true, epoch)", cacheReadNull, cacheCreationNull, effectiveFrom)
	}
}

// assertGovernanceEnumsAccepted inserts every allowed governance enum through real constraints.
func assertGovernanceEnumsAccepted(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	projectID := insertEnumProject(t, ctx, pool)
	keyID := insertEnumClientKey(t, ctx, pool, projectID)
	insertEnumBudgets(t, ctx, pool, projectID)

	requestIDs := insertEnumRequests(t, ctx, pool, projectID, keyID)
	insertEnumAttempts(t, ctx, pool, requestIDs)
}

// insertEnumProject creates a separate project for enum constraint probes.
func insertEnumProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()

	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO project (name) VALUES ('enum-probe') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert enum project: %v", err)
	}
	return id
}

// insertEnumClientKey creates a valid key for request enum constraint probes.
func insertEnumClientKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64) int64 {
	t.Helper()

	const query = `
INSERT INTO client_key (project_id, name, public_id, digest)
VALUES ($1, 'enum-key', 'llmgw_enum', decode(repeat('00', 32), 'hex'))
RETURNING id`

	var id int64
	if err := pool.QueryRow(ctx, query, projectID).Scan(&id); err != nil {
		t.Fatalf("insert enum client key: %v", err)
	}
	return id
}

// insertEnumBudgets exercises every valid dimension, window, and action value.
func insertEnumBudgets(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64) {
	t.Helper()

	const query = `
INSERT INTO budget_limit (project_id, dimension, "window", max_value, action)
VALUES ($1, 'calls', 'hour', 1, 'block'),
       ($1, 'tokens', 'day', 2, 'warn'),
       ($1, 'cost', 'day', 0.5, 'block')`
	if _, err := pool.Exec(ctx, query, projectID); err != nil {
		t.Fatalf("insert governance budget enums: %v", err)
	}
}

// insertEnumRequests exercises every valid operation, request state, and accounting state.
func insertEnumRequests(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	projectID int64,
	keyID int64,
) []string {
	t.Helper()

	probes := []struct {
		operation  string
		state      string
		accounting string
	}{
		{"generation", "in_flight", "pending"},
		{"metadata", "completed", "observed"},
		{"generation", "completed", "accounting_unknown"},
		{"metadata", "completed", "resolved_zero"},
		{"generation", "completed", "not_applicable"},
	}

	requestIDs := make([]string, 0, len(probes))
	for _, probe := range probes {
		requestIDs = append(requestIDs, insertEnumRequest(t, ctx, pool, projectID, keyID, probe.operation, probe.state, probe.accounting))
	}
	return requestIDs
}

// insertEnumRequest creates one request constraint probe and returns its UUID.
func insertEnumRequest(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	projectID int64,
	keyID int64,
	operation string,
	state string,
	accounting string,
) string {
	t.Helper()

	const query = `
INSERT INTO request_event (
    id, project_id, client_key_id, operation, requested_at,
    method, path, state, accounting_state
) VALUES (gen_random_uuid(), $1, $2, $3, now(), 'POST', '/probe', $4, $5)
RETURNING id`

	var id string
	if err := pool.QueryRow(ctx, query, projectID, keyID, operation, state, accounting).Scan(&id); err != nil {
		t.Fatalf("insert request enums (%s, %s, %s): %v", operation, state, accounting, err)
	}
	return id
}

// insertEnumAttempts exercises every valid pricing state.
func insertEnumAttempts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, requestIDs []string) {
	t.Helper()

	for index, pricingState := range []string{"priced", "unknown_pricing"} {
		const query = `
INSERT INTO usage_attempt (
    id, request_id, provider, executor_type, resolved_model, requested_alias,
    upstream_auth_id, upstream_auth_type, input_tokens, output_tokens,
    reasoning_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
    unclassified_tokens, service_tier, response_service_tier, failed,
    latency_ms, ttft_ms, cost_usd, pricing_state, created_at
) VALUES (
    gen_random_uuid(), $1, 'probe', 'probe', 'probe-model', 'probe-alias',
    'probe-auth', 'probe-auth-type', 0, 0, 0, 0, 0, 0, 0, 'standard',
    'standard', false, 0, 0, $2, $3, now()
)`

		var cost *float64
		if pricingState == "priced" {
			value := 0.5
			cost = &value
		}
		if _, err := pool.Exec(ctx, query, requestIDs[index], cost, pricingState); err != nil {
			t.Fatalf("insert pricing state %q: %v", pricingState, err)
		}
	}
}

// assertRowCount fails unless relation contains exactly the expected number of rows.
func assertRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, relation string, expected int64) {
	t.Helper()

	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+relation).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", relation, err)
	}
	if count != expected {
		t.Fatalf("%s row count = %d, want %d", relation, count, expected)
	}
}

// assertRelationExists fails unless a public relation's existence matches expected.
func assertRelationExists(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	relation string,
	expected bool,
) {
	t.Helper()

	var exists bool
	const query = `SELECT to_regclass('public.' || $1) IS NOT NULL`
	if err := pool.QueryRow(ctx, query, relation).Scan(&exists); err != nil {
		t.Fatalf("check relation %s: %v", relation, err)
	}
	if exists != expected {
		t.Fatalf("relation %s exists = %v, want %v", relation, exists, expected)
	}
}
