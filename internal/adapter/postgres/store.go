package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/clemsix6/LLMGW/internal/domain"
)

const (
	// DefaultProviderName identifies the single Claude Max provider used in V1. The handler
	// records it as the serving backend on every usage_event.
	DefaultProviderName = "claude_max"

	// CodexProviderName identifies the ChatGPT Codex provider seeded by migration 0006.
	CodexProviderName = "chatgpt-codex"
)

// compile-time assertion that Store satisfies the domain port.
var _ domain.Store = (*Store)(nil)

// Store is the PostgreSQL-backed implementation of the domain.Store port.
type Store struct {
	pool *pgxpool.Pool // pool is the connection pool to the state database.

	providerIDMu    sync.Mutex       // providerIDMu guards providerIDCache.
	providerIDCache map[string]int64 // providerIDCache memoises provider name → id lookups so each name hits the DB at most once.
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

	return &Store{pool: pool, providerIDCache: make(map[string]int64)}, nil
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

// PriceFor returns the notional per-million-token input/output USD prices for a model from the
// model_price table. ok is false (with a nil error) when no price row exists, so callers can
// distinguish an unpriced model from a query failure.
func (s *Store) PriceFor(ctx context.Context, model string) (in, out float64, ok bool, err error) {
	const query = `SELECT input_usd_per_mtok, output_usd_per_mtok FROM model_price WHERE model = $1`

	err = s.pool.QueryRow(ctx, query, model).Scan(&in, &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("price for model %q:\n%w", model, err)
	}
	return in, out, true, nil
}

// LoadToken returns the persisted OAuth token for an account label under the named provider.
// It returns domain.ErrTokenNotFound when no row exists for the account.
func (s *Store) LoadToken(ctx context.Context, providerName, account string) (domain.Token, error) {
	providerID, err := s.providerIDByName(ctx, providerName)
	if err != nil {
		return domain.Token{}, err
	}

	const query = `
SELECT access_token, refresh_token, session_key, chatgpt_account_id, expires_at
FROM oauth_token
WHERE provider_id = $1 AND account_label = $2`

	var token domain.Token
	var access, refresh, session, accountID *string
	var expires *time.Time

	err = s.pool.QueryRow(ctx, query, providerID, account).Scan(&access, &refresh, &session, &accountID, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Token{}, fmt.Errorf("load token %q:\n%w", account, domain.ErrTokenNotFound)
	}
	if err != nil {
		return domain.Token{}, fmt.Errorf("load token %q:\n%w", account, err)
	}

	if access != nil {
		token.AccessToken = *access
	}
	if refresh != nil {
		token.RefreshToken = *refresh
	}
	if session != nil {
		token.SessionKey = *session
	}
	if accountID != nil {
		token.ChatGPTAccountID = *accountID
	}
	if expires != nil {
		token.ExpiresAt = *expires
	}
	return token, nil
}

// SaveToken upserts the OAuth token for an account label under the named provider.
// It never touches cooldown_until (owned by the provider pool, a later batch).
func (s *Store) SaveToken(ctx context.Context, providerName, account string, t domain.Token) error {
	providerID, err := s.providerIDByName(ctx, providerName)
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

// SeedSessionKey inserts an account's durable session key when the account has no row yet under
// the named provider. It is the seed path's sole writer for session_key (never overwriting an
// existing row), keeping that column's ownership separate from SaveToken, which writes only the
// derived access/refresh tokens.
func (s *Store) SeedSessionKey(ctx context.Context, providerName, account, sessionKey string) error {
	providerID, err := s.providerIDByName(ctx, providerName)
	if err != nil {
		return err
	}

	const query = `
INSERT INTO oauth_token (provider_id, account_label, session_key, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (provider_id, account_label) DO NOTHING`

	if _, err := s.pool.Exec(ctx, query, providerID, account, sessionKey); err != nil {
		return fmt.Errorf("seed session key for %q:\n%w", account, err)
	}
	return nil
}

// SeedCodexAccount inserts an account's durable refresh token and ChatGPT account ID when the
// account has no row yet under the Codex provider. It is idempotent (never overwriting an existing
// row), keeping initial credential seeding separate from token refresh writes.
func (s *Store) SeedCodexAccount(ctx context.Context, label, refreshToken, accountID string) error {
	providerID, err := s.providerIDByName(ctx, CodexProviderName)
	if err != nil {
		return err
	}

	const query = `
INSERT INTO oauth_token (provider_id, account_label, refresh_token, chatgpt_account_id, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (provider_id, account_label) DO NOTHING`

	if _, err := s.pool.Exec(ctx, query, providerID, label, refreshToken, accountID); err != nil {
		return fmt.Errorf("seed codex account %q:\n%w", label, err)
	}
	return nil
}

// providerIDByName resolves the integer id of the named provider, memoising the result so each
// provider name hits the DB at most once per Store lifetime. The mutex is held during the DB call
// so concurrent misses for the same name serialise rather than issue duplicate queries.
func (s *Store) providerIDByName(ctx context.Context, name string) (int64, error) {
	s.providerIDMu.Lock()
	defer s.providerIDMu.Unlock()

	if id, ok := s.providerIDCache[name]; ok {
		return id, nil
	}

	const query = `SELECT id FROM provider WHERE name = $1`

	var id int64
	if err := s.pool.QueryRow(ctx, query, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("resolve provider %q:\n%w", name, err)
	}

	s.providerIDCache[name] = id
	return id, nil
}
