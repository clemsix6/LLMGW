package e2e

// This suite exercises the REAL multi-account happy path through the full gateway: with >=2 real
// accounts configured in LLMGW_OAUTH_REFRESH_TOKENS, a wave of requests is served by rotating
// across the pool. It is GATED on having >=2 configured accounts and SKIPS with a clear message
// when only one is present (the current single-account configuration), so it never requires a
// second real token to pass the suite.
//
// Note: it refreshes each configured token once (the refresh tokens rotate); with >=2 accounts the
// TestMain .env write-back (single-account oriented) does not persist every rotation, so a >=2
// account run should re-seed tokens deliberately. This is out of scope while one account is configured.
//
//	set -a; . ./.env; set +a; go test ./test/e2e -run MultiAccount -v

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"

	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
)

// accountSeed is one configured account's label and seed refresh token.
type accountSeed struct {
	label string // label is the account label (oauth_token account_label).

	token string // token is the seed OAuth refresh token.
}

// TestMultiAccountRealHappyPath refreshes each configured account once, seeds them into the pool,
// and fires a wave of real requests, asserting every one is served (rotation across accounts).
func TestMultiAccountRealHappyPath(t *testing.T) {
	accounts := configuredAccounts()
	if len(accounts) < 2 {
		t.Skipf("multi-account test needs >=2 entries in LLMGW_OAUTH_REFRESH_TOKENS, found %d; skipping", len(accounts))
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startMultiAccountHarness(t, ctx, accounts)

	const project, tag = "multi-account", "news"
	for i := 0; i < 2*len(accounts); i++ { // 2 calls per account so the round-robin visits each.
		body := successfulCall(t, ctx, harness, project, tag)
		assertPlausibleMessagesBody(t, body)
	}
}

// startMultiAccountHarness boots the gateway, refreshes each configured account's token once, and
// seeds them all into the pool so requests rotate across the accounts.
func startMultiAccountHarness(t *testing.T, ctx context.Context, accounts []accountSeed) *Harness {
	t.Helper()

	harness, err := Start(ctx)
	if err != nil {
		t.Fatalf("start harness: %v", err)
	}
	t.Cleanup(func() { harness.Stop(context.Background()) })

	for _, account := range accounts {
		token, err := claudemax.Refresh(ctx, account.token)
		if err != nil {
			t.Fatalf("refresh account %q (re-seed required): %v", account.label, err)
		}
		if err := harness.SeedClaudeMax(ctx, account.label, token, testClaudeCodeVersion); err != nil {
			t.Fatalf("seed account %q: %v", account.label, err)
		}
	}
	return harness
}

// configuredAccounts parses LLMGW_OAUTH_REFRESH_TOKENS into label=token pairs.
func configuredAccounts() []accountSeed {
	raw := strings.TrimSpace(os.Getenv("LLMGW_OAUTH_REFRESH_TOKENS"))
	if raw == "" {
		return nil
	}

	var seeds []accountSeed
	for _, pair := range strings.Split(raw, ",") {
		label, token, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		seeds = append(seeds, accountSeed{label: strings.TrimSpace(label), token: strings.TrimSpace(token)})
	}
	return seeds
}
