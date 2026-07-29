package command

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestUsageCommandShowsWhitelistedGroupsAndExplicitResolution catches a command mutation that
// accepts unreviewed SQL grouping input, omits accounting state, or resolves without consent.
func TestUsageCommandShowsWhitelistedGroupsAndExplicitResolution(t *testing.T) {
	dsn := commandStore(t)
	streams := commandStreams(t, dsn)
	if err := runKey(context.Background(), []string{"create", "truewallet", "--name", "server-1"}, streams); err != nil {
		t.Fatalf("key create: %v", err)
	}
	store, err := postgres.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open reporting store: %v", err)
	}
	t.Cleanup(store.Close)
	key := usageTestKey(t, context.Background(), store)
	now := time.Now().UTC().Add(-time.Hour)
	requestID := usageTestRequest(t, context.Background(), store, key, now)
	pricedCost := 0.25
	usageTestObservedRequest(t, context.Background(), store, key,
		"00000000-0000-4000-8000-000000000010", "00000000-0000-4000-8000-000000000110",
		"provider-one", "resolved-one", 30, &pricedCost, false, governance.PricingPriced, now.Add(time.Minute))
	usageTestObservedRequest(t, context.Background(), store, key,
		"00000000-0000-4000-8000-000000000011", "00000000-0000-4000-8000-000000000111",
		"provider-two", "resolved-two", 20, nil, true, governance.PricingUnknown, now.Add(2*time.Minute))

	for _, group := range []string{"key", "model", "provider"} {
		streams.Out.(*bytes.Buffer).Reset()
		if err := runUsage(context.Background(), []string{"show", "truewallet", "--since", "24h", "--by", group}, streams); err != nil {
			t.Fatalf("usage show %s: %v", group, err)
		}
		if strings.Contains(streams.Out.(*bytes.Buffer).String(), "upstream-secret-id") {
			t.Fatal("usage output leaked upstream auth identifier")
		}
		groups := parseUsageGroups(t, streams.Out.(*bytes.Buffer).String())
		switch group {
		case "key":
			assertUsageGroup(t, groups, "server-1", "3", "50", "0.25", "1", "1", "1")
		case "model":
			assertUsageGroup(t, groups, "requested-only", "1", "0", "0", "0", "0", "1")
			assertUsageGroup(t, groups, "resolved-one", "1", "30", "0.25", "0", "0", "0")
			assertUsageGroup(t, groups, "resolved-two", "1", "20", "0", "1", "1", "0")
		case "provider":
			assertUsageGroup(t, groups, "", "1", "0", "0", "0", "0", "1")
			assertUsageGroup(t, groups, "provider-one", "1", "30", "0.25", "0", "0", "0")
			assertUsageGroup(t, groups, "provider-two", "1", "20", "0", "1", "1", "0")
		}
	}
	if err := runUsage(context.Background(), []string{"show", "truewallet", "--since", "24h", "--by", "key; DROP TABLE project;--"}, streams); err == nil {
		t.Fatal("usage show accepted raw grouping expression")
	}
	if err := runUsage(context.Background(), []string{"resolve", requestID}, streams); err == nil {
		t.Fatal("usage resolve accepted missing --assume-zero")
	}
	if err := runUsage(context.Background(), []string{"resolve", requestID, "--assume-zero=true"}, streams); err == nil {
		t.Fatal("usage resolve accepted --assume-zero=true instead of the literal confirmation flag")
	}
	streams.Out.(*bytes.Buffer).Reset()
	if err := runUsage(context.Background(), []string{"resolve", requestID, "--assume-zero"}, streams); err != nil {
		t.Fatalf("usage resolve: %v", err)
	}
	resolved := streams.Out.(*bytes.Buffer).String()
	if !strings.Contains(resolved, requestID) || !strings.Contains(resolved, "accounting_unknown") || !strings.Contains(resolved, "resolved_zero") {
		t.Fatalf("resolution output = %q", resolved)
	}
}

// usageTestKey finds the key created through the command's real project-key service.
func usageTestKey(t *testing.T, ctx context.Context, store *postgres.Store) governance.ClientKey {
	t.Helper()
	keys, err := store.ListKeys(ctx, "truewallet")
	if err != nil || len(keys) != 1 {
		t.Fatalf("list test key = (%v, %d)", err, len(keys))
	}
	return keys[0]
}

// usageTestRequest persists an explicit unknown generation for resolution testing.
func usageTestRequest(t *testing.T, ctx context.Context, store *postgres.Store, key governance.ClientKey, at time.Time) string {
	t.Helper()
	requestID := "00000000-0000-4000-8000-000000000009"
	model := "requested-only"
	if _, err := store.AdmitGeneration(ctx, governance.RequestEvent{
		ID: requestID, ProjectID: key.ProjectID, ClientKeyID: key.ID,
		Operation: governance.OperationGeneration, RequestedAt: at,
		Method: "POST", Path: "/v1/chat/completions", RequestedModel: &model,
		State: governance.RequestInFlight, AccountingState: governance.AccountingPending,
	}, at); err != nil {
		t.Fatalf("admit request: %v", err)
	}
	if err := store.CompleteRequest(ctx, requestID, 500, at.Add(time.Second)); err != nil {
		t.Fatalf("complete request: %v", err)
	}
	if _, err := store.ReconcileAccounting(ctx, at.Add(time.Hour), time.Second, time.Hour); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	return requestID
}

// usageTestObservedRequest persists one completed request and its real normalized attempt.
func usageTestObservedRequest(t *testing.T, ctx context.Context, store *postgres.Store, key governance.ClientKey, requestID, attemptID, provider, model string, tokens int64, cost *float64, failed bool, pricing governance.PricingState, at time.Time) {
	t.Helper()
	requested := "requested-alias"
	if _, err := store.AdmitGeneration(ctx, governance.RequestEvent{
		ID: requestID, ProjectID: key.ProjectID, ClientKeyID: key.ID,
		Operation: governance.OperationGeneration, RequestedAt: at,
		Method: "POST", Path: "/v1/chat/completions", RequestedModel: &requested,
		State: governance.RequestInFlight, AccountingState: governance.AccountingPending,
	}, at); err != nil {
		t.Fatalf("admit observed request: %v", err)
	}
	if err := store.CompleteRequest(ctx, requestID, 200, at.Add(time.Second)); err != nil {
		t.Fatalf("complete observed request: %v", err)
	}
	if err := store.RecordAttempt(ctx, governance.UsageAttempt{
		ID: attemptID, RequestID: requestID, ClientKeyPublicID: key.PublicID,
		Provider: provider, ExecutorType: "test", ResolvedModel: model, RequestedAlias: requested,
		UpstreamAuthID: "upstream-secret-id", UpstreamAuthType: "oauth",
		Tokens: governance.TokenBreakdown{Total: tokens}, ServiceTier: "standard", ResponseServiceTier: "standard",
		Failed: failed, CostUSD: cost, PricingState: pricing, CreatedAt: at,
	}); err != nil {
		t.Fatalf("record observed attempt: %v", err)
	}
}

// parseUsageGroups parses stable command output into literal values keyed by report group.
func parseUsageGroups(t *testing.T, output string) map[string]map[string]string {
	t.Helper()
	groups := make(map[string]map[string]string)
	var current map[string]string
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed usage output line %q", line)
		}
		if parts[0] == "group" {
			current = make(map[string]string)
			groups[parts[1]] = current
			continue
		}
		if current == nil {
			t.Fatalf("usage field before group: %q", line)
		}
		current[parts[0]] = parts[1]
	}
	return groups
}

// assertUsageGroup compares one report group to hand-derived aggregate literals.
func assertUsageGroup(t *testing.T, groups map[string]map[string]string, group, calls, tokens, cost, failed, unknownPricing, unknownAccounting string) {
	t.Helper()
	got, ok := groups[group]
	if !ok {
		t.Fatalf("usage group %q missing from %#v", group, groups)
	}
	want := map[string]string{"calls": calls, "tokens": tokens, "cost_usd": cost, "failed_attempts": failed, "unknown_pricing": unknownPricing, "unknown_accounting": unknownAccounting}
	for field, value := range want {
		if got[field] != value {
			t.Fatalf("usage group %q field %s = %q, want %q", group, field, got[field], value)
		}
	}
}
