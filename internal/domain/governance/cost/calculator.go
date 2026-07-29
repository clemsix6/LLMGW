package cost

import (
	"math"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

const tokensPerMillion = 1_000_000

// pricedTokenBucket pairs one canonical token count with its optional rate.
type pricedTokenBucket struct {
	tokens int64    // tokens is the canonical non-overlapping bucket count.
	rate   *float64 // rate is the optional USD per-million-token rate.
}

// Calculate returns the notional USD cost of a complete canonical token breakdown.
func Calculate(tokens governance.TokenBreakdown, rule governance.PriceRule) (float64, bool) {
	if !validTokens(tokens) {
		return 0, false
	}

	cost, known := pricedBucket(tokens.UncachedInput, rule.InputPerMillion)
	if !known {
		return 0, false
	}
	for _, bucket := range []pricedTokenBucket{
		{tokens.CacheRead, rule.CacheReadPerMillion},
		{tokens.CacheCreation, rule.CacheCreationPerMillion},
		{tokens.Output, rule.OutputPerMillion},
	} {
		value, bucketKnown := pricedBucket(bucket.tokens, bucket.rate)
		if !bucketKnown {
			return 0, false
		}
		cost += value
	}
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return 0, false
	}
	return cost / tokensPerMillion, true
}

// validTokens verifies the domain's non-overlapping accounting invariants.
func validTokens(tokens governance.TokenBreakdown) bool {
	if tokens.UncachedInput < 0 || tokens.CacheRead < 0 ||
		tokens.CacheCreation < 0 || tokens.Output < 0 ||
		tokens.Reasoning < 0 || tokens.Total < 0 ||
		tokens.Unclassified != 0 || tokens.Reasoning > tokens.Output {
		return false
	}
	classified, ok := nonNegativeSum(
		tokens.UncachedInput,
		tokens.CacheRead,
		tokens.CacheCreation,
		tokens.Output,
	)
	return ok && classified == tokens.Total
}

// nonNegativeSum rejects integer overflow across canonical token buckets.
func nonNegativeSum(values ...int64) (int64, bool) {
	var total int64
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

// pricedBucket returns one finite bucket contribution before million-token scaling.
func pricedBucket(tokens int64, rate *float64) (float64, bool) {
	if tokens == 0 {
		return 0, true
	}
	if rate == nil || math.IsNaN(*rate) || math.IsInf(*rate, 0) || *rate < 0 {
		return 0, false
	}
	value := float64(tokens) * *rate
	return value, !math.IsNaN(value) && !math.IsInf(value, 0)
}
