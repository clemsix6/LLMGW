package cliproxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/google/uuid"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
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

// usageRepositoryStub controls the two Task 7 repository operations.
type usageRepositoryStub struct {
	priceRuleFor  func(context.Context, string, string, string, time.Time) (governance.PriceRule, bool, error) // priceRuleFor controls price lookup.
	recordAttempt func(context.Context, governance.UsageAttempt) error                                         // recordAttempt controls persistence.
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

// TestResolvedModelFollowsTheAnsweringModel proves a fallback turn is visible:
// the recorded model is the one that answered when the SDK reports it, and the
// routed model otherwise.
func TestResolvedModelFollowsTheAnsweringModel(t *testing.T) {
	tests := []struct {
		name          string
		responseModel string
		want          string
	}{
		{name: "empty", responseModel: "", want: "routed-model"},
		{name: "equal", responseModel: "routed-model", want: "routed-model"},
		{name: "different", responseModel: "fallback-model", want: "fallback-model"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := sdkusage.Record{Model: "routed-model", ResponseModel: test.responseModel}
			got := mapUsageRecord(record, usageCorrelation{}, uuid.NewString())
			if got.ResolvedModel != test.want {
				t.Fatalf("ResolvedModel = %q, want %q", got.ResolvedModel, test.want)
			}
		})
	}
}

// TestPricingIgnoresTheAnsweringModel proves recording a different answering
// model leaves pricing alone: the price rule is still looked up by the model
// the request was routed to.
func TestPricingIgnoresTheAnsweringModel(t *testing.T) {
	var pricedModel string
	repo := &usageRepositoryStub{
		priceRuleFor: func(_ context.Context, _, model, _ string, _ time.Time) (governance.PriceRule, bool, error) {
			pricedModel = model
			return governance.PriceRule{}, false, nil
		},
	}
	record := sdkusage.Record{
		Provider:      "claude",
		Model:         "routed-model",
		ResponseModel: "fallback-model",
	}
	attempt := mapUsageRecord(record, usageCorrelation{}, uuid.NewString())

	priced, err := NewUsagePlugin(repo, nil, nil).price(context.Background(), record, attempt)
	if err != nil {
		t.Fatalf("price failed: %v", err)
	}
	if pricedModel != "routed-model" || priced.ResolvedModel != "fallback-model" {
		t.Fatalf("priced by %q, recorded %q; want priced by routed-model, recorded fallback-model",
			pricedModel, priced.ResolvedModel)
	}
}
