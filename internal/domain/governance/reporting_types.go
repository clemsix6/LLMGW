package governance

import "time"

// PriceRule defines effective provider pricing for a model and service tier.
type PriceRule struct {
	ID                      int64     // ID is the database identifier.
	Provider                string    // Provider is the provider name or wildcard.
	ModelPattern            string    // ModelPattern identifies matching resolved models.
	ServiceTier             string    // ServiceTier is the matching tier or wildcard.
	InputPerMillion         *float64  // InputPerMillion is the optional uncached input price.
	OutputPerMillion        *float64  // OutputPerMillion is the optional output price.
	CacheReadPerMillion     *float64  // CacheReadPerMillion is the optional cache-read price.
	CacheCreationPerMillion *float64  // CacheCreationPerMillion is the optional cache-write price.
	EffectiveFrom           time.Time // EffectiveFrom is the UTC start of rule validity.
}

// UsageQuery selects and groups project usage.
type UsageQuery struct {
	Project string    // Project is the unique project name.
	Since   time.Time // Since is the inclusive UTC query boundary.
	GroupBy string    // GroupBy identifies the requested reporting dimension.
}

// UsageSummary contains one usage reporting group.
type UsageSummary struct {
	Group             string  // Group is the grouped dimension value.
	Calls             int64   // Calls is the number of generation requests.
	Tokens            int64   // Tokens is the normalized total-token count.
	CostUSD           float64 // CostUSD is the priced cost in USD.
	FailedAttempts    int64   // FailedAttempts is the number of failed upstream attempts.
	UnknownPricing    int64   // UnknownPricing is the number of unpriced attempts.
	UnknownAccounting int64   // UnknownAccounting is the number of unresolved requests.
}
