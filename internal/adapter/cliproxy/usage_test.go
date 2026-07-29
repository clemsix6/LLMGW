package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestUsageRecordMapping verifies only canonical and approved SDK fields persist.
func TestUsageRecordMapping(t *testing.T) {
	requestedAt := time.Date(
		2026, 7, 27, 12, 11, 12, 0,
		time.FixedZone("callback-source", 2*60*60),
	)
	upstreamStatus := http.StatusTooManyRequests
	record := sdkusage.Record{
		Provider:            "openai-compatibility",
		ExecutorType:        "OpenAICompatExecutor",
		Model:               "upstream-model",
		Alias:               "test-model",
		APIKey:              "untrusted-principal",
		AuthID:              "account-a",
		AuthType:            "api-key",
		Source:              "must-not-persist",
		ServiceTier:         "auto",
		ResponseServiceTier: "default",
		RequestedAt:         requestedAt,
		Latency:             1750 * time.Millisecond,
		TTFT:                125 * time.Millisecond,
		Failed:              true,
		Fail: sdkusage.Failure{
			StatusCode: upstreamStatus,
			Body:       "must-not-persist",
		},
		Detail: sdkusage.Detail{
			InputTokens:         100,
			OutputTokens:        60,
			ReasoningTokens:     15,
			CacheReadTokens:     20,
			CacheCreationTokens: 10,
			TotalTokens:         160,
		},
		ResponseHeaders: http.Header{"X-Secret": []string{"must-not-persist"}},
	}

	attemptID := uuid.NewString()
	correlation := usageCorrelation{
		requestID:   "f5efc3a8-e6c3-49fd-bad6-6532fa51d216",
		keyPublicID: "MDEyMzQ1Njc4OWFi",
	}
	got := mapUsageRecord(record, correlation, attemptID)
	wantTokens := governance.TokenBreakdown{
		Total:         160,
		UncachedInput: 70,
		CacheRead:     20,
		CacheCreation: 10,
		Output:        60,
		Reasoning:     15,
	}
	if got.ID != attemptID || got.RequestID != correlation.requestID ||
		got.ClientKeyPublicID != correlation.keyPublicID {
		t.Fatalf("attempt identity = (%q, %q, %q)", got.ID, got.RequestID, got.ClientKeyPublicID)
	}
	if got.Provider != record.Provider || got.ExecutorType != record.ExecutorType ||
		got.ResolvedModel != record.Model || got.RequestedAlias != record.Alias ||
		got.UpstreamAuthID != record.AuthID || got.UpstreamAuthType != record.AuthType {
		t.Fatalf("mapped principal fields = %#v", got)
	}
	if got.Tokens != wantTokens {
		t.Fatalf("tokens = %#v, want %#v", got.Tokens, wantTokens)
	}
	if got.ServiceTier != record.ServiceTier ||
		got.ResponseServiceTier != record.ResponseServiceTier ||
		got.Latency != record.Latency || got.TTFT != record.TTFT ||
		!got.Failed || got.UpstreamStatus == nil || *got.UpstreamStatus != upstreamStatus ||
		!got.CreatedAt.Equal(requestedAt) || got.CreatedAt.Location() != time.UTC {
		t.Fatalf("mapped attempt fields = %#v", got)
	}
}

// TestUsageIdentityBridgeDoesNotFollowReusedGinContext proves delayed usage
// attribution remains attached to request A after Gin recycles the same context
// for request B.
func TestUsageIdentityBridgeDoesNotFollowReusedGinContext(t *testing.T) {
	bridge := fixedUsageBridge(t)
	requestA := uuid.NewString()
	requestB := uuid.NewString()
	identityA := RequestIdentity{RequestID: requestA, KeyPublicID: "MDEyMzQ1Njc4OWFi"}
	identityB := RequestIdentity{RequestID: requestB, KeyPublicID: "YWJjZGVmZ2hpamts"}
	if !bridge.reserve(identityA.RequestID) {
		t.Fatal("reserve request A failed")
	}
	reused := usageGinContext(t, identityA)
	reused.Set("accessProvider", AccessProviderType)
	reused.Set("accessMetadata", map[string]string{"request_id": requestA})
	callbackA := context.WithValue(context.Background(), "gin", reused)
	principalA := principalFor(t, bridge, identityA)

	next := usageGinContext(t, identityB)
	reused.Request = next.Request
	reused.Keys = next.Keys
	reused.Set("accessProvider", AccessProviderType)
	reused.Set("accessMetadata", map[string]string{"request_id": requestB})

	var got governance.UsageAttempt
	repository := successfulUsageRepository(func(attempt governance.UsageAttempt) {
		got = attempt
	})
	NewUsagePlugin(repository, bridge, nil).HandleUsage(
		callbackA,
		usageRecordForTest(principalA),
	)

	if got.RequestID != requestA || got.ClientKeyPublicID != identityA.KeyPublicID {
		t.Fatalf("delayed request A correlation = (%q, %q), want (%q, %q)",
			got.RequestID, got.ClientKeyPublicID, requestA, identityA.KeyPublicID)
	}
}

func TestUsagePluginRejectsUnauthenticatedPrincipal(t *testing.T) {
	bridge := fixedUsageBridge(t)
	repository := successfulUsageRepository(func(governance.UsageAttempt) {
		t.Fatal("malformed principal reached persistence")
	})

	NewUsagePlugin(repository, bridge, nil).HandleUsage(
		context.Background(),
		sdkusage.Record{APIKey: "public-id-or-raw-key"},
	)
}

// usageRecordForTest returns a valid authenticated SDK callback. Tests for
// control records or a missing RequestedAt construct their records directly.
func usageRecordForTest(apiKey string) sdkusage.Record {
	return sdkusage.Record{
		APIKey:      apiKey,
		RequestedAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
	}
}

// usageGinContext creates the public Gin bridge retained by the pinned SDK.
func usageGinContext(t *testing.T, identity RequestIdentity) *gin.Context {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request = request.WithContext(WithIdentity(request.Context(), identity))
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = request
	return context
}

// usageRepositoryStub controls the two Task 7 repository operations.
type usageRepositoryStub struct {
	priceRuleFor  func(context.Context, string, string, string, time.Time) (governance.PriceRule, bool, error) // priceRuleFor controls price lookup.
	recordAttempt func(context.Context, governance.UsageAttempt) error                                         // recordAttempt controls persistence.
}

func successfulUsageRepository(record func(governance.UsageAttempt)) *usageRepositoryStub {
	return &usageRepositoryStub{
		priceRuleFor: func(
			context.Context,
			string,
			string,
			string,
			time.Time,
		) (governance.PriceRule, bool, error) {
			return governance.PriceRule{}, false, nil
		},
		recordAttempt: func(_ context.Context, attempt governance.UsageAttempt) error {
			record(attempt)
			return nil
		},
	}
}

// PriceRuleFor invokes the configured price behavior.
func (s *usageRepositoryStub) PriceRuleFor(
	ctx context.Context,
	provider string,
	model string,
	tier string,
	requestedAt time.Time,
) (governance.PriceRule, bool, error) {
	return s.priceRuleFor(ctx, provider, model, tier, requestedAt)
}

// RecordAttempt invokes the configured persistence behavior.
func (s *usageRepositoryStub) RecordAttempt(
	ctx context.Context,
	attempt governance.UsageAttempt,
) error {
	if s.recordAttempt == nil {
		return nil
	}
	return s.recordAttempt(ctx, attempt)
}
