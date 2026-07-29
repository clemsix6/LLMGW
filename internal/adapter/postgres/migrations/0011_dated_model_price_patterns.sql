-- Providers resolve aliases to dated model identifiers: a request for
-- "claude-haiku-4-5" is served and reported as "claude-haiku-4-5-20251001".
-- Seeded patterns only matched the undated name, so every real attempt was
-- recorded as unknown_pricing and cost budgets could never block.
--
-- Add a dated sibling for every literal pattern. The "-" keeps the wildcard
-- anchored to a version suffix, and rule selection already prefers the most
-- literal match, so a more specific rule still wins.
INSERT INTO model_price (
    provider, model_pattern, service_tier,
    input_per_million, output_per_million,
    cache_read_per_million, cache_creation_per_million,
    effective_from
)
SELECT provider, model_pattern || '-*', service_tier,
       input_per_million, output_per_million,
       cache_read_per_million, cache_creation_per_million,
       effective_from
FROM model_price
WHERE position('*' in model_pattern) = 0
ON CONFLICT (provider, model_pattern, service_tier, effective_from) DO NOTHING;
