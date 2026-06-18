package e2e

// This suite exercises the REAL multi-account happy path through the full gateway: with >=2 real
// accounts configured in LLMGW_SESSION_KEYS, a wave of requests is served by rotating across the
// pool. It is GATED on having >=2 configured accounts and SKIPS with a clear message when only one
// is present (the current single-account configuration), so it never requires a second real
// session key to pass the suite.
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

// accountSeed is one configured account's label and seed session key.
type accountSeed struct {
	label string // label is the account label (oauth_token account_label).

	key string // key is the durable claude.ai session key.
}

// TestMultiAccountRealHappyPath bootstraps each configured account once, seeds them into the pool,
// and fires a wave of real requests, asserting every one is served (rotation across accounts).
func TestMultiAccountRealHappyPath(t *testing.T) {
	accounts := configuredAccounts()
	if len(accounts) < 2 {
		t.Skipf("multi-account test needs >=2 entries in LLMGW_SESSION_KEYS, found %d; skipping", len(accounts))
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

// startMultiAccountHarness boots the gateway, bootstraps each configured account from its session
// key once, and seeds them all into the pool so requests rotate across the accounts.
func startMultiAccountHarness(t *testing.T, ctx context.Context, accounts []accountSeed) *Harness {
	t.Helper()

	harness, err := Start(ctx)
	if err != nil {
		t.Fatalf("start harness: %v", err)
	}
	t.Cleanup(func() { harness.Stop(context.Background()) })

	for _, account := range accounts {
		token, err := claudemax.Bootstrap(ctx, account.key, testClaudeCodeVersion)
		if err != nil {
			t.Fatalf("bootstrap account %q (re-seed required): %v", account.label, err)
		}
		if err := harness.SeedClaudeMax(ctx, account.label, token, testClaudeCodeVersion); err != nil {
			t.Fatalf("seed account %q: %v", account.label, err)
		}
	}
	return harness
}

// configuredAccounts parses LLMGW_SESSION_KEYS into label=key pairs.
func configuredAccounts() []accountSeed {
	raw := strings.TrimSpace(os.Getenv("LLMGW_SESSION_KEYS"))
	if raw == "" {
		return nil
	}

	var seeds []accountSeed
	for _, pair := range strings.Split(raw, ",") {
		label, key, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		seeds = append(seeds, accountSeed{label: strings.TrimSpace(label), key: strings.TrimSpace(key)})
	}
	return seeds
}
