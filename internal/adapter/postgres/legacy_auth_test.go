package postgres

import (
	"context"
	"testing"
	"time"
)

// TestLegacyCredentialsReadsProviderRowsWithoutMutation catches a repository mutation that
// filters away legacy rows, loses nullable fields, or changes source credentials while listing.
func TestLegacyCredentialsReadsProviderRowsWithoutMutation(t *testing.T) {
	store := newGovernanceStore(t)
	ctx := context.Background()
	expires := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	const insert = `
INSERT INTO oauth_token (provider_id, account_label, access_token, refresh_token, session_key, chatgpt_account_id, expires_at)
VALUES ((SELECT id FROM provider WHERE type = $1), $2, $3, $4, $5, $6, $7)`
	if _, err := store.pool.Exec(ctx, insert, "claude_max_oauth", "claude-label", "claude-access", "claude-refresh", "claude-session", nil, expires); err != nil {
		t.Fatalf("seed claude credential: %v", err)
	}
	if _, err := store.pool.Exec(ctx, insert, "chatgpt_codex_oauth", "codex-label", "codex-access", "codex-refresh", nil, "codex-account", nil); err != nil {
		t.Fatalf("seed codex credential: %v", err)
	}

	credentials, err := store.LegacyCredentials(ctx)

	if err != nil {
		t.Fatalf("list legacy credentials: %v", err)
	}
	if len(credentials) != 2 {
		t.Fatalf("credentials = %#v, want 2", credentials)
	}
	if credentials[0].Provider != "chatgpt_codex_oauth" || credentials[0].AccountLabel != "codex-label" || credentials[0].ChatGPTAccountID != "codex-account" {
		t.Fatalf("codex credential = %#v", credentials[0])
	}
	if credentials[1].Provider != "claude_max_oauth" || credentials[1].AccountLabel != "claude-label" || credentials[1].SessionKey != "claude-session" || credentials[1].ExpiresAt == nil || !credentials[1].ExpiresAt.Equal(expires) {
		t.Fatalf("claude credential = %#v", credentials[1])
	}

	var rows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_token WHERE account_label IN ('claude-label', 'codex-label')`).Scan(&rows); err != nil {
		t.Fatalf("count source credentials: %v", err)
	}
	if rows != 2 {
		t.Fatalf("source rows after list = %d, want 2", rows)
	}
}
