package postgres

import (
	"context"
	"fmt"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// LegacyCredentials reads historical provider credentials without modifying their source rows.
func (s *Store) LegacyCredentials(ctx context.Context) ([]governance.LegacyCredential, error) {
	const query = `
SELECT p.type, o.account_label, o.access_token, o.refresh_token,
       o.chatgpt_account_id, o.expires_at
FROM oauth_token o
JOIN provider p ON p.id = o.provider_id
ORDER BY p.type, o.account_label`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list legacy credentials:\n%w", err)
	}
	defer rows.Close()
	credentials := make([]governance.LegacyCredential, 0)
	for rows.Next() {
		var credential governance.LegacyCredential
		var access, refresh, accountID *string
		if err := rows.Scan(&credential.Provider, &credential.AccountLabel, &access, &refresh, &accountID, &credential.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan legacy credential:\n%w", err)
		}
		if access != nil {
			credential.AccessToken = *access
		}
		if refresh != nil {
			credential.RefreshToken = *refresh
		}
		if accountID != nil {
			credential.ChatGPTAccountID = *accountID
		}
		credentials = append(credentials, credential)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate legacy credentials:\n%w", err)
	}
	return credentials, nil
}
