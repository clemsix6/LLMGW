package config

import "testing"

// TestLoadParsesCodexAccounts verifies that LLMGW_CODEX_ACCOUNTS is parsed into
// the CodexAccounts slice with all three fields populated correctly.
func TestLoadParsesCodexAccounts(t *testing.T) {
	t.Setenv("LLMGW_CODEX_ACCOUNTS", "main:rt_abc:acct_123")
	t.Setenv("LLMGW_POSTGRES_DSN", "postgres://x")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CodexAccounts) != 1 || cfg.CodexAccounts[0].AccountID != "acct_123" {
		t.Fatalf("CodexAccounts = %+v", cfg.CodexAccounts)
	}
}

// TestParseCodexTripletRejectsFourFields verifies that a four-colon-separated entry is rejected,
// so a token or account_id containing a colon does not silently fold into the wrong field.
func TestParseCodexTripletRejectsFourFields(t *testing.T) {
	t.Setenv("LLMGW_CODEX_ACCOUNTS", "main:rt_abc:acct_123:extra")
	t.Setenv("LLMGW_POSTGRES_DSN", "postgres://x")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for four-field codex triplet, got nil")
	}
}
