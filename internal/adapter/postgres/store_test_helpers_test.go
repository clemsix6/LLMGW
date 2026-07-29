package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// newGovernanceStore starts PostgreSQL 16, applies every migration, and returns a ready Store.
func newGovernanceStore(t *testing.T) *Store {
	t.Helper()

	ctx := context.Background()
	dsn := startGovernancePostgres(t, ctx)

	store, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("open governance store: %v", err)
	}
	t.Cleanup(store.Close)

	return store
}

// startGovernancePostgres starts an ephemeral PostgreSQL 16 instance and returns its DSN.
func startGovernancePostgres(t *testing.T, ctx context.Context) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("llmgw"),
		tcpostgres.WithUsername("llmgw"),
		tcpostgres.WithPassword("llmgw"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start governance postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate governance postgres: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("governance postgres connection string: %v", err)
	}

	return dsn
}
