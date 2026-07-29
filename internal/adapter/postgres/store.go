package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the PostgreSQL-backed governance repository.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a connection pool and applies every pending historical and governance migration.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool:\n%w", err)
	}
	if err := runMigrations(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply migrations:\n%w", err)
	}
	return &Store{pool: pool}, nil
}

// Ping verifies connectivity before the embedded proxy is constructed.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres:\n%w", err)
	}
	return nil
}

// Close releases the PostgreSQL pool after the SDK and workers have drained.
func (s *Store) Close() {
	s.pool.Close()
}
