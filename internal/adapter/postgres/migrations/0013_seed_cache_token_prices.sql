-- Fills the cache token rates the seeded prices never carried.
--
-- Migration 0010 copied the legacy two-column prices into model_price with
-- cache_read_per_million and cache_creation_per_million literally NULL, and
-- 0011 duplicated that gap onto the dated patterns. A null rate is not free:
-- cost.Calculate returns known=false as soon as a bucket has tokens and no
-- rate, the attempt is recorded as unknown_pricing, and unknown pricing
-- blocks an active cost budget by design.
--
-- Anthropic reports cache_read_input_tokens on nearly every Claude Code
-- request, so any project with a cost budget was refused from its first real
-- request and stayed refused for the whole window. Cost also read as zero in
-- usage reports, since a null cost sums to nothing.
--
-- Rates are derived from each provider's published multipliers rather than
-- typed in, so they stay correct for every model already seeded:
--   Anthropic  cache read 0.1x input, cache write 1.25x input (5-minute TTL)
--   OpenAI     cached input 0.1x input, no separate cache-creation charge
--
-- Only rows that are still null are touched, so an operator who priced their
-- own models keeps their values.

UPDATE model_price
SET cache_read_per_million     = COALESCE(cache_read_per_million, input_per_million * 0.1),
    cache_creation_per_million = COALESCE(cache_creation_per_million, input_per_million * 1.25)
WHERE model_pattern LIKE 'claude-%'
  AND input_per_million IS NOT NULL
  AND (cache_read_per_million IS NULL OR cache_creation_per_million IS NULL);

UPDATE model_price
SET cache_read_per_million     = COALESCE(cache_read_per_million, input_per_million * 0.1),
    cache_creation_per_million = COALESCE(cache_creation_per_million, 0)
WHERE model_pattern NOT LIKE 'claude-%'
  AND input_per_million IS NOT NULL
  AND (cache_read_per_million IS NULL OR cache_creation_per_million IS NULL);
