package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// PriceRuleFor resolves the most specific price rule effective at requestedAt.
func (s *Store) PriceRuleFor(
	ctx context.Context,
	provider string,
	model string,
	serviceTier string,
	requestedAt time.Time,
) (governance.PriceRule, bool, error) {
	rules, err := s.priceCandidates(ctx, provider, serviceTier, requestedAt.UTC())
	if err != nil {
		return governance.PriceRule{}, false, err
	}

	var selected governance.PriceRule
	found := false
	for _, rule := range rules {
		if wildcardMatch(rule.ModelPattern, model) &&
			(!found || priceRuleBetter(rule, selected, provider, serviceTier)) {
			selected = rule
			found = true
		}
	}
	return selected, found, nil
}

// priceCandidates loads only provider, tier, and time-compatible price rules.
func (s *Store) priceCandidates(
	ctx context.Context,
	provider string,
	serviceTier string,
	requestedAt time.Time,
) ([]governance.PriceRule, error) {
	const query = `
SELECT id, provider, model_pattern, service_tier, input_per_million,
       output_per_million, cache_read_per_million,
       cache_creation_per_million, effective_from
FROM model_price
WHERE provider IN ($1, '*')
  AND service_tier IN ($2, '*')
  AND effective_from <= $3`
	rows, err := s.pool.Query(ctx, query, provider, serviceTier, requestedAt)
	if err != nil {
		return nil, fmt.Errorf("load effective price rules:\n%w", err)
	}
	defer rows.Close()

	var rules []governance.PriceRule
	for rows.Next() {
		var rule governance.PriceRule
		if err := rows.Scan(
			&rule.ID,
			&rule.Provider,
			&rule.ModelPattern,
			&rule.ServiceTier,
			&rule.InputPerMillion,
			&rule.OutputPerMillion,
			&rule.CacheReadPerMillion,
			&rule.CacheCreationPerMillion,
			&rule.EffectiveFrom,
		); err != nil {
			return nil, fmt.Errorf("scan effective price rule:\n%w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective price rules:\n%w", err)
	}
	return rules, nil
}

// priceRuleBetter applies provider, tier, literal-byte, and effective-time priority.
func priceRuleBetter(
	candidate governance.PriceRule,
	current governance.PriceRule,
	provider string,
	serviceTier string,
) bool {
	candidateRank := priceRuleRank(candidate, provider, serviceTier)
	currentRank := priceRuleRank(current, provider, serviceTier)
	for index := range candidateRank {
		if candidateRank[index] != currentRank[index] {
			return candidateRank[index] > currentRank[index]
		}
	}
	if !candidate.EffectiveFrom.Equal(current.EffectiveFrom) {
		return candidate.EffectiveFrom.After(current.EffectiveFrom)
	}
	return candidate.ID > current.ID
}

// priceRuleRank returns exact-provider, exact-tier, and literal-byte specificity.
func priceRuleRank(
	rule governance.PriceRule,
	provider string,
	serviceTier string,
) [3]int {
	return [3]int{
		boolRank(rule.Provider == provider),
		boolRank(rule.ServiceTier == serviceTier),
		literalBytes(rule.ModelPattern),
	}
}

// boolRank converts exact-match truth into an ordered integer rank.
func boolRank(value bool) int {
	if value {
		return 1
	}
	return 0
}

// literalBytes counts non-wildcard bytes in a model pattern.
func literalBytes(pattern string) int {
	count := 0
	for index := 0; index < len(pattern); index++ {
		if pattern[index] != '*' {
			count++
		}
	}
	return count
}

// wildcardMatch matches bytes where only an asterisk has wildcard meaning.
func wildcardMatch(pattern string, value string) bool {
	patternIndex, valueIndex := 0, 0
	starIndex, retryIndex := -1, 0
	for valueIndex < len(value) {
		if patternIndex < len(pattern) && pattern[patternIndex] == value[valueIndex] {
			patternIndex++
			valueIndex++
			continue
		}
		if patternIndex < len(pattern) && pattern[patternIndex] == '*' {
			starIndex = patternIndex
			retryIndex = valueIndex
			patternIndex++
			continue
		}
		if starIndex < 0 {
			return false
		}
		retryIndex++
		valueIndex = retryIndex
		patternIndex = starIndex + 1
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// RecordAttempt atomically inserts one attempt and resolves its generation parent.
func (s *Store) RecordAttempt(ctx context.Context, attempt governance.UsageAttempt) error {
	if attempt.CreatedAt.IsZero() {
		return fmt.Errorf("record usage attempt %q:\ncreation time is required", attempt.ID)
	}
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin usage attempt:\n%w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := validateUsageCorrelation(ctx, tx, attempt); err != nil {
		return err
	}
	inserted, err := insertUsageAttempt(ctx, tx, attempt)
	if err != nil {
		return err
	}
	if inserted {
		if err := observeAttemptParent(ctx, tx, attempt); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit usage attempt:\n%w", err)
	}
	return nil
}
