package e2e

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
)

// expectedTables are the schema tables migration 0001 must create.
var expectedTables = []string{
	"budget_limit",
	"model_price",
	"oauth_token",
	"project",
	"provider",
	"reservation",
	"route",
	"usage_event",
}

// TestHealth boots the gateway against an ephemeral Postgres, checks GET /health,
// and verifies the schema migration created the expected tables.
func TestHealth(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness, err := Start(ctx)
	if err != nil {
		t.Fatalf("start harness: %v", err)
	}
	t.Cleanup(func() { harness.Stop(context.Background()) })

	assertHealthOK(t, ctx, harness)
	assertTablesExist(t, ctx, harness.DSN)
}

// assertHealthOK asserts GET /health returns 200.
func assertHealthOK(t *testing.T, ctx context.Context, harness *Harness) {
	t.Helper()

	resp, err := harness.Get(ctx, "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", resp.StatusCode)
	}
}

// assertTablesExist asserts every expected table is present in the migrated database.
func assertTablesExist(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to db: %v", err)
	}
	defer conn.Close(ctx)

	for _, table := range expectedTables {
		if !tableExists(t, ctx, conn, table) {
			t.Errorf("expected table %q to exist after migration", table)
		}
	}
}

// tableExists reports whether table is present in the public schema.
func tableExists(t *testing.T, ctx context.Context, conn *pgx.Conn, table string) bool {
	t.Helper()

	const query = `
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_schema = 'public' AND table_name = $1
)`

	var exists bool
	if err := conn.QueryRow(ctx, query, table).Scan(&exists); err != nil {
		t.Fatalf("query table %q: %v", table, err)
	}
	return exists
}
