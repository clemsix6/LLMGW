package cliproxy

import (
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// mapUsageRecord copies only approved normalized SDK fields.
func mapUsageRecord(
	record sdkusage.Record,
	correlation usageCorrelation,
	attemptID string,
) governance.UsageAttempt {
	detail := sdkusage.EnsureTokenBreakdownForProvider(
		record.Detail,
		record.Provider,
		record.ExecutorType,
	)
	breakdown := detail.TokenBreakdown
	return governance.UsageAttempt{
		ID:                  attemptID,
		RequestID:           correlation.requestID,
		ClientKeyPublicID:   correlation.keyPublicID,
		Provider:            record.Provider,
		ExecutorType:        record.ExecutorType,
		ResolvedModel:       resolvedModel(record),
		RequestedAlias:      record.Alias,
		UpstreamAuthID:      record.AuthID,
		UpstreamAuthType:    record.AuthType,
		Tokens:              mapTokenBreakdown(breakdown),
		ServiceTier:         record.ServiceTier,
		ResponseServiceTier: record.ResponseServiceTier,
		Failed:              record.Failed,
		UpstreamStatus:      positiveStatus(record.Fail.StatusCode),
		UpstreamError:       upstreamError(record),
		Latency:             nonNegativeDuration(record.Latency),
		TTFT:                nonNegativeDuration(record.TTFT),
		CreatedAt:           record.RequestedAt.UTC(),
	}
}

// resolvedModel names the model that served the answer. The SDK reports it as
// the response model; when the upstream did not say, the model the request was
// routed to stands in. A fallback the upstream applied on its own, such as a
// refusal fallback, is therefore visible in the record.
func resolvedModel(record sdkusage.Record) string {
	if record.ResponseModel != "" {
		return record.ResponseModel
	}
	return record.Model
}

// mapTokenBreakdown converts the SDK's v2 canonical buckets without overlap.
func mapTokenBreakdown(breakdown sdkusage.TokenBreakdown) governance.TokenBreakdown {
	return governance.TokenBreakdown{
		UncachedInput: breakdown.Input.UncachedTokens,
		CacheRead:     breakdown.Input.CacheReadTokens,
		CacheCreation: breakdown.Input.CacheWriteTokens,
		Output:        breakdown.Output.TotalTokens,
		Reasoning:     breakdown.Output.ReasoningTokens,
		Total:         breakdown.TotalTokens,
		Unclassified:  breakdown.UnclassifiedTokens,
	}
}

// positiveStatus copies only actual positive upstream status values.
func positiveStatus(status int) *int {
	if status <= 0 {
		return nil
	}
	copy := status
	return &copy
}

// nonNegativeDuration prevents invalid SDK durations from breaking audit persistence.
func nonNegativeDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}

// upstreamError keeps the error type and message of a failed 4xx attempt when
// the SDK handed over the standard error JSON body, and nothing otherwise.
func upstreamError(record sdkusage.Record) *governance.ErrorDetail {
	if !record.Failed || !governance.IsClientErrorStatus(record.Fail.StatusCode) {
		return nil
	}
	return governance.ParseErrorDetail([]byte(record.Fail.Body))
}
