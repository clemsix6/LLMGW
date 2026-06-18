package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// errNotImplemented is returned by store methods built in later batches.
var errNotImplemented = errors.New("not implemented")

const (
	// defaultProviderName identifies the single Claude Max provider used in V1.
	defaultProviderName = "claude_max"

	// defaultProviderType is the backend type of the V1 Claude Max OAuth provider.
	defaultProviderType = "claude_max_oauth"
)

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

// LoadToken returns the persisted OAuth token for an account label under the default provider.
// It returns domain.ErrTokenNotFound when no row exists for the account.
func (s *Store) LoadToken(ctx context.Context, account string) (domain.Token, error) {
	providerID, err := s.defaultProviderID(ctx)
	if err != nil {
		return domain.Token{}, err
	}

	const query = `
SELECT access_token, refresh_token, expires_at
FROM oauth_token
WHERE provider_id = $1 AND account_label = $2`

	var token domain.Token
	var access *string
	var expires *time.Time

	err = s.pool.QueryRow(ctx, query, providerID, account).Scan(&access, &token.RefreshToken, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Token{}, fmt.Errorf("load token %q:\n%w", account, domain.ErrTokenNotFound)
	}
	if err != nil {
		return domain.Token{}, fmt.Errorf("load token %q:\n%w", account, err)
	}

	if access != nil {
		token.AccessToken = *access
	}
	if expires != nil {
		token.ExpiresAt = *expires
	}
	return token, nil
}

// SaveToken upserts the OAuth token for an account label under the default provider.
// It never touches cooldown_until (owned by the provider pool, a later batch).
func (s *Store) SaveToken(ctx context.Context, account string, t domain.Token) error {
	providerID, err := s.defaultProviderID(ctx)
	if err != nil {
		return err
	}

	const query = `
INSERT INTO oauth_token (provider_id, account_label, access_token, refresh_token, expires_at, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (provider_id, account_label) DO UPDATE SET
    access_token  = EXCLUDED.access_token,
    refresh_token = EXCLUDED.refresh_token,
    expires_at    = EXCLUDED.expires_at,
    updated_at    = now()`

	if _, err := s.pool.Exec(ctx, query, providerID, account, t.AccessToken, t.RefreshToken, t.ExpiresAt); err != nil {
		return fmt.Errorf("save token %q:\n%w", account, err)
	}
	return nil
}

// defaultProviderID resolves the id of the single V1 Claude Max provider by name.
// The provider row is seeded by migration 0002; this is a read-only lookup with no
// write side-effect. Filtering token operations on it keeps them unambiguous if a
// second provider is ever added.
func (s *Store) defaultProviderID(ctx context.Context) (int64, error) {
	const query = `SELECT id FROM provider WHERE name = $1`

	var id int64
	if err := s.pool.QueryRow(ctx, query, defaultProviderName).Scan(&id); err != nil {
		return 0, fmt.Errorf("resolve default provider %q:\n%w", defaultProviderName, err)
	}
	return id, nil
}
