package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// errNotImplemented is returned by store methods built in later batches.
var errNotImplemented = errors.New("not implemented")

// compile-time assertion that Store satisfies the domain port.
var _ domain.Store = (*Store)(nil)

// Store is the PostgreSQL-backed implementation of the domain.Store port.
type Store struct {
	pool *pgxpool.Pool // pool is the connection pool to the state database.
}

// New opens a connection pool to dsn and applies any pending schema migrations.
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

// Ping verifies connectivity to the database.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres:\n%w", err)
	}
	return nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// EnsureProject returns the id of the project named name, creating it if absent.
func (s *Store) EnsureProject(ctx context.Context, name string) (int64, error) {
	const query = `
INSERT INTO project (name) VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING id`

	var id int64
	if err := s.pool.QueryRow(ctx, query, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("ensure project %q:\n%w", name, err)
	}
	return id, nil
}
