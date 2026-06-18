package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// LoadAccounts returns every account under the default provider with its cooldown state, ordered
// by label so the provider pool selects accounts in a stable round-robin order. A NULL
// cooldown_until (never rate-limited) maps to the zero time.
func (s *Store) LoadAccounts(ctx context.Context) ([]domain.Account, error) {
	providerID, err := s.defaultProviderID(ctx)
	if err != nil {
		return nil, err
	}

	const query = `
SELECT account_label, cooldown_until
FROM oauth_token
WHERE provider_id = $1
ORDER BY account_label`

	rows, err := s.pool.Query(ctx, query, providerID)
	if err != nil {
		return nil, fmt.Errorf("load accounts:\n%w", err)
	}
	defer rows.Close()

	return scanAccounts(rows)
}

// scanAccounts reads the account rows into a slice, mapping a NULL cooldown_until to the zero time.
func scanAccounts(rows pgx.Rows) ([]domain.Account, error) {
	var accounts []domain.Account

	for rows.Next() {
		var account domain.Account
		var cooldown *time.Time

		if err := rows.Scan(&account.Label, &cooldown); err != nil {
			return nil, fmt.Errorf("scan account:\n%w", err)
		}
		if cooldown != nil {
			account.CooldownUntil = *cooldown
		}
		accounts = append(accounts, account)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts:\n%w", err)
	}
	return accounts, nil
}

// SetCooldown records that an account is rate-limited until the given time. It updates only the
// cooldown column; the OAuth token columns are owned by the refresh path.
func (s *Store) SetCooldown(ctx context.Context, account string, until time.Time) error {
	providerID, err := s.defaultProviderID(ctx)
	if err != nil {
		return err
	}

	const query = `
UPDATE oauth_token SET cooldown_until = $3, updated_at = now()
WHERE provider_id = $1 AND account_label = $2`

	if _, err := s.pool.Exec(ctx, query, providerID, account, until); err != nil {
		return fmt.Errorf("set cooldown for %q:\n%w", account, err)
	}
	return nil
}
