package governance

import (
	"errors"
	"time"
)

// ErrUsageCorrelation reports a usage attempt whose authenticated request and
// key correlation does not match its durable request parent.
var ErrUsageCorrelation = errors.New("usage correlation does not match request parent")

// Operation identifies the governed kind of downstream request.
type Operation string

const (
	// OperationGeneration identifies a model generation request.
	OperationGeneration Operation = "generation"
	// OperationMetadata identifies a non-generation metadata request.
	OperationMetadata Operation = "metadata"
)

// RequestState identifies the lifecycle state of a request.
type RequestState string

const (
	// RequestInFlight identifies a request that has not completed.
	RequestInFlight RequestState = "in_flight"
	// RequestCompleted identifies a request that has completed.
	RequestCompleted RequestState = "completed"
)

// AccountingState identifies the accounting resolution of a request.
type AccountingState string

const (
	// AccountingPending identifies a request awaiting accounting evidence.
	AccountingPending AccountingState = "pending"
	// AccountingObserved identifies a request with observed usage.
	AccountingObserved AccountingState = "observed"
	// AccountingUnknown identifies a request whose usage could not be determined.
	AccountingUnknown AccountingState = "accounting_unknown"
	// AccountingResolvedZero identifies unknown accounting resolved as zero usage.
	AccountingResolvedZero AccountingState = "resolved_zero"
	// AccountingNotApplicable identifies a request that does not require usage accounting.
	AccountingNotApplicable AccountingState = "not_applicable"
)

// PricingState identifies whether an attempt has a known price.
type PricingState string

const (
	// PricingPriced identifies an attempt with a resolved price.
	PricingPriced PricingState = "priced"
	// PricingUnknown identifies an attempt without a resolved price.
	PricingUnknown PricingState = "unknown_pricing"
)

// RequestEvent records a governed downstream request.
type RequestEvent struct {
	ID                   string          // ID is the request UUID.
	ProjectID            int64           // ProjectID identifies the governed project.
	ClientKeyID          int64           // ClientKeyID identifies the authenticating key.
	Operation            Operation       // Operation identifies the request kind.
	RequestedAt          time.Time       // RequestedAt is the UTC admission time.
	CompletedAt          *time.Time      // CompletedAt is the optional UTC completion time.
	Method               string          // Method is the downstream HTTP method.
	Path                 string          // Path is the downstream request path.
	RequestedModel       *string         // RequestedModel is the optional requested model alias.
	State                RequestState    // State identifies the request lifecycle state.
	AccountingState      AccountingState // AccountingState identifies usage resolution.
	DownstreamStatus     *int            // DownstreamStatus is the optional returned HTTP status.
	AccountingResolvedAt *time.Time      // AccountingResolvedAt is the optional UTC resolution time.
}

// TokenBreakdown contains normalized token categories for one usage attempt.
type TokenBreakdown struct {
	UncachedInput int64 // UncachedInput is the uncached input-token count.
	CacheRead     int64 // CacheRead is the cache-read token count.
	CacheCreation int64 // CacheCreation is the cache-creation token count.
	Output        int64 // Output is the output-token count.
	Reasoning     int64 // Reasoning is the reasoning-token count.
	Total         int64 // Total is the provider-reported total-token count.
	Unclassified  int64 // Unclassified is the token count without a known category.
}

// UsageAttempt records normalized usage for one upstream execution attempt.
type UsageAttempt struct {
	ID                  string         // ID is the attempt UUID.
	RequestID           string         // RequestID identifies the owning request UUID.
	ClientKeyPublicID   string         // ClientKeyPublicID authenticates the owning request key.
	Provider            string         // Provider identifies the serving provider.
	ExecutorType        string         // ExecutorType identifies the SDK executor.
	ResolvedModel       string         // ResolvedModel is the upstream model used.
	RequestedAlias      string         // RequestedAlias is the model alias from the request.
	UpstreamAuthID      string         // UpstreamAuthID identifies the provider credential.
	UpstreamAuthType    string         // UpstreamAuthType identifies the credential kind.
	Tokens              TokenBreakdown // Tokens contains normalized token counts.
	ServiceTier         string         // ServiceTier is the requested pricing tier.
	ResponseServiceTier string         // ResponseServiceTier is the observed pricing tier.
	Failed              bool           // Failed reports whether the attempt failed.
	UpstreamStatus      *int           // UpstreamStatus is the optional upstream HTTP status.
	Latency             time.Duration  // Latency is the end-to-end upstream duration.
	TTFT                time.Duration  // TTFT is the upstream time to first token.
	CostUSD             *float64       // CostUSD is the optional priced cost in USD.
	PricingState        PricingState   // PricingState identifies price resolution.
	CreatedAt           time.Time      // CreatedAt is the UTC attempt creation time.
}

// ReconcileResult summarizes accounting reconciliation outcomes.
type ReconcileResult struct {
	Observed int64 // Observed is the number of requests resolved with usage.
	Unknown  int64 // Unknown is the number of requests left without known usage.
}
