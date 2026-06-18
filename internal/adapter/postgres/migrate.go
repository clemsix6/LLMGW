package postgres

import (
	"context"
	"embed"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// runMigrations applies every embedded migration that has not yet been recorded.
func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := applyIfNeeded(ctx, pool, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureMigrationsTable creates the migration bookkeeping table if absent.
func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	const query = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

	if _, err := pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("create schema_migrations:\n%w", err)
	}
	return nil
}

// migrationNames returns the embedded migration file names in lexical order.
func migrationNames() ([]string, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir:\n%w", err)
	}

	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}

	sort.Strings(names)
	return names, nil
}

// applyIfNeeded runs the named migration unless it is already recorded as applied.
func applyIfNeeded(ctx context.Context, pool *pgxpool.Pool, name string) error {
	applied, err := isApplied(ctx, pool, name)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}
	return applyMigration(ctx, pool, name)
}

// isApplied reports whether the named migration is recorded as applied.
func isApplied(ctx context.Context, pool *pgxpool.Pool, name string) (bool, error) {
	const query = `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`

	var exists bool
	if err := pool.QueryRow(ctx, query, name).Scan(&exists); err != nil {
		return false, fmt.Errorf("check migration %q:\n%w", name, err)
	}
	return exists, nil
}

// applyMigration executes a migration file and records it in a single transaction.
func applyMigration(ctx context.Context, pool *pgxpool.Pool, name string) error {
	statements, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("read migration %q:\n%w", name, err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %q:\n%w", name, err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, string(statements)); err != nil {
		return fmt.Errorf("exec migration %q:\n%w", name, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
		return fmt.Errorf("record migration %q:\n%w", name, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %q:\n%w", name, err)
	}
	return nil
}
